# Recorded registry API responses

Fixtures for the offline tests of `scripts/lib/registry_client.py` (via
`scripts/demo-preprod_test.sh`). They exercise the production parsing code
against real API shapes — the bash-level `REGISTRY_TAG_FIXTURE` mechanism
bypasses it entirely.

**Recorded on: 2026-11-25** against:
- quay.io tag API, repository `project-koku/koku-service-operator-catalog`
- docker.io (registry-1.docker.io), repository `library/postgres`, tag `16`

## Refresh procedure

If a registry changes its response shapes (or the tests fail mysteriously),
re-capture with `curl` from this directory:

```bash
QUAY_REPO=project-koku/koku-service-operator-catalog
curl -s "https://quay.io/api/v1/repository/${QUAY_REPO}/tag/?limit=5&page=1&onlyActiveTags=true" > quay-tag-page-1.json
curl -s "https://quay.io/api/v1/repository/${QUAY_REPO}/tag/?limit=5&page=2&onlyActiveTags=true" > quay-tag-page-2.json
curl -s "https://quay.io/api/v1/repository/${QUAY_REPO}/tag/?limit=100&page=1&filter_tag_name=like:v0.0.1&onlyActiveTags=true" > quay-tag-digest-v0.0.1.json

curl -sI https://registry-1.docker.io/v2/ | grep -i '^www-authenticate' | sed 's/^[Ww][Ww][Ww]-[Aa]uthenticate: //' | tr -d '\r' > docker-io-challenge.txt

TOKEN=$(curl -s 'https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/postgres:pull' | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
curl -s -H "Authorization: Bearer $TOKEN" \
  -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json' \
  'https://registry-1.docker.io/v2/library/postgres/manifests/16' > docker-io-manifest-list-postgres16.json
CHILD=$(python3 -c 'import json; d=json.load(open("docker-io-manifest-list-postgres16.json")); print([m for m in d["manifests"] if m["platform"].get("architecture")=="amd64"][0]["digest"])')
curl -s -H "Authorization: Bearer $TOKEN" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
  "https://registry-1.docker.io/v2/library/postgres/manifests/${CHILD}" > docker-io-child-postgres16-amd64.json
CONFIG=$(python3 -c 'import json; print(json.load(open("docker-io-child-postgres16-amd64.json"))["config"]["digest"])')
curl -sL -H "Authorization: Bearer $TOKEN" "https://registry-1.docker.io/v2/library/postgres/blobs/${CONFIG}" > docker-io-config-postgres16-amd64.json
```

Update the "Recorded on" date above when you refresh.
