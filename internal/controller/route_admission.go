package controller

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	routeKind         = "Route"
	routeAdmittedType = "Admitted"
	routeStatusTrue   = "True"
)

// routeAdmitted reports whether an OpenShift Route has been admitted by a router.
// An ingress entry counts only when it has type=Admitted status=True. A host
// with no conditions is not admission — routers always set the condition.
func routeAdmitted(u *unstructured.Unstructured) bool {
	if u == nil {
		return false
	}
	ingress, found, err := unstructured.NestedSlice(u.Object, "status", "ingress")
	if err != nil || !found || len(ingress) == 0 {
		return false
	}
	for _, item := range ingress {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		conds, condsFound, condErr := unstructured.NestedSlice(m, "conditions")
		if condErr != nil || !condsFound {
			continue
		}
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			typ, _, _ := unstructured.NestedString(cm, "type")
			status, _, _ := unstructured.NestedString(cm, "status")
			if typ == routeAdmittedType && status == routeStatusTrue {
				return true
			}
		}
	}
	return false
}
