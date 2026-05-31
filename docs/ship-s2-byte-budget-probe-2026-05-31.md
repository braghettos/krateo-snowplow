# S.2 byte-budget empirical probe — 2026-05-31

Cache-developer. Captured against `gke_neon-481711_us-central1-a_cluster-1` at 50K production scale. Discharges PM Condition 1 of Ship S.2 (`docs/ship-s2-pm-gate-verdict-2026-05-31.md`).

## Path: A (direct cluster-LIST probe + apistage-shape simulation)

Path A was sufficient because the apistage Put writes `RawJSON: rawEnvelope` **verbatim** (cluster_list.go:312-319). The cached envelope shape is the raw apiserver wire LIST envelope, not a projected subset. The refresh-bound gate (design §7.2) reads `len(entry.RawJSON)`. Therefore `kubectl get --raw <cluster-LIST-path> | wc -c` is byte-identical to what S.2 would store.

Path B (candidate-binary probe) not needed — would have produced the same number at the cost of a GKE deploy + revert.

## RA → GVR mapping (the cells S.2 would store)

Only RAs whose stage matches the design §4.4 collapse pattern ("iterator over `.namespaces` + per-iter LIST against a UAF-resolved GVR") qualify. Live enumeration via `kubectl get restaction -n krateo-system`:

| RA | iterator path template | derived GVR |
|---|---|---|
| `compositions-panels` | `/apis/widgets.templates.krateo.io/v1beta1/namespaces/<ns>/panels` | `widgets.templates.krateo.io/v1beta1/panels` |
| `blueprints-panels`   | `/apis/widgets.templates.krateo.io/v1beta1/namespaces/<ns>/panels` | `widgets.templates.krateo.io/v1beta1/panels` |

**KEY: both qualifying RAs target the SAME GVR (`panels`).** S.2 produces **ONE** cluster-LIST apistage cell on this cluster, shared across both RAs (same `contentKey` from `cluster_list.go:312` — derived from GVR + empty ns + empty name).

The earlier 0.30.212 design notes that mentioned "buttons / markdowns / forms / flowcharts / eventlists" did NOT correspond to S.2's collapse logic — those are per-widget Kind cells, not cluster-LIST cells. S.2's `deriveTargetGVRForClusterListFromUAFStage` derives **one** GVR per qualifying stage, from `apiCall.Path` + the sibling stage's `userAccessFilter.resource`. The `panels` resource hosts ALL widget kinds (each panel object carries `metadata.labels["widget.krateo.io/type"]` or equivalent); they share one `PanelList` envelope.

## Per-GVR results

| GVR | items | wire bytes | source command |
|---|---|---|---|
| `widgets.templates.krateo.io/v1beta1/panels` | **33,758** (first page; `remainingItemCount=0` → fully loaded) | **96,636,785** (probe 2/3, stable) | `kubectl get --raw "/apis/widgets.templates.krateo.io/v1beta1/panels"` |

Three back-to-back probes returned `96,545,209`, `96,636,785`, `96,636,785` — variance ≤0.1% (one new panel created mid-probe). **The cluster-LIST envelope for the single S.2 cell on this cluster is ~92 MiB.**

### Stripped-shape comparison (informational only — S.2 does NOT strip RawJSON)

The cache's `Items` field has `managedFields` stripped (apistage.go:158, cluster_list.go:588) but **`RawJSON` is stored verbatim** (cluster_list.go:314 — `RawJSON: rawEnvelope`).

| Shape | bytes | delta |
|---|---|---|
| Raw wire envelope (what S.2 stores in `RawJSON`)              | 96,636,785 | — |
| Stripped-managedFields envelope (informational; NOT stored) | 69,142,524 | -28.4% |
| `kubectl.kubernetes.io/last-applied-configuration` annotation | ~3 bytes  | negligible |

The 28% managedFields share is structural — if a future ship moves to a strip-on-Put envelope, the worst-case envelope falls to ~69 MB. **For S.2 as currently designed, the gate sees 96.6 MB.**

## Worst-case single GVR envelope

**96,636,785 bytes (~92.2 MiB)** — the `panels` cluster-LIST.

## Sum across candidate set

**96,636,785 bytes** — both qualifying RAs share the same cell. The cap is per-cell, so the sum is 1×, not 2×.

## Recommended `CLUSTER_LIST_REFRESH_BYTE_BUDGET` default

`min(empirical_worst_case × 0.75, 100 MiB)` = `min(72,477,588, 104,857,600)` = **72,477,588 bytes ≈ 69 MiB**

