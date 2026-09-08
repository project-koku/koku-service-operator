package rbac_seed

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

//go:embed data/*.json rbac_config_ref
var embeddedFS embed.FS

const (
	rbacConfigRepo       = "project-kessel/rbac-config"
	rbacConfigConfigPath = "configs/prod"
	upstreamFetchTimeout = 30 * time.Second
)

// ConfigRef returns the pinned project-kessel/rbac-config commit SHA.
func ConfigRef() (string, error) {
	b, err := embeddedFS.ReadFile("rbac_config_ref")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// PermissionsConfigMapData returns rbac-config permission JSON keyed for
// insights-rbac management/role/permissions/*.json mounts.
func PermissionsConfigMapData() (map[string]string, error) {
	cm, err := readEmbeddedDataFile("cost-management.permissions.json")
	if err != nil {
		return nil, err
	}
	src, err := readEmbeddedDataFile("sources.permissions.json")
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"cost-management.json": cm,
		"sources.json":         src,
	}, nil
}

// DefinitionsConfigMapData returns rbac-config role JSON keyed for
// insights-rbac management/role/definitions/*.json mounts.
func DefinitionsConfigMapData() (map[string]string, error) {
	cm, err := readEmbeddedDataFile("cost-management.roles.json")
	if err != nil {
		return nil, err
	}
	src, err := readEmbeddedDataFile("sources.roles.json")
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"cost-management.json": cm,
		"sources.json":         src,
	}, nil
}

func readEmbeddedDataFile(name string) (string, error) {
	raw, err := embeddedFS.ReadFile("data/" + name)
	if err != nil {
		return "", err
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("embedded %s is not valid JSON", name)
	}
	return string(raw), nil
}

func upstreamHTTPClient() *http.Client {
	client := *http.DefaultClient
	client.Timeout = upstreamFetchTimeout
	return &client
}

// EmbeddedFile returns the bytes of an embedded rbac-config snapshot file.
func EmbeddedFile(name string) ([]byte, error) {
	return embeddedFS.ReadFile("data/" + name)
}

// FetchUpstreamFile downloads a rbac-config file at the pinned ConfigRef commit.
// name is an embedded snapshot basename (e.g. cost-management.permissions.json).
func FetchUpstreamFile(client *http.Client, name string) ([]byte, error) {
	ref, err := ConfigRef()
	if err != nil {
		return nil, err
	}
	upstreamName, subdir, err := upstreamLocation(name)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = upstreamHTTPClient()
	}
	url := fmt.Sprintf(
		"https://raw.githubusercontent.com/%s/%s/%s/%s/%s",
		rbacConfigRepo, ref, rbacConfigConfigPath, subdir, upstreamName,
	)
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func upstreamLocation(embeddedName string) (fileName, subdir string, err error) {
	switch embeddedName {
	case "cost-management.permissions.json":
		return "cost-management.json", "permissions", nil
	case "cost-management.roles.json":
		return "cost-management.json", "roles", nil
	case "sources.permissions.json":
		return "sources.json", "permissions", nil
	case "sources.roles.json":
		return "sources.json", "roles", nil
	default:
		return "", "", fmt.Errorf("unknown embedded rbac-config snapshot %q", embeddedName)
	}
}

// NormalizeJSON returns canonical JSON bytes for drift comparison.
func NormalizeJSON(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// CheckEmbeddedMatchesUpstream compares embedded snapshots to rbac-config at ConfigRef.
func CheckEmbeddedMatchesUpstream(client *http.Client) error {
	files := []string{
		"cost-management.permissions.json",
		"cost-management.roles.json",
		"sources.permissions.json",
		"sources.roles.json",
	}
	var diffs []string
	for _, name := range files {
		embedded, err := EmbeddedFile(name)
		if err != nil {
			return err
		}
		upstream, err := FetchUpstreamFile(client, name)
		if err != nil {
			return err
		}
		embNorm, err := NormalizeJSON(embedded)
		if err != nil {
			return fmt.Errorf("%s embedded: %w", name, err)
		}
		upNorm, err := NormalizeJSON(upstream)
		if err != nil {
			return fmt.Errorf("%s upstream: %w", name, err)
		}
		if !bytes.Equal(embNorm, upNorm) {
			diffs = append(diffs, name)
		}
	}
	if len(diffs) > 0 {
		ref, _ := ConfigRef()
		return fmt.Errorf(
			"embedded rbac-config snapshots diverge from %s@%s: %s — update internal/resources/rbac_seed/data/ and rbac_config_ref, then bump rbacSeedRevision if the migration script changes",
			rbacConfigRepo, ref, strings.Join(diffs, ", "),
		)
	}
	return nil
}
