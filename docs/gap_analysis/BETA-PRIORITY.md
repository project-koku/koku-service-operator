# Beta Gap Priority Summary

**Audited inputs:** [COST-7678](COST-7678.md)–[COST-7685](COST-7685.md), [COST-7687](COST-7687.md)–[COST-7692](COST-7692.md) (2026-08-10; refreshed for `spec.ros.enabled` / `ROSEnabled`)
**Also considered:** [docs/tasks.md](../tasks.md) (COST-7686, COST-7693–7700 — no per-ticket gap docs yet)
**Beta bar used here:** A beta customer can install the operator, apply a BYOI `CostManagementServiceConfig` with **`ros.enabled: false`**, reach an honest Ready/Available state for the **core Cost** stack (Koku/Masu/Listener, Celery on-prem workers, RBAC, Ingress, Gateway, UI — **not ROS, not Kruize**), and not hit silent false-success or plaintext-secret API footguns. HA polish, full monitoring parity, E2E suites, and day-2 rotation can trail.

**Scope note (2026-08-10):** ROS and Kruize are **out of beta**. Name them separately in gaps (different secrets, migrate, readiness). Optional install, independent Kruize enablement, and day-2 enablement are owned by **[COST-8054](../jira/COST-8054.md)** — not by COST-7678–7692 closure.

**Code progress since first draft:** `spec.ros.enabled` (`*bool`, default `true`) + `ROSEnabled()` gate Deployments/CronJobs/migrate/Envoy ROS routes/NPs/SMs; `reconcileROSFeature` sets condition `ROSEnabled` and cleans up when flipped off. BYOI samples set `ros.enabled: false`. **Still open for Cost-only honesty:** Validation **always** requires `ros-user` / `ros-password` (not conditional; not Kruize keys); Kruize is not independently toggleable (tied to ROS today); default remains enabled unless the CR sets `false`. “Out of beta scope” means *do not treat ROS or Kruize readiness/hardening as Cost beta blockers*.

Per-ticket detail stays in the linked audits. This doc is the cross-cut ranking.

---

## How to read the ranks

| Rank | Meaning |
|------|---------|
| **P0 — Beta blocker** | Wrong/unsafe API, false Ready, or a path required for BYOI beta that is broken in-cluster |
| **P1 — Beta should-have** | Strongly improves install safety, ops, or demo credibility; ship if capacity allows |
| **P2 — Post-beta / GA** | Correctness polish, HA sizing, monitoring completeness, day-2 lifecycle |
| **Defer / accept** | Intentional or production-irrelevant; document and move on |

Cross-cutting themes are listed once (with ticket IDs), then a suggested sprint order.

---

## Cross-cutting themes (deduplicated)

### T1. Status lies / condition collisions — **P0**

Several paths report success while the underlying dependency is wrong or while another stage overwrites a failure.

| Gap | Tickets | Why beta-blocking |
|-----|---------|-------------------|
| Edge overwrites `AuthenticationReady` (`OIDCUnreachable` → `GatewayReady`) | [7684 R1](COST-7684.md), [7688 G1](COST-7688.md) | Auth failure erased same reconcile; OIDC failure is non-blocking so Edge still runs; Available/Ready can look healthy |
| S3 Secret keys never checked; `StorageReady=True` on user path | [7684 G2](COST-7684.md), [7683 R1](COST-7683.md) | Bad/missing credentials still “ready” |
| No S3 connectivity probe | [7684 G1](COST-7684.md) | Endpoint/TLS/network failures invisible until upload fails |
| OIDC HTTP probe accepts any status &lt; 500 | [7684 G3](COST-7684.md) | 401/404 still `AuthenticationReady=True` |
| External DB secret validation omits `kruize-user` / `kruize-password` | [7684 R3](COST-7684.md) | **→ [COST-8054](../jira/COST-8054.md)** (Kruize); not a Cost beta blocker |
| External DB secret validation always requires `ros-user` / `ros-password` | [7684](COST-7684.md), [8054](../jira/COST-8054.md) | **→ [COST-8054](../jira/COST-8054.md)** — blocks true Cost-only BYOI even when `ros.enabled=false` |
| RBAC Deployments applied but never gated; no `RBACReady` | [7689 G2](COST-7689.md) | Core can go Available while RBAC is down |
| `UIReady` / Ingress / workers not truly readiness-gated | [7690 R2](COST-7690.md), [7688 R4](COST-7688.md), [7687](COST-7687.md) | Weaker than RBAC/S3; still erodes trust in status |

