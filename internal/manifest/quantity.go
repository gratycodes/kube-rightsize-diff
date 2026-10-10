package manifest

import "k8s.io/apimachinery/pkg/api/resource"

func eqQ(a, b *resource.Quantity) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Cmp(*b) == 0
}

// Cores returns a CPU quantity in cores, or 0 for nil.
func Cores(q *resource.Quantity) float64 {
	if q == nil {
		return 0
	}
	return float64(q.MilliValue()) / 1000
}

// Bytes returns a memory quantity in bytes, or 0 for nil.
func Bytes(q *resource.Quantity) float64 {
	if q == nil {
		return 0
	}
	return q.AsApproximateFloat64()
}
