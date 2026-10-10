// Package analyze compares requested resources with observed usage and
// prices the difference.
package analyze

import (
	"fmt"
	"math"
	"sort"

	"github.com/gratycodes/kube-rightsize-diff/internal/manifest"
	"github.com/gratycodes/kube-rightsize-diff/internal/usage"
)

// HoursPerMonth is the usual cloud-billing month (365*24/12).
const HoursPerMonth = 730

const gib = 1 << 30

// Severity orders findings: risk beats waste beats info.
type Severity string

const (
	Risk  Severity = "risk"
	Waste Severity = "waste"
	Info  Severity = "info"
)

func (s Severity) rank() int {
	switch s {
	case Risk:
		return 3
	case Waste:
		return 2
	case Info:
		return 1
	}
	return 0
}

// Config holds thresholds and prices. Zero values are not useful; start
// from DefaultConfig.
type Config struct {
	// Headroom added on top of observed usage for the suggested request.
	CPUHeadroom float64 `json:"cpu_headroom"`
	MemHeadroom float64 `json:"memory_headroom"`
	// A request is over-provisioned when it exceeds the suggestion by
	// this factor (1.5 = 50% above) and the waste is at least MinWasteUSD.
	OverFactor  float64 `json:"over_factor"`
	MinWasteUSD float64 `json:"min_waste_usd"`
	// LimitMargin flags a limit when usage is within this fraction of it.
	LimitMargin float64 `json:"limit_margin"`
	// Floors for suggestions so tiny containers don't get 1m CPU.
	MinCPUCores float64 `json:"min_cpu_cores"`
	MinMemBytes float64 `json:"min_memory_bytes"`
	// Prices. The defaults are illustrative examples, not a quote.
	PricePerVCPUHour float64 `json:"price_per_vcpu_hour"`
	PricePerGiBHour  float64 `json:"price_per_gib_hour"`
	// DaemonSetNodes multiplies DaemonSet cost (pods = nodes).
	DaemonSetNodes int `json:"daemonset_nodes"`
}

// DefaultConfig returns conservative defaults. The prices are EXAMPLE
// on-demand-style numbers so the tool produces a figure out of the box;
// replace them with your own blended rates.
func DefaultConfig() Config {
	return Config{
		CPUHeadroom:      0.20,
		MemHeadroom:      0.20,
		OverFactor:       1.5,
		MinWasteUSD:      1,
		LimitMargin:      0.10,
		MinCPUCores:      0.010,
		MinMemBytes:      32 << 20,
		PricePerVCPUHour: 0.0316,
		PricePerGiBHour:  0.0042,
		DaemonSetNodes:   1,
	}
}

// Validate rejects nonsensical settings.
func (c Config) Validate() error {
	switch {
	case c.CPUHeadroom < 0 || c.MemHeadroom < 0:
		return fmt.Errorf("headroom must be >= 0")
	case c.OverFactor < 1:
		return fmt.Errorf("over factor %v must be >= 1", c.OverFactor)
	case c.LimitMargin < 0 || c.LimitMargin >= 1:
		return fmt.Errorf("limit margin %v must be in [0, 1)", c.LimitMargin)
	case c.PricePerVCPUHour < 0 || c.PricePerGiBHour < 0:
		return fmt.Errorf("prices must be >= 0")
	case c.DaemonSetNodes < 1:
		return fmt.Errorf("daemonset nodes must be >= 1")
	}
	return nil
}

// Finding is one rule that fired for a container.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Resource string   `json:"resource"`
	Message  string   `json:"message"`
}

// Spec is the resource settings of one side (base or head).
type Spec struct {
	Replicas   int32    `json:"replicas"`
	CPURequest *float64 `json:"cpu_request_cores"`
	CPULimit   *float64 `json:"cpu_limit_cores"`
	MemRequest *float64 `json:"memory_request_bytes"`
	MemLimit   *float64 `json:"memory_limit_bytes"`
	// MonthlyUSD is the cost of the requests; nil when not meaningful
	// (Jobs and CronJobs run intermittently).
	MonthlyUSD *float64 `json:"monthly_usd"`
}

