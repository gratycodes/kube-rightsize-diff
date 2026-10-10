// Command kube-rightsize-diff checks Kubernetes CPU and memory requests in
// rendered manifests against observed usage, optionally only for what a
// pull request changed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gratycodes/kube-rightsize-diff/internal/analyze"
	"github.com/gratycodes/kube-rightsize-diff/internal/manifest"
	"github.com/gratycodes/kube-rightsize-diff/internal/report"
	"github.com/gratycodes/kube-rightsize-diff/internal/usage"
)

// version is set by -ldflags at release time.
var version = "dev"

const (
	exitOK        = 0
	exitError     = 1
	exitThreshold = 2
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, http.DefaultClient)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, hc *http.Client) int {
	fs := flag.NewFlagSet("kube-rightsize-diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var head, base multiFlag
	fs.Var(&head, "head", "rendered manifests after the change: file, directory or - for stdin (repeatable, required)")
	fs.Var(&base, "base", "rendered manifests before the change (repeatable); enables diff mode")
	usageFile := fs.String("usage", "", "usage snapshot file (.json or .csv)")
	promURL := fs.String("prometheus-url", "", "Prometheus base URL; bearer token read from $PROMETHEUS_TOKEN")
	cpuPct := fs.Float64("cpu-percentile", 0.95, "CPU usage percentile to size against")
	memPct := fs.Float64("memory-percentile", 0.99, "memory usage percentile to size against")
	window := fs.String("window", "7d", "Prometheus lookback window")
	resolution := fs.String("resolution", "1m", "Prometheus subquery resolution")
	rateInterval := fs.String("rate-interval", "5m", "rate() interval for CPU")
	promSelector := fs.String("prometheus-selector", "", `extra label matchers, e.g. cluster="prod"`)
	promTimeout := fs.Duration("prometheus-timeout", 60*time.Second, "Prometheus request timeout")
	namespace := fs.String("namespace", "default", "namespace for objects without metadata.namespace")
	initC := fs.Bool("include-init-containers", false, "also check initContainers")

	def := analyze.DefaultConfig()
	cfg := def
	fs.Float64Var(&cfg.CPUHeadroom, "cpu-headroom", def.CPUHeadroom, "headroom over observed CPU for suggestions (0.2 = 20%)")
	fs.Float64Var(&cfg.MemHeadroom, "memory-headroom", def.MemHeadroom, "headroom over observed memory for suggestions")
	fs.Float64Var(&cfg.OverFactor, "over-factor", def.OverFactor, "flag requests above suggestion * factor")
	fs.Float64Var(&cfg.MinWasteUSD, "min-waste-usd", def.MinWasteUSD, "ignore over-provisioning worth less than this per month")
	fs.Float64Var(&cfg.LimitMargin, "limit-margin", def.LimitMargin, "flag limits when usage is within this fraction of them")
	fs.Float64Var(&cfg.PricePerVCPUHour, "price-vcpu-hour", def.PricePerVCPUHour, "price per vCPU-hour (default is an EXAMPLE price)")
	fs.Float64Var(&cfg.PricePerGiBHour, "price-gib-hour", def.PricePerGiBHour, "price per GiB-hour of memory (default is an EXAMPLE price)")
	fs.IntVar(&cfg.DaemonSetNodes, "daemonset-nodes", def.DaemonSetNodes, "node count used to price DaemonSets")

	format := fs.String("format", "table", "output format: table, json or markdown")
	output := fs.String("output", "", "write the report to this file instead of stdout")
	jsonOutput := fs.String("json-output", "", "also write the JSON report to this file (used by the GitHub Action)")
	failOn := fs.String("fail-on", "none", "exit 2 when a finding of this severity exists: none, risk, waste or any")
	maxIncrease := fs.Float64("max-cost-increase", -1, "exit 2 when requested cost rises by more than this many USD/month (diff mode; -1 disables)")
	showVersion := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	if *showVersion {
		fmt.Fprintln(stdout, "kube-rightsize-diff", version)
		return exitOK
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "kube-rightsize-diff: "+format+"\n", a...)
		return exitError
	}
	if fs.NArg() > 0 {
		return fail("unexpected arguments %v", fs.Args())
	}
	if len(head) == 0 {
		return fail("--head is required")
	}
	if *usageFile != "" && *promURL != "" {
		return fail("use either --usage or --prometheus-url, not both")
	}
	switch *format {
	case "table", "json", "markdown", "md":
	default:
		return fail("unknown --format %q", *format)
	}
	switch *failOn {
	case "none", "risk", "waste", "any":
	default:
		return fail("unknown --fail-on %q", *failOn)
	}
	if countStdin(head)+countStdin(base) > 1 {
		return fail("stdin (-) can only be used once")
	}

	opts := manifest.Options{DefaultNamespace: *namespace, IncludeInit: *initC}
	headC, err := manifest.LoadPaths(head, stdin, opts)
	if err != nil {
		return fail("reading head manifests: %v", err)
	}
	var baseC []manifest.Container
	if len(base) > 0 {
		baseC, err = manifest.LoadPaths(base, stdin, opts)
		if err != nil {
			return fail("reading base manifests: %v", err)
		}
		if baseC == nil {
			baseC = []manifest.Container{} // diff mode even if base is empty
		}
	}

	var u *usage.Data
	switch {
	case *usageFile != "":
		u, err = usage.LoadFile(*usageFile)
	case *promURL != "":
		pc := usage.PromConfig{
			URL: *promURL, CPUPercentile: *cpuPct, MemPercentile: *memPct, Window: *window,
			Resolution: *resolution, RateInterval: *rateInterval, ExtraSelector: *promSelector,
			BearerToken: os.Getenv("PROMETHEUS_TOKEN"), Timeout: *promTimeout,
		}
		u, err = usage.FetchPrometheus(ctx, pc, headC, hc)
	default:
		fmt.Fprintln(stderr, "kube-rightsize-diff: no --usage or --prometheus-url given; running static checks only")
	}
	if err != nil {
		return fail("loading usage: %v", err)
	}

	rep, err := analyze.Analyze(baseC, headC, u, cfg)
	if err != nil {
		return fail("%v", err)
	}

	if err := writeReport(rep, *format, *output, stdout); err != nil {
		return fail("writing report: %v", err)
	}
	if *jsonOutput != "" {
		if err := writeReport(rep, "json", *jsonOutput, nil); err != nil {
			return fail("writing JSON report: %v", err)
		}
	}

	if reason := thresholdReached(rep, *failOn, *maxIncrease); reason != "" {
		fmt.Fprintln(stderr, "kube-rightsize-diff: failing:", reason)
		return exitThreshold
	}
	return exitOK
}

func writeReport(rep analyze.Report, format, path string, stdout io.Writer) (err error) {
	out := stdout
	if path != "" {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}()
		out = f
	}
	switch format {
	case "json":
		return report.JSON(out, rep)
	case "markdown", "md":
		return report.Markdown(out, rep)
	default:
		return report.Table(out, rep)
	}
}

func countStdin(paths []string) int {
	n := 0
	for _, p := range paths {
		if p == "-" {
			n++
		}
	}
	return n
}

func thresholdReached(rep analyze.Report, failOn string, maxIncrease float64) string {
	t := rep.Totals
	switch {
	case (failOn == "risk" || failOn == "any") && t.Risks > 0:
		return fmt.Sprintf("%d containers at risk", t.Risks)
	case (failOn == "waste" || failOn == "any") && t.Wastes > 0:
		return fmt.Sprintf("%d containers over-provisioned", t.Wastes)
	case maxIncrease >= 0 && rep.DiffMode && t.DeltaUSD > maxIncrease:
		return fmt.Sprintf("requested cost rises by $%.2f/month, above the $%.2f limit", t.DeltaUSD, maxIncrease)
	}
	return ""
}
