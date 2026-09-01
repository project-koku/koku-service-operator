package rbac_seed

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

//go:embed data/*.json rbac_config_ref
var embeddedFS embed.FS

const (
	rbacConfigRepo       = "project-kessel/rbac-config"
	rbacConfigConfigPath = "configs/prod"
)

// ConfigRef returns the pinned project-kessel/rbac-config commit SHA.
func ConfigRef() (string, error) {
	b, err := embeddedFS.ReadFile("rbac_config_ref")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

type roleFile struct {
	Roles []roleDef `json:"roles"`
}

type roleDef struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	AdminDefault    bool   `json:"admin_default"`
	PlatformDefault bool   `json:"platform_default"`
	Access          []struct {
		Permission string `json:"permission"`
	} `json:"access"`
}

type resourceVerb struct {
	resource string
	verb     string
}

type roleSeed struct {
	name            string
	description     string
	adminDefault    bool
	platformDefault bool
	access          []appResourceVerb
}

type appResourceVerb struct {
	app      string
	resource string
	verb     string
}

// CostManagementSeedPython returns the Django shell block that seeds
// cost-management/sources permissions and roles from rbac-config snapshots.
func CostManagementSeedPython() (string, error) {
	perms, err := costManagementPermissionTuples()
	if err != nil {
		return "", err
	}
	roles, err := roleSeedTuples()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(`from api.models import Tenant
from management.models import Permission, Role, Access

public_tenant = Tenant.objects.get(tenant_name='public')

cm_perms = [
`)
	for i, p := range perms {
		fmt.Fprintf(&b, "    (%q, %q)", p.resource, p.verb)
		if i < len(perms)-1 {
			b.WriteString(",\n")
		}
	}
	b.WriteString(`
]

perm_count = 0
for res, verb in cm_perms:
    _, created = Permission.objects.get_or_create(
        application="cost-management", resource_type=res, verb=verb,
        defaults={"permission": f"cost-management:{res}:{verb}", "tenant": public_tenant}
    )
    if created:
        perm_count += 1

_, created = Permission.objects.get_or_create(
    application="sources", resource_type="*", verb="*",
    defaults={"permission": "sources:*:*", "tenant": public_tenant}
)
if created:
    perm_count += 1
print(f"Seeded {perm_count} permissions")

roles = [
`)
	for i, r := range roles {
		fmt.Fprintf(&b, "    (%q, %q, %t, %t, [", r.name, r.description, r.adminDefault, r.platformDefault)
		for j, a := range r.access {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "(%q, %q, %q)", a.app, a.resource, a.verb)
		}
		b.WriteString("])")
		if i < len(roles)-1 {
			b.WriteString(",\n")
		}
	}
	b.WriteString(`
]

role_count = 0
for name, desc, admin_default, platform_default, access_list in roles:
    role, created = Role.objects.get_or_create(
        name=name, tenant=public_tenant,
        defaults={"description": desc, "system": True, "platform_default": platform_default, "admin_default": admin_default, "version": 2}
    )
    if created:
        role_count += 1
    for app, res, verb in access_list:
        perm = Permission.objects.get(application=app, resource_type=res, verb=verb, tenant=public_tenant)
        Access.objects.get_or_create(role=role, permission=perm, defaults={"tenant": public_tenant})

print(f"Seeded {role_count} roles (total: {Role.objects.count()})")
print("RBAC seeding complete.")`)

	return b.String(), nil
}

func costManagementPermissionTuples() ([]resourceVerb, error) {
	raw, err := embeddedFS.ReadFile("data/cost-management.permissions.json")
	if err != nil {
		return nil, err
	}
	return parsePermissionTuplesInFileOrder(raw, "cost-management")
}

func parsePermissionTuplesInFileOrder(raw []byte, application string) ([]resourceVerb, error) {
	var ordered map[string][]struct {
		Verb string `json:"verb"`
	}
	if err := json.Unmarshal(raw, &ordered); err != nil {
		return nil, err
	}
	// Extract key order from raw JSON by scanning - simpler: use known order from file.
	// For stability, decode with json.Decoder and Token() - overkill.
	// Use resource order as stored in our embedded file (matches legacy script).
	order := resourceOrderFromJSON(raw)
	out := make([]resourceVerb, 0, 32)
	for _, res := range order {
		verbs, ok := ordered[res]
		if !ok {
			continue
		}
		for _, v := range verbs {
			out = append(out, resourceVerb{resource: res, verb: v.Verb})
		}
	}
	if application == "cost-management" && len(out) == 0 {
		return nil, fmt.Errorf("no permissions parsed for %s", application)
	}
	return out, nil
}

func resourceOrderFromJSON(raw []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil
	}
	var order []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return order
		}
		key, ok := tok.(string)
		if !ok {
			return order
		}
		order = append(order, key)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return order
		}
	}
	return order
}

func roleSeedTuples() ([]roleSeed, error) {
	cmRoles, err := readRolesFile("data/cost-management.roles.json")
	if err != nil {
		return nil, err
	}
	srcRoles, err := readRolesFile("data/sources.roles.json")
	if err != nil {
		return nil, err
	}
	all := append(cmRoles, srcRoles...)
	out := make([]roleSeed, 0, len(all))
	for _, r := range all {
		rs := roleSeed{
			name:            r.Name,
			description:     r.Description,
			adminDefault:    r.AdminDefault,
			platformDefault: r.PlatformDefault,
		}
		for _, a := range r.Access {
			app, resource, verb, err := splitPermission(a.Permission)
			if err != nil {
				return nil, err
			}
			rs.access = append(rs.access, appResourceVerb{app: app, resource: resource, verb: verb})
		}
		out = append(out, rs)
	}
	return out, nil
}

func readRolesFile(path string) ([]roleDef, error) {
	raw, err := embeddedFS.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rf roleFile
	if err := json.Unmarshal(raw, &rf); err != nil {
		return nil, err
	}
	return rf.Roles, nil
}

func splitPermission(permission string) (app, resource, verb string, err error) {
	parts := strings.Split(permission, ":")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("invalid permission %q", permission)
	}
	return parts[0], parts[1], parts[2], nil
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
		client = http.DefaultClient
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
