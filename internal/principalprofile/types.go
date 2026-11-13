// Package principalprofile implements the Cost-owned principal profile HTTP
// service used by insights-rbac BYPASS enrichment (COST-8167).
package principalprofile

// Profile is the SaaS-parity principal profile fields the RBAC UI expects.
type Profile struct {
	Email      string `json:"email"`
	FirstName  string `json:"first_name"`
	LastName   string `json:"last_name"`
	IsOrgAdmin bool   `json:"is_org_admin"`
	Enabled    bool   `json:"enabled"`
}

// Response is the spike/production HTTP contract body.
type Response struct {
	Profiles map[string]Profile `json:"profiles"`
}
