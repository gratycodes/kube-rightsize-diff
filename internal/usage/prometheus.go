package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gratycodes/kube-rightsize-diff/internal/manifest"
)

// PromConfig configures FetchPrometheus.
type PromConfig struct {
	URL           string
	CPUPercentile float64 // e.g. 0.95
	MemPercentile float64 // e.g. 0.99
	Window        string  // Prometheus duration, e.g. "7d"
	Resolution    string  // subquery step, e.g. "1m"
	RateInterval  string  // rate() interval, e.g. "5m"
	BearerToken   string
	Timeout       time.Duration
	// Extra label matchers added to every selector, e.g. `cluster="prod"`.
	ExtraSelector string
}

var promDuration = regexp.MustCompile(`^([0-9]+(ms|s|m|h|d|w|y))+$`)

func (c *PromConfig) validate() error {
	if c.URL == "" {
		return errors.New("prometheus url is empty")
	}
	for name, p := range map[string]float64{"cpu percentile": c.CPUPercentile, "memory percentile": c.MemPercentile} {
		if p <= 0 || p > 1 {
			return fmt.Errorf("%s %v must be in (0, 1]", name, p)
		}
	}
	for name, d := range map[string]string{"window": c.Window, "resolution": c.Resolution, "rate interval": c.RateInterval} {
		if !promDuration.MatchString(d) {
			return fmt.Errorf("%s %q is not a Prometheus duration", name, d)
		}
	}
	if strings.ContainsAny(c.ExtraSelector, "{}") {
		return errors.New("extra selector must be bare label matchers, without braces")
	}
	return nil
}

// Queries returns the PromQL used for CPU (cores) and memory (bytes).
// Exposed so users can paste them into Grafana to sanity-check numbers.
func (c PromConfig) Queries(namespaces []string) (cpu, mem string) {
	quoted := make([]string, len(namespaces))
	for i, n := range namespaces {
		quoted[i] = regexp.QuoteMeta(n)
	}
	sel := fmt.Sprintf(`namespace=~"%s",container!="",container!="POD"`, strings.Join(quoted, "|"))
	if c.ExtraSelector != "" {
		sel += "," + c.ExtraSelector
	}
	cpu = fmt.Sprintf(`quantile_over_time(%g, sum by (namespace, pod, container) (rate(container_cpu_usage_seconds_total{%s}[%s]))[%s:%s])`,
		c.CPUPercentile, sel, c.RateInterval, c.Window, c.Resolution)
	mem = fmt.Sprintf(`quantile_over_time(%g, max by (namespace, pod, container) (container_memory_working_set_bytes{%s})[%s:%s])`,
		c.MemPercentile, sel, c.Window, c.Resolution)
	return cpu, mem
}

// FetchPrometheus queries usage for the given containers. Per-pod
// percentiles are folded into one value per container by taking the max
// across the workload's pods, which is the conservative choice for sizing.
func FetchPrometheus(ctx context.Context, cfg PromConfig, containers []manifest.Container, hc *http.Client) (*Data, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	nsSet := map[string]bool{}
	for _, c := range containers {
		nsSet[c.Namespace] = true
	}
	namespaces := make([]string, 0, len(nsSet))
	for n := range nsSet {
		namespaces = append(namespaces, n)
	}
	sort.Strings(namespaces)

	d := NewData("prometheus "+cfg.URL, cfg.CPUPercentile, cfg.MemPercentile, cfg.Window)
	if len(namespaces) == 0 {
		return d, nil
	}
	cpuQ, memQ := cfg.Queries(namespaces)
	cpu, err := instantQuery(ctx, hc, cfg, cpuQ)
	if err != nil {
		return nil, fmt.Errorf("cpu query: %w", err)
	}
	mem, err := instantQuery(ctx, hc, cfg, memQ)
	if err != nil {
		return nil, fmt.Errorf("memory query: %w", err)
	}

	owners := newOwnerIndex(containers)
	wanted := map[manifest.Key]bool{}
	for _, c := range containers {
		wanted[c.Key()] = true
	}
	agg := map[manifest.Key]*Sample{}
	pods := map[manifest.Key]map[string]bool{}
	fold := func(series []promSample, apply func(*Sample, float64)) {
		for _, s := range series {
			w, ok := owners.owner(s.namespace, s.pod)
			if !ok {
				continue
			}
			k := manifest.Key{Namespace: w.namespace, Kind: w.kind, Workload: w.name, Container: s.container}
			if !wanted[k] {
				continue
			}
			if agg[k] == nil {
				agg[k] = &Sample{}
				pods[k] = map[string]bool{}
			}
			apply(agg[k], s.value)
			pods[k][s.pod] = true
		}
	}
	fold(cpu, func(s *Sample, v float64) { s.CPUCores = max(s.CPUCores, v) })
	fold(mem, func(s *Sample, v float64) { s.MemoryBytes = max(s.MemoryBytes, v) })
	for k, s := range agg {
		s.Pods = len(pods[k])
		d.Set(k, *s)
	}
	return d, nil
}

type promSample struct {
	namespace, pod, container string
	value                     float64
}

type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func instantQuery(ctx context.Context, hc *http.Client, cfg PromConfig, q string) ([]promSample, error) {
	endpoint := strings.TrimRight(cfg.URL, "/") + "/api/v1/query"
	form := url.Values{"query": {q}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("HTTP %d, undecodable body: %w", resp.StatusCode, err)
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("HTTP %d: %s: %s", resp.StatusCode, pr.ErrorType, pr.Error)
	}
	if pr.Data.ResultType != "vector" {
		return nil, fmt.Errorf("unexpected result type %q", pr.Data.ResultType)
	}
	out := make([]promSample, 0, len(pr.Data.Result))
	for _, r := range pr.Data.Result {
		s, ok := r.Value[1].(string)
		if !ok {
			return nil, fmt.Errorf("malformed sample value %v", r.Value)
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("sample value %q: %w", s, err)
		}
		if v != v { // NaN, e.g. no data points in window
			continue
		}
		out = append(out, promSample{namespace: r.Metric["namespace"], pod: r.Metric["pod"], container: r.Metric["container"], value: v})
	}
	return out, nil
}
