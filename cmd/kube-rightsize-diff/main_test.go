package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const shop = "../../testdata/shop/"

func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errb, http.DefaultClient)
	return code, out.String(), errb.String()
}

func TestDiffMarkdownMatchesGolden(t *testing.T) {
	code, out, errOut := runCLI(t, "", "--head", shop+"head", "--base", shop+"base", "--usage", shop+"usage.json", "--format", "markdown")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want, err := os.ReadFile("../../internal/report/testdata/shop-diff.md")
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("CLI markdown differs from report golden:\n%s", out)
	}
}

func TestFailThresholds(t *testing.T) {
	cases := []struct {
		args []string
		code int
	}{
		{[]string{"--fail-on", "none"}, exitOK},
		{[]string{"--fail-on", "risk"}, exitThreshold},
		{[]string{"--fail-on", "waste"}, exitThreshold},
		{[]string{"--fail-on", "any"}, exitThreshold},
		{[]string{"--max-cost-increase", "200"}, exitOK},
		{[]string{"--max-cost-increase", "100"}, exitThreshold},
	}
	for _, c := range cases {
		args := append([]string{"--head", shop + "head", "--base", shop + "base", "--usage", shop + "usage.csv", "--format", "json"}, c.args...)
		code, _, errOut := runCLI(t, "", args...)
		if code != c.code {
			t.Errorf("%v: exit %d, want %d (%s)", c.args, code, c.code, errOut)
		}
	}
}

func TestStdinAndOutputFile(t *testing.T) {
	data, err := os.ReadFile(shop + "head/app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(t.TempDir(), "r.json")
	code, stdout, errOut := runCLI(t, string(data), "--head", "-", "--format", "json", "--output", outFile)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if stdout != "" {
		t.Error("report should go to --output, not stdout")
	}
	if !strings.Contains(errOut, "static checks only") {
		t.Error("expected a note about missing usage")
	}
	var rep struct {
		DiffMode bool `json:"diff_mode"`
		Results  []any
	}
	b, _ := os.ReadFile(outFile)
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.DiffMode || len(rep.Results) != 4 {
		t.Errorf("got diff=%v results=%d", rep.DiffMode, len(rep.Results))
	}
}

func TestPrometheusEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth = %q", got)
		}
		_ = r.ParseForm()
		v := "0.35"
		if strings.Contains(r.PostForm.Get("query"), "memory") {
			v = "419430400"
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"shop","pod":"search-5f6d7c8b9-abcde","container":"app"},"value":[1,%q]}]}}`, v)
	}))
	defer srv.Close()
	t.Setenv("PROMETHEUS_TOKEN", "tok")
	code, out, errOut := runCLI(t, "", "--head", shop+"head/app.yaml", "--base", shop+"base/app.yaml",
		"--prometheus-url", srv.URL, "--window", "1d", "--format", "table")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "cpu-request-below-usage") || !strings.Contains(out, "350m") {
		t.Errorf("search should be flagged from Prometheus data:\n%s", out)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"--head", shop + "head", "--format", "xml"},
		{"--head", shop + "head", "--fail-on", "sometimes"},
		{"--head", shop + "head", "--usage", "a.json", "--prometheus-url", "http://x"},
		{"--head", "-", "--base", "-"},
		{"--head", shop + "head", "extra"},
		{"--head", "missing-dir"},
		{"--head", shop + "head", "--base", "missing-dir"},
		{"--head", shop + "head", "--usage", "missing.json"},
		{"--head", shop + "head", "--prometheus-url", "http://x", "--window", "a week"},
		{"--head", shop + "head", "--over-factor", "0.5"},
		{"--no-such-flag"},
	}
	for _, args := range cases {
		if code, _, _ := runCLI(t, "", args...); code != exitError {
			t.Errorf("%v: exit %d, want %d", args, code, exitError)
		}
	}
}

func TestVersionAndHelp(t *testing.T) {
	code, out, _ := runCLI(t, "", "--version")
	if code != exitOK || !strings.HasPrefix(out, "kube-rightsize-diff ") {
		t.Errorf("version: %d %q", code, out)
	}
	if code, _, _ := runCLI(t, "", "-h"); code != exitOK {
		t.Errorf("help exit %d", code)
	}
}
