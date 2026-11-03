package principalprofile

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const defaultCacheTTL = 5 * time.Minute

// Provider serves org-scoped profiles from Keycloak or a static fixture.
type Provider struct {
	kc      *Client
	ttl     time.Duration
	fixture map[string]Profile

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	profiles  map[string]Profile
	expiresAt time.Time
}

// NewProviderFromEnv builds a provider. Fixture mode is used when Keycloak is unset.
func NewProviderFromEnv() (*Provider, error) {
	ttl := defaultCacheTTL
	if v := os.Getenv("PRINCIPAL_PROFILE_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			ttl = d
		}
	}
	p := &Provider{
		ttl:   ttl,
		cache: map[string]cacheEntry{},
	}
	cfg := ConfigFromEnv()
	if cfg.Configured() {
		kc, err := NewClient(cfg)
		if err != nil {
			return nil, err
		}
		p.kc = kc
		log.Printf("principal-profile: Keycloak mode url=%s realm=%s", cfg.URL, cfg.Realm)
		return p, nil
	}
	profiles, err := loadFixtureProfiles()
	if err != nil {
		return nil, err
	}
	p.fixture = profiles
	log.Printf("principal-profile: fixture mode (%d users)", len(profiles))
	return p, nil
}

// ProfilesForOrg returns cached profiles for the org.
func (p *Provider) ProfilesForOrg(ctx context.Context, orgID string) (map[string]Profile, error) {
	if orgID == "" {
		return nil, fmt.Errorf("org_id required")
	}
	now := time.Now()
	p.mu.Lock()
	if e, ok := p.cache[orgID]; ok && now.Before(e.expiresAt) {
		out := e.profiles
		p.mu.Unlock()
		return out, nil
	}
	p.mu.Unlock()

	var (
		profiles map[string]Profile
		err      error
	)
	if p.kc != nil {
		profiles, err = p.kc.ProfilesForOrg(ctx, orgID)
	} else {
		profiles = p.fixture
	}
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.cache[orgID] = cacheEntry{profiles: profiles, expiresAt: now.Add(p.ttl)}
	p.mu.Unlock()
	return profiles, nil
}

func loadFixtureProfiles() (map[string]Profile, error) {
	if path := os.Getenv("PRINCIPAL_PROFILE_FIXTURE"); path != "" {
		// Dev/spike only: path comes from operator-controlled env, not end-user input.
		raw, err := os.ReadFile(path) //nolint:gosec // PRINCIPAL_PROFILE_FIXTURE is operator-set
		if err != nil {
			return nil, err
		}
		var body Response
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		if body.Profiles == nil {
			return nil, fmt.Errorf("fixture missing profiles")
		}
		return normalizeKeys(body.Profiles), nil
	}
	return normalizeKeys(map[string]Profile{
		"admin": {
			Email: "admin@example.com", FirstName: "Org", LastName: "Admin",
			IsOrgAdmin: true, Enabled: true,
		},
		"alice": {
			Email: "alice@example.com", FirstName: "Alice", LastName: "User",
			IsOrgAdmin: false, Enabled: true,
		},
		"bob": {
			Email: "bob@example.com", FirstName: "Bob", LastName: "Disabled",
			IsOrgAdmin: false, Enabled: false,
		},
	}), nil
}

func normalizeKeys(in map[string]Profile) map[string]Profile {
	out := make(map[string]Profile, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}
