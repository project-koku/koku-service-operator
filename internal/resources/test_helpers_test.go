package resources

import (
	corev1 "k8s.io/api/core/v1"
)

// envValues returns a map of EnvVar Name→Value for vars with a literal Value set.
// Secret/ConfigMap refs (Value == "") are omitted.
func envValues(c corev1.Container) map[string]string {
	out := make(map[string]string, len(c.Env))
	for _, e := range c.Env {
		if e.Value != "" {
			out[e.Name] = e.Value
		}
	}
	return out
}
