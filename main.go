package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"flag"

	"github.com/andyleap/boltinspect"
	"github.com/boltdb/bolt"
	"github.com/dustin/go-humanize"
	"github.com/influxdb/influxdb/client/v2"
)

var (
	influxUser = flag.String("u", "", "InfluxDB User")
	influxPass = flag.String("p", "", "InfluxDB Password")
)

type RelayStatus struct {
	BytesProxied      uint64   `json:"bytesProxied"`
	NumActiveSessions uint64   `json:"numActiveSessions"`
	NumConnections    uint64   `json:"numConnections"`
	Rates             []uint64 `json:"kbps10s1m5m15m30m60m"`
	Options           struct {
		ProvidedBy string `json:"provided-by"`
	} `json:"options"`
}

func GetStatus(relay string) (relayurl string, status *RelayStatus, err error) {
	purl, _ := url.Parse(relay)
	values := purl.Query()
	host := strings.Split(purl.Host, ":")[0]
	relayurl = purl.Host
	statusUrl := fmt.Sprintf("http://%s%s/status", host, values.Get("statusAddr"))
	resp, err := http.Get(statusUrl)
	if err != nil {
		return relayurl, nil, err
	}
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return relayurl, nil, err
	}
	err = json.Unmarshal(data, &status)
	return
}

func GetRelays() []string {
	resp, _ := http.Get("https://relays.syncthing.net/endpoint")
	body, _ := ioutil.ReadAll(resp.Body)
	var relaydata map[string]interface{}
	json.Unmarshal(body, &relaydata)
	relays := []string{}
	for _, relay := range relaydata["relays"].([]interface{}) {
		relays = append(relays, (relay.(map[string]interface{}))["url"].(string))
	}
	return relays
}

var (
	db *bolt.DB
	influx client.Client
)

func main() {
	flag.Parse()
	
	u, _ := url.Parse("http://localhost:8086")
    influx = client.NewClient(client.Config{
        URL: u,
        Username: *influxUser,
        Password: *influxPass,
    })
	
	mainTmpl = template.Must(template.New("main").Funcs(template.FuncMap{
		"Bytes": humanize.IBytes,
	}).Parse(mainTmplText))

	http.HandleFunc("/", Status)
	var err error
	db, err = bolt.Open("/opt/relaystats/relaystats.db", 0666, &bolt.Options{})
	if err != nil {
		log.Fatalf("Error, cannot open db: %s", err)
	}

	db.Update(func(tx *bolt.Tx) error {
		tx.CreateBucketIfNotExists([]byte(`TIMESTATS`))
		return nil
	})

	dbinspect := boltinspect.New(db)
	http.HandleFunc("/bolt", dbinspect.InspectEndpoint)

	go WatchRelays()
	http.ListenAndServe(":20000", nil)
}

func WatchRelays() {
	nextRelaysPoll := time.Now()
	relays := []string{}
	for {
		if time.Now().After(nextRelaysPoll) {
			relays = GetRelays()
			nextRelaysPoll = nextRelaysPoll.Add(60 * time.Second)
		}
		rchan := make(chan *RelayInfo, 2)
		wg := &sync.WaitGroup{}

		for _, relay := range relays {
			wg.Add(1)
			go func(relay string) {
				relayurl, status, err := GetStatus(relay)
				if err != nil {
					wg.Done()
					return
				}
				rchan <- &RelayInfo{Url: relayurl, Status: status}
			}(relay)
		}
		go func() {
			wg.Wait()
			close(rchan)
		}()
		bp, _ := client.NewBatchPoints(client.BatchPointsConfig{
	        Database:  "syncthingrelay",
	        Precision: "s",
	    })
		allTime := time.Now()
		fields := []map[string]interface{}{
			map[string]interface{}{},
			map[string]interface{}{},
			map[string]interface{}{},
			map[string]interface{}{},
			map[string]interface{}{},
			map[string]interface{}{},
		}
		
		db.Update(func(tx *bolt.Tx) error {
			timestats := tx.Bucket([]byte(`TIMESTATS`))
			now, _ := timestats.CreateBucket([]byte(time.Now().Format(time.RFC3339)))
			for ri := range rchan {
				data, _ := json.Marshal(ri.Status)
				now.Put([]byte(ri.Url), data)
			    fields[0][ri.Url] = ri.Status.Rates[0]
				fields[0][ri.Url] = ri.Status.Rates[1]
				fields[0][ri.Url] = ri.Status.Rates[2]
				fields[0][ri.Url] = ri.Status.Rates[3]
				fields[0][ri.Url] = ri.Status.Rates[4]
				fields[0][ri.Url] = ri.Status.Rates[5]
			    
				wg.Done()
			}
			return nil
		})
		pt, _ := client.NewPoint("bandwidth-10s", nil, fields[0], allTime)
		bp.AddPoint(pt)
		pt, _ = client.NewPoint("bandwidth-1m", nil, fields[1], allTime)
		bp.AddPoint(pt)
		pt, _ = client.NewPoint("bandwidth-5m", nil, fields[2], allTime)
		bp.AddPoint(pt)
		pt, _ = client.NewPoint("bandwidth-15m", nil, fields[3], allTime)
		bp.AddPoint(pt)
		pt, _ = client.NewPoint("bandwidth-30m", nil, fields[4], allTime)
		bp.AddPoint(pt)
		pt, _ = client.NewPoint("bandwidth-60m", nil, fields[5], allTime)
		bp.AddPoint(pt)
		influx.Write(bp)
		time.Sleep(5 * time.Second)
	}
}

var mainTmpl *template.Template
var mainTmplText = `{{define "main"}}
<html>
<head>
<title>SyncThing Relay Status</title>
<style>
table {
    border-collapse: collapse;
}

table, th, td {
   border: 1px solid black;
}
</style>
</head>
<body>
<table>
<tr><th>Relay URL</th><th>Bytes Proxied</th><th>10 s</th><th>1 m</th><th>5 m</th><th>15 m</th><th>30 m</th><th>60 m</th><th>Provided by</th></tr>
{{range .Relays}}
<tr><td>{{.Url}}</td>
{{if .Error}}
<td colspan="8">{{.Error}}</td>
{{else}}
{{with .Status}}
<td>{{Bytes .BytesProxied}}</td>{{range .Rates}}<td>{{.}}</td>{{end}}<td>{{.Options.ProvidedBy}}</td>
{{end}}
{{end}}
</tr>
{{end}}
</table>
</body>
</html>
{{end}}
`

type RelayInfo struct {
	Url    string
	Error  error
	Status *RelayStatus
}

func Status(rw http.ResponseWriter, req *http.Request) {
	relayData := []RelayInfo{}

	db.View(func(tx *bolt.Tx) error {
		timestats := tx.Bucket([]byte(`TIMESTATS`))
		latestkey, _ := timestats.Cursor().Last()
		latest := timestats.Bucket(latestkey)
		latest.ForEach(func(k, v []byte) error {
			var rs *RelayStatus
			json.Unmarshal(v, &rs)
			ri := RelayInfo{
				Url:    string(k),
				Status: rs,
			}
			relayData = append(relayData, ri)
			return nil
		})
		return nil
	})

	err := mainTmpl.ExecuteTemplate(rw, "main", struct {
		Relays []RelayInfo
	}{
		Relays: relayData,
	})
	if err != nil {
		fmt.Println(err)
	}
}