Rounded to a round constant: **`72 MiB = 75,497,472`** (cleaner Go literal `72 * 1024 * 1024`).

**Rationale:**
- 0.75 × worst-case is below the 100 MiB ceiling, so 0.75× is the binding constraint.
- The per-NS apistage entries S.2 must NOT mis-classify are **~2 MB** (probed: `panels` in `bench-ns-01` = 2,173,124 bytes; `bench-ns-50` ≈ 150 bytes for empty NS; average 1,932,735 bytes). **~35-50× separation** between normal per-NS entries and the cluster-LIST cell. The gate fires reliably on the cluster-LIST cell with zero false-positive risk on normal entries.
- Memory `feedback_capacity_caps_empirical_per_entry_cost` (0.30.151 was 180× off design-time estimate). This default is derived from 3 live probes against the actual production cluster, not from intuition.

**The design draft default of 50 MiB (cluster_list.go design line 339) would also fire on `panels` (50 < 92), and gives even more headroom against normal entries (~25× separation). 50 MiB is acceptable but slightly aggressive.** Recommendation: bump to **72 MiB** to match the empirical 0.75× rule the PM specified.

## Caveats

1. **`RawJSON` is the raw wire envelope, NOT a projected/stripped form.** The cache stores byte-identical apiserver wire bytes. The 28% managedFields share is in RawJSON. Design §7.2's gate predicate `len(entry.RawJSON) > CLUSTER_LIST_REFRESH_BYTE_BUDGET` therefore sees the 96.6 MB number, not 69 MB.
2. **No gzip applied at apistage layer.** Phase B 0.30.185 attempted per-cohort gzip; Fix A (0.30.194) removed it (`apistage_cohort_memo.go:45,220`). Today's L1 stores uncompressed bytes. Future ship could revisit; out of scope for S.2.
3. **Cluster-LIST is one cell, not N.** Both qualifying RAs (`compositions-panels` + `blueprints-panels`) collapse to the same `(panels GVR, empty ns, empty name)` content key. The gate fires per-cell, so it fires **once** across the entire compositions-panels + blueprints-panels surface — exactly the design intent.
4. **No other GVRs qualify on this cluster.** `sidebar-nav-menu-items` (`navmenuitems`, 5 KB), `all-routes` (`routes`, 3 KB), `blueprints-list` (`compositiondefinitions`, 2 KB) all have stages where either (a) the iterator is not `.namespaces` or (b) the resulting envelope is far below the gate. Re-probe at scaling moments — but no other widgets/CRD GVRs are likely to host 30K+ items.
5. **First-page completeness.** The probed envelope `remainingItemCount=0` — apiserver returned the entire 33,758-item list in one HTTP response. Snowplow's `dispatchViaInformer` (cluster_list.go:252-253) returns the same envelope shape via the informer pivot, also unpaginated. No `?continue=` pagination concerns for S.2's collapse path.
6. **GVR-cluster-LIST does NOT carry `metadata.namespace` index.** Verified at the wire (`design §2.4`): every item carries its own `metadata.namespace` field. `CohortNSACL.populateMemoFromKeptNames` (apistage_cohort_memo.go:298-309) walks `it.GetNamespace()` — same field, same call. Gate at the right layer.

## Provenance

Artifacts captured in `/tmp/s2-probe/`:
- `panels.json` — raw cluster-LIST envelope (96,545,209 bytes, first probe).
- `panels_stripped.json` — managedFields-stripped envelope (69,142,524 bytes, informational).
- `panels_per_ns.json` — per-NS LIST (`bench-ns-01`, 2,173,124 bytes).
- `routes.json`, `navmenuitems.json`, `compositiondefinitions.json` — sister-GVR baselines (all <6 KB; not gate targets).

Probes executed at 2026-05-31 against `gke_neon-481711_us-central1-a_cluster-1` (context verified per `feedback_kubectl_verify_gke_context`).

## Ledger row note

Append to S.2 ledger row when ship lands:
- `byte_budget_probe_artifact = docs/ship-s2-byte-budget-probe-2026-05-31.md`
- `recommended_cluster_list_refresh_byte_budget_default = 72 * 1024 * 1024 (72 MiB)`
- `empirical_worst_case_envelope_bytes = 96,636,785`
- `qualifying_RAs_on_50K_cluster = [compositions-panels, blueprints-panels]`
- `cluster_list_cells_produced = 1 (shared panels GVR cell)`

— cache-developer, 2026-05-31
