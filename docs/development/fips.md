# Native Go FIPS builds and verification

The release image builds both `/manager` and `/wait-for` with the Red Hat Go
toolchain and its native Go cryptographic module. This implements the security
goal of [COST-8215](https://redhat.atlassian.net/browse/COST-8215) using native
cryptography rather than the ticket's originally prescribed CGO/OpenSSL backend.
The ticket's implementation-specific acceptance criteria need to reflect this
decision. The operator image's evidence does not establish compliance of Envoy,
Keycloak, Koku, or other managed applications.

## Build contract

`Dockerfile` is the release build contract:

- `GOFIPS140=v1.0.0` selects the validated snapshot rather than the live or
  in-process module. The current builder resolves it to `v1.0.0-c2097c7c`.
- `CGO_ENABLED=0` and `-tags=no_openssl` select native Go crypto without a
  runtime OpenSSL dependency. The runtime remains UBI 9 Micro.
- `-tags=nativefips` includes each main package's `//go:debug fips140=auto`
  directive. These files are excluded from ordinary developer builds.
- `GOEXPERIMENT=strictfipsruntime` rejects startup if FIPS is disabled on a
  FIPS host. `GOTOOLCHAIN=local` prevents automatic toolchain replacement.

The pinned Red Hat Go 1.26.10 builder supports `fips140=auto`: it reads
`/proc/sys/crypto/fips_enabled` at startup. On a FIPS host the native module is
enabled; on an ordinary host it is disabled. `auto` is a downstream extension,
so do not infer support from the upstream Go version alone. Ordinary local
`make build`, `make run`, and `make test` omit `nativefips` and work with upstream
Go. Such local binaries are not the release FIPS artifacts.

Changing the compiler, snapshot, runtime, architecture, or relevant build flags
requires repeating the verification below. A checker pass does not establish
eligibility under the module certificate's operating-environment requirements.
Record the exact image, compiler, module, architecture, and security policy.

## Local runtime checks

```sh
make docker-build IMG=koku-service-operator:fips-check
make verify-fips IMG=koku-service-operator:fips-check
```

`verify-fips` checks both executables in the final runtime image:

1. The compiled-in `auto` default enables native FIPS on a simulated FIPS host.
2. Explicit `on` and test-only `only` modes report the selected module enabled.
3. Explicit `off` on a simulated FIPS host fails with the strict startup
   diagnostic, rather than silently running without FIPS.

The host simulation uses the Red Hat toolchain's
`GOLANG_NATIVE_HOSTFIPS_OVERRIDE=1` test hook. It is not a substitute for a host
installed in FIPS mode, and must not be set in customer deployments.
`GODEBUG=fips140=only` is for testing, not a production default.

Both executables accept `--fips-status` before their normal required arguments:

```sh
docker run --rm --entrypoint /manager koku-service-operator:fips-check --fips-status
docker run --rm --entrypoint /wait-for koku-service-operator:fips-check --fips-status
```

They print JSON containing `enabled` and `version`, obtained from the native
`crypto/fips140` API. `enabled: false` is expected on a non-FIPS host in `auto`
mode. GitHub Actions runs `verify-fips` against the runtime image. It also runs
unit tests in the Red Hat builder with `GODEBUG=fips140=only`, the same native
build tags, and the pinned snapshot. Those tests exercise API/resource builders,
credentials, helper behavior, and dependency-validation paths. Controller tests
that require envtest still run in the normal full Go suite; real cluster
acceptance remains separate.

Pull-request runs verify a locally loaded image without publishing it. Main-branch
runs build and push a unique `candidate-<run-id>-<attempt>` tag with provenance,
then pull and verify the registry artifact by its immutable digest. After both
image verification and native-FIPS tests pass, CI promotes that same digest to
`latest`, `main`, and the commit tag using `buildx imagetools create`, preserving
the index and its provenance without rebuilding. A failed run can leave a
candidate in Quay, but does not promote it to the normal deployment tags.

## Run check-payload before cluster verification

Use a recorded revision of `openshift/check-payload` that implements native Go
FIPS validation. Older versions require CGO/OpenSSL even for valid native
builds; the README may also lag the validation implementation. The checker must
recognize the native activation mode in `DefaultGODEBUG` and the selected
`GOFIPS140` version in **both** binaries' build metadata.

Record the checker commit, root configuration checksum, image digest, target
architecture, operator commit and working-tree changes, command, exit status,
and full report. Use the checker's normal root `config.toml` and fail on
warnings. Do not add ignores merely to make the scan green.

On Linux, with an image accessible to Podman:

```sh
CHECKER=/path/to/check-payload
"$CHECKER/check-payload" scan image \
  --spec '<image@sha256:digest>' \
  --config "$CHECKER/config.toml" \
  --fail-on-warnings \
  --output-format markdown --output-file /tmp/operator-fips-report.md
```

For an image exported from Docker, unpack the complete merged filesystem into
a dedicated directory and scan that directory inside Linux. On macOS, unpack
the archive inside Linux as well, so zero-permission system files retain Linux
filesystem semantics rather than bind-mount translation:

```sh
"$CHECKER/check-payload" scan local \
  --path /path/to/extracted-rootfs \
  --config "$CHECKER/config.toml" \
  --fail-on-warnings \
  --output-format markdown --output-file /tmp/operator-fips-report.md
```

`--path` is the actual root filesystem, not an OCI bundle parent directory.
Do not use `--rpm-scan`: `/manager` and `/wait-for` are copied into the image
and are not RPM-owned. Inspect their individual report rows. The scanner can
analyze ELF files for a different target architecture, but that is not a
runtime test for that architecture.

## FIPS-enabled OpenShift acceptance

### Credential and dependency regression tests

The normal test suite and native-FIPS CI selection cover:

- Generated database, Django, and UI cookie credential formats.
- Create-only Secret preservation across repeated reconciliation, controller
  reconstruction, and administrator replacement.
- S3 and Keycloak JWKS HTTPS trust, hostname rejection, and recovery.
- S3 authorization rejection and recovery through dependency conditions.

These tests use local TLS servers and a fake Kubernetes client. The S3 server
returns controlled responses; it does not validate request signatures. Live
acceptance must also exercise signed `HeadBucket` requests against a real S3
implementation, reject invalid credentials, and verify recovery. A JWKS fetch
checks the operator's client, not browser login or downstream JWT verification.

Use disposable resources for live negative tests and preserve the active CR,
existing Secret values and UIDs, and working uploads. Record old/new manager
image digests when testing image-change preservation; controller reconstruction
alone does not prove a complete product or schema upgrade. A TLS front end with
an HTTP backend proves only the operator-to-front-end HTTPS hop. Keep lab-specific
fixtures, orchestration, and captured evidence outside the product repository.
Random samples and correct output lengths alone do not prove cryptographic quality.

### Broader acceptance

After the checker scan passes, deploy the same image to a cluster installed
with `fips: true`. Record host FIPS state and confirm it is visible in the
operator and helper containers. Inspect `--fips-status` from both executables,
startup logs, webhook TLS, initial and subsequent reconciliation, generated
credentials, and secured Kafka, S3, and Keycloak JWKS connections. Confirm
helper-based init containers complete. Test failure and recovery with the
supported dependency configuration; a TCP-only readiness check does not prove
application TLS or authentication works.

Keep existing generated credentials across upgrades and reconciliation. Native
crypto does not migrate or reissue existing Secrets. Repeat ordinary-cluster
smoke testing to verify the `auto` path remains usable outside FIPS mode.

## References

- [Go FIPS documentation](https://go.dev/doc/security/fips140)
- [Go module certificate #5247](https://csrc.nist.gov/projects/cryptographic-module-validation-program/certificate/5247)
- [Certificate security policy](https://csrc.nist.gov/CSRC/media/projects/cryptographic-module-validation-program/documents/security-policies/140sp5247.pdf)
- [Red Hat Go 1.26 host-auto patch](https://github.com/golang-fips/go/blob/go1.26-fips-release/patches/004-host-fips-auto.patch)
- [check-payload](https://github.com/openshift/check-payload)
