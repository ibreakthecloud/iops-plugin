package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

func setupSocket(socketPath string) (net.Listener, error) {
	os.RemoveAll(filepath.Dir(socketPath))
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		return nil, fmt.Errorf("failed to create directory %q: %v", filepath.Dir(socketPath), err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %q: %v", socketPath, err)
	}

	log.Printf("Listening on: unix://%s", socketPath)
	return listener, nil
}

func setupSignals(socketPath string) {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interrupt
		os.RemoveAll(filepath.Dir(socketPath))
		os.Exit(0)
	}()
}

//Iops is the structure for IOPS Json
type Iops struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric struct {
				Name              string `json:"__name__"`
				Instance          string `json:"instance"`
				Job               string `json:"job"`
				KubernetesPodName string `json:"kubernetes_pod_name"`
				OpenebsPv         string `json:"openebs_pv"`
			} `json:"metric"`
			Value []interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func getValue(body []byte) (*Iops, error) {
	var s = new(Iops)
	err := json.Unmarshal(body, &s)
	if err != nil {
		fmt.Println("whoops:", err)
	}
	return s, err
}

func main() {
	// We put the socket in a sub-directory to have more control on the permissions
	const socketPath = "/var/run/scope/plugins/iowait/iowait.sock"
	hostID, _ := os.Hostname()

	url := "cortex-agent-service.maya-system.svc.cluster.local:80/api/v1/query?query=OpenEBS_write_iops"

	// Get request to url
	res, err := http.Get(url)
	if err != nil {
		panic(err.Error())
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		panic(err.Error())
	}

	s, err := getValue([]byte(body))

	logrus.Infof("%+v", s)

	// Handle the exit signal
	setupSignals(socketPath)

	log.Printf("Starting on %s...\n", hostID)

	// Check we can get the iowait for the system
	_, err = iowait()
	if err != nil {
		log.Fatal(err)
	}

	listener, err := setupSocket(socketPath)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		listener.Close()
		os.RemoveAll(filepath.Dir(socketPath))
	}()

	plugin := &Plugin{HostID: hostID}
	http.HandleFunc("/report", plugin.Report)
	http.HandleFunc("/control", plugin.Control)
	if err := http.Serve(listener, nil); err != nil {
		log.Printf("error: %v", err)
	}
}

// Plugin groups the methods a plugin needs
type Plugin struct {
	HostID string

	lock       sync.Mutex
	iowaitMode bool
}

type request struct {
	NodeID  string
	Control string
}

type response struct {
	ShortcutReport *report `json:"shortcutReport,omitempty"`
}

type report struct {
	Host    topology
	Plugins []pluginSpec
}

type topology struct {
	Nodes           map[string]node           `json:"nodes"`
	MetricTemplates map[string]metricTemplate `json:"metric_templates"`
	Controls        map[string]control        `json:"controls"`
}

type node struct {
	Metrics        map[string]metric       `json:"metrics"`
	LatestControls map[string]controlEntry `json:"latestControls,omitempty"`
}

type metric struct {
	Samples []sample `json:"samples,omitempty"`
	Min     float64  `json:"min"`
	Max     float64  `json:"max"`
}

type sample struct {
	Date  time.Time `json:"date"`
	Value float64   `json:"value"`
}

type controlEntry struct {
	Timestamp time.Time   `json:"timestamp"`
	Value     controlData `json:"value"`
}

type controlData struct {
	Dead bool `json:"dead"`
}

type metricTemplate struct {
	ID       string  `json:"id"`
	Label    string  `json:"label,omitempty"`
	Format   string  `json:"format,omitempty"`
	Priority float64 `json:"priority,omitempty"`
}

type control struct {
	ID    string `json:"id"`
	Human string `json:"human"`
	Icon  string `json:"icon"`
	Rank  int    `json:"rank"`
}

type pluginSpec struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Interfaces  []string `json:"interfaces"`
	APIVersion  string   `json:"api_version,omitempty"`
}

func (p *Plugin) makeReport() (*report, error) {
	metrics, err := p.metrics()
	if err != nil {
		return nil, err
	}
	rpt := &report{
		Host: topology{
			Nodes: map[string]node{
				p.getTopologyHost(): {
					Metrics:        metrics,
					LatestControls: p.latestControls(),
				},
			},
			MetricTemplates: p.metricTemplates(),
			Controls:        p.controls(),
		},
		Plugins: []pluginSpec{
			{
				ID:          "iowait",
				Label:       "iowait",
				Description: "Adds a graph of CPU IO Wait to hosts",
				Interfaces:  []string{"reporter", "controller"},
				APIVersion:  "1",
			},
		},
	}
	return rpt, nil
}

func (p *Plugin) metrics() (map[string]metric, error) {
	value, err := p.metricValue()
	if err != nil {
		return nil, err
	}
	id, _ := p.metricIDAndName()
	metrics := map[string]metric{
		id: {
			Samples: []sample{
				{
					Date:  time.Now(),
					Value: value,
				},
			},
			Min: 0,
			Max: 100,
		},
	}
	return metrics, nil
}

