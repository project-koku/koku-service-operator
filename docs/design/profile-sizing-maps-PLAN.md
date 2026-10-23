# Profile Sizing Maps Implementation Plan (COST-8095)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Living document.** Update the [Progress](#progress) table and tick steps as
> work lands. Append to the [Decision log](#decision-log) whenever a choice is
> made or changed — never rewrite past entries; supersede them.

**Goal:** `spec.profile: small|medium|large|xlarge` drives validated CPU/memory, replicas, upload limits and gateway timeouts for every managed workload; any explicit CR value overrides it.

**Architecture:** One pure table in `internal/resources/profiles.go` (small = chart `values.yaml`; larger tiers = small + chart overlay deltas) with accessors that take the CR and the explicit CR value. Builders call the accessors at render time; SSA already re-applies every reconcile. Replica and upload-size fields become pointers so "unset" is detectable.

**Tech Stack:** Go, kubebuilder/controller-gen v0.18.0, controller-runtime SSA, envtest, bash script tests (`make test-scripts`).

**Spec:** [docs/design/profile-sizing-maps.md](profile-sizing-maps.md) — read it first; §5 holds every number.

## Global Constraints

- Branch `cost-8095-profile-sizing-maps`; PR title `[COST-8095] Implement spec.profile sizing maps`.
- Every number traces to chart `main` @ `09c2560` (`values.yaml`, `values-{medium,large,xlarge}.yaml`) as transcribed in spec §5. No invented values.
- Precedence: explicit CR value wins. Resources: any entry in `requests` or `limits` → CR block used whole, no merge. Replicas: non-nil pointer (including 0) used as-is.
- Empty or unknown `spec.profile` resolves to `small` inside the accessors.
- No backward compatibility: `standard`/`ha` removed outright; CRD default changes are allowed to break stored CRs.
- Defaulting happens in builders, never in the mutating webhook.
- SaaS Celery queues (`hcs`, `subs_extraction`, `subs_transmission`) are not profile-driven: replicas default 0, resources pass-through.
- Bundled PostgreSQL/Valkey and Migration Jobs keep current behaviour.
- After API edits: `make generate && make manifests`; generated files committed in the same commit.
- Before PR: `make test`, `make test-scripts`, `golangci-lint run ./...`, `govulncheck ./...`, coverage per `CLAUDE.md` (no 0% functions in the diff).
- Tests asserting on rendered YAML round-trip through `yaml.Unmarshal` (CLAUDE.md).

## Review Focus

1. **Partial explicit resources** (user sets only `limits.memory`) → CR block used exactly as given, no profile requests injected. Test: `TestProfileResources_PartialExplicitBlockUsedAsIs` (Task 1).
2. **Explicit `replicas: 0`** on a profile-driven workload (e.g. koku API) → 0 replicas, not the profile value and not 1. Test: `TestKokuAPIDeployment_ExplicitZeroReplicas` (Task 2).
3. **Switching `spec.profile` on a live CR** → next render returns the new tier's values (no values frozen into the CR). Test: `TestProfileSwitchRerenders` (Task 3).
4. **Shared table mutation** — a builder mutating returned `ResourceRequirements` must not alter later renders. Test: `TestProfileResources_ReturnsDeepCopy` (Task 1).
5. **Large upload through the gateway** — rendered Envoy HCM has no `request_timeout`, and the ingress route carries the profile timeouts. Test: `TestEnvoyYAML_HCMParityWithChart` (Task 4).

---

## Progress

| Task | Status | Commit / PR | Notes |
|------|--------|-------------|-------|
| 1. Profile enum + sizing table | ✅ done | 5626b2a, 9fdec8c | review clean; prod sample enum bumped ha→large early (T6 finishes it) |
| 2. Replica pointers + replica wiring | ✅ done | 0743ca3, 943b82f | review clean after 1 fix round (OLM bundle regen); e2e has compile-only stopgap until T6 |
| 3. Resource wiring in builders | ✅ done | 6fce0e9 | review clean |
| 4. Ingress / gateway knobs + Envoy HCM parity | ✅ done | 0a4dc94 | review clean; duration pattern + Go→Envoy "Ns" normalisation |
| 5. `status.profile` + printer column | ✅ done | 3e63174 | review clean |
| 6. Samples + e2e helpers | ✅ done | 1821a55, 05c0726 | review clean after 1 fix round (sample listener override must not downgrade `large` memory) |
| 7. Perf harness uses `spec.profile` | ✅ done | 89cdb40, bd709cc | review clean after 1 fix round (wait for status.observedGeneration + status.profile before verifying); polling untested until Task 10 |
| 8. Docs | ✅ done | 17164fe, 9518372, 47f8f2b | review clean after 2 fix rounds; mapping doc now points at design §5 instead of duplicating per-tier YAML |
| 9. Final verification + PR | ✅ done | [PR #195](https://github.com/project-koku/koku-service-operator/pull/195) | re-run green after Task 10; push + PR approved by user 2026-10-23 |
| 10. Cluster Bot end-to-end validation | ✅ done | 775acb6, 0738a1a, 37a6aac, 37220cf (fixes found live) | chat-bot-qdbjn 2026-10-23, image cost-8095-37220cf; see Results below |

## Follow-ups (not in the COST-8095 PR)

Found during review and the Cluster Bot run; deliberately left out of scope.
Track as tickets when picked up.

| # | Follow-up | Why / where |
|---|-----------|-------------|
| F1 | Direct builder tests for profile-driven replicas of UI, Masu, RBAC API/worker and Kruize | Covered today only indirectly (table tests + small/xlarge builder tests); one-line `ProfileReplicas` calls |
| F2 | `scripts/perf-testing_test.sh` `run_profile` helper swallows exit codes (`\|\| true`) | A regression in the unknown-profile fail-fast path would only show in output text, not the exit status |
| F3 | Release note: stored CRs carrying `profile: standard\|ha` or old CRD-written defaults (`replicas: 1`, `maxUploadSize`) | Old enum values are rejected on update; stored numeric defaults now act as explicit pins and override the profile. Beta, no migration — just tell users to drop them |
| F4 | Re-check D19 worker memory against the current koku image when it changes | D19 deviates from chart values because koku `422f758` workers idle at ~750Mi; a lighter image could allow the chart numbers again |
| F5 | Sources API validation returns 201 instead of 400 (duplicate `cluster_id`, missing `source_ref`) | 2 pytest failures in the Cluster Bot run; koku application behaviour, unrelated to sizing |
| F6 | COST-8153 / COST-8154 (Thomas): fuller rewrite of the mapping doc and an operator sizing guide on top of the implemented tiers | This PR only points the docs at design §5 |

## Decision log

Newest last. Each entry: date · decision · why · source.

| # | Date | Decision | Why | Source |
|---|------|----------|-----|--------|
| D1 | 2026-10-22 | `spec.profile` enum = `small\|medium\|large\|xlarge`, default `small` | These tiers were validated end-to-end; `ha` had no measured numbers | User; Thomas Stetson's COST-7567/7618; Jira comment 19074158 on COST-8095 |
| D2 | 2026-10-22 | Remove `standard` and `ha`, no aliases | Beta, no installs to preserve | User |
| D3 | 2026-10-22 | Thomas's research is authoritative; no sign-off round | Ticket text (Lucas) was cleanup without perf context | User |
| D4 | 2026-10-22 | Chart numbers transfer as-is, no "pending operator re-validation" caveat | Chart perf runs used the same / nearly the same images | User |
| D5 | 2026-10-22 | No backward compatibility for stored CRs | Beta | User |
| D6 | 2026-10-22 | Profile also owns upload size, ingress `maxMemory`, Envoy route/per-try timeouts, HAProxy route timeout | `large` without 600s timeout fails big uploads | User (recommended by Claude) |
| D7 | 2026-10-22 | Every profile value overridable by an explicit CR field; only cap is Kruize replicas ≤ 1 | Customer control; PERF-FINDING-004 | User |
| D8 | 2026-10-22 | Envoy HCM matches chart: drop `request_timeout: 60s`, add `idle_timeout: 300s`, `max_connection_duration: 600s`, all tiers | Operator's 60s cap would cut uploads the tiers allow; chart runs never had it | User (found by builder check) |
| D9 | 2026-10-22 | Perf harness switched to `spec.profile` in this ticket | `oc scale` gets reverted by SSA drift correction; PERF-FINDING-039/040 | User (found by builder check) |
| D10 | 2026-10-22 | Defaulting in builders, not the mutating webhook | Webhooks skipped in `--dev`; webhook defaulting freezes values into the CR | Claude's call |
| D11 | 2026-10-22 | Resources: no field-level merge with profile | Half-merged blocks can make request > limit | Claude's call |
| D12 | 2026-10-22 | Chart `resources.application` bump applied to ingress only; ROS API/poller/housekeeper stay `small` | PERF-FINDING-022 attributes the need to ingress; COST-8175 | Claude's call |
| D13 | 2026-10-22 | Kruize `small` = `values.yaml` (1000m/2000m, 1Gi/2Gi), not sizing-guide table (500m) | `values.yaml` is the stated numeric source of truth | Claude's call |
| D14 | 2026-10-22 | BYOI DB/Kafka tier sizing stays docs-only; bundled DB/Valkey unchanged | Production is BYOI | Claude's call (CLAUDE.md) |
| D15 | 2026-10-22 | Design and plan live in `docs/design/` | Repo convention over skill default path | Claude's call |
| D16 | 2026-10-22 | No extra CEL guards on overrides (Kruize CPU ≥ 2000m, upload caps, timeout floors) | Overrides are the customer's call | Claude's call |
| D17 | 2026-10-22 | Every API-changing task regenerates the OLM bundle (`make bundle IMAGE_SHA=<committed containerImage>`) | CI `bundle-validate` regenerates and fails on drift; bundle CRD carries `spec.profile` | Claude's call (found during Task 2) |
| D18 | 2026-10-22 | Validate the whole branch on a live cluster before opening the PR — Cluster Bot 3+3 (CRC only as fallback) | Unit/envtest can't prove scheduling, rollouts or real uploads; 3 workers can actually schedule `large` | User |
| D19 | 2026-10-23 | Celery workers `celery` (default), `cost_model`, `refresh`, `download`: memory 1Gi/2Gi (req/lim) in every tier, overriding chart values | Live Cluster Bot run: `celery`/`cost_model` OOMKilled at boot at chart limits 400Mi/512Mi; healthy workers idle at ~750Mi on operator koku image 422f758 (chart values reference image 03b958b); koku hardcodes concurrency 1 | User (recommended by Claude, Task 10 evidence) |
| D20 | 2026-10-23 | `status.profile` = tier whose sized workloads were applied for the current generation, set once the workload-applying phases succeed, even if later readiness gates (e.g. UIReady) are False | Lab setups without the UI OAuth secret never reach full success, so the 'fully successful pass' rule left status.profile empty forever; health is Available/Degraded's job | User (recommended by Claude, Task 10 evidence) |

---

### Task 1: Profile enum + sizing table

**Files:**
- Modify: `api/v1alpha1/costmanagementserviceconfig_types.go:49-60` (Profile type/consts), `:655-659` (spec field godoc + default)
- Create: `internal/resources/profiles.go`
- Create: `internal/resources/profiles_test.go`
- Modify: `internal/resources/ui.go:147-190` (delete `uiProfileResources`, call accessor)
- Modify: `internal/resources/ui_test.go:195-240` (profile cases → new enum)
- Regenerate: `config/crd/bases/…`, `api/v1alpha1/zz_generated.deepcopy.go`

**Interfaces:**
- Produces (api): `ProfileSmall`, `ProfileMedium`, `ProfileLarge`, `ProfileXLarge` (`Profile` string consts); `ProfileStandard`/`ProfileHA` deleted.
- Produces (resources):
  - `type Workload string` with consts `WorkloadKokuAPI`, `WorkloadMasu`, `WorkloadListener`, `WorkloadCeleryBeat`, `WorkloadRBACAPI`, `WorkloadRBACWorker`, `WorkloadKeycloakSync`, `WorkloadEnvoy`, `WorkloadUIOAuthProxy`, `WorkloadUIApp`, `WorkloadIngress`, `WorkloadROSAPI`, `WorkloadROSPoller`, `WorkloadROSHousekeeper`, `WorkloadROSProcessor`, `WorkloadROSPartitionCleaner`, `WorkloadKruize`, `WorkloadKruizePartitions`
  - `func CeleryWorkload(queue string) Workload` — queue is the builder's queue arg (`celery`, `priority`, `summary`, `ocp`, `cost_model`, `refresh`, `download`)
  - `func EffectiveProfile(cfg *costv1alpha1.CostManagementServiceConfig) costv1alpha1.Profile` — empty/unknown → `ProfileSmall`
  - `func ProfileResources(cfg *costv1alpha1.CostManagementServiceConfig, w Workload, explicit corev1.ResourceRequirements) corev1.ResourceRequirements` — explicit non-empty → explicit; workload not in table → explicit; else deep copy of table entry
  - `func ProfileReplicas(cfg *costv1alpha1.CostManagementServiceConfig, w Workload, explicit *int32) int32` — non-nil → `*explicit`; else table; workload with no replica entry → 1
  - `type ProfileKnobs struct { MaxUploadSize int64; MaxMemory int64; EnvoyIngressTimeout string; EnvoyIngressPerTryTimeout string; RouteTimeout string }` and `func ProfileKnobsFor(cfg *costv1alpha1.CostManagementServiceConfig) ProfileKnobs`

- [ ] **Step 1: Create branch**

Done by controller: worktree `../koku-service-operator-cost-8095` on branch `cost-8095-profile-sizing-maps` (bin/ symlinked). Skip.
Expected: on new branch; untracked files in the main checkout are left alone (do not `git add -A`).

- [ ] **Step 2: Write failing tests in `internal/resources/profiles_test.go`**

Table-driven `TestProfileTable` with one row per (profile, workload) cell in spec §5.1–5.4 asserting requests/limits CPU+memory and replicas — every cell, written out literally from the spec (this file is the executable copy of the tables). Plus:

```go
func TestEffectiveProfile_EmptyAndUnknownAreSmall(t *testing.T)      // "" and "ha" → ProfileSmall
func TestProfileResources_ExplicitBlockWins(t *testing.T)            // explicit req+lim returned unchanged
func TestProfileResources_PartialExplicitBlockUsedAsIs(t *testing.T) // only Limits.memory=3Gi → Requests empty, Limits == {memory:3Gi}
func TestProfileResources_ReturnsDeepCopy(t *testing.T)              // mutate result; second call still equals table value
func TestProfileResources_UnknownWorkloadPassesThrough(t *testing.T) // CeleryWorkload("hcs") with empty explicit → empty
func TestProfileReplicas_ExplicitZeroWins(t *testing.T)              // ptr.To[int32](0) → 0
func TestProfileReplicas_Defaults(t *testing.T)                      // small: ocp 2, summary 2, download 0, refresh 0, listener 2, envoy 2; large: listener 3
func TestProfileKnobsFor(t *testing.T)                               // small 104857600/33554432/"180s"/"60s"/"180s"; medium 209715200/67108864/"180s"/"180s"/"180s"; large+xlarge 524288000/134217728/"600s"/"300s"/"600s"
```

Compare quantities with `.Cmp()` / `.Equal()`, not string formatting.

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/resources/ -run 'Profile' -v`
Expected: build failure (undefined `Workload`, `ProfileResources`, `ProfileSmall` …).

- [ ] **Step 4: Change the API enum**

`+kubebuilder:validation:Enum=small;medium;large;xlarge`; consts per Interfaces; spec field `+kubebuilder:default:=small`; godoc per spec §3.1. Then `make generate && make manifests`.

- [ ] **Step 5: Implement `internal/resources/profiles.go`**

Build `small` as a literal map; derive `medium`/`large`/`xlarge` by copying `small` and applying per-workload deltas mirroring the chart overlays (spec §5.2–5.4, §5.5) so each tier reads like its overlay file. Package-level `var` initialised once; accessors `.DeepCopy()` resources.

- [ ] **Step 6: Fold UI**

Delete `uiProfileResources`; `UIDeployment` uses `ProfileResources(cfg, WorkloadUIOAuthProxy, spec.OAuthProxy.Resources)` and `…WorkloadUIApp…`. Update `ui_test.go` profile table: `small` → 50m/64Mi req, 100m/128Mi lim; every tier gets the same UI values (chart overlays don't touch UI). Replace `ProfileHA` uses in other UI tests with `ProfileLarge` and expect small values.

- [ ] **Step 7: Run tests**

Run: `go test ./internal/resources/ ./api/... -v -run 'Profile|UI'`
Expected: PASS. Then `go build ./...` — PASS.

- [ ] **Step 8: Commit; update Progress row 1**

```bash
git add api/ config/crd internal/resources/profiles.go internal/resources/profiles_test.go internal/resources/ui.go internal/resources/ui_test.go docs/design/profile-sizing-maps-PLAN.md
git commit -m "[COST-8095] Add small/medium/large/xlarge sizing profiles table"
```

---

### Task 2: Replica pointers + replica wiring

**Files:**
- Modify: `api/v1alpha1/costmanagementserviceconfig_types.go` — fields listed in spec §3.2 (`EnvoySpec`, `RBACComponentSpec`, `KruizeConfig`, `ROSProcessorSpec`, `KokuAPISpec`, `MasuSpec`, `ListenerSpec`, `CeleryWorkerSpec`, `UIConfig.ReplicaCount`)
- Modify: `SaaSCeleryWorkerSpec.CeleryWorkerSpec()` (explicit copy)
- Modify: `internal/resources/{koku,rbac,envoy,kruize,ros,ui}.go` replica lines (`koku.go:21,87,147,210`, `rbac.go:134,225`, `envoy.go:163`, `kruize.go:212`, `ros.go:314`, `ui.go:169`)
- Modify tests: `internal/resources/koku_test.go:104-105,183-184`, `ros_test.go:111`, `ui_test.go:21`, `api/v1alpha1/celery_workers_crd_test.go`
- Test: `internal/resources/koku_test.go`, `api/v1alpha1/celery_workers_crd_test.go`

**Interfaces:**
- Consumes: `ProfileReplicas`, `CeleryWorkload` (Task 1).
- Produces: replica fields are `*int32` (`k8s.io/utils/ptr` for literals in tests); `SaaSCeleryWorkerSpec.CeleryWorkerSpec()` returns `CeleryWorkerSpec{Replicas: ptr.To(s.Replicas), Concurrency: s.Concurrency, Resources: s.Resources}`.

- [ ] **Step 1: Write failing tests**

```go
func TestCeleryWorkerDeployments_SmallProfileReplicas(t *testing.T) // celery 1, priority 1, summary 2, ocp 2, cost_model 1, refresh 0, download 0, hcs 0, subs_* 0
func TestCeleryWorkerDeployments_LargeProfileReplicas(t *testing.T) // summary 3, ocp 3, others as small
func TestKokuAPIDeployment_ExplicitZeroReplicas(t *testing.T)      // API.Replicas = ptr.To[int32](0) → 0
func TestListenerDeployment_ProfileReplicas(t *testing.T)          // small 2, large 3, explicit 5 → 5
func TestROSProcessorDeployment_ProfileReplicas(t *testing.T)      // medium 2, xlarge 3
func TestEnvoyDeployment_DefaultReplicas(t *testing.T)             // nil → 2
```

In `api/v1alpha1/celery_workers_crd_test.go` add `TestCRDReplicaFieldsHaveNoDefault` (parse CRD YAML: `default` absent on every §3.2 replica path; SaaS `hcs.replicas` default still 0) and `TestCRDKruizeReplicasMaximumOne` (`maximum: 1` on `spec.kruize.replicas`).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/resources/ ./api/... -run 'Replicas|CRD' -v`
Expected: compile errors (int32 vs *int32) / FAIL.

- [ ] **Step 3: Change field types**

`*int32`, remove `+kubebuilder:default`, add `+kubebuilder:validation:Minimum=0`; Kruize also `Maximum=1`. `make generate && make manifests`.

- [ ] **Step 4: Wire builders**

Replace each `replicas := spec.Replicas; if replicas == 0 {…}` with `ProfileReplicas(cfg, <Workload>, spec.Replicas)`. `CeleryWorkerDeployment` uses `CeleryWorkload(queue)`; SaaS queues arrive with explicit pointers so they bypass the table. Update existing tests that assigned plain ints to use `ptr.To`.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/resources/ ./api/... && go build ./...`
Expected: PASS. Then `make test` — PASS (envtest picks up new CRD).

- [ ] **Step 6: Commit; update Progress row 2**

```bash
git commit -am "[COST-8095] Make replica fields optional and drive defaults from profile"
```

---

### Task 3: Resource wiring in builders

**Files:**
- Modify: `internal/resources/koku.go:45,107,160,181-189,227`; `rbac.go:190,263`; `rbac_keycloak_sync.go:118`; `envoy.go:236`; `ingress.go:148`; `ros.go:269,409,487,554,608`; `kruize.go:239,277,346`
- Test: `internal/resources/{koku,rbac,rbac_keycloak_sync,envoy,ingress,ros,kruize_rbac}_test.go`, new `internal/resources/profiles_builders_test.go`

**Interfaces:**
- Consumes: `ProfileResources`, `Workload*` consts (Task 1).

- [ ] **Step 1: Write failing tests in `profiles_builders_test.go`**

```go
func TestBuildersApplySmallProfileResources(t *testing.T)
// table: builder func → container name → expected req/lim from spec §5.1, for
// KokuAPIDeployment, MasuDeployment, ListenerDeployment, CeleryBeatDeployment,
// each CeleryWorkerDeployments entry except SaaS, RBACAPIDeployment,
// RBACWorkerDeployment, KeycloakSyncCronJob, EnvoyDeployment, IngressDeployment,
// ROSAPIDeployment, ROSPollerDeployment, ROSHousekeeperDeployment,
// ROSProcessorDeployment, ROSPartitionCleanerCronJob, KruizeDeployment,
// KruizeDeletePartitionsCronJob, plus the Kruize partition init container.
// ROS/Kruize builders run with ROS enabled in the test cfg.

func TestBuildersApplyXLargeDeltas(t *testing.T)  // ocp worker 1000m/2000m, 2Gi/4Gi; summary cpu 1000m/2000m; ingress mem 2Gi/4Gi; ROS API stays 500m/1, 1Gi/1Gi
func TestBuilderExplicitResourcesWin(t *testing.T) // MasuDeployment with explicit {limits: cpu 3} → exactly that
func TestProfileSwitchRerenders(t *testing.T)      // same cfg: render listener at small (300Mi/600Mi), set Profile=large, render again → 2Gi/4Gi
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/resources/ -run 'Builders|ProfileSwitch|BuilderExplicit' -v`
Expected: FAIL (empty resources).

- [ ] **Step 3: Wire each listed call site** to `ProfileResources(cfg, <Workload>, <existing spec field>)`. Celery Beat's hardcoded block moves into the table (`WorkloadCeleryBeat`, values unchanged) and Beat passes an empty explicit block.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/resources/...`
Expected: PASS (fix any existing test that asserted empty resources).

- [ ] **Step 5: Coverage check**

Run: `go test ./internal/resources/... -coverprofile /tmp/cover.out && go tool cover -func /tmp/cover.out | grep -E 'profiles.go|0.0%'`
Expected: no 0% function in `profiles.go` or changed builders.

- [ ] **Step 6: Commit; update Progress row 3**

```bash
git commit -am "[COST-8095] Apply profile resource defaults in all workload builders"
```

---

### Task 4: Ingress / gateway knobs + Envoy HCM parity

**Files:**
- Modify: `api/v1alpha1/costmanagementserviceconfig_types.go` — `IngressConfig.MaxUploadSize` → `*int64` no default `Minimum=1`; new `IngressConfig.MaxMemory *int64` `Minimum=1`; new `EnvoySpec.IngressTimeout string`, `EnvoySpec.IngressPerTryTimeout string` (`json:"ingressTimeout,omitempty"` / `ingressPerTryTimeout`) with duration validation
- Modify: `internal/resources/ingress.go:69-84`, `internal/resources/route.go:14-15` and its default-annotation logic, `internal/resources/envoy.go:438-440,480,484`
- Test: `internal/resources/ingress_test.go`, `route_test.go:87-118`, `envoy_test.go`

**Interfaces:**
- Consumes: `ProfileKnobsFor` (Task 1).

- [ ] **Step 1: Write failing tests**

```go
func TestIngressEnv_ProfileUploadLimits(t *testing.T)        // small: INGRESS_DEFAULTMAXSIZE=104857600, INGRESS_MAXUPLOADMEM=33554432; large: 524288000 / 134217728
func TestIngressEnv_ExplicitUploadLimitsWin(t *testing.T)    // MaxUploadSize=ptr 1000, MaxMemory=ptr 2000 → env "1000"/"2000"
func TestGatewayAPIRouteProfileTimeout(t *testing.T)         // small → annotation "180s"; large → "600s"; explicit annotation "42s" → "42s"
func TestEnvoyYAML_IngressRouteProfileTimeouts(t *testing.T) // yaml.Unmarshal rendered config; ingress route timeout/per_try_timeout: small 180s/60s, medium 180s/180s, large 600s/300s; explicit "900s"/"400s" wins
func TestEnvoyYAML_HCMParityWithChart(t *testing.T)          // yaml.Unmarshal; HCM has no request_timeout; stream_idle_timeout 300s; common_http_protocol_options.idle_timeout 300s, max_connection_duration 600s
func TestEnvoyConfigHashChangesWithProfile(t *testing.T)     // small vs large → different envoyConfigHash
```

Add a CRD-parse test `TestCRDEnvoyTimeoutValidation` (rule/pattern present on both new fields) in `api/v1alpha1/`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/resources/ ./api/... -run 'Ingress|Route|EnvoyYAML|EnvoyConfigHash|EnvoyTimeout' -v`
Expected: FAIL / compile errors.

- [ ] **Step 3: API changes**, then `make generate && make manifests`.

- [ ] **Step 4: Wire builders** — ingress env from explicit-or-knob; route default annotation from `ProfileKnobsFor(cfg).RouteTimeout` (delete `gatewayRouteTimeoutDefault` const); Envoy template takes the two ingress timeouts as template values; HCM edited per spec §3.6.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/resources/... ./api/...`
Expected: PASS.

- [ ] **Step 6: Commit; update Progress row 4**

```bash
git commit -am "[COST-8095] Profile-driven upload limits and gateway timeouts; Envoy HCM parity with chart"
```

---

### Task 5: `status.profile` + printer column

**Files:**
- Modify: `api/v1alpha1/costmanagementserviceconfig_types.go:723-744` (add `Profile Profile \`json:"profile,omitempty"\``), printcolumn near `:755`
- Modify: `internal/controller/costmanagementserviceconfig_controller.go:188,264` (set alongside `ObservedGeneration`)
- Test: existing controller envtest file that asserts `ObservedGeneration` (find with `grep -rn ObservedGeneration internal/controller/*_test.go`)

**Interfaces:**
- Consumes: `resources.EffectiveProfile` (Task 1).

- [ ] **Step 1: Write failing test** `TestReconcileSetsStatusProfile`: CR without `spec.profile` → `status.profile == "small"`; patch to `large` → `status.profile == "large"` after reconcile.
- [ ] **Step 2: Run** `go test ./internal/controller/ -run StatusProfile -v` — FAIL.
- [ ] **Step 3: Implement**: add field + `+kubebuilder:printcolumn:name="Profile",type=string,JSONPath=\`.status.profile\``; set `cfg.Status.Profile = resources.EffectiveProfile(cfg)` at both ObservedGeneration sites. `make generate && make manifests`.
- [ ] **Step 4: Run** `make test` — PASS.
- [ ] **Step 5: Commit; update Progress row 5** — `git commit -am "[COST-8095] Report effective profile in status"`

---

### Task 6: Samples + e2e helpers

**Files:**
- Modify: `config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_production.yaml` (spec §7)
- Modify: `test/e2e/cmsc_drift_test.go:44`, `test/e2e/cmsc_pause_test.go:48,75`, `test/e2e/cmsc_helpers.go`
- Test: `api/v1alpha1/samples_test.go`

- [ ] **Step 1: Write failing test** in `samples_test.go`: `TestProductionSampleUsesLargeProfile` — production sample parses, `Spec.Profile == ProfileLarge`, and at least one explicit `resources` override remains (precedence demo).
- [ ] **Step 2: Run** `go test ./api/... -run Sample -v` — FAIL.
- [ ] **Step 3: Update sample**: `profile: large`; drop overrides that only existed for missing maps, keep the listener override as the precedence example; rewrite header comment (no COST-8095 "tracked" note, no `ha` wording); keep monitoring enabled. Check `grep -rn 'profile:' config/ scripts/ test/ hack/` for other `standard`/`ha` users and fix.
- [ ] **Step 4: e2e** — add `func desiredAPIReplicas(cfg *costv1alpha1.CostManagementServiceConfig) int32` in `cmsc_helpers.go` returning `resources.ProfileReplicas(cfg, resources.WorkloadKokuAPI, cfg.Spec.CostManagement.API.Replicas)`; use it at the three call sites. Run `go vet ./test/...` — PASS (e2e itself runs in CI Kind job).
- [ ] **Step 5: Run** `go test ./api/... && go build ./...` — PASS.
- [ ] **Step 6: Commit; update Progress row 6** — `git commit -am "[COST-8095] Production sample uses large profile; e2e reads effective replicas"`

---

### Task 7: Perf harness uses `spec.profile`

**Files:**
- Modify: `scripts/lib/perf-testing.sh:27-260` (`apply_perf_profile_config`)
- Create: `scripts/perf-testing_test.sh`
- Modify: `Makefile:118-124` (add to `test-scripts`)
- Modify: `docs/performance/OPERATOR-FINDINGS.md` (FINDING-039, -040 status)

**Interfaces:**
- Consumes: CMSC `spec.profile` enum (Task 1). Globals already used by the script: `NAMESPACE`, `CMSC_NAME`, `DRY_RUN`, `PERF_PROFILE`.

- [ ] **Step 1: Write failing script test** `scripts/perf-testing_test.sh` in the style of `scripts/deploy-test-operator_test.sh` (`assert_contains`, `DRY_RUN=true`, stub logging helpers, source the lib):
  - `PERF_PROFILE=large` → output contains `oc patch cmsc <CMSC_NAME>` with `{"spec":{"profile":"large"}}`, and contains no `oc scale`.
  - `PERF_PROFILE=baseline` → patch with `"small"`.
  - `PERF_PROFILE=medium` → replica verification expects listener 2, ocp 2, summary 2, ros-processor 2, kruize 1.
  - `PERF_PROFILE=large` → verification expects 3/3/3/3/1.
- [ ] **Step 2: Run** `./scripts/perf-testing_test.sh` — FAIL.
- [ ] **Step 3: Implement**: map `baseline|small→small`, others pass through; one `oc patch cmsc "${CMSC_NAME}" -n "${namespace}" --type merge` setting `spec.profile` and nulling the explicit replica/resources fields the old matrix touched (listener, ocp, summary, ros processor, kruize, ingress `maxUploadSize`/`maxMemory`); honour `DRY_RUN`; remove `oc scale` block; keep `_verify_replicas` with the per-profile expected numbers (spec §5), waiting for rollout. Rewrite the function header comment.
- [ ] **Step 4: Run** `./scripts/perf-testing_test.sh && make test-scripts && shellcheck scripts/lib/perf-testing.sh scripts/perf-testing_test.sh` — PASS.
- [ ] **Step 5: Update OPERATOR-FINDINGS** FINDING-039 → Resolved (COST-8095), FINDING-040 → CMSC default fixed by `small` ocp worker 512Mi/1Gi (COST-8095); gap table rows likewise.
- [ ] **Step 6: Commit; update Progress row 7** — `git add scripts/ Makefile docs/performance/OPERATOR-FINDINGS.md docs/design/profile-sizing-maps-PLAN.md && git commit -m "[COST-8095] Perf harness applies spec.profile instead of oc scale"`

---

### Task 8: Docs

**Files:**
- Modify: `docs/performance/operator-profile-crd-mapping.md` (replace "`spec.profile` in the current CRD" and "Sizing abstraction"; move `maxMemory` + Envoy timeouts out of "not yet in CRD")
- Modify: `docs/performance/sizing-guide.md` (short "Operator" note near the top)
- Modify: `docs/install/production.md`, `docs/tasks.md`, `docs/gap_analysis/BETA-PRIORITY.md` (T5), `CLAUDE.md` if it mentions profiles
- Leave historical `docs/jira/*.md` and `docs/gap_analysis/COST-76xx.md` untouched (ticket snapshots)

- [ ] **Step 1:** `grep -rn -i "profile" docs/install docs/tasks.md docs/gap_analysis/BETA-PRIORITY.md docs/performance/operator-profile-crd-mapping.md docs/performance/sizing-guide.md CLAUDE.md` — list every `standard`/`ha`/"UI only"/"not yet in CRD" mention.
- [ ] **Step 2:** Edit each to the implemented behaviour, linking `docs/design/profile-sizing-maps.md`.
- [ ] **Step 3:** Run the repo link check used in CI (see `.github/workflows/ci.yml` link-check job) locally if available; otherwise verify relative links by hand.
- [ ] **Step 4: Commit; update Progress row 8** — `git commit -am "[COST-8095] Document spec.profile tiers"`

---

### Task 9: Final verification + PR

- [ ] **Step 1:** `make generate manifests && git diff --exit-code` — no drift.
- [ ] **Step 2:** `make test` — PASS; `make test-scripts` — PASS.
- [ ] **Step 3:** `golangci-lint run ./...` and `govulncheck ./...` — clean.
- [ ] **Step 4:** Coverage on `./internal/resources/... ./api/... ./internal/controller/...` — no 0% functions in the diff.
- [ ] **Step 5:** LSP `incomingCalls` on every new exported function in `profiles.go` — each has ≥1 non-test caller (dead-code check, CLAUDE.md).
- [ ] **Step 6:** Push branch; open PR `[COST-8095] Implement spec.profile sizing maps` with summary, link to design + this plan, decision log highlights, behaviour changes (spec §5.1 "Behaviour changes"), test evidence.
- [ ] **Step 7:** Update Progress row 9 with PR link; add a Jira comment on COST-8095 linking the PR.

---

### Task 10: Cluster Bot end-to-end validation

Runs after Task 9 Steps 1-5 and before Task 9 Steps 6-7. Cluster: Cluster Bot,
3 control-plane + 3 workers. Follow
[docs/development/clusterbot-operator-pytest.md](../development/clusterbot-operator-pytest.md)
(operator in-cluster via `scripts/deploy-incluster.sh`, BYOI via
`scripts/deploy-test-cost-onprem.sh --deploy-s4 --skip-helm --skip-chart-tests`,
pytest via `scripts/run-pytest.sh`). Do not use `make run` / `make deploy`
from a laptop (CLAUDE.md). CRC (`docs/development/crc-testing.md`) only as a
fallback. Record every check's command and observed output in this task's notes.

- [ ] **Step 1:** User provides the Cluster Bot kubeconfig; confirm `oc whoami`, 3 worker nodes Ready. Build + push the branch image (`quay.io/<user>/koku-service-operator:cost-8095-<sha>`), deploy with `IMG=… ./scripts/deploy-incluster.sh cost-onprem`, deploy BYOI infra per the runbook.
- [ ] **Step 2:** Deploy operator from this branch; apply a CR **without** `spec.profile`. Expect `status.profile: small`, `oc get cmsc` shows `Profile` column; every managed pod has requests+limits per spec §5.1; celery `ocp`/`summary` 2 replicas, `download`/`refresh` 0, listener 2, envoy 2.
- [ ] **Step 3:** Patch `spec.profile: large`. Expect listener/ocp/summary roll to 3 and all pods schedule (ROS processor 3 only if ROS enabled), listener mem 2Gi/4Gi, ingress mem 2Gi/4Gi, ingress env `INGRESS_DEFAULTMAXSIZE=524288000` / `INGRESS_MAXUPLOADMEM=134217728`, route annotation `haproxy.router.openshift.io/timeout: 600s`, Envoy config ingress route `600s`/`300s`, HCM without `request_timeout`, Envoy pod rolled (config-hash changed). If pods can't schedule, record node allocatable vs requests and verify rendered Deployment specs instead.
- [ ] **Step 4:** Set explicit `spec.costManagement.listener.resources`; switch profile `large → medium`; explicit block survives unchanged, other workloads move to medium.
- [ ] **Step 5:** `oc scale deploy/<celery-worker-ocp> --replicas=5`; within one reconcile / ≤5 min drift requeue it returns to the profile value.
- [ ] **Step 6:** Run the pytest suite against the operator stack (`NAMESPACE=cost-onprem ./scripts/run-pytest.sh --no-ui`) at `small`, then the ingestion subset at `large`; record pass/fail counts. Optionally run `PERF_PROFILE=large` through the updated perf harness (Task 7) to confirm it no longer fights the operator.
- [ ] **Step 7:** Record results here, update Progress row 10, commit `[COST-8095] Plan: Cluster Bot validation results`.

#### Task 10 results (2026-10-23, Cluster Bot `chat-bot-qdbjn`, OCP 4.22.19, 3 masters + 3 workers)

Image `quay.io/martin_povolny/koku-service-operator:cost-8095-37220cf` (first pass on `cost-8095-afb6ef4`).

| Check | Result |
|-------|--------|
| CR without `spec.profile` → stored default `small` | ✅ |
| `small` replicas/resources/upload/timeouts/Envoy HCM per §5.1 | ✅ after D19 — first pass: `celery` + `cost_model` workers OOMKilled at chart limits (idle workers ~750Mi on koku `422f758`) |
| Only bundled DB/Valkey BestEffort | ✅ |
| `small → large`: listener/ocp/summary 3, memory/CPU per §5.3, ingress 500MB/128MB, route 600s, Envoy 600s/300s, Envoy rolled | ✅ 50s rollout, all pods scheduled |
| Explicit listener block survives `large → medium`; `null` clears it back to profile | ✅ |
| `oc scale` drift reverted | ✅ in 2s |
| `status.profile` / `PROFILE` column on lab without UI OAuth secret | ✅ after D20 — first pass: never set (UIReady=False is the normal lab state) |
| Perf harness `apply_perf_profile_config` (`PERF_PROFILE=large`) live | ✅ patch → status wait → rollouts → 3/3/3 verified in 57s |
| `run-pytest.sh --no-ui` @ small | 211 passed, 2 failed, 40 skipped, 0 errors (15 min). Failures: `sources/test_sources_api.py` duplicate `cluster_id` / missing `source_ref` return 201 not 400 — koku app behaviour, unrelated to sizing. Runbook baseline: 209/5/30 errors |
| `run-pytest.sh --e2e` @ large | 61 passed, 0 failed, 3 skipped (6.5 min) — upload → manifest → MASU processing |

