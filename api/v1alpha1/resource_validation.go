package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// validateResourceRequirements ensures each named request does not exceed its limit.
// Keys present only in requests (no limit) are allowed — same as core Kubernetes Pod semantics.
func validateResourceRequirements(path *field.Path, res corev1.ResourceRequirements) field.ErrorList {
	var allErrs field.ErrorList

	for name, reqQty := range res.Requests {
		limitQty, ok := res.Limits[name]
		if !ok {
			continue
		}
		if reqQty.Cmp(limitQty) > 0 {
			allErrs = append(allErrs, field.Invalid(
				path.Child("requests").Key(string(name)),
				reqQty.String(),
				fmt.Sprintf("%s must be less than or equal to %s", reqQty.String(), limitQty.String()),
			))
		}
	}

	return allErrs
}
