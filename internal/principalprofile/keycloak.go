package principalprofile

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultPageSize    = 100
	defaultMaxPages    = 20 // 2000 users; log when capped
	defaultHTTPTimeout = 15 * time.Second
	tokenRefreshMargin = 0.8
	orgAdminRealmRole  = "org-admin"
	queryFirst         = "first"
	queryMax           = "max"
)

// KeycloakConfig holds Admin API settings (same env contract as the sync CronJob).
type KeycloakConfig struct {
	URL            string
	Realm          string
	ClientID       string
	ClientSecret   string
	TLSVerify      bool
	CAFile         string
	OrgGroupPrefix string
	PageSize       int
	MaxPages       int
}

// ConfigFromEnv loads Keycloak settings from the operator-wired environment.
func ConfigFromEnv() KeycloakConfig {
	verify := true
	switch strings.ToLower(os.Getenv("KEYCLOAK_TLS_VERIFY")) {
	case "false", "0", "no":
		verify = false
	}
	prefix := os.Getenv("SYNC_ORG_GROUP_PREFIX")
	if prefix == "" {
		prefix = "org-"
	}
	return KeycloakConfig{
		URL:            strings.TrimRight(os.Getenv("KEYCLOAK_URL"), "/"),
		Realm:          envOr("KEYCLOAK_REALM", "kubernetes"),
		ClientID:       os.Getenv("KEYCLOAK_CLIENT_ID"),
		ClientSecret:   os.Getenv("KEYCLOAK_CLIENT_SECRET"),
		TLSVerify:      verify,
		CAFile:         os.Getenv("KEYCLOAK_CA_FILE"),
		OrgGroupPrefix: prefix,
		PageSize:       defaultPageSize,
		MaxPages:       defaultMaxPages,
	}
}

// Configured reports whether Keycloak Admin credentials are present.
func (c KeycloakConfig) Configured() bool {
	return c.URL != "" && c.ClientID != "" && c.ClientSecret != ""
}

// Client talks to the Keycloak Admin REST API.
type Client struct {
	cfg        KeycloakConfig
	httpClient *http.Client

	mu            sync.Mutex
	accessToken   string
	tokenAcquired time.Time
	tokenExpires  time.Duration
}

// NewClient builds a Keycloak Admin client.
func NewClient(cfg KeycloakConfig) (*Client, error) {
	if !cfg.Configured() {
		return nil, fmt.Errorf("keycloak not configured")
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = defaultPageSize
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = defaultMaxPages
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !cfg.TLSVerify {
		//nolint:gosec // CR auth.keycloak.tls.insecureSkipVerify
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	// CA file is mounted by the operator; ssl uses system/SSL_CERT_FILE trust.
	_ = cfg.CAFile
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout:   defaultHTTPTimeout,
			Transport: transport,
		},
	}, nil
}

// ProfilesForOrg returns username(casefold) -> Profile for members of org-{orgId}.
func (c *Client) ProfilesForOrg(ctx context.Context, orgID string) (map[string]Profile, error) {
	groupName := c.cfg.OrgGroupPrefix + orgID
	groupID, err := c.findGroupID(ctx, groupName)
	if err != nil {
		return nil, err
	}
	if groupID == "" {
		return map[string]Profile{}, nil
	}
	members, err := c.listGroupMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	admins, err := c.listOrgAdminUsernames(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Profile, len(members))
	for _, u := range members {
		username, _ := u["username"].(string)
		if username == "" || strings.HasPrefix(username, "service-account-") {
			continue
		}
		key := strings.ToLower(username)
		email, _ := u["email"].(string)
		first, _ := u["firstName"].(string)
		last, _ := u["lastName"].(string)
		enabled := true
		if v, ok := u["enabled"].(bool); ok {
			enabled = v
		}
		out[key] = Profile{
			Email:      email,
			FirstName:  first,
			LastName:   last,
			IsOrgAdmin: admins[key],
			Enabled:    enabled,
		}
	}
	return out, nil
}

