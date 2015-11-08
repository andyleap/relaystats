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

	"github.com/andyleap/boltinspect"
	"github.com/boltdb/bolt"
	"github.com/dustin/go-humanize"
)

var (
	history    map[string]*RelayHistory
	httpClient *http.Client
)

type RelayHistory struct {
	BytesProxied     uint64
	LastBytesProxied uint64
}

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
	resp, err := httpClient.Get(statusUrl)
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
	resp, _ := httpClient.Get("https://relays.syncthing.net/endpoint")
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
)

func main() {
	history = make(map[string]*RelayHistory)

	httpClient = &http.Client{
		Timeout: 3 * time.Second,
	}

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
		timestats, _ := tx.CreateBucketIfNotExists([]byte(`TIMESTATS`))
		timestats.ForEach(func(k, v []byte) error {
			timestat := timestats.Bucket(k)
			timestat.ForEach(func(relay, v []byte) error {
				var rs *RelayStatus
				json.Unmarshal(v, &rs)
				if _, ok := history[string(relay)]; !ok {
					history[string(relay)] = &RelayHistory{}
				}
				if rs.BytesProxied < history[string(relay)].LastBytesProxied {
					history[string(relay)].BytesProxied += history[string(relay)].LastBytesProxied
				}
				history[string(relay)].LastBytesProxied = rs.BytesProxied
				return nil
			})
			return nil
		})
		return nil
	})

	dbinspect := boltinspect.New(db)
	http.HandleFunc("/bolt", dbinspect.InspectEndpoint)

	go WatchRelays()
	http.ListenAndServe(":20000", nil)
}

func WatchRelays() {
	nextRelaysPoll := time.Now().Add(-time.Second)
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
		db.Update(func(tx *bolt.Tx) error {
			timestats := tx.Bucket([]byte(`TIMESTATS`))
			now, _ := timestats.CreateBucket([]byte(time.Now().Format(time.RFC3339)))
			for ri := range rchan {
				data, _ := json.Marshal(ri.Status)
				now.Put([]byte(ri.Url), data)
				if _, ok := history[ri.Url]; !ok {
					history[ri.Url] = &RelayHistory{}
				}
				if ri.Status.BytesProxied < history[ri.Url].LastBytesProxied {
					history[ri.Url].BytesProxied += history[ri.Url].LastBytesProxied
				}
				history[ri.Url].LastBytesProxied = ri.Status.BytesProxied
				wg.Done()
			}
			return nil
		})
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
<tr><th>Relay URL</th><th>Active Sessions</th><th>Connections</th><th>Bytes Proxied</th><th>10 s</th><th>1 m</th><th>5 m</th><th>15 m</th><th>30 m</th><th>60 m</th><th>Provided by</th></tr>
{{range .Relays}}
<tr><td>{{.Url}}</td>
{{if .Error}}
<td colspan="10">{{.Error}}</td>
{{else}}
{{with .Status}}
<td>{{.NumActiveSessions}}</td><td>{{.NumConnections}}</td><td>{{Bytes .BytesProxied}}</td>{{range .Rates}}<td>{{.}}</td>{{end}}<td>{{.Options.ProvidedBy}}</td>
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
	relayTotal := &RelayStatus{Rates: make([]uint64, 6)}
	db.View(func(tx *bolt.Tx) error {
		timestats := tx.Bucket([]byte(`TIMESTATS`))
		latestkey, _ := timestats.Cursor().Last()
		latest := timestats.Bucket(latestkey)
		latest.ForEach(func(k, v []byte) error {
			var rs *RelayStatus
			json.Unmarshal(v, &rs)
			rs.BytesProxied += history[string(k)].BytesProxied
			ri := RelayInfo{
				Url:    string(k),
				Status: rs,
			}
			relayTotal.BytesProxied += rs.BytesProxied
			relayTotal.NumActiveSessions += rs.NumActiveSessions
			relayTotal.NumConnections += rs.NumConnections
			relayTotal.Rates[0] += rs.Rates[0]
			relayTotal.Rates[1] += rs.Rates[1]
			relayTotal.Rates[2] += rs.Rates[2]
			relayTotal.Rates[3] += rs.Rates[3]
			relayTotal.Rates[4] += rs.Rates[4]
			relayTotal.Rates[5] += rs.Rates[5]
			relayData = append(relayData, ri)
			return nil
		})
		return nil
	})

	relayData = append(relayData, RelayInfo{
		Url:    "Totals",
		Status: relayTotal,
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
