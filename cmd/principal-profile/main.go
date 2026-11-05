// principal-profile is the Cost-owned principal profile service for COST-8167.
//
// insights-rbac BYPASS principals call:
//
//	GET /v1/orgs/{org_id}/profiles
//
// When KEYCLOAK_* is set (same contract as the Keycloak sync CronJob), profiles
// are loaded from Keycloak Admin API (org group members + org-admin realm role).
// Otherwise a static fixture is served for local development.
//
// The process listens on loopback by default so only co-located containers can
// reach it. Use --healthcheck for kubelet probes (HTTP probes use the pod IP).
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/project-koku/koku-service-operator/internal/principalprofile"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "GET /healthz on PRINCIPAL_PROFILE_ADDR and exit")
	flag.Parse()

	addr := os.Getenv("PRINCIPAL_PROFILE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8099"
	}

	if *healthcheck {
		if err := runHealthcheck(addr); err != nil {
			log.Fatal(err)
		}
		return
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

func runHealthcheck(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse PRINCIPAL_PROFILE_ADDR: %w", err)
	}
	switch host {
	case "127.0.0.1", "localhost", "::1":
	default:
		return fmt.Errorf("healthcheck only allowed on loopback, got %q", host)
	}
	// Literal loopback URL keeps the probe off the pod IP and avoids SSRF
	// findings on operator-controlled env values.
	url := "http://127.0.0.1:" + port + "/healthz"
	if host == "::1" {
		url = "http://[::1]:" + port + "/healthz"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url) //nolint:gosec // G704: URL host is restricted to loopback above
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz status %d", resp.StatusCode)
	}
	return nil
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