func (c *Client) findGroupID(ctx context.Context, name string) (string, error) {
	offset := 0
	for page := 0; page < c.cfg.MaxPages; page++ {
		raw, err := c.get(ctx, "/groups", url.Values{
			queryFirst:            {fmt.Sprintf("%d", offset)},
			queryMax:              {fmt.Sprintf("%d", c.cfg.PageSize)},
			"search":              {name},
			"briefRepresentation": {"false"},
		})
		if err != nil {
			return "", err
		}
		var groups []map[string]any
		if err := json.Unmarshal(raw, &groups); err != nil {
			return "", err
		}
		for _, g := range groups {
			if gname, _ := g["name"].(string); gname == name {
				id, _ := g["id"].(string)
				return id, nil
			}
		}
		if len(groups) < c.cfg.PageSize {
			return "", nil
		}
		offset += c.cfg.PageSize
	}
	return "", fmt.Errorf("group search capped at %d pages looking for %q", c.cfg.MaxPages, name)
}

func (c *Client) listGroupMembers(ctx context.Context, groupID string) ([]map[string]any, error) {
	var all []map[string]any
	offset := 0
	for page := 0; page < c.cfg.MaxPages; page++ {
		raw, err := c.get(ctx, "/groups/"+url.PathEscape(groupID)+"/members", url.Values{
			queryFirst: {fmt.Sprintf("%d", offset)},
			queryMax:   {fmt.Sprintf("%d", c.cfg.PageSize)},
		})
		if err != nil {
			return nil, err
		}
		var members []map[string]any
		if err := json.Unmarshal(raw, &members); err != nil {
			return nil, err
		}
		all = append(all, members...)
		if len(members) < c.cfg.PageSize {
			return all, nil
		}
		offset += c.cfg.PageSize
	}
	return all, fmt.Errorf("group members capped at %d pages", c.cfg.MaxPages)
}

func (c *Client) listOrgAdminUsernames(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	offset := 0
	for page := 0; page < c.cfg.MaxPages; page++ {
		raw, status, err := c.getWithStatus(ctx, "/roles/"+url.PathEscape(orgAdminRealmRole)+"/users", url.Values{
			queryFirst: {fmt.Sprintf("%d", offset)},
			queryMax:   {fmt.Sprintf("%d", c.cfg.PageSize)},
		})
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound {
			return out, nil
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("list org-admin users: status %d", status)
		}
		var users []map[string]any
		if err := json.Unmarshal(raw, &users); err != nil {
			return nil, err
		}
		for _, u := range users {
			if username, _ := u["username"].(string); username != "" {
				out[strings.ToLower(username)] = true
			}
		}
		if len(users) < c.cfg.PageSize {
			return out, nil
		}
		offset += c.cfg.PageSize
	}
	return out, fmt.Errorf("org-admin users capped at %d pages", c.cfg.MaxPages)
}

func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	body, status, err := c.getWithStatus(ctx, path, q)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("keycloak GET %s: status %d", path, status)
	}
	return body, nil
}

func (c *Client) getWithStatus(ctx context.Context, path string, q url.Values) ([]byte, int, error) {
	if err := c.ensureToken(ctx); err != nil {
		return nil, 0, err
	}
	u := fmt.Sprintf("%s/admin/realms/%s%s", c.cfg.URL, url.PathEscape(c.cfg.Realm), path)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	c.mu.Lock()
	token := c.accessToken
	c.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func (c *Client) ensureToken(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Since(c.tokenAcquired) < time.Duration(float64(c.tokenExpires)*tokenRefreshMargin) {
		return nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
	}
	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", c.cfg.URL, url.PathEscape(c.cfg.Realm))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("keycloak token: status %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return err
	}
	c.accessToken = tok.AccessToken
	c.tokenAcquired = time.Now()
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 300
	}
	c.tokenExpires = time.Duration(tok.ExpiresIn) * time.Second
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
