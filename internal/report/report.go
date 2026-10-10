// Package report renders an analysis as a terminal table, JSON or a
// Markdown pull request comment.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/gratycodes/kube-rightsize-diff/internal/analyze"
)

// Marker identifies the tool's PR comment so the Action can update it in
// place instead of posting a new one on every push.
const Marker = "<!-- kube-rightsize-diff -->"

// JSON writes the report as indented JSON.
func JSON(w io.Writer, rep analyze.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// Table writes a fixed-width table for terminals and CI logs.
func Table(w io.Writer, rep analyze.Report) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := "STATUS\tWORKLOAD\tCONTAINER\t"
	if rep.DiffMode {
		header += "CHANGE\t"
	}
	header += "CPU REQ\tCPU USED\tCPU SUGGEST\tMEM REQ\tMEM USED\tMEM SUGGEST\tMEM LIMIT\tPODS\t$/MONTH\tFINDINGS"
	fmt.Fprintln(tw, header)
	for _, r := range rep.Results {
		row := []string{strings.ToUpper(string(r.Status)), workload(r), r.Container}
		if rep.DiffMode {
			row = append(row, string(r.Change))
		}
		s := r.Head
		if s == nil {
			s = r.Base
		}
		row = append(row,
			cpu(s.CPURequest), usedCPU(r), cpu(r.SuggestCPU),
			mem(s.MemRequest), usedMem(r), mem(r.SuggestMem), mem(s.MemLimit),
			pods(r, s), money(r, s), ruleList(r))
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\n%s\n", summaryLine(rep))
	return err
}

func workload(r analyze.Result) string {
	return fmt.Sprintf("%s/%s/%s", r.Namespace, strings.ToLower(r.Kind), r.Workload)
}

func cpu(v *float64) string {
	if v == nil {
		return "-"
	}
	return analyze.FormatCPU(*v)
}

func mem(v *float64) string {
	if v == nil {
		return "-"
	}
	return analyze.FormatMem(*v)
}

func usedCPU(r analyze.Result) string {
	if r.Usage == nil {
		return "-"
	}
	return analyze.FormatCPU(r.Usage.CPUCores)
}

func usedMem(r analyze.Result) string {
	if r.Usage == nil {
		return "-"
	}
	return analyze.FormatMem(r.Usage.MemoryBytes)
}

func pods(r analyze.Result, s *analyze.Spec) string {
	p := fmt.Sprintf("%d", s.Replicas)
	if r.Base != nil && r.Head != nil && r.Base.Replicas != r.Head.Replicas {
		p = fmt.Sprintf("%d→%d", r.Base.Replicas, r.Head.Replicas)
	}
	if r.PerNodeCost {
		p += "/node"
	}
	return p
}

func money(r analyze.Result, s *analyze.Spec) string {
	if s.MonthlyUSD == nil {
		return "n/a"
	}
	if r.Head == nil {
		return fmt.Sprintf("%s (removed)", signed(r.DeltaUSD))
	}
	out := fmt.Sprintf("$%.2f", *s.MonthlyUSD)
	if r.Base != nil && r.DeltaUSD != 0 {
		out += fmt.Sprintf(" (%s)", signed(r.DeltaUSD))
	}
	if r.Base == nil && r.Change != "" {
		out += " (new)"
	}
	return out
}

func signed(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", -v)
	}
	return fmt.Sprintf("+$%.2f", v)
}

func ruleList(r analyze.Result) string {
	if len(r.Findings) == 0 {
		return "-"
	}
	rules := make([]string, len(r.Findings))
	for i, f := range r.Findings {
		rules[i] = f.Rule
	}
	return strings.Join(rules, ",")
}

func summaryLine(rep analyze.Report) string {
	t := rep.Totals
	scope := "containers"
	if rep.DiffMode {
		scope = "changed containers"
	}
	s := fmt.Sprintf("%d %s: %d at risk, %d over-provisioned, %d without usage data.", t.Containers, scope, t.Risks, t.Wastes, t.NoUsage)
	if rep.DiffMode {
		s += fmt.Sprintf(" Requested cost $%.2f → $%.2f/month (%s).", t.BaseUSD, t.HeadUSD, signed(t.DeltaUSD))
	} else {
		s += fmt.Sprintf(" Requested cost $%.2f/month.", t.HeadUSD)
	}
	s += fmt.Sprintf(" Trimming requests above the suggestions saves about $%.2f/month.", t.SavingsUSD)
	if t.Unchanged > 0 {
		s += fmt.Sprintf(" %d unchanged containers not shown.", t.Unchanged)
	}
	return s
}