**Beta minimum:** fix Auth condition collision + S3 secret-key check + tighten OIDC success + RBAC readiness gate.

---

### T2. API safety & validation — **P0 / P1**

| Gap | Tickets | Rank |
|-----|---------|------|
| `RealmUser.Password` in Spec (plaintext in etcd) | [7678 R1](COST-7678.md) | **P0** (design §4a; do not ship beta API with this) |
| Sensitive / unconstrained `Env map[string]string` | [7678 R1](COST-7678.md) | **P1** (tighten before GA; warn in beta docs) |
| CEL / OpenAPI validation (ports, `sslMode`, BYOI required-when-not-deployed, etc.) | [7678 G2](COST-7678.md), [7679 G3](COST-7679.md) | **P1** |
| Admission webhooks (`defaults.go` / `validation.go`) | [7678 G1](COST-7678.md) | **P1** (OpenAPI/CEL first; webhook for rules CEL cannot express) |

---

### T3. BYOI / discovery install paths — **P0 / P1**

| Gap | Tickets | Rank |
|-----|---------|------|
| Missing `objectbucket.io` RBAC → OBC discovery Forbidden | [7683 G1](COST-7683.md) | **P0** if ODF/OBC is a beta path; else P1 with “user secret only” documented |
| BYOI blocked when no default StorageClass (even with no bundled PVCs) | [7682 R1](COST-7682.md) | **P0** for pure BYOI clusters |
| Discovered StorageClass unused by PVC builders | [7682 R2](COST-7682.md) | **P1** (bundled/dev path) |
| Bucket name not in `DiscoveredS3` | [7683 G2](COST-7683.md) | **P2** |
| User CAs not merged into combined trust bundle | [7691 G3](COST-7691.md) | **P1** if beta needs custom Kafka/IdP CAs |

---

### T4. Wrong default workload footprint — **P0**

| Gap | Tickets | Rank |
|-----|---------|------|
| SaaS-only Celery queues (`hcs`, `subs_extraction`, `subs_transmission`) still deploy at `replicas: 1` | [7687 G2](COST-7687.md) | **P0** — wasted resources + wrong on-prem shape |

---

### T5. Profile / HA sizing (many tickets, one deliverable) — **P2 for beta**

`spec.profile` exists; **no code reads it**. Called out as G1/G4 across [7678](COST-7678.md), [7687](COST-7687.md), [7689](COST-7689.md), [7690](COST-7690.md); also [tasks.md](../tasks.md) COST-7686 / 7693.

**Beta stance:** ship `profile: standard` only; document that `ha` is reserved. Implement one shared sizing map post-beta (or late beta if HA demos are required).

---

### T6. Platform hardening (NetworkPolicy / SA / Routes) — **P1 / P2**

| Gap | Tickets | Rank |
|-----|---------|------|
| ROS workers omit `ServiceAccountName` despite ROS SA existing | [7691 G2](COST-7691.md) | **→ [COST-8054](../jira/COST-8054.md)** (ROS) |
| Dedicated SAs for gateway, ingress, RBAC, UI | [7691 G2](COST-7691.md) | **P2** (or document shared-SA model) |
| NetworkPolicies missing for UI, Masu, Listener, workers, … | [7691 G1](COST-7691.md) | **P1** for secured beta (UI + Masu/Listener); ROS and Kruize NPs → **[COST-8054](../jira/COST-8054.md)** |
| Ready not gated on Route admission | [7691 G4](COST-7691.md) | **P2** |
| Envoy ConfigMap changes do not roll pods | [7688 R3](COST-7688.md) | **P1** |
| Envoy `/api/ingress/` timeouts too short vs chart | [7688 R1](COST-7688.md) | **P1** (upload demos will flake) |

---

### T7. Reconciler skeleton / ops controls — **P1 / P2**

| Gap | Tickets | Rank |
|-----|---------|------|
| Pause/resume annotation | [7680 G1](COST-7680.md) | **P1** (support escape hatch) |
| Ready Event fires repeatedly (phase reset bug) | [7680 G3](COST-7680.md) | **P1** |
| Wait/probe exponential backoff (error path already CRT-limited) | [7680 G2](COST-7680.md) | **P2** |
| Bundled PostgreSQL StatefulSet bypasses SSA | [7681 G1](COST-7681.md) | **Defer** (dev-only path) |

