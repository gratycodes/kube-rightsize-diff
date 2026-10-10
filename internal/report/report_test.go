package report

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratycodes/kube-rightsize-diff/internal/analyze"
	"github.com/gratycodes/kube-rightsize-diff/internal/manifest"
	"github.com/gratycodes/kube-rightsize-diff/internal/usage"
)

var update = flag.Bool("update", false, "rewrite golden files")

func shop(t *testing.T, diff bool) analyze.Report {
	t.Helper()
	load := func(p string) []manifest.Container {
		cs, err := manifest.LoadPaths([]string{p}, nil, manifest.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}
	u, err := usage.LoadFile("../../testdata/shop/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	var base []manifest.Container
	if diff {
		base = load("../../testdata/shop/base")
	}
	rep, err := analyze.Analyze(base, load("../../testdata/shop/head"), u, analyze.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/report -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s mismatch; run go test ./internal/report -update and review the diff.\n--- got ---\n%s", name, got)
	}
}

func TestGolden(t *testing.T) {
	render := map[string]func(*bytes.Buffer, analyze.Report) error{
		"md":   func(b *bytes.Buffer, r analyze.Report) error { return Markdown(b, r) },
		"txt":  func(b *bytes.Buffer, r analyze.Report) error { return Table(b, r) },
		"json": func(b *bytes.Buffer, r analyze.Report) error { return JSON(b, r) },
	}
	for _, mode := range []string{"diff", "full"} {
		rep := shop(t, mode == "diff")
		for ext, fn := range render {
			t.Run(mode+"."+ext, func(t *testing.T) {
				var b bytes.Buffer
				if err := fn(&b, rep); err != nil {
					t.Fatal(err)
				}
				golden(t, "shop-"+mode+"."+ext, b.Bytes())
			})
		}
	}
}

func TestMarkdownHasMarkerAndOnlyChanged(t *testing.T) {
	var b bytes.Buffer
	if err := Markdown(&b, shop(t, true)); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.HasPrefix(out, Marker+"\n") {
		t.Error("comment must start with the marker so the Action can find it")
	}
	if strings.Contains(out, "catalog") || strings.Contains(out, "node-agent") {
		t.Error("unchanged workloads leaked into the PR comment")
	}
	if !strings.Contains(out, "2 unchanged containers not shown") {
		t.Error("missing unchanged count")
	}
}

func TestMarkdownNoChanges(t *testing.T) {
	cs, err := manifest.LoadPaths([]string{"../../testdata/shop/base"}, nil, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := analyze.Analyze(cs, cs, nil, analyze.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := Markdown(&b, rep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "No container requests, limits or replica counts changed") {
		t.Errorf("got %s", b.String())
	}
}

func TestJSONRoundTrips(t *testing.T) {
	var b bytes.Buffer
	if err := JSON(&b, shop(t, true)); err != nil {
		t.Fatal(err)
	}
	var back analyze.Report
	if err := json.Unmarshal(b.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back.Totals.Risks != 3 || len(back.Results) != 5 {
		t.Errorf("round trip lost data: %+v", back.Totals)
	}
}
