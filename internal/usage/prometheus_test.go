package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gratycodes/kube-rightsize-diff/internal/manifest"
)

type fakeProm struct {
	mu      sync.Mutex
	queries []string
	auth    []string
	cpu     string // JSON result arrays
	mem     string
	status  int
	body    string // overrides everything when set
}

func (f *fakeProm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/prom/api/v1/query" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := r.PostForm.Get("query")
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if f.status != 0 {
		w.WriteHeader(f.status)
	}
	if f.body != "" {
		fmt.Fprint(w, f.body)
		return
	}
	result := f.mem
	if strings.Contains(q, "container_cpu_usage_seconds_total") {
		result = f.cpu
	}
	fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":%s}}`, result)
}

func series(ns, pod, container, value string) string {
	b, _ := json.Marshal(map[string]any{
		"metric": map[string]string{"namespace": ns, "pod": pod, "container": container},
		"value":  []any{1760000000.0, value},
	})
	return string(b)
}

func arr(items ...string) string { return "[" + strings.Join(items, ",") + "]" }

var testContainers = []manifest.Container{
	{Namespace: "shop", Kind: "Deployment", Workload: "checkout", Name: "app"},
	{Namespace: "shop", Kind: "StatefulSet", Workload: "cart-redis", Name: "redis"},
	{Namespace: "monitoring", Kind: "DaemonSet", Workload: "node-agent", Name: "agent"},
}

func baseCfg(url string) PromConfig {
	return PromConfig{URL: url + "/prom/", CPUPercentile: 0.95, MemPercentile: 0.99, Window: "7d", Resolution: "1m", RateInterval: "5m", Timeout: 5 * time.Second}
}

func TestFetchPrometheus(t *testing.T) {
	fp := &fakeProm{
		cpu: arr(
			series("shop", "checkout-7d9f8c6b5-aaaaa", "app", "0.120"),
			series("shop", "checkout-7d9f8c6b5-bbbbb", "app", "0.180"),       // max across pods wins
			series("shop", "checkout-7d9f8c6b5-bbbbb", "istio-proxy", "0.5"), // container not in manifests
			series("shop", "cart-redis-0", "redis", "0.05"),
			series("shop", "stranger-abcde", "app", "9"), // no owner
			series("monitoring", "node-agent-k8s7p", "agent", "NaN"),
		),
		mem: arr(
			series("shop", "checkout-7d9f8c6b5-aaaaa", "app", "314572800"),
			series("shop", "cart-redis-0", "redis", "293601280"),
			series("monitoring", "node-agent-k8s7p", "agent", "62914560"),
		),
	}
	srv := httptest.NewServer(fp)
	defer srv.Close()
	cfg := baseCfg(srv.URL)
	cfg.BearerToken = "s3cret"
	cfg.ExtraSelector = `cluster="prod"`

	d, err := FetchPrometheus(context.Background(), cfg, testContainers, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if d.Len() != 3 {
		t.Fatalf("got %d samples, want 3", d.Len())
	}
	co, _ := d.Lookup(manifest.Key{Namespace: "shop", Kind: "Deployment", Workload: "checkout", Container: "app"})
	if co.CPUCores != 0.18 || co.MemoryBytes != 314572800 || co.Pods != 2 {
		t.Errorf("checkout = %+v", co)
	}
	na, _ := d.Lookup(manifest.Key{Namespace: "monitoring", Kind: "DaemonSet", Workload: "node-agent", Container: "agent"})
	if na.CPUCores != 0 || na.MemoryBytes != 62914560 {
		t.Errorf("NaN cpu should be skipped, got %+v", na)
	}
	if len(fp.queries) != 2 {
		t.Fatalf("want 2 queries, got %d", len(fp.queries))
	}
	wantCPU := `quantile_over_time(0.95, sum by (namespace, pod, container) (rate(container_cpu_usage_seconds_total{namespace=~"monitoring|shop",container!="",container!="POD",cluster="prod"}[5m]))[7d:1m])`
	if fp.queries[0] != wantCPU {
		t.Errorf("cpu query:\n got %s\nwant %s", fp.queries[0], wantCPU)
	}
	if !strings.HasPrefix(fp.queries[1], "quantile_over_time(0.99, max by (namespace, pod, container) (container_memory_working_set_bytes{") {
		t.Errorf("memory query: %s", fp.queries[1])
	}
	for _, a := range fp.auth {
		if a != "Bearer s3cret" {
			t.Errorf("auth header = %q", a)
		}
	}
}

func TestFetchPrometheusErrors(t *testing.T) {
	cases := map[string]*fakeProm{
		"api error":  {status: 400, body: `{"status":"error","errorType":"bad_data","error":"parse error"}`},
		"not json":   {status: 502, body: `<html>bad gateway</html>`},
		"matrix":     {body: `{"status":"success","data":{"resultType":"matrix","result":[]}}`},
		"bad value":  {cpu: arr(series("shop", "cart-redis-0", "redis", "abc")), mem: "[]"},
		"value type": {body: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,2]}]}}`},
	}
	for name, fp := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(fp)
			defer srv.Close()
			if _, err := FetchPrometheus(context.Background(), baseCfg(srv.URL), testContainers, srv.Client()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestFetchPrometheusTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	cfg := baseCfg(srv.URL)
	cfg.Timeout = 50 * time.Millisecond
	_, err := FetchPrometheus(context.Background(), cfg, testContainers, srv.Client())
	if err == nil {
		t.Fatal("expected timeout")
	}
}

func TestPromConfigValidation(t *testing.T) {
	bad := []func(*PromConfig){
		func(c *PromConfig) { c.URL = "" },
		func(c *PromConfig) { c.CPUPercentile = 0 },
		func(c *PromConfig) { c.MemPercentile = 1.5 },
		func(c *PromConfig) { c.Window = "7 days" },
		func(c *PromConfig) { c.Resolution = "" },
		func(c *PromConfig) { c.RateInterval = "5" },
		func(c *PromConfig) { c.ExtraSelector = `}) or vector(1) #` },
	}
	for i, mut := range bad {
		cfg := baseCfg("http://x")
		mut(&cfg)
		if err := cfg.validate(); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
	ok := baseCfg("http://x")
	ok.Window = "1h30m"
	if err := ok.validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestFetchPrometheusNoContainers(t *testing.T) {
	d, err := FetchPrometheus(context.Background(), baseCfg("http://127.0.0.1:1"), nil, nil)
	if err != nil || d.Len() != 0 {
		t.Fatalf("got %v, %v", d, err)
	}
}
