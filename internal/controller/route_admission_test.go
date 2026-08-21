package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestRouteAdmitted(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		u    *unstructured.Unstructured
		want bool
	}{
		{name: "nil", u: nil, want: false},
		{
			name: "empty status",
			yaml: "status: {}\n",
			want: false,
		},
		{
			name: "empty ingress",
			yaml: "status:\n  ingress: []\n",
			want: false,
		},
		{
			name: "host without conditions stays unready",
			yaml: `status:
  ingress:
  - host: cost.apps.example.com
`,
			want: false,
		},
		{
			name: "admitted true",
			yaml: `status:
  ingress:
  - host: cost.apps.example.com
    conditions:
    - type: Admitted
      status: "True"
`,
			want: true,
		},
		{
			name: "admitted false keeps unready even with host",
			yaml: `status:
  ingress:
  - host: cost.apps.example.com
    conditions:
    - type: Admitted
      status: "False"
`,
			want: false,
		},
		{
			name: "second router admitted",
			yaml: `status:
  ingress:
  - host: a.example.com
    conditions:
    - type: Admitted
      status: "False"
  - host: b.example.com
    conditions:
    - type: Admitted
      status: "True"
`,
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := tt.u
			if tt.yaml != "" {
				obj := map[string]any{}
				if err := yaml.Unmarshal([]byte(tt.yaml), &obj); err != nil {
					t.Fatalf("yaml.Unmarshal: %v", err)
				}
				// Round-trip: NestedSlice must see []any, not a typed slice.
				ingress, found, err := unstructured.NestedSlice(obj, "status", "ingress")
				if err != nil {
					t.Fatalf("NestedSlice: %v", err)
				}
				if found {
					_ = unstructured.SetNestedSlice(obj, ingress, "status", "ingress")
				}
				u = &unstructured.Unstructured{Object: obj}
			}
			if got := routeAdmitted(u); got != tt.want {
				t.Errorf("routeAdmitted = %v, want %v", got, tt.want)
			}
		})
	}
}