func (p *Plugin) latestControls() map[string]controlEntry {
	ts := time.Now()
	ctrls := map[string]controlEntry{}
	for _, details := range p.allControlDetails() {
		ctrls[details.id] = controlEntry{
			Timestamp: ts,
			Value: controlData{
				Dead: details.dead,
			},
		}
	}
	return ctrls
}

func (p *Plugin) metricTemplates() map[string]metricTemplate {
	id, name := p.metricIDAndName()
	return map[string]metricTemplate{
		id: {
			ID:       id,
			Label:    name,
			Format:   "percent",
			Priority: 0.1,
		},
	}
}

func (p *Plugin) controls() map[string]control {
	ctrls := map[string]control{}
	for _, details := range p.allControlDetails() {
		ctrls[details.id] = control{
			ID:    details.id,
			Human: details.human,
			Icon:  details.icon,
			Rank:  1,
		}
	}
	return ctrls
}

// Report is called by scope when a new report is needed. It is part of the
// "reporter" interface, which all plugins must implement.
func (p *Plugin) Report(w http.ResponseWriter, r *http.Request) {
	p.lock.Lock()
	defer p.lock.Unlock()
	log.Println(r.URL.String())
	rpt, err := p.makeReport()
	if err != nil {
		log.Printf("error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	raw, err := json.Marshal(*rpt)
	if err != nil {
		log.Printf("error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

// Control is called by scope when a control is activated. It is part
// of the "controller" interface.
func (p *Plugin) Control(w http.ResponseWriter, r *http.Request) {
	p.lock.Lock()
	defer p.lock.Unlock()
	log.Println(r.URL.String())
	xreq := request{}
	err := json.NewDecoder(r.Body).Decode(&xreq)
	if err != nil {
		log.Printf("Bad request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	thisNodeID := p.getTopologyHost()
	if xreq.NodeID != thisNodeID {
		log.Printf("Bad nodeID, expected %q, got %q", thisNodeID, xreq.NodeID)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	expectedControlID, _, _ := p.controlDetails()
	if expectedControlID != xreq.Control {
		log.Printf("Bad control, expected %q, got %q", expectedControlID, xreq.Control)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p.iowaitMode = !p.iowaitMode
	rpt, err := p.makeReport()
	if err != nil {
		log.Printf("error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res := response{ShortcutReport: rpt}
	raw, err := json.Marshal(res)
	if err != nil {
		log.Printf("error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

func (p *Plugin) getTopologyHost() string {
	store := fmt.Sprintf("%s;<host>", p.HostID)
	logrus.Infof("%+v", store)
	return store
}

func (p *Plugin) metricIDAndName() (string, string) {
	if p.iowaitMode {
		return "iowait", "IO Wait"
	}
	return "idle", "Idle"
}

func (p *Plugin) metricValue() (float64, error) {
	if p.iowaitMode {
		return iowait()
	}
	return idle()
}

type controlDetails struct {
	id    string
	human string
	icon  string
	dead  bool
}

func (p *Plugin) allControlDetails() []controlDetails {
	return []controlDetails{
		{
			id:    "switchToIdle",
			human: "Switch to idle",
			icon:  "fa-gears",
			dead:  !p.iowaitMode,
		},
		{
			id:    "switchToIOWait",
			human: "Switch to IO wait",
			icon:  "fa-clock-o",
			dead:  p.iowaitMode,
		},
	}
}

func (p *Plugin) controlDetails() (string, string, string) {
	for _, details := range p.allControlDetails() {
		if !details.dead {
			return details.id, details.human, details.icon
		}
	}
	return "", "", ""
}

func iowait() (float64, error) {
	return iostatValue(3)
}

func idle() (float64, error) {
	return iostatValue(5)
}

func iostatValue(idx int) (float64, error) {
	values, err := iostat()
	if err != nil {
		return 0, err
	}
	if idx >= len(values) {
		return 0, fmt.Errorf("invalid iostat field index %d", idx)
	}

	return strconv.ParseFloat(values[idx], 64)
}

// Get the latest iostat values
func iostat() ([]string, error) {
	out, err := exec.Command("iostat", "-c").Output()
	if err != nil {
		return nil, fmt.Errorf("iowait: %v", err)
	}

	// Linux 4.2.0-25-generic (a109563eab38)	04/01/16	_x86_64_(4 CPU)
	//
	// avg-cpu:  %user   %nice %system %iowait  %steal   %idle
	//	          2.37    0.00    1.58    0.01    0.00   96.04
	lines := strings.Split(string(out), "\n")
	if len(lines) < 4 {
		return nil, fmt.Errorf("iowait: unexpected output: %q", out)
	}

	values := strings.Fields(lines[3])
	if len(values) != 6 {
		return nil, fmt.Errorf("iowait: unexpected output: %q", out)
	}
	return values, nil
}
