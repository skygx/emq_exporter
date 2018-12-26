package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace = "emq"
)

var (
	//scraping endpoints for EMQ v2 api version
	targetsV2 = map[string]string{
		"monitoring_metrics": "/api/v2/monitoring/metrics/%s",
		"monitoring_stats":   "/api/v2/monitoring/stats/%s",
		"monitoring_nodes":   "/api/v2/monitoring/nodes/%s",
		"management_nodes":   "/api/v2/management/nodes/%s",
	}
	//scraping endpoints for EMQ v3 api version
	targetsV3 = map[string]string{
		"node_metrics": "/api/v3/nodes/%s/metrics/",
		"node_stats":   "/api/v3/nodes/%s/stats/",
		"nodes":        "/api/v3/nodes/%s",
	}
)

type metric struct {
	kind  prometheus.ValueType
	desc  *prometheus.Desc
	value float64
}

type emqResponse struct {
	Code   float64                `json:"code,omitempty"`
	Result map[string]interface{} `json:"result,omitempty"` //api v2 json key
	Data   map[string]interface{} `json:"data,omitempty"`   //api v3 json key
}

// Exporter collects EMQ stats from the given URI and exports them using
// the prometheus metrics package.
type Exporter struct {
	URI                      string
	client                   http.Client
	username, password, node string
	up                       prometheus.Gauge
	totalScrapes             prometheus.Counter
	apiVersion               string

	metrics []*metric
}

// NewExporter returns an initialized Exporter.
func NewExporter(uri, username, password, node string, timeout time.Duration, apiVersion string) (*Exporter, error) {

	return &Exporter{
		URI:        uri,
		username:   username,
		password:   password,
		node:       node,
		apiVersion: apiVersion,
		client: http.Client{
			Timeout: timeout,
		},
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Was the last scrape of EMQ successful",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_total_scrapes",
			Help:      "Current total scrapes.",
		}),
	}, nil

}

// Collect fetches the stats from configured EMQ location and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.totalScrapes.Inc()

	if err := e.scrape(); err != nil {
		e.up.Set(0)
		log.Warnln(err)
	} else {
		e.up.Set(1)
	}

	for _, m := range e.metrics {
		ch <- prometheus.MustNewConstMetric(
			m.desc,
			m.kind,
			m.value,
		)
	}

	ch <- e.up
	ch <- e.totalScrapes
}

// Describe describes all the metrics ever exported by the EMQ exporter. It
// implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range e.metrics {
		ch <- m.desc
	}

	ch <- e.up.Desc()
	ch <- e.totalScrapes.Desc()
}

// get the json responses from the targets map, process them and
// insert them into exporter.metrics array
func (e *Exporter) scrape() error {

	var targets = make(map[string]string)
	var data = make(map[string]interface{})

	if e.apiVersion == "v2" {
		targets = targetsV2
	} else {
		targets = targetsV3
	}

	for name, path := range targets {

		resp, err := e.fetch(path)
		if err != nil {
			return err
		}

		if resp.Code != 0 {
			return fmt.Errorf("Received code != 0")
		}

		if e.apiVersion == "v2" {
			data = resp.Result
		} else {
			data = resp.Data
		}

		for k, v := range data {
			fqName := fmt.Sprintf("%s_%s_%s", namespace, name, strings.Replace(k, "/", "_", -1))
			switch vv := v.(type) {
			case string:
				val, err := parseString(vv)
				if err != nil {
					break
				}
				e.addMetric(fqName, k, val, nil)
			case float64:
				e.addMetric(fqName, k, vv, nil)
			default:
				log.Debugln(k, "is of type I don't know how to handle")
			}
		}
	}

	return nil
}

func (e *Exporter) addMetric(fqName, help string, value float64, labels []string) {
	//check if the metric with a given fqName exists, and update its value
	for _, v := range e.metrics {
		if strings.Contains(v.desc.String(), fqName) {
			v.value = value
			return
		}
	}

	//append a new metric to the metrics array
	e.metrics = append(e.metrics, &metric{
		kind:  prometheus.GaugeValue,
		desc:  prometheus.NewDesc(fqName, help, labels, nil),
		value: value,
	})

}

//get the response from the provided target url
func (e *Exporter) fetch(target string) (emqResponse, error) {
	var dat emqResponse

	u := e.URI + fmt.Sprintf(target, e.node)

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return dat, fmt.Errorf("Failed to get metrics from %s", u)
	}

	req.SetBasicAuth(e.username, e.password)
	res, err := e.client.Do(req)
	if err != nil {
		return dat, fmt.Errorf("Failed to get metrics from %s", u)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return dat, fmt.Errorf("Failed to get metrics from %s", u)
	}

	if err := json.Unmarshal(streamToByte(res.Body), &dat); err != nil {
		return dat, fmt.Errorf("Failed to unmarshal json")
	}

	return dat, nil
}

//Convert a stream into byte array
func streamToByte(stream io.Reader) []byte {
	buf := new(bytes.Buffer)
	buf.ReadFrom(stream)
	return buf.Bytes()
}

//Try to parse value from string to float64, return error on failure
func parseString(s string) (float64, error) {

	v, err := strconv.ParseFloat(s, 64)

	if err != nil {
		//try to convert to bytes
		u, err := bytefmt.ToBytes(s)
		if err != nil {
			log.Debugln("can't parse", s, err)
			return v, err
		}
		v = float64(u)
	}

	return v, nil
}

func main() {

	var (
		listenAddress = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9505").String()
		metricsPath   = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		emqURI        = kingpin.Flag("emq.uri", "HTTP API address of the EMQ node.").Default("http://127.0.0.1:8080").String()
		emqUsername   = kingpin.Flag("emq.username", "EMQ username (or use $EMQ_USERNAME env var)").Default("admin").Envar("EMQ_USERNAME").String()
		emqPassword   = kingpin.Flag("emq.password", "EMQ password (or use $EMQ_PASSWORD env var)").Default("public").Envar("EMQ_PASSWORD").String()
		emqNodeName   = kingpin.Flag("emq.node", "Node name of the emq node to scrape.").Default("emq@127.0.0.1").String()
		emqTimeout    = kingpin.Flag("emq.timeout", "Timeout for trying to get stats from emq").Default("5s").Duration()
		emqAPIVersion = kingpin.Flag("emq.api-version", "The API version used by EMQ. Valid values: [v2, v3]").Default("v2").Enum("v2", "v3")
	)

	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(printVersion())
	kingpin.CommandLine.HelpFlag.Short('h')

	kingpin.Parse()

	log.Infoln("Starting emq_exporter")
	log.Infoln("Version", printVersion())

	exporter, err := NewExporter(*emqURI, *emqUsername, *emqPassword, *emqNodeName, *emqTimeout, *emqAPIVersion)
	if err != nil {
		log.Fatal(err)
	}

	prometheus.MustRegister(exporter)

	log.Infoln("Listening on", *listenAddress)
	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>EMQ Exporter</title></head>
             <body>
             <h1>EMQ Exporter</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})

	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
