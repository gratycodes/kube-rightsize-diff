package manifest

import (
	"strings"
	"testing"
)

func mustLoad(t *testing.T, opts Options, paths ...string) []Container {
	t.Helper()
	cs, err := LoadPaths(paths, nil, opts)
	if err != nil {
		t.Fatalf("LoadPaths(%v): %v", paths, err)
	}
	return cs
}

func find(t *testing.T, cs []Container, key string) Container {
	t.Helper()
	for _, c := range cs {
		if c.Key().String() == key {
			return c
		}
	}
	t.Fatalf("container %s not found", key)
	return Container{}
}

func TestLoadDirectoryAllKinds(t *testing.T) {
	cs := mustLoad(t, Options{}, "../../testdata/shop/base")
	want := []string{
		"monitoring/DaemonSet/node-agent/agent",
		"shop/CronJob/nightly-report/report",
		"shop/Deployment/catalog/app",
		"shop/Deployment/checkout/app",
		"shop/Deployment/search/app",
		"shop/StatefulSet/cart-redis/redis",
	}
	if len(cs) != len(want) {
		t.Fatalf("got %d containers, want %d", len(cs), len(want))
	}
	for i, w := range want {
		if got := cs[i].Key().String(); got != w {
			t.Errorf("container %d = %s, want %s (sorted order)", i, got, w)
		}
	}
	co := find(t, cs, "shop/Deployment/checkout/app")
	if co.Replicas != 3 {
		t.Errorf("replicas = %d, want 3", co.Replicas)
	}
	if got := Cores(co.Resources.CPURequest); got != 0.5 {
		t.Errorf("cpu request = %v, want 0.5", got)
	}
	if got := Bytes(co.Resources.MemLimit); got != 1<<30 {
		t.Errorf("mem limit = %v, want 1Gi", got)
	}
}

func TestHelmRenderedDefaultsAndQuantities(t *testing.T) {
	cs := mustLoad(t, Options{DefaultNamespace: "web-ns"}, "../../testdata/manifests/helm-rendered.yaml")
	if len(cs) != 2 {
		t.Fatalf("got %d containers, want 2 (Service skipped, init containers off)", len(cs))
	}
	web := find(t, cs, "web-ns/Deployment/web/web")
	// Missing requests default to limits, like the API server does.
	if got := Cores(web.Resources.CPURequest); got != 0.5 {
		t.Errorf("cpu request defaulted to %v, want 0.5", got)
	}
	if got := Bytes(web.Resources.MemRequest); got != 256<<20 {
		t.Errorf("mem request defaulted to %v, want 256Mi", got)
	}
	if web.Replicas != 1 {
		t.Errorf("replicas = %d, want API default 1", web.Replicas)
	}
	side := find(t, cs, "web-ns/Deployment/web/sidecar")
	if got := Cores(side.Resources.CPURequest); got != 0.05 {
		t.Errorf("float cpu = %v, want 0.05", got)
	}
	if got := Bytes(side.Resources.MemRequest); got != 64<<20 {
		t.Errorf("plain-byte memory = %v, want 64Mi", got)
	}
	if side.Resources.CPULimit != nil || side.Resources.MemLimit != nil {
		t.Error("sidecar should have no limits")
	}
}

func TestIncludeInitContainers(t *testing.T) {
	cs := mustLoad(t, Options{IncludeInit: true}, "../../testdata/manifests/helm-rendered.yaml")
	find(t, cs, "default/Deployment/web/migrate")
}

func TestListAndJobParallelism(t *testing.T) {
	cs := mustLoad(t, Options{}, "../../testdata/manifests/list.json")
	if len(cs) != 2 {
		t.Fatalf("got %d, want 2", len(cs))
	}
	if j := find(t, cs, "etl/Job/backfill/worker"); j.Replicas != 3 {
		t.Errorf("job parallelism = %d, want 3", j.Replicas)
	}
	p := find(t, cs, "etl/Pod/debug/shell")
	if p.Resources.CPURequest != nil {
		t.Error("pod without resources should have nil request")
	}
}

func TestStdin(t *testing.T) {
	in := strings.NewReader("apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec:\n  containers: [{name: c, resources: {requests: {cpu: 10m}}}]\n")
	cs, err := LoadPaths([]string{"-"}, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Source != "-" {
		t.Fatalf("got %+v", cs)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"bad quantity": "kind: Pod\nmetadata: {name: p}\nspec: {containers: [{name: c, resources: {requests: {cpu: lots}}}]}\n",
		"no name":      "kind: Deployment\nmetadata: {}\nspec: {}\n",
		"bad yaml":     "kind: [\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(in), "x.yaml", Options{}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestDuplicateAcrossFilesRejected(t *testing.T) {
	_, err := LoadPaths([]string{"../../testdata/shop/base/app.yaml", "../../testdata/shop/base/app.yaml"}, nil, Options{})
	if err == nil || !strings.Contains(err.Error(), "defined twice") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestMissingPath(t *testing.T) {
	if _, err := LoadPaths([]string{"does-not-exist"}, nil, Options{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestDiff(t *testing.T) {
	base := mustLoad(t, Options{}, "../../testdata/shop/base")
	head := mustLoad(t, Options{}, "../../testdata/shop/head")
	got := map[string]ChangeType{}
	for _, c := range Diff(base, head) {
		got[c.Key.String()] = c.Type
	}
	want := map[string]ChangeType{
		"shop/Deployment/checkout/app":          Modified,
		"shop/Deployment/catalog/app":           Unchanged, // only the image changed
		"shop/Deployment/search/app":            Modified,
		"shop/Deployment/recommendations/model": Added,
		"shop/StatefulSet/cart-redis/redis":     Modified,
		"shop/CronJob/nightly-report/report":    Removed,
		"monitoring/DaemonSet/node-agent/agent": Unchanged,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d: %v", len(got), len(want), got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

func TestDiffReplicaOnlyChange(t *testing.T) {
	a := Container{Namespace: "n", Kind: "Deployment", Workload: "w", Name: "c", Replicas: 1}
	b := a
	b.Replicas = 5
	ch := Diff([]Container{a}, []Container{b})
	if len(ch) != 1 || ch[0].Type != Modified {
		t.Fatalf("replica change should be Modified, got %+v", ch)
	}
}
