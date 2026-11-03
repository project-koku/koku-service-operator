// principal-profile is the Cost-owned principal profile service for COST-8167.
//
// insights-rbac BYPASS principals call:
//
//	GET /v1/orgs/{org_id}/profiles
//
// When KEYCLOAK_* is set (same contract as the Keycloak sync CronJob), profiles
// are loaded from Keycloak Admin API (org group members + org-admin realm role).
// Otherwise a static fixture is served for local spikes.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/project-koku/koku-service-operator/internal/principalprofile"
)

func main() {
	addr := os.Getenv("PRINCIPAL_PROFILE_ADDR")
	if addr == "" {
		addr = ":8099"
	}
	provider, err := principalprofile.NewProviderFromEnv()
	if err != nil {
		log.Fatalf("provider: %v", err)
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           principalprofile.NewMux(provider),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("principal-profile listening on %s", sanitizeAddr(addr))
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func sanitizeAddr(addr string) string {
	// Addr is operator-controlled env; strip CR/LF so logs stay single-line.
	out := make([]rune, 0, len(addr))
	for _, r := range addr {
		if r == '\r' || r == '\n' {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