---

### T8. Monitoring & samples — **P1 / P2**

| Gap | Tickets | Rank |
|-----|---------|------|
| App ServiceMonitors largely non-functional (ports/Services/NP) | [7692 G2](COST-7692.md) | **P2** unless beta promises in-cluster scrape |
| Custom operator metrics + ticket alert names | [7692 G1/G3](COST-7692.md) | **P2** |
| Missing Event reasons (`PhaseChanged`, `DependencyFailed`, …) | [7692 G4](COST-7692.md), [7680 G3](COST-7680.md) | **P2** |
| Minimal + HA sample CRs | [7679 G1/G2](COST-7679.md) | **P1** for beta docs/demos |
| Legacy stale CRD file still in tree | [7679 R1](COST-7679.md), [7678 R2](COST-7678.md) | **Resolved** (file removed; CRC path uses `make install`) |
| BYOI template keeps `kruize:` block while `ros.enabled: false` | [7679 R3](COST-7679.md) | **P2** (docs clarity) |

---

### T9. Outside this gap-analysis set (from `tasks.md`) — still beta-relevant

| Ticket | Summary | Suggested beta rank |
|--------|---------|---------------------|
| [COST-7695](https://redhat.atlassian.net/browse/COST-7695) | OLM bundle | **P0** if beta is delivered via OLM; else P1 |
| [COST-7686](https://redhat.atlassian.net/browse/COST-7686) | App services — profile sizing + 5m readiness → Degraded | Profile **P2**; readiness timeout **P1** |
| [COST-7693](https://redhat.atlassian.net/browse/COST-7693) | Upgrade/scaling/rollback | **P2** (basic image-tag migrate exists) |
| [COST-7694](https://redhat.atlassian.net/browse/COST-7694) | Secret rotation annotation | **P2** (day-2); CA merge overlaps [7691 G3](COST-7691.md) **P1** |
| [COST-7696](https://redhat.atlassian.net/browse/COST-7696)–[7699](https://redhat.atlassian.net/browse/COST-7699) | Bundle CI / E2E / OpenShift CI | **P2** for beta; **P1** if beta needs automated install proof |
| [COST-7700](https://redhat.atlassian.net/browse/COST-7700) | Install/config guides | **P1** (beta customers need a BYOI quickstart with `ros.enabled: false`) |

Tickets marked ✅ in `tasks.md` that gap audits found incomplete: **7683, 7684, 7687, 7688, 7689, 7690, 7691, 7692** (docs drift — do not treat tracker checkmarks as beta-ready).

---

## Ranked backlog (recommended beta order)

### P0 — Do before beta

1. **Auth status honesty** — Split/compose gateway vs OIDC; stop Edge clearing `OIDCUnreachable` ([7688 G1](COST-7688.md) / [7684 R1](COST-7684.md)).
2. **S3 credential validation** — `checkSecretKeys` on `objectStorage.secretName`; fail `StorageReady` on missing keys ([7684 G2](COST-7684.md)).
3. **Remove/replace `RealmUser.Password`** in Spec ([7678 R1](COST-7678.md)).
4. **Exclude SaaS Celery queues** by default (`replicas: 0` or omit) ([7687 G2](COST-7687.md)).
5. **BYOI StorageClass soft-fail** when no bundled PVC consumers ([7682 R1](COST-7682.md)).
6. **`objectbucket.io` RBAC** if OBC is in-scope for beta ([7683 G1](COST-7683.md)); otherwise document user-secret-only.
7. **OIDC probe** — require 2xx + parse JWKS `keys` ([7684 G3](COST-7684.md)).
8. **RBAC readiness gate + condition** before core Available ([7689 G2](COST-7689.md)).
9. **OLM bundle** ([COST-7695](../tasks.md)) if that is the beta install vehicle.

### P1 — Ship with beta if possible

10. OpenAPI/CEL markers (ports, `sslMode`, conditional BYOI required fields) ([7678 G2](COST-7678.md)).
11. Minimal sample CR + remove/stop applying legacy CRD ([7679 G1/R1](COST-7679.md) / [7678 R2](COST-7678.md)); point CRC docs at `make install` or the canonical base only.
12. Pause annotation ([7680 G1](COST-7680.md)) + fix Ready Event spam ([7680 G3](COST-7680.md)).
13. Envoy ConfigMap checksum rollout + ingress timeouts ([7688 R3/R1](COST-7688.md)).
14. User/proxy CA merge into combined bundle ([7691 G3](COST-7691.md) / COST-7694 overlap).
15. S3 connectivity probe (ListBuckets/HeadBucket) ([7684 G1](COST-7684.md)).
16. NetworkPolicies for UI + Masu/Listener (start of [7691 G1](COST-7691.md); skip ROS and Kruize).
17. App readiness timeout → Degraded for **core** Deployments only (COST-7686 note in tasks.md).
18. BYOI install/config quickstart: `ros.enabled: false`, ROS/Kruize unsupported in beta (COST-7700); remaining optionality → COST-8054.
19. Admission webhook scaffold for rules CEL cannot express ([7678 G1](COST-7678.md)).
20. **Conditional `ros-*` Validation keys** when `ros.enabled=false` ([COST-8054](../jira/COST-8054.md)) — required for honest Cost-only BYOI secrets (otherwise customers must still provision unused ROS DB keys).

### P2 — After beta / toward GA (Cost core)

21. Shared **profile sizing maps** for Cost workloads + wire ([7678 G4](COST-7678.md) + 7686/87/89/90/93). ROS/Kruize map rows → COST-8054.
22. HA sample + `profile: ha` behavior ([7679 G2](COST-7679.md)).
23. Monitoring: real **Cost** scrape targets, operator metrics, alert parity ([7692](COST-7692.md)). ROS/Kruize scrapes → COST-8054.
24. Full NetworkPolicy / dedicated SA matrix for **core** ([7691](COST-7691.md)). ROS/Kruize matrix → COST-8054.
25. Route admission gating ([7691 G4](COST-7691.md)); UI Route global-domain fallback ([7691 R4](COST-7691.md)); `Available` vs `StorageReady` policy ([7683 R2](COST-7683.md)).
26. Wait-path exponential backoff ([7680 G2](COST-7680.md)); richer Events ([7692 G4](COST-7692.md)).
27. Secret rotation annotation (COST-7694); upgrade rollback / rolling strategy (COST-7693).
28. E2E + OpenShift CI (COST-7697–7699); tighten `Env` maps ([7678 R1](COST-7678.md)).
29. `dataRetentionMonths` → `RETAIN_NUM_MONTHS` wired in `KokuCommonEnv` with Go-level fallback to `4` ([7678 G3](COST-7678.md) closed).

### → [COST-8054](../jira/COST-8054.md) — ROS and Kruize (not Cost beta)

Track separately; do not conflate the two components. **Partial progress already in-tree** (`spec.ros.enabled` gate + cleanup + BYOI samples).

30. Finish Cost-only honesty: conditional `ros-*` / `kruize-*` Validation keys; independent Kruize enablement (or document “Kruize requires ROS”); day-2 enable without reinstall.
31. Wire ROS Processor/Poller/Housekeeper to ROS SA ([7691 G2](COST-7691.md)); ROS NPs.
32. Kruize DB secret keys in Validation ([7684 R3](COST-7684.md)); Kruize SM port/`metrics` ([7692](COST-7692.md)).
33. ROS processor/poller Services + scrape; ROS/Kruize profile sizing rows; optional ROS bucket discovery ([7683](COST-7683.md)).

### Accept / defer

| Item | Rationale |
|------|-----------|
| [7681 G1](COST-7681.md) Bundled PG StatefulSet non-SSA | Dev-only; production is BYOI |
| [7685](COST-7685.md) Migration AC | Done for ticket; ROS migrate skip when `ros.enabled=false` **already landed** |
| [7682](COST-7682.md) ticket AC | Done; only related risks above |
| ROS migrate / workload skip via `spec.ros.enabled` | Landed; remaining Cost-only gaps (Validation keys, Kruize independence) → [COST-8054](../jira/COST-8054.md) |
| ROS and Kruize hardening gaps (SA, NP, secret keys, SM) | Owned by [COST-8054](../jira/COST-8054.md) |
| File-split CRD types, phase rename, flat spec, `deploy` flags | Intentional ([design-vs-jira.md](../design/design-vs-jira.md)) |

---

## COST-8054: `spec.ros.enabled` landed; Cost-only path still incomplete

**Landed (interim Option B partial):**

| Behavior | Status |
|----------|--------|
| Skip `ROSMigrationJob` when `ros.enabled=false` | Done (`reconcileMigration` + `ROSEnabled()`) |
| Skip ROS/Kruize Deployments, CronJobs, Envoy ROS routes, Kruize NP/SM | Done (gated apply + `reconcileROSFeature` cleanup) |
| `ROSEnabled` condition | Done |
| BYOI samples default `ros.enabled: false` | Done |
| Default when unset | Still **enabled** (`+kubebuilder:default:=true`) |

**Still owned by [COST-8054](../jira/COST-8054.md):**

| Gap | Impact |
|-----|--------|
| Validation always requires `ros-user` / `ros-password` | Cost-only BYOI secrets cannot omit ROS keys |
| No independent Kruize flag (tied to ROS) | Product optionality incomplete |
| Day-2 enable polish / docs | Full AC of 8054 |
| Hardening (SA, NP, SM, profile rows, bucket discovery) | Post-beta ROS/Kruize quality |

**Beta posture today:** document and sample `ros.enabled: false`; do not gate Available on ROS/Kruize; treat unused `ros-*` secret keys as a known COST-8054 footgun until Validation is conditional.

S3/OBC items stay relevant either way — Ingress/uploads still need object storage; only the default OBC name `ros-data-ceph` is ROS-flavored.

---

## Ticket rollup (audited set)

| Ticket | Verdict from audit | Highest open gap for beta |
|--------|--------------------|---------------------------|
| [COST-7678](COST-7678.md) | Not done | Password in Spec (R1 / P0); validation/webhooks (P1) |
| [COST-7679](COST-7679.md) | Not done | Minimal sample + legacy CRD (P1) |
| [COST-7680](COST-7680.md) | Not done | Pause + Ready Event bug (P1); wait-path backoff Partial (P2) |
| [COST-7681](COST-7681.md) | Largely done | Bundled SSA — defer |
| [COST-7682](COST-7682.md) | Done | BYOI StorageClass soft-fail (P0) |
| [COST-7683](COST-7683.md) | Not fully done | OBC RBAC (P0 if OBC in scope) |
| [COST-7684](COST-7684.md) | Not fully done | S3 keys + Auth overwrite + OIDC (P0); always-required `ros-*` keys + Kruize keys R3 → [COST-8054](../jira/COST-8054.md) |
| [COST-7685](COST-7685.md) | Done | Tests only (G1); ROS migrate skip already gated by `ros.enabled` |
| [COST-7687](COST-7687.md) | Not fully done | SaaS queue exclusion (P0); ROS/Kruize workloads gated when disabled; sizing → [COST-8054](../jira/COST-8054.md) |
| [COST-7688](COST-7688.md) | Largely done | Auth condition collision (P0) |
| [COST-7689](COST-7689.md) | Not done | RBAC readiness (P0) |
| [COST-7690](COST-7690.md) | Not fully done | Profile sizing (P2); Related `UIReady` ≠ pods Ready (T1 trust) |
| [COST-7691](COST-7691.md) | Not done | CA merge + core NPs (P1); ROS/Kruize SA/NP → [COST-8054](../jira/COST-8054.md) |
| [COST-7692](COST-7692.md) | Not done | Monitoring completeness (P2); Kruize SM only when `ROSEnabled`; ROS/Kruize SM → [COST-8054](../jira/COST-8054.md) |
| [COST-8054](../jira/COST-8054.md) | In progress (partial) | Conditional Validation keys; independent Kruize; day-2; hardening gaps |

No gap doc yet for **COST-7686** (app services) — treat readiness timeout as P1 and profile sizing as part of T5.

---

## What “beta done” looks like

- BYOI CR applies without spurious Discovery failure (no default SC required when unused).
- Bad S3/OIDC/DB secrets surface as **False** conditions; Edge does not clear OIDC failures.
- No plaintext realm password field on the CR.
- On-prem Celery footprint excludes SaaS queues by default.
- RBAC API readiness participates in core Available.
- Install path documented (and OLM-bundled if that is the channel); samples/docs use `ros.enabled: false` and call ROS/Kruize unsupported in beta (remaining optionality → [COST-8054](../jira/COST-8054.md)).
- Ideally: Cost-only Validation does not require `ros-*` keys (COST-8054 P1 item above).
- ROS and Kruize hardening, `profile: ha`, full NetworkPolicy matrix, scrapable metrics, secret rotation, and E2E remain explicitly out of Cost beta scope unless product expands the bar.
