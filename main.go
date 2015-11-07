package main

import (
	"sort"
	"sync"
	"html/template"
	"time"
	"io/ioutil"
	"fmt"
	"net/http"
	"encoding/json"
	"net/url"
	"strings"
	"github.com/pmylund/go-cache"
	"github.com/dustin/go-humanize"
)

type RelayStatus struct{
	BytesProxied uint64 `json:"bytesProxied"`
	Rates []uint64 `json:"kbps10s1m5m15m30m60m"`
	Options struct {
		ProvidedBy string `json:"provided-by"`
	} `json:"options"`
}

func GetStatus(relay string) (relayurl string, status *RelayStatus, err error) {
	purl, _ := url.Parse(relay)
	values := purl.Query()
	host := strings.Split(purl.Host, ":")[0]
	relayurl = purl.Host
	statusUrl := fmt.Sprintf("http://%s%s/status", host, values.Get("statusAddr"))
	if status, ok := dataCache.Get(statusUrl); ok {
		return relayurl, status.(*RelayStatus), nil
	}
	resp, err := http.Get(statusUrl)
	if err != nil {
	        return relayurl, nil, err
	}
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
	        return relayurl, nil, err
	}
	err = json.Unmarshal(data, &status)
	
	dataCache.Set(statusUrl, status, cache.DefaultExpiration)
	return
}

func GetRelays() []string {
	if relays, ok := dataCache.Get("relays"); ok {
		return relays.([]string)
	}
	resp, _ := http.Get("https://relays.syncthing.net/endpoint")
	body, _ := ioutil.ReadAll(resp.Body)
	var relaydata map[string]interface{}
	json.Unmarshal(body, &relaydata)
	relays := []string{}
	for _, relay := range relaydata["relays"].([]interface{}) {
		relays = append(relays, (relay.(map[string]interface{}))["url"].(string))
	}
	dataCache.Set("relays", relays, cache.DefaultExpiration)
	return relays
}

var (
	dataCache *cache.Cache
)

func main() {
	dataCache = cache.New(30 * time.Second, 5 * time.Second)
	mainTmpl = template.Must(template.New("main").Funcs(template.FuncMap{
		"Bytes": humanize.IBytes,
	}).Parse(mainTmplText))
	
	http.HandleFunc("/", Status)
	http.ListenAndServe(":20000", nil)
	
	
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
	Url string
	Error error
	Status *RelayStatus
}

func Status(rw http.ResponseWriter, req *http.Request) {
	relays := GetRelays()
	relayData := make([]RelayInfo, len(relays))
	wg := &sync.WaitGroup{}
	
	sort.Strings(relays)
	
	for i, relay := range relays {
		wg.Add(1)
		go func(i int, relay string) {
			relayurl, status, err := GetStatus(relay)
			if err != nil {
				relayData[i] = RelayInfo{Url: relayurl, Error: err}
			} else {
				relayData[i] = RelayInfo{Url: relayurl, Status: status}
			}
			wg.Done()
		}(i, relay)
	}
	wg.Wait()
	
	err := mainTmpl.ExecuteTemplate(rw, "main", struct{
		Relays []RelayInfo
	}{
		Relays: relayData,
	})
	if err != nil {
		fmt.Println(err)
	}
}
