package analyze

import "k8s.io/apimachinery/pkg/api/resource"

func q(s string) *resource.Quantity {
	if s == "" {
		return nil
	}
	v := resource.MustParse(s)
	return &v
}