// Result is the analysis of one container.
type Result struct {
	Key         manifest.Key        `json:"-"`
	Namespace   string              `json:"namespace"`
	Kind        string              `json:"kind"`
	Workload    string              `json:"workload"`
	Container   string              `json:"container"`
	Change      manifest.ChangeType `json:"change"`
	Base        *Spec               `json:"base,omitempty"`
	Head        *Spec               `json:"head,omitempty"`
	Usage       *usage.Sample       `json:"usage,omitempty"`
	SuggestCPU  *float64            `json:"suggested_cpu_request_cores,omitempty"`
	SuggestMem  *float64            `json:"suggested_memory_request_bytes,omitempty"`
	SavingsUSD  float64             `json:"monthly_savings_if_rightsized_usd"`
	DeltaUSD    float64             `json:"monthly_delta_usd"`
	Findings    []Finding           `json:"findings"`
	Status      Severity            `json:"status"`
	PerNodeCost bool                `json:"per_node_cost,omitempty"`
}

func (r Result) has(sev Severity) bool {
	for _, f := range r.Findings {
		if f.Severity == sev {
			return true
		}
	}
	return false
}

// Totals summarises the results that are in scope.
type Totals struct {
	Containers int `json:"containers"`
	// Risks and Wastes count containers with at least one finding of that
	// severity; a container can be in both.
	Risks      int     `json:"risks"`
	Wastes     int     `json:"wastes"`
	NoUsage    int     `json:"no_usage"`
	BaseUSD    float64 `json:"base_monthly_usd"`
	HeadUSD    float64 `json:"head_monthly_usd"`
	DeltaUSD   float64 `json:"monthly_delta_usd"`
	SavingsUSD float64 `json:"monthly_savings_if_rightsized_usd"`
	Unchanged  int     `json:"unchanged_hidden"`
}

// Report is the full output of an analysis.
type Report struct {
	DiffMode    bool     `json:"diff_mode"`
	Config      Config   `json:"config"`
	UsageSource string   `json:"usage_source"`
	CPUPct      float64  `json:"cpu_percentile"`
	MemPct      float64  `json:"memory_percentile"`
	Window      string   `json:"window"`
	Results     []Result `json:"results"`
	Totals      Totals   `json:"totals"`
}

// Analyze evaluates head containers against usage. When base is non-nil
// only containers that were added, modified or removed between base and
// head are reported; otherwise every head container is.
func Analyze(base, head []manifest.Container, u *usage.Data, cfg Config) (Report, error) {
	if err := cfg.Validate(); err != nil {
		return Report{}, err
	}
	rep := Report{Config: cfg, DiffMode: base != nil}
	if u != nil {
		rep.UsageSource, rep.CPUPct, rep.MemPct, rep.Window = u.Source, u.CPUPercentile, u.MemPercentile, u.Window
	}
	var changes []manifest.Change
	if base != nil {
		changes = manifest.Diff(base, head)
	} else {
		for i := range head {
			changes = append(changes, manifest.Change{Key: head[i].Key(), Type: manifest.Unchanged, Head: &head[i]})
		}
	}
	for _, ch := range changes {
		if rep.DiffMode && ch.Type == manifest.Unchanged {
			rep.Totals.Unchanged++
			continue
		}
		r := evaluate(ch, u, cfg)
		if !rep.DiffMode {
			r.Change = ""
		}
		rep.Results = append(rep.Results, r)
		t := &rep.Totals
		t.Containers++
		if r.has(Risk) {
			t.Risks++
		}
		if r.has(Waste) {
			t.Wastes++
		}
		if r.Usage == nil && r.Head != nil {
			t.NoUsage++
		}
		if r.Base != nil && r.Base.MonthlyUSD != nil {
			t.BaseUSD += *r.Base.MonthlyUSD
		}
		if r.Head != nil && r.Head.MonthlyUSD != nil {
			t.HeadUSD += *r.Head.MonthlyUSD
		}
		t.DeltaUSD += r.DeltaUSD
		t.SavingsUSD += r.SavingsUSD
	}
	t := &rep.Totals
	t.BaseUSD, t.HeadUSD, t.DeltaUSD, t.SavingsUSD = round2(t.BaseUSD), round2(t.HeadUSD), round2(t.DeltaUSD), round2(t.SavingsUSD)
	sort.SliceStable(rep.Results, func(i, j int) bool {
		a, b := rep.Results[i], rep.Results[j]
		if a.Status.rank() != b.Status.rank() {
			return a.Status.rank() > b.Status.rank()
		}
		return a.Key.String() < b.Key.String()
	})
	return rep, nil
}

