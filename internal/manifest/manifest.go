// Package manifest extracts container resource settings from rendered
// Kubernetes manifests (plain YAML, `kustomize build` or `helm template`
// output). It never talks to a cluster.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Resources holds the parsed requests and limits of one container. A nil
// pointer means the field is not set in the manifest.
type Resources struct {
	CPURequest *resource.Quantity
	CPULimit   *resource.Quantity
	MemRequest *resource.Quantity
	MemLimit   *resource.Quantity
}

// Container is one container of one workload.
type Container struct {
	Namespace string
	Kind      string
	Workload  string
	Name      string
	Replicas  int32
	Resources Resources
	// Source is the file (or "-" for stdin) the container was read from.
	Source string
}

// Key identifies a container across base and head manifests.
type Key struct {
	Namespace string
	Kind      string
	Workload  string
	Container string
}

func (k Key) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", k.Namespace, k.Kind, k.Workload, k.Container)
}

// Key returns the identity of the container.
func (c Container) Key() Key {
	return Key{Namespace: c.Namespace, Kind: c.Kind, Workload: c.Workload, Container: c.Name}
}

// Options controls parsing.
type Options struct {
	// DefaultNamespace is used for objects without metadata.namespace,
	// matching what kubectl apply would do. Defaults to "default".
	DefaultNamespace string
	// IncludeInit also reports initContainers. Off by default because
	// init containers rarely run long enough to matter for cost.
	IncludeInit bool
}

// podSpecPaths maps a workload kind to the path of its PodSpec.
var podSpecPaths = map[string][]string{
	"Pod":         {"spec"},
	"Deployment":  {"spec", "template", "spec"},
	"StatefulSet": {"spec", "template", "spec"},
	"DaemonSet":   {"spec", "template", "spec"},
	"ReplicaSet":  {"spec", "template", "spec"},
	"Job":         {"spec", "template", "spec"},
	"CronJob":     {"spec", "jobTemplate", "spec", "template", "spec"},
}

// LoadPaths reads every path. A directory is walked for *.yaml / *.yml /
// *.json files; "-" reads stdin.
func LoadPaths(paths []string, stdin io.Reader, opts Options) ([]Container, error) {
	var out []Container
	for _, p := range paths {
		if p == "-" {
			cs, err := Parse(stdin, "-", opts)
			if err != nil {
				return nil, err
			}
			out = append(out, cs...)
			continue
		}
		files, err := expand(p)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			cs, err := parseFile(f, opts)
			if err != nil {
				return nil, err
			}
			out = append(out, cs...)
		}
	}
	return dedupe(out)
}

func expand(p string) ([]string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{p}, nil
	}
	var files []string
	err = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func parseFile(path string, opts Options) ([]Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f, path, opts)
}

// Parse reads a multi-document YAML (or JSON) stream. Unknown kinds are
// skipped; v1 List objects (kubectl get -o yaml) are flattened.
func Parse(r io.Reader, source string, opts Options) ([]Container, error) {
	if opts.DefaultNamespace == "" {
		opts.DefaultNamespace = "default"
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []Container
	for doc := 1; ; doc++ {
		var obj map[string]any
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, doc, err)
		}
		cs, err := fromObject(obj, source, opts)
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, doc, err)
		}
		out = append(out, cs...)
	}
	return out, nil
}

func fromObject(obj map[string]any, source string, opts Options) ([]Container, error) {
	if obj == nil {
		return nil, nil
	}
	kind, _ := obj["kind"].(string)
	if strings.HasSuffix(kind, "List") {
		items, _ := obj["items"].([]any)
		var out []Container
		for _, it := range items {
			m, _ := it.(map[string]any)
			cs, err := fromObject(m, source, opts)
			if err != nil {
				return nil, err
			}
			out = append(out, cs...)
		}
		return out, nil
	}
	path, ok := podSpecPaths[kind]
	if !ok {
		return nil, nil
	}
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("%s without metadata.name", kind)
	}
	ns, _ := meta["namespace"].(string)
	if ns == "" {
		ns = opts.DefaultNamespace
	}
	replicas := replicaCount(obj, kind)
	podSpec := dig(obj, path...)
	if podSpec == nil {
		return nil, nil
	}
	fields := []string{"containers"}
	if opts.IncludeInit {
		fields = append(fields, "initContainers")
	}
	var out []Container
	for _, field := range fields {
		list, _ := podSpec[field].([]any)
		for _, raw := range list {
			c, _ := raw.(map[string]any)
			cname, _ := c["name"].(string)
			res, err := parseResources(c)
			if err != nil {
				return nil, fmt.Errorf("%s %s/%s container %q: %w", kind, ns, name, cname, err)
			}
			out = append(out, Container{
				Namespace: ns, Kind: kind, Workload: name, Name: cname,
				Replicas: replicas, Resources: res, Source: source,
			})
		}
	}
	return out, nil
}

// replicaCount returns the number of pods the workload runs. DaemonSets
// run one pod per node, which is unknown offline, so they count as 1 and
// costs are reported per node.
func replicaCount(obj map[string]any, kind string) int32 {
	switch kind {
	case "Deployment", "StatefulSet", "ReplicaSet":
		spec, _ := obj["spec"].(map[string]any)
		switch v := spec["replicas"].(type) {
		case int:
			return int32(v)
		case float64:
			return int32(v)
		case nil:
			return 1 // API server default
		}
		return 1
	case "Job":
		spec, _ := obj["spec"].(map[string]any)
		if v, ok := spec["parallelism"].(int); ok {
			return int32(v)
		}
	}
	return 1
}

func dig(obj map[string]any, path ...string) map[string]any {
	cur := obj
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

func parseResources(c map[string]any) (Resources, error) {
	var r Resources
	res, _ := c["resources"].(map[string]any)
	req, _ := res["requests"].(map[string]any)
	lim, _ := res["limits"].(map[string]any)
	var err error
	if r.CPURequest, err = quantity(req, "cpu"); err != nil {
		return r, err
	}
	if r.MemRequest, err = quantity(req, "memory"); err != nil {
		return r, err
	}
	if r.CPULimit, err = quantity(lim, "cpu"); err != nil {
		return r, err
	}
	if r.MemLimit, err = quantity(lim, "memory"); err != nil {
		return r, err
	}
	// Kubernetes defaults a missing request to the limit.
	if r.CPURequest == nil && r.CPULimit != nil {
		q := r.CPULimit.DeepCopy()
		r.CPURequest = &q
	}
	if r.MemRequest == nil && r.MemLimit != nil {
		q := r.MemLimit.DeepCopy()
		r.MemRequest = &q
	}
	return r, nil
}

func quantity(m map[string]any, key string) (*resource.Quantity, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	s := fmt.Sprint(v)
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return nil, fmt.Errorf("invalid %s quantity %q: %w", key, s, err)
	}
	return &q, nil
}

func dedupe(cs []Container) ([]Container, error) {
	seen := make(map[Key]string, len(cs))
	for _, c := range cs {
		if prev, ok := seen[c.Key()]; ok {
			return nil, fmt.Errorf("%s defined twice (%s and %s)", c.Key(), prev, c.Source)
		}
		seen[c.Key()] = c.Source
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Key().String() < cs[j].Key().String() })
	return cs, nil
}
