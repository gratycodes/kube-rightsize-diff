package usage

import (
	"strings"
	"testing"

	"github.com/gratycodes/kube-rightsize-diff/internal/manifest"
)

func key(ns, kind, w, c string) manifest.Key {
	return manifest.Key{Namespace: ns, Kind: kind, Workload: w, Container: c}
}

func TestJSONAndCSVSnapshotsAgree(t *testing.T) {
	j, err := LoadFile("../../testdata/shop/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile("../../testdata/shop/usage.csv")
	if err != nil {
		t.Fatal(err)
	}
	if j.Len() != 6 || c.Len() != 6 {
		t.Fatalf("lens = %d, %d; want 6", j.Len(), c.Len())
	}
	if j.CPUPercentile != 0.95 || j.MemPercentile != 0.99 || j.Window != "7d" {
		t.Errorf("metadata not read: %+v", j)
	}
	k := key("shop", "Deployment", "checkout", "app")
	js, _ := j.Lookup(k)
	cs, _ := c.Lookup(k)
	if js != cs {
		t.Errorf("json %+v != csv %+v", js, cs)
	}
	if js.CPUCores != 0.18 || js.MemoryBytes != 300<<20 {
		t.Errorf("checkout = %+v", js)
	}
}

func TestLookupKindFallback(t *testing.T) {
	d := NewData("t", 0.95, 0.99, "1d")
	d.Set(key("ns", "", "w", "c"), Sample{CPUCores: 1})
	if s, ok := d.Lookup(key("ns", "StatefulSet", "w", "c")); !ok || s.CPUCores != 1 {
		t.Fatal("kind-less entry should match any kind")
	}
	if _, ok := d.Lookup(key("ns", "StatefulSet", "w", "other")); ok {
		t.Fatal("unexpected match")
	}
	var nilData *Data
	if _, ok := nilData.Lookup(key("a", "b", "c", "d")); ok {
		t.Fatal("nil data should not match")
	}
}

func TestSnapshotErrors(t *testing.T) {
	jsonCases := map[string]string{
		"unknown field": `{"containers": [], "oops": 1}`,
		"bad cpu":       `{"containers": [{"namespace":"a","workload":"b","container":"c","cpu":"fast"}]}`,
		"bad memory":    `{"containers": [{"namespace":"a","workload":"b","container":"c","memory":"big"}]}`,
		"missing ns":    `{"containers": [{"workload":"b","container":"c"}]}`,
		"duplicate":     `{"containers": [{"namespace":"a","workload":"b","container":"c"},{"namespace":"a","workload":"b","container":"c"}]}`,
	}
	for name, in := range jsonCases {
		t.Run("json "+name, func(t *testing.T) {
			if _, err := ParseJSON(strings.NewReader(in), "x.json"); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	csvCases := map[string]string{
		"missing column": "namespace,workload,container,cpu\na,b,c,1\n",
		"empty":          "",
		"ragged":         "namespace,workload,container,cpu,memory\na,b\n",
	}
	for name, in := range csvCases {
		t.Run("csv "+name, func(t *testing.T) {
			if _, err := ParseCSV(strings.NewReader(in), "x.csv"); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestCSVOptionalKindColumn(t *testing.T) {
	d, err := ParseCSV(strings.NewReader("namespace,workload,container,cpu,memory\nshop,api,app,250m,1Gi\n"), "x.csv")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := d.Lookup(key("shop", "Deployment", "api", "app"))
	if !ok || s.CPUCores != 0.25 || s.MemoryBytes != 1<<30 {
		t.Fatalf("got %+v %v", s, ok)
	}
}

func TestOwnerIndex(t *testing.T) {
	cs := []manifest.Container{
		{Namespace: "shop", Kind: "Deployment", Workload: "web", Name: "c"},
		{Namespace: "shop", Kind: "Deployment", Workload: "web-api", Name: "c"},
		{Namespace: "shop", Kind: "StatefulSet", Workload: "db", Name: "c"},
		{Namespace: "shop", Kind: "DaemonSet", Workload: "agent", Name: "c"},
		{Namespace: "shop", Kind: "CronJob", Workload: "report", Name: "c"},
		{Namespace: "shop", Kind: "Pod", Workload: "debug", Name: "c"},
	}
	idx := newOwnerIndex(cs)
	cases := []struct {
		pod, want string
	}{
		{"web-7d9f8c6b5-x2x9q", "web"},
		{"web-api-6c8b9d7f4-abcde", "web-api"},
		{"db-0", "db"},
		{"db-12", "db"},
		{"agent-k8s7p", "agent"},
		{"report-29123400-qwert", "report"},
		{"debug", "debug"},
		{"debug-2", ""},
		{"db-x", ""},
		{"unrelated-7d9f8c6b5-x2x9q", ""},
	}
	for _, c := range cases {
		w, ok := idx.owner("shop", c.pod)
		got := ""
		if ok {
			got = w.name
		}
		if got != c.want {
			t.Errorf("owner(%s) = %q, want %q", c.pod, got, c.want)
		}
	}
	if _, ok := idx.owner("other-ns", "web-7d9f8c6b5-x2x9q"); ok {
		t.Error("namespace must match")
	}
}
