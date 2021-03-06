package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/buildkite/go-buildkite/buildkite"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

var (
	apiToken       = kingpin.Flag("buildkite-token", "Buildkite API token. Requires `read_builds` permissions.").Required().String()
	org            = kingpin.Flag("buildkite-org", "Buildkite organization which is to be scraped.").Required().String()
	port           = kingpin.Flag("port", "TCP port which the HTTP server should listen on.").Default("8080").Int()
	memcachedAddrs = kingpin.Flag("memcache", "Memcache broker addresses (eg. 127.0.0.1:11211).").Strings()

	serveCmd      = kingpin.Command("serve", "serve the the web app.")
	reports       = serveCmd.Flag("report", `Report. Example: {"name": "Slow master builds", "from": "started", "to": "finished", "pipelines": ".*", "branches: "master", "group": "{{.Pipeline}}"} where 1) 'from'/'to' must be created, scheduled, started or finished, 2) 'pipelines'/'branches' is a regexp of what we are interested in, 3) name can be anything human readable, 4) 'group' is how all builds are grouped (a Golang template from Build).`).Required().Strings()
	scrapeHistory = serveCmd.Flag("scrape-history", "How far back in time we scrape builds. Defaults to 28 days.").Default("672h").Duration()

	refreshCmd     = kingpin.Command("refresh", "rewrite recent data to cache. recommended to do in background regularly if you have a lot of builds.")
	refreshHistory = refreshCmd.Flag("refresh-history", "How far back in time we update the cache.").Default("3h").Duration()
)

func main() {
	cmd := kingpin.Parse()

	//buildkite.SetHttpDebug(true) // Useful when debugging.
	config, err := buildkite.NewTokenConfig(optionalFileExpansion(*apiToken), false)

	if err != nil {
		log.Fatal("Incorrect token:", err)
	}

	cache := &MemcacheCache{memcache.New(*memcachedAddrs...)}

	queries := mustBuildQueries(*reports)

	client := buildkite.NewClient(config.Client())
	client.UserAgent = "tink-buildkite-stats/v1.0.0"
	bk := &NetworkBuildkite{
		Client: client,
		Org:    *org,
		Cache:  cache,
	}

	switch cmd {
	case "serve":
		serve(bk, queries)
	case "refresh":
		refresh(bk)
	}
}

func serve(bk *NetworkBuildkite, queries []Query) {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.DefaultLogger)
	r.Mount("/", (&Routes{bk, queries, *scrapeHistory}).Routes())

	go func() {
		// pprof registers on default mux so starting it on a separate port.
		// pprof is being imported an anonymous import in the web package.
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	log.Printf("Listening on port %d", *port)
	server := http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: r}
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("HTTP server error: %v", err)
	}
}

func refresh(bk *NetworkBuildkite) {
	from := time.Now().Add(-*refreshHistory)
	log.Printf("Starting refresh between [%s, now)\n", from)
	if err := bk.RefreshCache(from); err != nil {
		log.Fatalln(err)
	}
	log.Println("Refresh finished succesfully.")
}

type MemcacheCache struct {
	c *memcache.Client
}

func (m *MemcacheCache) Put(k string, v []byte, ttl time.Duration) error {
	return m.c.Set(&memcache.Item{
		Key:        k,
		Value:      v,
		Expiration: int32(time.Now().Add(ttl).Unix()),
	})
}

func (m *MemcacheCache) Get(k string) ([]byte, error) {
	var res []byte
	item, err := m.c.Get(k)
	if err == nil {
		res = item.Value
	}
	return res, err
}

func mustBuildQueries(queries []string) (res []Query) {
	for _, q := range queries {
		res = append(res, mustBuildQuery(q))
	}
	return
}

func mustBuildQuery(query string) Query {
	var raw JSONQuery
	if err := json.Unmarshal([]byte(query), &raw); err != nil {
		log.Fatalln("unable to parse report:", err)
	}

	return Query{
		Name:      raw.Name,
		from:      mustParseQueryTimestamp(raw.From),
		to:        mustParseQueryTimestamp(raw.To),
		pipelines: regexp.MustCompile(raw.Pipelines),
		branches:  regexp.MustCompile(raw.Branches),
		group:     template.Must(template.New("group").Parse(raw.Group)),
	}
}

type JSONQuery struct {
	Name      string `json:"name"`
	From      string `json:"from"`
	To        string `json:"to"`
	Pipelines string `json:"pipelines"`
	Branches  string `json:"branches"`
	Group     string `json:"group"`
}

type Query struct {
	Name      string
	from      QueryTimestamp
	to        QueryTimestamp
	pipelines *regexp.Regexp
	branches  *regexp.Regexp
	group     *template.Template
}

func (q Query) Predicate(b Build) bool {
	return q.pipelines.MatchString(b.Pipeline.Name) && q.branches.MatchString(b.Branch)
}

func (q Query) Duration(b Build) time.Duration {
	return q.to.Extract(b).Sub(q.from.Extract(b))
}

func (q Query) Group(b Build) string {
	var buf bytes.Buffer
	if err := q.group.Execute(&buf, b); err != nil {
		log.Panicln("extract the build group:", err)
	}
	return string(buf.Bytes())
}

type QueryTimestamp int

const (
	CreatedTimestamp QueryTimestamp = iota
	ScheduledTimestamp
	StartedTimestamp
	FinishedTimestamp
)

func mustParseQueryTimestamp(s string) QueryTimestamp {
	switch s {
	case "created":
		return CreatedTimestamp
	case "scheduled":
		return ScheduledTimestamp
	case "started":
		return StartedTimestamp
	case "finished":
		return FinishedTimestamp
	default:
		log.Fatalln("unable to parse timestamp")
	}

	// will never happen
	return 0
}

func (t QueryTimestamp) Extract(b Build) time.Time {
	switch t {
	case CreatedTimestamp:
		return b.CreatedAt
	case ScheduledTimestamp:
		return b.ScheduledAt
	case StartedTimestamp:
		return b.StartedAt
	case FinishedTimestamp:
		return b.FinishedAt
	default:
		log.Panicln("unrecognized timestamp type:", t)
	}

	// will never happen
	return time.Now()
}

func optionalFileExpansion(s string) string {
	if strings.HasPrefix(s, "@") {
		// Trimming trailing newline from K8s configmap.
		return strings.TrimRight(string(readFileContent(s[1:])), "\n")
	}
	return s
}

func readFileContent(filename string) []byte {
	content, err := ioutil.ReadFile(filename)
	if err != nil {
		log.Fatal(err.Error())
	}
	return content
}
