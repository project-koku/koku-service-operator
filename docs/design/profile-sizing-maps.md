# Profile sizing maps (`spec.profile`)

> **Status**: Design approved 2026-10-22 — implementation tracked in
> [profile-sizing-maps-PLAN.md](profile-sizing-maps-PLAN.md)
> **Ticket**: [COST-8095](https://redhat.atlassian.net/browse/COST-8095) (parent COST-8091)
> **Source of truth for numbers**: Thomas Stetson's CoP performance work
> ([COST-7567](https://redhat.atlassian.net/browse/COST-7567),
> [COST-7618](https://redhat.atlassian.net/browse/COST-7618)), captured in
> [docs/performance/sizing-guide.md](../performance/sizing-guide.md),
> [docs/performance/operator-profile-crd-mapping.md](../performance/operator-profile-crd-mapping.md),
> and `cost-onprem-chart/cost-onprem/values{,-medium,-large,-xlarge}.yaml`
> (chart `main` @ `09c2560`).

## 1. Intent

A customer sets one field — `spec.profile: small | medium | large | xlarge` —
and every managed workload gets CPU/memory requests and limits, replica
counts, upload limits, and gateway timeouts that were validated end-to-end by
the chart performance runs. Any value the customer sets explicitly in the CR
still wins.

### Why

- **Most workloads run BestEffort today.** The CRD has no resource defaults and
  builders pass `spec.*.resources` straight through. Without overrides,
  koku-api, masu, listener, Celery workers, RBAC, ROS, Kruize, ingress and
  Envoy get no requests or limits. Only the UI and Celery Beat have hardcoded
  values.
- **Replica defaults drifted from the validated chart.** Chart (post-COST-7599)
  runs `ocp`/`summary` workers at 2 and `download`/`refresh` at 0 (OCP-only
  installs don't use them); the operator runs all four at 1.
- **The tier data lives in a Helm chart that is being retired.** Encoding it
  in the operator keeps it actionable.
- **`profile: ha` is a lie today.** It only touches UI containers.

### Decisions already made (with the user, 2026-10-22)

| Decision | Detail |
|----------|--------|
| Tiers replace `standard \| ha` | Enum becomes `small \| medium \| large \| xlarge`, default `small`. `standard` and `ha` are removed, no aliases. Rationale posted on COST-8095 (comment 19074158). |
| Thomas's research is authoritative | No separate sign-off needed. |
| Chart numbers transfer as-is | Chart perf runs used the same (or nearly the same) images; values/measurements hold for the operator. No "pending re-validation" caveat. COST-8149–8151 are confirmation runs, not prerequisites. |
| No backward compatibility | Beta; no installs to preserve. API changes may break stored CRs. |
| Profile owns non-resource knobs too | Upload size, ingress in-memory buffer, Envoy route/per-try timeouts, HAProxy route timeout. |
| Everything is overridable | Any field set in the CR wins over the profile. The one exception is a validation cap (Kruize replicas ≤ 1). |
| Envoy HTTP connection manager matches the chart | Drop `request_timeout: 60s`; add `common_http_protocol_options.idle_timeout: 300s` and `max_connection_duration: 600s`. Applies to all tiers (§3.6). |
| Perf harness is in scope | `scripts/lib/perf-testing.sh` sets `spec.profile` from `PERF_PROFILE` instead of `oc scale` (§7.1). |

The running decision log, with rationale and dates, is in
[profile-sizing-maps-PLAN.md](profile-sizing-maps-PLAN.md#decision-log).

## 2. Goals, non-goals, anti-goals

**Goals**

- One shared sizing table in `internal/resources/profiles.go` covering every
  in-scope workload (§5).
- Every in-scope builder consults it when the corresponding CR field is unset.
- Default install (`profile` omitted → `small`) reproduces the chart's
  `values.yaml` sizing, except the documented D19 worker-memory deviation (§5.1).
- `medium`/`large`/`xlarge` reproduce `small` + the corresponding chart overlay.
- Applied profile visible in status (`status.profile`, §3.4).

**Non-goals**

- HA / redundancy as a concept. Not tested by the perf work; separate concern.
- HPA / autoscaling.
- Sizing BYOI infrastructure (PostgreSQL, Kafka, object storage). The operator
  doesn't run it in production; tier numbers for it are customer guidance in
  docs only.
- Sizing the bundled dev-only PostgreSQL/Valkey (`database.deploy` /
  `cache.deploy`). Keep current minimal behaviour.
- SaaS Celery queues (`hcs`, `subsExtraction`, `subsTransmission`) — stay at
  hard default 0 replicas, not profile-driven (COST-7687).
- Celery `concurrency` — no tier changes it; stays a CRD default of 5.
- Migration Jobs — keep current hardcoded resources.
- Writing the operator-facing sizing guide (COST-8154) and updating the
  mapping doc (COST-8153). This ticket only updates them enough to stay
  truthful (§8).

**Anti-goals (would make this a failure even if it works)**

- Inventing numbers. Every value must trace to the chart `values*.yaml`
  files, except the documented D19 worker-memory deviation (§5.1). Where
  an overlay doesn't mention a workload, it inherits `small`.
- Profile silently overriding an explicit CR value.
- A half-merged resources block (e.g. user limit + profile request) that the
  API server rejects because request > limit.
- Profile behaviour differing between `make run` (`--dev`, no webhooks) and
  in-cluster.

## 3. API changes (`api/v1alpha1/costmanagementserviceconfig_types.go`)

### 3.1 `Profile`

```go
// Profile selects a validated workload sizing tier. Values come from the
// Cost On-Prem performance testing (see docs/performance/sizing-guide.md).
// Explicit per-component fields in the spec always override profile values.
// +kubebuilder:validation:Enum=small;medium;large;xlarge
type Profile string

const (
	ProfileSmall  Profile = "small"
	ProfileMedium Profile = "medium"
	ProfileLarge  Profile = "large"
	ProfileXLarge Profile = "xlarge"
)
```

`spec.profile` keeps `+kubebuilder:default:=small` (was `standard`). Godoc on
the spec field drops the "UI only" wording.

### 3.2 Replica fields become `*int32` with no CRD default

Today these are `int32` with `+kubebuilder:default:=N`. The API server writes
the default into the stored CR, so the operator can't distinguish "unset" from
"explicitly N". Change each to `*int32`, remove the `default` marker, add
`+kubebuilder:validation:Minimum=0`:

| Field | Current default |
|-------|-----------------|
| `spec.costManagement.api.replicas` | 1 |
| `spec.costManagement.masu.replicas` | 1 |
| `spec.costManagement.listener.replicas` | 2 |
| `spec.costManagement.celery.workers.{default,priority,summary,ocp,costModel,refresh,download}.replicas` (`CeleryWorkerSpec`) | 1 |
| `spec.rbac.api.replicas`, `spec.rbac.worker.replicas` (`RBACComponentSpec`) | 1 |
| `spec.ros.processor.replicas` | 1 |
| `spec.kruize.replicas` | 1 — add `+kubebuilder:validation:Maximum=1` (PERF-FINDING-004) |
| `spec.auth.envoy.replicas` | 2 |
| `spec.ui.replicaCount` | 1 |

Semantics: `nil` → profile value; any non-nil value (including `0`) is used
as-is. This also fixes a latent bug: today several builders treat `0` as unset
and coerce it to 1 (e.g. `internal/resources/koku.go:21-23`), so scale-to-zero
only works for Celery workers.

`SaaSCeleryWorkerSpec` stays `int32` with `default:=0`. It currently converts
to `CeleryWorkerSpec` via a type conversion (`CeleryWorkerSpec()` method);
once the two diverge, replace the conversion with an explicit copy that sets
`Replicas` to a pointer.

Workloads with hardcoded replicas and no CR field (ingress, ROS
API/poller/housekeeper, Celery Beat) stay hardcoded at 1. Not adding new
replica fields in this ticket.

### 3.3 Non-resource knobs

| Field | Change | Unset means |
|-------|--------|-------------|
| `spec.ingress.maxUploadSize` | `int64` + `default:=104857600` → `*int64`, no default, `Minimum=1` | profile value |
| `spec.ingress.maxMemory` | **new** `*int64`, `Minimum=1`; feeds `INGRESS_MAXUPLOADMEM` (currently hardcoded 32 MiB in `internal/resources/ingress.go:84`) | profile value |
| `spec.auth.envoy.ingressTimeout` | **new** `string`, Go duration format (validate with CEL `duration(self)` or a pattern) | profile value |
| `spec.auth.envoy.ingressPerTryTimeout` | **new** `string`, same validation | profile value |
| `spec.gatewayRoute.annotations["haproxy.router.openshift.io/timeout"]` | no API change | profile value (replaces constant `gatewayRouteTimeoutDefault` in `internal/resources/route.go:15`) |

Envoy timeouts replace the hardcoded `timeout: 180s` / `per_try_timeout: 60s`
on the ingress route in the rendered Envoy config
(`internal/resources/envoy.go:480,484`). Other Envoy route timeouts are
unchanged. Config changes already roll the Envoy pod via the
`koku.costmanagement.io/envoy-config-hash` annotation, so the chart's
PERF-FINDING-020 problem does not apply.

### 3.6 Envoy HTTP connection manager parity (no API change)

The operator's rendered Envoy config sets `request_timeout: 60s` on the
HTTP connection manager (`internal/resources/envoy.go:438`), capping the time
to receive a whole request regardless of route timeouts. The chart sets no
`request_timeout` (`cost-onprem-chart/cost-onprem/templates/gateway/configmap-envoy.yaml:38-52`),
so the validated tier timeouts never had that cap. Make the operator's HCM
match the chart:

- remove `request_timeout`
- keep `stream_idle_timeout: 300s`
- add `common_http_protocol_options.idle_timeout: 300s` and
  `max_connection_duration: 600s`

Not profile-driven. Other chart HCM settings (`max_request_headers_kb`,
`max_requests_per_connection`, `headers_with_underscores_action`,
`http2_protocol_options`) are out of scope unless the builder finds they're
needed for parity of the timeouts above.

### 3.4 Status

Add `status.profile` (`Profile`) — the tier whose sized workloads were applied
for the current generation (D20). The controller sets it as soon as the last
phase that unconditionally applies sized Deployments has run (`reconcileEdge`,
immediately after the Envoy Deployment is applied), *without* waiting for
readiness-only gates: Envoy/Route admission, the UI (user-provided OAuth client
Secret, `UIReady=False` is a normal lab state) or `Progressing=False`. It is
left unchanged when the pass is paused, or fails/gates before those workloads
were applied (migration, core-service or worker readiness, validation).
Consumers that need rollout completion must wait on Deployment rollouts, not on
this field. Add a printer column `Profile` for `oc get cmsc`.

### 3.5 Resources (no API change)

`corev1.ResourceRequirements` fields stay as they are. Rule, same as the
existing UI behaviour (`internal/resources/ui.go:183-190`):

- If the CR block has **any** `requests` or `limits` entry → use the CR block
  as-is, no merge with the profile.
- Else → use the profile entry.

## 4. Implementation shape

### 4.1 `internal/resources/profiles.go`

- A `Workload` string type with one constant per row in §5.
- A table `map[Profile]map[Workload]sizing` where
  `sizing{Replicas *int32; Resources corev1.ResourceRequirements}`.
  Build `medium`/`large`/`xlarge` from `small` plus explicit deltas, so an
  untouched workload visibly inherits `small` and the deltas read like the
  chart overlays.
- Separate per-profile struct for non-resource knobs
  (`MaxUploadSize`, `MaxMemory`, `EnvoyIngressTimeout`,
  `EnvoyIngressPerTryTimeout`, `RouteTimeout`).
- Accessors, all taking the CR:
  - `ProfileResources(cfg, w Workload, explicit corev1.ResourceRequirements) corev1.ResourceRequirements`
  - `ProfileReplicas(cfg, w Workload, explicit *int32) int32`
  - `ProfileIngress(cfg) ...` / small helpers for each scalar knob
- An empty/unknown `cfg.Spec.Profile` resolves to `small` (defensive; the CRD
  default and enum should prevent it, but unit tests build CRs in Go without
  defaulting).
- Return deep copies of `ResourceRequirements` (`.DeepCopy()`), since the
  table is shared.
- Fold `uiProfileResources` (`internal/resources/ui.go:147`) into the table.

### 4.2 Builders

Every in-scope builder replaces `spec.Resources` / `spec.Replicas` (and the
`if replicas == 0 { replicas = N }` fallbacks) with the accessors. Celery
Beat's hardcoded resources (`koku.go:181-189`) move into the table (values
unchanged). Builders stay pure functions of the CR; no reconciler changes are
needed beyond writing `status.profile`.

### 4.3 Where defaulting happens

In the builders, at render time — **not** in the mutating webhook. The webhook
`Default()` is a no-op today, and webhooks are skipped in `--dev` / `make run`
(`cmd/main.go:192`); webhook-side defaulting would make local and in-cluster
behaviour diverge and would freeze profile values into the stored CR, so
changing `spec.profile` later would not resize anything.

### 4.4 Drift

No new mechanism. SSA with `ForceOwnership` already re-applies the rendered
replicas/resources every reconcile and on the 5-minute `requeueDrift`, so a
manual `kubectl scale` or resource edit is reverted whether the value came
from the CR or the profile. Changing `spec.profile` re-renders and re-applies.

### 4.5 Validation (webhook or CEL)

- `spec.kruize.replicas` ≤ 1 (CRD `Maximum=1`, §3.2).
- Duration format on the two new Envoy timeout strings.

Not encoding: "Kruize CPU limit ≥ 2000m when overridden", "upload size ≤
500 MB unless large/xlarge", "large → timeout ≥ 600s" from the mapping doc's
CEL suggestions. Explicit overrides are the customer's call; the profile
already gets these right. (Builder's call to revisit.)

## 5. Profile tables

All values from chart `main` @ `09c2560`. CPU in cores/millicores, memory in
binary units, as written in the chart. `req` = requests, `lim` = limits.

### 5.1 `small` (= chart `values.yaml`)

| Workload | Chart source | Replicas | CPU req / lim | Mem req / lim |
|----------|--------------|----------|---------------|---------------|
| Koku API | `costManagement.api` | 1 | 250m / 1 | 1Gi / 2Gi |
| MASU | `costManagement.masu` | 1 | 250m / 500m | 1Gi / 2Gi |
| Listener | `costManagement.listener` | 2 | 150m / 300m | 300Mi / 600Mi |
| Celery Beat | `costManagement.celery.beat` | (hardcoded 1) | 50m / 100m | 200Mi / 400Mi |
| Worker `default` | `…workers.default` | 1 | 100m / 200m | 1Gi / 2Gi † |
| Worker `priority` | `…workers.priority` | 1 | 250m / 500m | 1Gi / 2Gi |
| Worker `refresh` | `…workers.refresh` | **0** | 100m / 200m | 1Gi / 2Gi † |
| Worker `summary` | `…workers.summary` | **2** | 250m / 500m | 1Gi / 2Gi |
| Worker `download` | `…workers.download` | **0** | 200m / 400m | 1Gi / 2Gi † |
| Worker `ocp` | `…workers.ocp` | **2** | 250m / 500m | 512Mi / 1Gi |
| Worker `costModel` | `…workers.costModel` | 1 | 100m / 200m | 1Gi / 2Gi † |
| RBAC API | `rbac.api` | 1 | 250m / 500m | 512Mi / 1Gi |
| RBAC worker | `rbac.worker` | 1 | 100m / 500m | 512Mi / 2Gi |
| RBAC Keycloak sync CronJob | `rbac.keycloakSync` | – | 100m / 250m | 256Mi / 512Mi |
| Envoy gateway | `jwtAuth.envoy` | 2 | 100m / 500m | 128Mi / 256Mi |
| UI oauth-proxy | `ui.oauthProxy` | 1 (`ui.replicaCount`) | 50m / 100m | 64Mi / 128Mi |
| UI app | `ui.app` | (shares pod) | 50m / 100m | 64Mi / 128Mi |
| Ingress | `resources.application` | (hardcoded 1) | 500m / 1 | 1Gi / 1Gi |
| ROS API | `resources.application` | (hardcoded 1) | 500m / 1 | 1Gi / 1Gi |
| ROS recommendation poller | `resources.application` | (hardcoded 1) | 500m / 1 | 1Gi / 1Gi |
| ROS housekeeper | `resources.application` | (hardcoded 1) | 500m / 1 | 1Gi / 1Gi |
| ROS processor | `resources.rosProcessor` | 1 | 500m / 1 | 1Gi / 1Gi |
| ROS partition cleaner CronJob | `ros.housekeeper.partitionCleaner` | – | 100m / 300m | 256Mi / 512Mi |
| Kruize | `resources.kruize` | 1 | 1000m / 2000m | 1Gi / 2Gi |
| Kruize partitions (init + delete CronJob) | `kruize.partitions` | – | 100m / 300m | 256Mi / 512Mi |

† Deviates from chart values.yaml (D19): chart's 200Mi/400Mi, 256Mi/512Mi,
512Mi/1Gi limits OOM at boot on koku image 422f758 — healthy workers idle at
~750Mi (Cluster Bot run 2026-10-23).

Non-resource knobs (`small`): `maxUploadSize` 104857600 (100 MB),
`maxMemory` 33554432 (32 MB), Envoy ingress timeout `180s`, per-try `60s`,
HAProxy route timeout `180s`. These equal today's operator hardcodes.

**Behaviour changes vs. today's operator defaults** (intentional, chart
parity): `ocp` and `summary` 1 → 2 replicas; `download` and `refresh` 1 → 0
(chart comments: OCP uses push via Kafka; refresh only serves
OCP-on-cloud deletes); every workload above gains requests/limits; the four
workers marked † get 1Gi/2Gi memory instead of the chart values (D19).

Note: `sizing-guide.md` lists Kruize small as 500m/2000m CPU; the chart
`values.yaml` (stated source of truth) has 1000m/2000m and memory 1Gi/2Gi.
Use `values.yaml`.

### 5.2 `medium` (= `small` + `values-medium.yaml`)

Only the listed fields change; everything else inherits `small`.

| Workload | Change |
|----------|--------|
| Listener | mem 1Gi / 2Gi (replicas 2, CPU unchanged) |
| Worker `ocp` | CPU 250m / 1000m, mem 512Mi / 2Gi (replicas 2) |
| Worker `summary` | CPU 250m / 1000m (replicas 2, mem unchanged) |
| ROS processor | replicas 2, mem 2Gi / 4Gi |
| Kruize | CPU 1000m / 2000m (no change vs `small`) |
| Ingress | mem 1Gi / 2Gi |

Knobs: `maxUploadSize` 209715200 (200 MB), `maxMemory` 67108864 (64 MB),
Envoy `180s` / per-try `180s`, HAProxy `180s`.

### 5.3 `large` (= `small` + `values-large.yaml`)

| Workload | Change |
|----------|--------|
| Listener | replicas 3, mem 2Gi / 4Gi |
| Worker `ocp` | replicas 3, CPU 500m / 1000m, mem 1Gi / 2Gi |
| Worker `summary` | replicas 3, CPU 500m / 1000m |
| ROS processor | replicas 3, mem 2Gi / 4Gi |
| Kruize | mem 2Gi / 4Gi (CPU 1000m / 2000m) |
| Ingress | mem 2Gi / 4Gi |

Knobs: `maxUploadSize` 524288000 (500 MB), `maxMemory` 134217728 (128 MB),
Envoy `600s` / per-try `300s`, HAProxy `600s`.

### 5.4 `xlarge` (= `small` + `values-xlarge.yaml`)

Same as `large`, except:

| Workload | Change vs `large` |
|----------|-------------------|
| Worker `ocp` | CPU 1000m / 2000m, mem 2Gi / 4Gi |
| Worker `summary` | CPU 1000m / 2000m |

Knobs: same as `large`.

### 5.5 `resources.application` split

The chart's overlays raise `resources.application` memory, which in the chart
also hits ROS API / poller / housekeeper. PERF-FINDING-022 attributes the
need to ingress only and calls the shared block an over-provisioning side
effect (COST-8175). The operator applies the overlay's `application` bump to
**ingress only**; ROS API / poller / housekeeper stay at `small` in every
tier.

### 5.6 Out-of-table infrastructure (docs only)

Chart overlays also size `resources.database` (PostgreSQL) and the sizing
guide sizes Kafka brokers. Production is BYOI, so these stay customer
guidance in `docs/performance/sizing-guide.md`. Bundled dev PostgreSQL/Valkey
keep current behaviour (they use `spec.database.resources` /
`spec.cache.resources` pass-through).

## 6. Testing

- **Table tests for `profiles.go`**: for each profile × workload, assert the
  exact requests/limits/replicas from §5 (these tests are the executable copy
  of the tables — a value change must touch both).
- **Precedence tests**: explicit resources block wins whole (no merge);
  partial explicit block (limits only) is used as-is; explicit `replicas: 0`
  produces 0; nil replicas → profile; unknown/empty profile → `small`.
- **Builder tests**: extend existing per-builder tests so each in-scope
  builder is checked at `small` and at least one larger tier; YAML round-trip
  (`yaml.Unmarshal`) where those tests already render YAML (Envoy config for
  the new timeouts).
- **Kruize cap**: CRD validation rejects `replicas: 2` (envtest).
- **Samples**: `api/v1alpha1/samples_test.go` keeps samples parseable after
  the enum change.
- **Coverage**: per `CLAUDE.md` checklist on `internal/resources/...` and
  `api/v1alpha1/...`; no 0% functions in the diff.

## 7. Samples

- `config/samples/…_production.yaml`: `profile: ha` → `profile: large`;
  remove the explicit `resources` overrides that only existed because profile
  maps were missing, keep one or two to demonstrate precedence; refresh the
  header comment (drop the "tracked in COST-8095" note). Monitoring stays
  enabled. (This covers COST-7679's sample.)
- Any other sample/fixture/script referencing `standard` or `ha`
  (`grep -rn 'profile:' config/ scripts/ docs/ test/`) updated to the new enum.

### 7.1 Perf harness

`scripts/lib/perf-testing.sh` `apply_perf_profile_config()` currently stubs
Phase 1 (resources/timeouts) and runs `oc scale` in Phase 2. With
profile-driven replicas, SSA reverts `oc scale` within one reconcile/drift
cycle. Change it to:

- patch the CMSC `spec.profile` to `PERF_PROFILE` (map the harness's
  baseline/small/medium/large/xlarge names onto the enum; baseline → small);
- remove the `oc scale` calls; keep the replica verification step, now
  checking the profile's values from §5;
- clear explicit per-component replicas/resources the harness set
  previously, so the profile actually applies.

This resolves PERF-FINDING-039 and the CMSC-default half of PERF-FINDING-040
in `docs/performance/OPERATOR-FINDINGS.md`; update their status.

## 8. Documentation

- `docs/performance/operator-profile-crd-mapping.md`: replace the
  "`spec.profile` in the current CRD" and "Sizing abstraction" sections with a
  short description of the implemented behaviour and a pointer to this
  design; move `spec.ingress.maxMemory` and Envoy timeouts out of "not yet in
  CRD". Fuller rewrite remains COST-8153.
- `docs/performance/sizing-guide.md`: add an "Operator" note that
  `spec.profile` applies these tiers and that per-component CR fields
  override them. Fuller CMSC translation remains COST-8154.
- `CLAUDE.md` / install and pre-prod guides: replace any `standard` / `ha`
  wording; state beta default is `small`.
- `docs/tasks.md`: mark COST-8095 progress.
- `docs/gap_analysis/BETA-PRIORITY.md` T5: point at this design.

## 9. Left to the builder

- Exact Go type names and accessor signatures in `profiles.go`.
- Whether the knobs struct lives in the same table or alongside it.
- CEL vs. pattern for duration validation.
- Whether to add a `Warnings` from the validating webhook when an explicit
  override is below the profile minimum (optional, not required).
- Commit structure (suggested: API change + generated files; profiles table +
  tests; builder wiring; samples + docs).
