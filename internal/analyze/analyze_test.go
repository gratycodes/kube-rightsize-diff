package analyze

import (
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/gratycodes/kube-rightsize-diff/internal/manifest"
	"github.com/gratycodes/kube-rightsize-diff/internal/usage"
)

func load(t *testing.T, path string) []manifest.Container {
	t.Helper()
	cs, err := manifest.LoadPaths([]string{path}, nil, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func shopReport(t *testing.T, diff bool) Report {
	t.Helper()
	u, err := usage.LoadFile("../../testdata/shop/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	var base []manifest.Container
	if diff {
		base = load(t, "../../testdata/shop/base")
	}
	rep, err := Analyze(base, load(t, "../../testdata/shop/head"), u, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func rules(r Result) string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, f.Rule)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func byWorkload(rep Report) map[string]Result {
	m := map[string]Result{}
	for _, r := range rep.Results {
		m[r.Workload] = r
	}
	return m
}

func TestShopDiff(t *testing.T) {
	rep := shopReport(t, true)
	got := byWorkload(rep)
	want := map[string]struct {
		change manifest.ChangeType
		status Severity
		rules  string
	}{
		"checkout":        {manifest.Modified, Waste, "cpu-over-provisioned,memory-over-provisioned"},
		"search":          {manifest.Modified, Risk, "cpu-request-below-usage"},
		"cart-redis":      {manifest.Modified, Risk, "cpu-over-provisioned,memory-limit-too-close"},
		"recommendations": {manifest.Added, Risk, "missing-cpu-request,missing-memory-limit,missing-memory-request,no-usage-data"},
		"nightly-report":  {manifest.Removed, Info, ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %v", len(got), len(want), got)
	}
	for w, exp := range want {
		r := got[w]
		if r.Change != exp.change || r.Status != exp.status || rules(r) != exp.rules {
			t.Errorf("%s: change=%s status=%s rules=%q; want %s %s %q", w, r.Change, r.Status, rules(r), exp.change, exp.status, exp.rules)
		}
	}
	if rep.Totals.Unchanged != 2 {
		t.Errorf("unchanged hidden = %d, want 2 (catalog, node-agent)", rep.Totals.Unchanged)
	}
	if rep.Totals.Risks != 3 || rep.Totals.Wastes != 2 || rep.Totals.NoUsage != 1 {
		t.Errorf("totals = %+v", rep.Totals)
	}
	// Risk first, then waste, then info.
	if rep.Results[0].Status != Risk || rep.Results[len(rep.Results)-1].Status != Info {
		t.Errorf("results not sorted by severity: %v", rep.Results)
	}
}

func TestShopCosts(t *testing.T) {
	r := byWorkload(shopReport(t, true))["checkout"]
	// head: 4 pods * 730h * (2 vCPU * 0.0316 + 2 GiB * 0.0042) = 209.07
	// base: 3 pods * 730h * (0.5 * 0.0316 + 0.5 * 0.0042)      =  39.20
	if *r.Head.MonthlyUSD != 209.07 || *r.Base.MonthlyUSD != 39.20 || r.DeltaUSD != 169.87 {
		t.Errorf("checkout cost head=%v base=%v delta=%v", *r.Head.MonthlyUSD, *r.Base.MonthlyUSD, r.DeltaUSD)
	}
	if *r.SuggestCPU != 0.22 || *r.SuggestMem != 360<<20 {
		t.Errorf("suggestions cpu=%v mem=%v", *r.SuggestCPU, *r.SuggestMem)
	}
	// savings: 4*730*((2-0.22)*0.0316 + (2-360/1024)*0.0042) = 184.47
	if math.Abs(r.SavingsUSD-184.47) > 0.01 {
		t.Errorf("savings = %v", r.SavingsUSD)
	}
	removed := byWorkload(shopReport(t, true))["nightly-report"]
	if removed.Base.MonthlyUSD != nil || removed.DeltaUSD != 0 {
		t.Errorf("CronJob cost should be n/a, got %+v", removed.Base)
	}
}

func TestFullScanReportsEverything(t *testing.T) {
	rep := shopReport(t, false)
	if rep.DiffMode || len(rep.Results) != 6 || rep.Totals.Unchanged != 0 {
		t.Fatalf("full scan: diff=%v results=%d", rep.DiffMode, len(rep.Results))
	}
	m := byWorkload(rep)
	if m["catalog"].Change != "" {
		t.Error("change should be blank outside diff mode")
	}
	// catalog memory: 256Mi is above 1.5x the 144Mi suggestion, but the
	// excess is worth $0.67/month, under MinWasteUSD, so only CPU fires.
	if rules(m["catalog"]) != "cpu-over-provisioned" {
		t.Errorf("catalog rules = %q", rules(m["catalog"]))
	}
	if !m["node-agent"].PerNodeCost {
		t.Error("daemonset cost should be marked per node")
	}
}

func c(kind string, replicas int32, cpuReq, cpuLim, memReq, memLim string) manifest.Container {
	return manifest.Container{
		Namespace: "ns", Kind: kind, Workload: "w", Name: "c", Replicas: replicas,
		Resources: manifest.Resources{CPURequest: q(cpuReq), CPULimit: q(cpuLim), MemRequest: q(memReq), MemLimit: q(memLim)},
	}
}

func one(t *testing.T, ct manifest.Container, cpu, mem float64, cfg Config) Result {
	t.Helper()
	u := usage.NewData("t", 0.95, 0.99, "1d")
	u.Set(ct.Key(), usage.Sample{CPUCores: cpu, MemoryBytes: mem})
	rep, err := Analyze(nil, []manifest.Container{ct}, u, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rep.Results[0]
}

func TestRules(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name     string
		ct       manifest.Container
		cpu, mem float64
		rules    string
		status   Severity
	}{
		{"right-sized", c("Deployment", 2, "120m", "", "128Mi", "256Mi"), 0.1, 100 << 20, "", "ok"},
		{"cpu limit throttling", c("Deployment", 1, "500m", "500m", "128Mi", "256Mi"), 0.48, 100 << 20, "cpu-limit-throttling", Risk},
		{"memory below request", c("Deployment", 1, "100m", "", "128Mi", "1Gi"), 0.09, 200 << 20, "memory-request-below-usage", Risk},
		{"memory at limit", c("Deployment", 1, "100m", "", "512Mi", "512Mi"), 0.09, 500 << 20, "memory-limit-too-close", Risk},
		// Over factor reached but waste is below MinWasteUSD ($1): stays quiet.
		{"tiny waste ignored", c("Deployment", 1, "50m", "", "64Mi", "128Mi"), 0.01, 20 << 20, "", "ok"},
		// Jobs have no monthly cost, so the factor alone decides.
		{"job over-provisioned", c("Job", 1, "50m", "", "64Mi", "128Mi"), 0.01, 20 << 20, "cpu-over-provisioned,memory-over-provisioned", Waste},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := one(t, tc.ct, tc.cpu, tc.mem, cfg)
			if rules(r) != tc.rules || r.Status != tc.status {
				t.Errorf("rules=%q status=%s; want %q %s (%+v)", rules(r), r.Status, tc.rules, tc.status, r.Findings)
			}
		})
	}
}

func TestMinimumSuggestions(t *testing.T) {
	r := one(t, c("Deployment", 1, "1", "", "1Gi", "1Gi"), 0.0001, 1<<20, DefaultConfig())
	if *r.SuggestCPU != 0.01 || *r.SuggestMem != 32<<20 {
		t.Errorf("floors not applied: cpu=%v mem=%v", *r.SuggestCPU, *r.SuggestMem)
	}
}

func TestDaemonSetNodes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DaemonSetNodes = 10
	r := one(t, c("DaemonSet", 1, "1", "", "", ""), 1, 1<<30, cfg)
	if *r.Head.MonthlyUSD != round2(10*730*0.0316) || r.PerNodeCost {
		t.Errorf("cost=%v perNode=%v", *r.Head.MonthlyUSD, r.PerNodeCost)
	}
}

func TestConfigValidate(t *testing.T) {
	bad := []func(*Config){
		func(c *Config) { c.CPUHeadroom = -1 },
		func(c *Config) { c.OverFactor = 0.5 },
		func(c *Config) { c.LimitMargin = 1 },
		func(c *Config) { c.PricePerGiBHour = -0.1 },
		func(c *Config) { c.DaemonSetNodes = 0 },
	}
	for i, mut := range bad {
		cfg := DefaultConfig()
		mut(&cfg)
		if _, err := Analyze(nil, nil, nil, cfg); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestFormat(t *testing.T) {
	cpu := map[float64]string{0.5: "500m", 2: "2", 0.0123: "12m", 1.25: "1250m", 0.0002: "<1m", 0: "0"}
	for in, want := range cpu {
		if got := FormatCPU(in); got != want {
			t.Errorf("FormatCPU(%v) = %s, want %s", in, got, want)
		}
	}
	mem := map[float64]string{512 << 20: "512Mi", 2 << 30: "2Gi", 1.5 * (1 << 30): "1.50Gi", 1.5 * (1 << 20): "1.5Mi"}
	for in, want := range mem {
		if got := FormatMem(in); got != want {
			t.Errorf("FormatMem(%v) = %s, want %s", in, got, want)
		}
	}
}
