// Package usage loads observed container usage, either from a snapshot
// file (JSON or CSV, for offline runs and tests) or from Prometheus.
package usage

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gratycodes/kube-rightsize-diff/internal/manifest"
)

// Sample is the observed usage of one container at the configured
// percentiles, taken as the maximum across the workload's pods.
type Sample struct {
	CPUCores    float64 `json:"cpu_cores"`
	MemoryBytes float64 `json:"memory_bytes"`
	// Pods is the number of pod series that contributed (0 for snapshots).
	Pods int `json:"pods,omitempty"`
}

// Data is a set of usage samples plus a description of how they were taken.
type Data struct {
	Source        string  `json:"source"`
	CPUPercentile float64 `json:"cpu_percentile"`
	MemPercentile float64 `json:"memory_percentile"`
	Window        string  `json:"window"`
	samples       map[manifest.Key]Sample
}

// NewData returns an empty data set.
func NewData(source string, cpuP, memP float64, window string) *Data {
	return &Data{Source: source, CPUPercentile: cpuP, MemPercentile: memP, Window: window, samples: map[manifest.Key]Sample{}}
}

// Set stores a sample. An empty Kind matches any kind on lookup.
func (d *Data) Set(k manifest.Key, s Sample) { d.samples[k] = s }

// Len reports the number of samples.
func (d *Data) Len() int { return len(d.samples) }

// Lookup finds usage for a container, falling back to a kind-less entry.
func (d *Data) Lookup(k manifest.Key) (Sample, bool) {
	if d == nil {
		return Sample{}, false
	}
	if s, ok := d.samples[k]; ok {
		return s, true
	}
	k.Kind = ""
	s, ok := d.samples[k]
	return s, ok
}

type snapshotFile struct {
	Source        string          `json:"source"`
	CPUPercentile float64         `json:"cpu_percentile"`
	MemPercentile float64         `json:"memory_percentile"`
	Window        string          `json:"window"`
	Containers    []snapshotEntry `json:"containers"`
}

type snapshotEntry struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Workload  string `json:"workload"`
	Container string `json:"container"`
	CPU       string `json:"cpu"`
	Memory    string `json:"memory"`
}

// LoadFile reads a JSON or CSV snapshot, chosen by file extension.
func LoadFile(path string) (*Data, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if strings.EqualFold(filepath.Ext(path), ".csv") {
		return ParseCSV(f, path)
	}
	return ParseJSON(f, path)
}

// ParseJSON reads a JSON snapshot.
func ParseJSON(r io.Reader, name string) (*Data, error) {
	var sf snapshotFile
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sf); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if sf.Source == "" {
		sf.Source = name
	}
	d := NewData(sf.Source, sf.CPUPercentile, sf.MemPercentile, sf.Window)
	for i, e := range sf.Containers {
		if err := d.add(e); err != nil {
			return nil, fmt.Errorf("%s: container %d: %w", name, i, err)
		}
	}
	return d, nil
}

// ParseCSV reads a CSV snapshot with the header
// namespace,kind,workload,container,cpu,memory. Lines starting with # are
// comments; kind may be empty.
func ParseCSV(r io.Reader, name string) (*Data, error) {
	cr := csv.NewReader(r)
	cr.Comment = '#'
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: reading header: %w", name, err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, need := range []string{"namespace", "workload", "container", "cpu", "memory"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("%s: missing column %q", name, need)
		}
	}
	get := func(rec []string, c string) string {
		if i, ok := col[c]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	d := NewData(name, 0, 0, "")
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		e := snapshotEntry{
			Namespace: get(rec, "namespace"), Kind: get(rec, "kind"), Workload: get(rec, "workload"),
			Container: get(rec, "container"), CPU: get(rec, "cpu"), Memory: get(rec, "memory"),
		}
		if err := d.add(e); err != nil {
			return nil, fmt.Errorf("%s: record %d: %w", name, line, err)
		}
	}
	return d, nil
}

func (d *Data) add(e snapshotEntry) error {
	if e.Namespace == "" || e.Workload == "" || e.Container == "" {
		return errors.New("namespace, workload and container are required")
	}
	var s Sample
	if e.CPU != "" {
		q, err := resource.ParseQuantity(e.CPU)
		if err != nil {
			return fmt.Errorf("cpu %q: %w", e.CPU, err)
		}
		s.CPUCores = float64(q.MilliValue()) / 1000
	}
	if e.Memory != "" {
		q, err := resource.ParseQuantity(e.Memory)
		if err != nil {
			return fmt.Errorf("memory %q: %w", e.Memory, err)
		}
		s.MemoryBytes = q.AsApproximateFloat64()
	}
	k := manifest.Key{Namespace: e.Namespace, Kind: e.Kind, Workload: e.Workload, Container: e.Container}
	if _, dup := d.samples[k]; dup {
		return fmt.Errorf("duplicate entry for %s", k)
	}
	d.samples[k] = s
	return nil
}