func evaluate(ch manifest.Change, u *usage.Data, cfg Config) Result {
	r := Result{
		Key: ch.Key, Namespace: ch.Key.Namespace, Kind: ch.Key.Kind, Workload: ch.Key.Workload,
		Container: ch.Key.Container, Change: ch.Type, Findings: []Finding{},
	}
	r.PerNodeCost = ch.Key.Kind == "DaemonSet" && cfg.DaemonSetNodes == 1
	if ch.Base != nil {
		r.Base = spec(*ch.Base, cfg)
	}
	if ch.Head == nil { // removed
		if r.Base.MonthlyUSD != nil {
			r.DeltaUSD = round2(-*r.Base.MonthlyUSD)
		}
		r.Status = Info
		return r
	}
	r.Head = spec(*ch.Head, cfg)
	if r.Head.MonthlyUSD != nil {
		d := *r.Head.MonthlyUSD
		if r.Base != nil && r.Base.MonthlyUSD != nil {
			d -= *r.Base.MonthlyUSD
		}
		r.DeltaUSD = round2(d)
	}

	res := ch.Head.Resources
	add := func(rule string, sev Severity, resource, format string, args ...any) {
		r.Findings = append(r.Findings, Finding{Rule: rule, Severity: sev, Resource: resource, Message: fmt.Sprintf(format, args...)})
	}
	if res.CPURequest == nil {
		add("missing-cpu-request", Risk, "cpu", "no CPU request: the scheduler reserves nothing and the container gets the smallest share under contention")
	}
	if res.MemRequest == nil {
		add("missing-memory-request", Risk, "memory", "no memory request: the scheduler reserves nothing and the pod is among the first evicted under node memory pressure")
	}
	if res.MemLimit == nil {
		add("missing-memory-limit", Risk, "memory", "no memory limit: a leak can consume the node's memory before the kubelet reacts")
	}

	s, ok := u.Lookup(ch.Key)
	if !ok {
		msg := "no usage data for this container; only static checks ran"
		if ch.Type == manifest.Added {
			msg = "new container, no usage history yet; only static checks ran"
		}
		add("no-usage-data", Info, "", "%s", msg)
		r.Status = worst(r.Findings)
		return r
	}
	r.Usage = &s
	replicas := float64(r.Head.Replicas) * nodeFactor(ch.Key.Kind, cfg)
	costApplies := r.Head.MonthlyUSD != nil

	sugCPU := math.Max(cfg.MinCPUCores, roundUp(s.CPUCores*(1+cfg.CPUHeadroom), 0.005))
	sugMem := math.Max(cfg.MinMemBytes, roundUp(s.MemoryBytes*(1+cfg.MemHeadroom), 8<<20))
	r.SuggestCPU, r.SuggestMem = &sugCPU, &sugMem
	cpuReq, memReq := manifest.Cores(res.CPURequest), manifest.Bytes(res.MemRequest)

	cpuWaste := replicas * HoursPerMonth * math.Max(0, cpuReq-sugCPU) * cfg.PricePerVCPUHour
	memWaste := replicas * HoursPerMonth * math.Max(0, memReq-sugMem) / gib * cfg.PricePerGiBHour
	if costApplies {
		r.SavingsUSD = round2(cpuWaste + memWaste)
	}

	if res.CPURequest != nil {
		switch {
		case s.CPUCores > cpuReq:
			add("cpu-request-below-usage", Risk, "cpu", "observed CPU %s exceeds the %s request; under node contention it is held to its request share and slows down",
				FormatCPU(s.CPUCores), FormatCPU(cpuReq))
		case cpuReq > sugCPU*cfg.OverFactor && (!costApplies || cpuWaste >= cfg.MinWasteUSD):
			add("cpu-over-provisioned", Waste, "cpu", "request %s is %.1fx the suggested %s (observed %s + %.0f%% headroom)%s",
				FormatCPU(cpuReq), cpuReq/sugCPU, FormatCPU(sugCPU), FormatCPU(s.CPUCores), cfg.CPUHeadroom*100, usdSuffix(costApplies, cpuWaste))
		}
	}
	if res.CPULimit != nil {
		lim := manifest.Cores(res.CPULimit)
		if s.CPUCores >= lim*(1-cfg.LimitMargin) {
			add("cpu-limit-throttling", Risk, "cpu", "observed CPU %s is %.0f%% of the %s limit; expect CFS throttling and latency spikes",
				FormatCPU(s.CPUCores), 100*s.CPUCores/lim, FormatCPU(lim))
		}
	}
	if res.MemRequest != nil {
		switch {
		case s.MemoryBytes > memReq:
			add("memory-request-below-usage", Risk, "memory", "observed memory %s exceeds the %s request; the pod is an early eviction candidate under node memory pressure",
				FormatMem(s.MemoryBytes), FormatMem(memReq))
		case memReq > sugMem*cfg.OverFactor && (!costApplies || memWaste >= cfg.MinWasteUSD):
			add("memory-over-provisioned", Waste, "memory", "request %s is %.1fx the suggested %s (observed %s + %.0f%% headroom)%s",
				FormatMem(memReq), memReq/sugMem, FormatMem(sugMem), FormatMem(s.MemoryBytes), cfg.MemHeadroom*100, usdSuffix(costApplies, memWaste))
		}
	}
	if res.MemLimit != nil {
		lim := manifest.Bytes(res.MemLimit)
		if s.MemoryBytes >= lim*(1-cfg.LimitMargin) {
			add("memory-limit-too-close", Risk, "memory", "observed memory %s is %.0f%% of the %s limit; OOMKill risk on the next spike",
				FormatMem(s.MemoryBytes), 100*s.MemoryBytes/lim, FormatMem(lim))
		}
	}
	r.Status = worst(r.Findings)
	return r
}

