package principalprofile

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

// NewMux returns the HTTP handlers for the principal-profile service.
func NewMux(provider *Provider) http.Handler {
	token := os.Getenv("PRINCIPAL_PROFILE_TOKEN")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /v1/orgs/{org_id}/profiles", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		orgID := sanitizeLog(r.PathValue("org_id"))
		profiles, err := provider.ProfilesForOrg(r.Context(), orgID)
		if err != nil {
			log.Printf("profiles org_id=%s error=%v", orgID, err)
			http.Error(w, "profile backend error", http.StatusBadGateway)
			return
		}
		log.Printf("profiles org_id=%s count=%d", orgID, len(profiles))
		writeJSON(w, Response{Profiles: profiles})
	})
	return mux
}

func sanitizeLog(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\r' || r == '\n' {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
