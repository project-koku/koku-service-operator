package principalprofile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFixtureProviderAndHandler(t *testing.T) {
	t.Setenv("KEYCLOAK_URL", "")
	t.Setenv("KEYCLOAK_CLIENT_ID", "")
	t.Setenv("KEYCLOAK_CLIENT_SECRET", "")
	t.Setenv("PRINCIPAL_PROFILE_FIXTURE", "")
	t.Setenv("PRINCIPAL_PROFILE_TOKEN", "spike")

	p, err := NewProviderFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := p.ProfilesForOrg(context.Background(), "12345")
	if err != nil {
		t.Fatal(err)
	}
	if !profiles["admin"].IsOrgAdmin {
		t.Fatalf("admin = %#v", profiles["admin"])
	}

	mux := NewMux(p)
	req := httptest.NewRequest(http.MethodGet, "/v1/orgs/12345/profiles", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/orgs/12345/profiles", nil)
	req.Header.Set("Authorization", "Bearer spike")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Profiles["alice"].Email != "alice@example.com" {
		t.Fatalf("body=%#v", body)
	}
}

func TestFixtureFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	fixture := `{"profiles":{"carol":{"email":"c@e.com","first_name":"C","last_name":"D",` +
		`"is_org_admin":true,"enabled":true}}}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEYCLOAK_URL", "")
	t.Setenv("KEYCLOAK_CLIENT_ID", "")
	t.Setenv("KEYCLOAK_CLIENT_SECRET", "")
	t.Setenv("PRINCIPAL_PROFILE_FIXTURE", path)
	t.Setenv("PRINCIPAL_PROFILE_TOKEN", "")

	p, err := NewProviderFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := p.ProfilesForOrg(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if !profiles["carol"].IsOrgAdmin {
		t.Fatalf("%#v", profiles)
	}
}