func usdSuffix(applies bool, usd float64) string {
	if !applies {
		return ""
	}
	return fmt.Sprintf(", about $%.2f/month above the suggestion", usd)
}

func nodeFactor(kind string, cfg Config) float64 {
	if kind == "DaemonSet" {
		return float64(cfg.DaemonSetNodes)
	}
	return 1
}

func spec(c manifest.Container, cfg Config) *Spec {
	s := &Spec{Replicas: c.Replicas}
	res := c.Resources
	if res.CPURequest != nil {
		v := manifest.Cores(res.CPURequest)
		s.CPURequest = &v
	}
	if res.CPULimit != nil {
		v := manifest.Cores(res.CPULimit)
		s.CPULimit = &v
	}
	if res.MemRequest != nil {
		v := manifest.Bytes(res.MemRequest)
		s.MemRequest = &v
	}
	if res.MemLimit != nil {
		v := manifest.Bytes(res.MemLimit)
		s.MemLimit = &v
	}
	if c.Kind != "Job" && c.Kind != "CronJob" {
		pods := float64(c.Replicas) * nodeFactor(c.Kind, cfg)
		cost := round2(pods * HoursPerMonth * (manifest.Cores(res.CPURequest)*cfg.PricePerVCPUHour +
			manifest.Bytes(res.MemRequest)/gib*cfg.PricePerGiBHour))
		s.MonthlyUSD = &cost
	}
	return s
}

func worst(fs []Finding) Severity {
	w := Severity("ok")
	for _, f := range fs {
		if f.Severity.rank() > w.rank() {
			w = f.Severity
		}
	}
	return w
}

func roundUp(v, step float64) float64 {
	return math.Ceil(v/step-1e-9) * step
}

func round2(v float64) float64 {
	r := math.Round(v*100) / 100
	if r == 0 {
		return 0 // avoid -0 in output
	}
	return r
}

// FormatCPU renders cores the way manifests usually do (500m, 2).
func FormatCPU(cores float64) string {
	if cores > 0 && cores < 0.0005 {
		return "<1m" // idle but not zero; "0" reads like missing data
	}
	m := int64(math.Round(cores * 1000))
	if m%1000 == 0 {
		return fmt.Sprintf("%d", m/1000)
	}
	return fmt.Sprintf("%dm", m)
}

// FormatMem renders bytes as Gi or Mi.
func FormatMem(b float64) string {
	mi := b / (1 << 20)
	if mi >= 1024 && math.Mod(mi, 1024) == 0 {
		return fmt.Sprintf("%.0fGi", mi/1024)
	}
	if mi >= 1024 {
		return fmt.Sprintf("%.2fGi", mi/1024)
	}
	if mi == math.Trunc(mi) {
		return fmt.Sprintf("%.0fMi", mi)
	}
	return fmt.Sprintf("%.1fMi", mi)
}
