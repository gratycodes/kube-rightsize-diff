package manifest

import "sort"

// ChangeType describes how a container differs between base and head.
type ChangeType string

const (
	Added     ChangeType = "added"
	Modified  ChangeType = "modified"
	Removed   ChangeType = "removed"
	Unchanged ChangeType = "unchanged"
)

// Change pairs the base and head view of one container.
type Change struct {
	Key  Key
	Type ChangeType
	Base *Container
	Head *Container
}

// Diff compares base and head containers. Only resources and replica
// counts are considered: an image bump that leaves resources alone is
// Unchanged for the purpose of this tool.
func Diff(base, head []Container) []Change {
	b := index(base)
	h := index(head)
	keys := make(map[Key]struct{}, len(b)+len(h))
	for k := range b {
		keys[k] = struct{}{}
	}
	for k := range h {
		keys[k] = struct{}{}
	}
	out := make([]Change, 0, len(keys))
	for k := range keys {
		bc, inBase := b[k]
		hc, inHead := h[k]
		ch := Change{Key: k}
		switch {
		case inBase && inHead:
			ch.Base, ch.Head = &bc, &hc
			ch.Type = Unchanged
			if !sameResources(bc, hc) {
				ch.Type = Modified
			}
		case inHead:
			ch.Head, ch.Type = &hc, Added
		default:
			ch.Base, ch.Type = &bc, Removed
		}
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.String() < out[j].Key.String() })
	return out
}

func index(cs []Container) map[Key]Container {
	m := make(map[Key]Container, len(cs))
	for _, c := range cs {
		m[c.Key()] = c
	}
	return m
}

func sameResources(a, b Container) bool {
	if a.Replicas != b.Replicas {
		return false
	}
	ra, rb := a.Resources, b.Resources
	return eqQ(ra.CPURequest, rb.CPURequest) && eqQ(ra.CPULimit, rb.CPULimit) &&
		eqQ(ra.MemRequest, rb.MemRequest) && eqQ(ra.MemLimit, rb.MemLimit)
}
