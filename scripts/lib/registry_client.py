#!/usr/bin/env python3
"""Registry API client for the demo installer's version/architecture discovery.

Structure: the pure parsing functions (parse_*) are separated from the thin
HTTP fetchers so the offline test suite can feed recorded API responses
(scripts/testdata/registry/) instead of bypassing the parsing entirely — the
fixture mechanism in demo-preprod.bash skips this code, which is exactly why
it missed two real bugs in it (docker.io redirect auth loss, tag+digest refs).

Command-line interface (used by scripts/lib/demo-preprod.bash):
    registry_client.py list-tags <repo>
    registry_client.py manifest-digest <repo> <tag>
    registry_client.py image-arches <image-ref>

Refresh procedure for the recorded responses: scripts/testdata/registry/README.md.
"""
import json
import re
import sys
import urllib.request

QUAY_API = "https://quay.io/api/v1/repository"
TAG_LIST_PAGE_CAP = 20


# --- pure parsing (tested offline against recorded responses) ---------------

def parse_tag_page(text):
    """quay tag-list page -> (tag names, has_additional)."""
    d = json.loads(text)
    return [t["name"] for t in d.get("tags", [])], bool(d.get("has_additional"))


def parse_tag_digest(text, tag):
    """quay tag-list page -> the tag's manifest_digest ('' when absent)."""
    d = json.loads(text)
    for t in d.get("tags", []):
        if t.get("name") == tag:
            return t.get("manifest_digest") or ""
    return ""


def split_image_ref(ref):
    """image reference -> (registry, repo, reference).

    docker.io redirects to registry-1.docker.io and urllib drops the auth
    header across hosts, so the real registry is addressed directly.
    Pod-spec refs may carry both tag and digest (repo:tag@sha256:...).
    """
    if "://" in ref:
        ref = ref.split("://", 1)[1]
    registry, rest = ref.split("/", 1)
    if registry in ("docker.io", "index.docker.io"):
        registry = "registry-1.docker.io"
    if "@" in rest:
        repo, _, reference = rest.rpartition("@")
        # Strip a tag before the digest (repo:tag@sha256:...) — but only when
        # a tag separator actually exists: rpartition returns the WHOLE string
        # as the last element when there is no colon, which used to turn a
        # single-component repo (registry.example:5000/operator@sha256:...)
        # into an empty repository.
        if ":" in repo:
            base_repo, _, maybe_tag = repo.rpartition(":")
            if "/" not in maybe_tag:
                repo = base_repo
    else:
        # Same care for untagged refs: without the colon check, a
        # single-component repo was misread as the tag of an empty repo.
        if ":" in rest:
            repo, _, tag = rest.rpartition(":")
            if "/" in tag or not tag:
                repo, reference = rest, "latest"
            else:
                reference = tag
        else:
            repo, reference = rest, "latest"
    return registry, repo, reference


def token_url_from_challenge(www_authenticate, repo):
    """WWW-Authenticate challenge -> token URL.

    docker.io's /v2/ ping challenge carries no scope; the pull scope for the
    repo is constructed (that missing-scope case was a real bug).
    """
    www = www_authenticate.replace('",', '" ')
    opts = dict(re.findall(r'(\w+)="([^"]*)"', www))
    realm = opts.get("realm")
    if not realm:
        return None
    url = f"{realm}?service={opts.get('service', '')}"
    scope = opts.get("scope") or f"repository:{repo}:pull"
    return f"{url}&scope={scope}"


def architectures_from_manifests(manifests):
    """OCI index / docker manifest list -> sorted linux architectures."""
    archs = set()
    for m in manifests:
        p = m.get("platform", {})
        if p.get("os", "linux") == "linux":
            archs.add(p.get("architecture", "unknown"))
    return sorted(archs)


def architecture_from_config(text):
    """image config blob -> architecture."""
    return json.loads(text).get("architecture", "unknown")


# --- thin HTTP fetchers ------------------------------------------------------

def _get(url, token=None, accept=None, timeout=30):
    req = urllib.request.Request(url)
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    if accept:
        req.add_header("Accept", accept)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read()


def _fetch_token(registry_base, repo):
    """Anonymous token for registries that challenge (docker.io)."""
    try:
        _get(f"{registry_base}/v2/")
        return ""
    except urllib.error.HTTPError as e:
        www = e.headers.get("WWW-Authenticate", "")
        url = token_url_from_challenge(www, repo)
        if not url:
            return ""
        try:
            tok = json.loads(_get(url))
            return tok.get("token") or tok.get("access_token") or ""
        except Exception:
            return ""
    except Exception:
        return ""


def fetch_list_tags(repo):
    """All tags of a quay repository, paginated (cap: 20 pages)."""
    tags, page = [], 1
    while True:
        url = f"{QUAY_API}/{repo}/tag/?limit=100&page={page}&onlyActiveTags=true"
        names, more = parse_tag_page(_get(url))
        tags += names
        if not more:
            break
        if page >= TAG_LIST_PAGE_CAP:
            print(f"warning: stopped listing tags after {TAG_LIST_PAGE_CAP} pages; "
                  "very old tags may be missing", file=sys.stderr)
            break
        page += 1
    return tags


def fetch_manifest_digest(repo, tag):
    """The manifest digest of a tag ('' when unknown), via the filtered query."""
    page = 1
    while True:
        url = (f"{QUAY_API}/{repo}/tag/?limit=100&page={page}"
               f"&filter_tag_name=like:{tag}&onlyActiveTags=true")
        digest = parse_tag_digest(_get(url), tag)
        if digest:
            return digest
        _, more = parse_tag_page(_get(url))
        if not more:
            return ""
        page += 1


MANIFEST_ACCEPT = ", ".join([
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
])


def fetch_image_arches(ref):
    """The architectures an image reference supports ('' when unknown)."""
    registry, repo, reference = split_image_ref(ref)
    base = f"https://{registry}"
    token = _fetch_token(base, repo)
    try:
        m = json.loads(_get(f"{base}/v2/{repo}/manifests/{reference}", token, MANIFEST_ACCEPT))
    except Exception:
        return ""
    if "manifests" in m:
        archs = architectures_from_manifests(m["manifests"])
        return " ".join(archs) if archs else "unknown"
    cfg = m.get("config", {}).get("digest")
    if not cfg:
        return "unknown"
    try:
        return architecture_from_config(_get(f"{base}/v2/{repo}/blobs/{cfg}", token))
    except Exception:
        return "unknown"


def main(argv):
    if len(argv) < 3:
        print("usage: registry_client.py list-tags|manifest-digest|image-arches ...", file=sys.stderr)
        return 2
    cmd, arg = argv[1], argv[2]
    if cmd == "list-tags" and len(argv) == 3:
        print("\n".join(fetch_list_tags(arg)))
    elif cmd == "manifest-digest" and len(argv) == 4:
        print(fetch_manifest_digest(arg, argv[3]))
    elif cmd == "image-arches" and len(argv) == 3:
        print(fetch_image_arches(arg))
    else:
        print(f"error: bad arguments: {argv[1:]}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
