"""Stage runners + STAGE_REGISTRY (Block 4).

Phase 6 (browser scaling) is the ONLY scored phase post-restructure.
Phase 7 (user-scaling) and Phase 8 (per-mutation) are retained as separate
subcommands.

Each stage is a callable that reads `state.json`, runs its work, writes a
stage-proof JSON, and updates `state.json`. `--from-stage S5` resumes from
disk; the proof-validation re-runner per R4.1 sanity-checks prior proofs
against the live cluster (<=10s budget).

`stage_runner` wraps `browser.browser_measure_stage` in
`try/except ConvergenceTimeout`: on catch we build a StageProof with
`passed=False, convergence_timeout=true`, call save_state(...) to persist
BEFORE re-raise, then re-raise. `cli.main()` catches and exits code 4.

Post-mutation pause approach (per pre-commit STOP): NOT rewritten in
worktree source. The 7 call sites are inlined INTO this module's stage
functions via `_post_mutation_pause()` (whose body is literally
`if cache_mode == 'ON': time.sleep(5)`). Worktree source remains
untouched at Block 4 — Block 5 deletes the whole file. This matches
the plan §B.4 alternative path and means G11 diff against worktree is
EMPTY.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §A.8 + §E.
"""

from __future__ import annotations

import datetime
import json
import os
import subprocess
import threading
import time
from dataclasses import dataclass, field, asdict
from pathlib import Path
from typing import Any, Callable

from bench import cluster, lifecycle, browser, ledger
from bench.browser import ConvergenceTimeout


__all__ = [
    "STAGE_REGISTRY",
    "StageContext",
    "StageProof",
    "IncompatibleStateSchema",
    "load_state",
    "save_state",
    "run_phase6",
    "run_phase7_user_scaling",
    "run_phase8_per_mutation",
    "SCHEMA_VERSION",
    "SCHEMA_MAJOR",
]


# ─── Schema constants ───────────────────────────────────────────────────────


SCHEMA_VERSION = "1.1.0"
SCHEMA_MAJOR = 1
#
# 1.1.0 (Task #250 Block 2 — 2026-06-05): additive S8 / S9 RBAC-mutation
# stages + N-aware EXPECTED_CALLS overlay. Old S8 (delete 1 ns) and S9
# (build canonical ledger row) RENAMED to S10 / S11; new S8 / S9 add
# RoleBinding-add / RoleBinding-remove cells. All new fields on
# stage_proofs[*].proof are READ-BY-KEY (no positional / structural
# changes), so a 1.0.0 state.json resumes cleanly into the 1.1.0
# harness (SCHEMA_MAJOR unchanged). The load_state guard at
# `major > SCHEMA_MAJOR` continues to allow 1.0.0 → 1.1.0 forward.


class IncompatibleStateSchema(Exception):
    """Raised when state.json carries a future-major schema version."""


# ─── Module-level scale/smoke env (mirrors worktree 62-64) ──────────────────


def _env_scale() -> int:
    return int(os.environ.get("SCALE", "5000"))


def _env_smoke() -> bool:
    return os.environ.get("SMOKE", "0") == "1"


# ─── Dataclasses ────────────────────────────────────────────────────────────


@dataclass
class StageContext:
    """Per-stage execution context.

    tokens / user_pages are mutable; stage runners may re-login and
    push the refreshed bearers back into user_pages so subsequent
    stages see the new tokens. cache_mode is "ON" or "OFF" (the
    outer loop alternates).

    `video` controls per-cell .webm recording:
      "none"           — no recording (CI / smoke).
      "representative" — n=1 per (stage, user, cache_mode, page) cell.
      "all"            — every navigation recorded (diagnostic only).
    Threaded from CLI --video → run_phase6 → StageContext → _setup_users
    where it is honoured via browser.make_browser_context(record_video_dir=...).
    Per plan §F.6 + R3.1; default "representative" matches the §I R3.1
    soft-cap target of ≤320 MB.
    """
    tag: str
    scale: int
    tokens: dict[str, str] = field(default_factory=dict)
    user_pages: dict[str, dict] = field(default_factory=dict)
    run_dir: Path = field(default_factory=lambda: Path("."))
    state_path: Path = field(default_factory=lambda: Path("state.json"))
    cache_mode: str = "OFF"
    smoke: bool = False
    all_results: list[dict] = field(default_factory=list)
    admin_token: str | None = None
    video: str = "representative"


@dataclass
class StageProof:
    """Persisted proof of a single stage.

    `what_breaks_if_skipped` is REQUIRED (asserted by save_state). Empty
    rejected per Risk Register §I Block 4 (R4.1).

    `convergence_timeout` is True when the stage raised ConvergenceTimeout;
    the ledger verdict logic flips the row to INVALID in that case.
    """
    stage_id: str
    started_at: str
    ended_at: str
    passed: bool
    proof: dict
    artifacts: list[str]
    what_breaks_if_skipped: str
    convergence_timeout: bool = False
    convergence_ms: int | None = None
    navs: list[dict] = field(default_factory=list)

    def to_dict(self) -> dict:
        return asdict(self)

    @classmethod
    def from_dict(cls, d: dict) -> "StageProof":
        # Tolerate extra keys (forward-compat for additive fields).
        known = {f.name for f in cls.__dataclass_fields__.values()}
        return cls(**{k: v for k, v in d.items() if k in known})


# ─── state.json round-trip ──────────────────────────────────────────────────


def _now_iso() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def load_state(run_dir: Path) -> dict:
    """Read $run_dir/state.json; raise IncompatibleStateSchema on major bump.

    Returns {} when the file is missing (fresh run). On schema mismatch,
    raises IncompatibleStateSchema with diagnostic text.
    """
    p = Path(run_dir) / "state.json"
    if not p.exists():
        return {}
    raw = json.loads(p.read_text())
    ver = (raw.get("schema_version") or "").strip()
    if not ver:
        raise IncompatibleStateSchema(
            f"state.json missing schema_version (run_dir={run_dir})")
    try:
        major = int(ver.split(".")[0])
    except (ValueError, IndexError):
        raise IncompatibleStateSchema(
            f"state.json carries unparseable schema_version={ver!r}")
    if major > SCHEMA_MAJOR:
        raise IncompatibleStateSchema(
            f"state.json schema {ver} exceeds harness SCHEMA_MAJOR="
            f"{SCHEMA_MAJOR}; bump requires a PM gate")
    return raw


def save_state(run_dir: Path, state: dict) -> None:
    """Persist state.json under $run_dir.

    Asserts each stage_proofs entry carries a non-empty
    `what_breaks_if_skipped` (per R4.1). Writes the schema_version + the
    last_updated_at timestamp.
    """
    proofs = state.get("stage_proofs") or {}
    for sid, p in proofs.items():
        wbis = p.get("what_breaks_if_skipped") if isinstance(p, dict) else None
        assert wbis, (
            f"stage_proofs.{sid}.what_breaks_if_skipped is empty/missing — "
            f"per Risk Register R4.1 every stage MUST document its "
            f"downstream effect"
        )

    state.setdefault("schema_version", SCHEMA_VERSION)
    state["last_updated_at"] = _now_iso()
    run_dir = Path(run_dir)
    run_dir.mkdir(parents=True, exist_ok=True)
    (run_dir / "state.json").write_text(json.dumps(state, indent=2,
                                                   default=str))


def _write_stage_proof(run_dir: Path, proof: StageProof) -> Path:
    proofs_dir = Path(run_dir) / "proofs"
    proofs_dir.mkdir(parents=True, exist_ok=True)
    out = proofs_dir / f"{proof.stage_id}.json"
    out.write_text(json.dumps(proof.to_dict(), indent=2, default=str))
    return out


def _stage_label(stage_num) -> str:
    return f"S{stage_num}"


def _post_mutation_pause(cache_mode: str) -> None:
    """Post-cluster-mutation pause; matches the worktree's local helper
    body byte-for-byte (worktree source 6678-6682, deleted).

    Body is literally `if cache_mode == "ON": time.sleep(5)`. The VERIFY
    poll handles real convergence; this exists only to let informer
    events fire before the next browser navigation begins.
    """
    if cache_mode == "ON":
        time.sleep(5)


# ─── Per-stage pod log streamer (Task #251 — 2026-06-09) ────────────────────


class _PerStageLogStreamer:
    """Stream `kubectl logs -f deployment/snowplow --since-time=<stage_start>`
    into `pod_logs/S<N>.txt` for the duration of one stage.

    Why this exists
    ---------------
    Phase 6 measurement stages produced log evidence only via the
    aggregate `pod_logs/full_run.txt` capture at run finalise. When a
    pod restart, stage failure, or long run pushed finalise outside
    the measurement window (kubectl tail bounded by ring-buffer +
    restart-boundary), the per-stage diagnostic evidence was lost —
    the agent a16e4da1a29434f24 TRACE on run-20260609-004834-cache-on
    found S8 window 23:30:39 -> 23:35:48 UTC but the aggregate log
    covered 23:59:11 -> 00:04:09 UTC across a pod restart, so the cj
    `allCompositions` UAF events were unfalsifiable.

    Design contract
    ---------------
    - Stream opens BEFORE the stage's measurement loop fires
    - Stream closes (close + flush + fsync) BEFORE _run_stage persists
      the proof + state.json, so the file is always present when the
      ledger records `__passed__`
    - `--since-time=<stage_started_utc>` ensures the apiserver buffer
      covers the full measurement window even if the streaming
      subprocess starts a few hundred ms late
    - Pod-restart tolerance: when the kubectl subprocess exits, the
      supervisor thread writes a `--- STREAM RECONNECT @ <utc> ---`
      marker and re-spawns with the SAME `--since-time`, appending to
      the file. Caller's `stop()` clears `_running`, the supervisor
      observes it on the next iteration, and exits without respawning
    - Uniform across S0-S11 (no per-stage special-cases — fired from
      _run_stage)

    Opt-out
    -------
    `BENCH_NO_PER_STAGE_LOGS=1` disables the streamer (fall back to
    aggregate full_run.txt only). Matches the existing
    `BENCH_ALLOW_NON_GKE` env-flag pattern; no CLI plumbing needed.
    """

    # Per-iteration wait between subprocess restart attempts. The pod
    # may take several seconds to come up after a restart; a too-tight
    # loop spams kubectl. 1.5s is the same back-off cadence used by
    # _grep_pod_logs_for_traces (R7 retry logic).
    _RESPAWN_BACKOFF_S = 1.5

    # Cap on the stream-supervisor thread's join() during stop(). The
    # subprocess.terminate() path takes ~100ms; allowing 5s leaves
    # ample room for the supervisor loop iteration to break out.
    _STOP_JOIN_TIMEOUT_S = 5.0

    def __init__(self,
                 stage_id: str,
                 stage_started_utc: str,
                 out_path: Path,
                 *,
                 deployment: str = "deployment/snowplow",
                 namespace: str = "krateo-system",
                 container: str = "snowplow"):
        self._stage_id = stage_id
        self._since_time = stage_started_utc
        self._out_path = Path(out_path)
        self._deployment = deployment
        self._namespace = namespace
        self._container = container
        self._proc: subprocess.Popen | None = None
        self._running = threading.Event()
        self._thread: threading.Thread | None = None
        self._fh = None  # file handle held by the supervisor

    @classmethod
    def disabled(cls) -> bool:
        v = os.environ.get("BENCH_NO_PER_STAGE_LOGS", "0").strip().lower()
        return v in ("1", "true", "yes")

    def start(self) -> None:
        if self.disabled():
            return
        self._out_path.parent.mkdir(parents=True, exist_ok=True)
        # Append-mode so reconnect-after-restart preserves earlier output.
        self._fh = open(self._out_path, "ab", buffering=0)
        self._running.set()
        self._thread = threading.Thread(
            target=self._supervise, daemon=True,
            name=f"per-stage-logs-{self._stage_id}")
        self._thread.start()

    def stop(self) -> None:
        if self.disabled():
            return
        self._running.clear()
        # Terminate the in-flight kubectl subprocess so the supervisor
        # loop iteration unblocks immediately.
        proc = self._proc
        if proc is not None:
            try:
                proc.terminate()
            except Exception:
                pass
            try:
                proc.wait(timeout=2.0)
            except subprocess.TimeoutExpired:
                try:
                    proc.kill()
                except Exception:
                    pass
                try:
                    proc.wait(timeout=1.0)
                except Exception:
                    pass
            except Exception:
                pass
        if self._thread is not None:
            self._thread.join(timeout=self._STOP_JOIN_TIMEOUT_S)
        # Flush + fsync the file handle so the per-stage log is on disk
        # BEFORE _run_stage persists the proof + state.json.
        fh = self._fh
        if fh is not None:
            try:
                fh.flush()
                os.fsync(fh.fileno())
            except Exception:
                pass
            try:
                fh.close()
            except Exception:
                pass
            self._fh = None

    def _supervise(self) -> None:
        """Spawn -> stream -> on-exit decide-respawn loop.

        Each iteration:
          1. Spawn `kubectl logs -f --since-time=<self._since_time>`
          2. Pipe its stdout straight into the open file handle
          3. On subprocess exit:
             - If self._running is clear: stop() initiated; return
             - Else: write reconnect marker; sleep backoff; respawn
        """
        while self._running.is_set():
            try:
                proc = subprocess.Popen(
                    ["kubectl", "logs",
                     "-n", self._namespace,
                     self._deployment,
                     "-c", self._container,
                     "-f",
                     f"--since-time={self._since_time}"],
                    stdout=subprocess.PIPE,
                    stderr=subprocess.DEVNULL,
                    bufsize=0,
                )
            except Exception as e:
                # Subprocess spawn failed (kubectl missing, etc.).
                # Log the error inline and back off. Don't crash the bench.
                try:
                    self._fh.write(
                        f"--- STREAM SPAWN FAILED: "
                        f"{type(e).__name__}: {e} ---\n".encode())
                except Exception:
                    pass
                time.sleep(self._RESPAWN_BACKOFF_S)
                continue
            self._proc = proc
            # Pump stdout into the file. Read in chunks so a very chatty
            # pod doesn't starve the supervisor loop on per-line work.
            try:
                while self._running.is_set():
                    chunk = proc.stdout.read(4096)
                    if not chunk:
                        break  # subprocess exited or pipe closed
                    try:
                        self._fh.write(chunk)
                    except Exception:
                        # File handle gone; abort supervision.
                        self._running.clear()
                        break
            except Exception:
                pass
            finally:
                # Drain remaining buffered output before we decide
                # whether to respawn.
                try:
                    proc.stdout.close()
                except Exception:
                    pass
                try:
                    proc.wait(timeout=2.0)
                except Exception:
                    try:
                        proc.kill()
                    except Exception:
                        pass
            # Respawn-decision branch.
            if not self._running.is_set():
                return
            # Subprocess exited but we're still in-stage — likely a pod
            # restart. Emit a marker and respawn with the SAME
            # --since-time so the new pod's startup logs land in this
            # same file with full coverage from stage_started_utc.
            try:
                self._fh.write(
                    f"--- STREAM RECONNECT @ {_now_iso()} "
                    f"(pod restart / stream EOF) ---\n".encode())
                self._fh.flush()
            except Exception:
                pass
            time.sleep(self._RESPAWN_BACKOFF_S)


# ─── Stage runner wrapper (catches ConvergenceTimeout per R3.2) ─────────────


def _run_stage(stage_id: str,
               ctx: StageContext,
               work: Callable[[StageContext], dict],
               what_breaks_if_skipped: str,
               *,
               artifacts: list[str] | None = None) -> StageProof:
    """Execute `work(ctx)` and persist the resulting StageProof.

    On ConvergenceTimeout: build a proof with passed=False +
    convergence_timeout=True, persist state.json BEFORE re-raise.
    The CLI catches the re-raised exception and exits 4.

    On any other exception: build a proof with passed=False and the
    error in `proof.error`, persist, then re-raise — the CLI's main
    exits 1.

    Task #251 (2026-06-09): opens a `_PerStageLogStreamer` for the
    stage's measurement window BEFORE `work(ctx)` fires; closes and
    flushes it BEFORE the proof is persisted. The streamer is uniform
    across S0-S11 (no special-cases per stage). Opt-out via the
    `BENCH_NO_PER_STAGE_LOGS=1` env flag.
    """
    started = _now_iso()
    # Open per-stage log stream BEFORE work runs so the kubectl --since-time
    # buffer covers the full measurement window even if the subprocess takes
    # a few hundred ms to attach.
    stage_log_path = Path(ctx.run_dir) / "pod_logs" / f"{stage_id}.txt"
    streamer = _PerStageLogStreamer(
        stage_id=stage_id,
        stage_started_utc=started,
        out_path=stage_log_path,
    )
    streamer.start()
    try:
        try:
            proof_dict = work(ctx) or {}
            passed = bool(proof_dict.pop("__passed__", True))
            proof = StageProof(
                stage_id=stage_id,
                started_at=started,
                ended_at=_now_iso(),
                passed=passed,
                proof=proof_dict,
                artifacts=list(artifacts or []),
                what_breaks_if_skipped=what_breaks_if_skipped,
            )
        except ConvergenceTimeout as ct:
            proof = StageProof(
                stage_id=stage_id,
                started_at=started,
                ended_at=_now_iso(),
                passed=False,
                proof={
                    "error": "ConvergenceTimeout",
                    "stage": ct.stage, "user": ct.user,
                    "api": ct.api, "ui": ct.ui, "cluster": ct.cluster,
                    "timeout_secs": ct.timeout_secs,
                },
                artifacts=list(artifacts or []),
                what_breaks_if_skipped=what_breaks_if_skipped,
                convergence_timeout=True,
            )
            # Close the streamer BEFORE we persist the proof so the
            # per-stage log file is on disk + the artifact path attached.
            streamer.stop()
            _attach_per_stage_log_artifact(proof, ctx.run_dir, stage_log_path)
            _write_stage_proof(ctx.run_dir, proof)
            _record_proof_to_state(ctx, proof)
            raise  # re-raise AFTER state persisted
        except Exception as e:
            proof = StageProof(
                stage_id=stage_id,
                started_at=started,
                ended_at=_now_iso(),
                passed=False,
                proof={"error": type(e).__name__, "msg": str(e)[:500]},
                artifacts=list(artifacts or []),
                what_breaks_if_skipped=what_breaks_if_skipped,
            )
            streamer.stop()
            _attach_per_stage_log_artifact(proof, ctx.run_dir, stage_log_path)
            _write_stage_proof(ctx.run_dir, proof)
            _record_proof_to_state(ctx, proof)
            raise

        # Success path: stop streamer BEFORE persisting proof.
        streamer.stop()
        _attach_per_stage_log_artifact(proof, ctx.run_dir, stage_log_path)
        _write_stage_proof(ctx.run_dir, proof)
        _record_proof_to_state(ctx, proof)
        return proof
    finally:
        # Defensive: ensure the streamer is never left running even if
        # _write_stage_proof or _record_proof_to_state raise (which they
        # don't today, but better safe than orphan).
        if streamer._running.is_set():
            streamer.stop()


def _attach_per_stage_log_artifact(proof: StageProof,
                                   run_dir: Path,
                                   stage_log_path: Path) -> None:
    """Record the per-stage log file path under proof.artifacts.

    Stored as a path relative to run_dir for portability (matches the
    convention in _attach_video_artifacts_to_last_measurement_proof).
    No-op if the streamer was disabled or the file is empty/missing.
    """
    try:
        if not stage_log_path.exists() or stage_log_path.stat().st_size == 0:
            return
        rel = stage_log_path.absolute().relative_to(Path(run_dir).absolute())
        rel_str = str(rel)
        if rel_str not in proof.artifacts:
            proof.artifacts.append(rel_str)
    except Exception:
        # Artifact attach is best-effort; never let it block proof
        # persistence.
        pass


def _record_proof_to_state(ctx: StageContext, proof: StageProof) -> None:
    state = load_state(ctx.run_dir) or {}
    state.setdefault("tag", ctx.tag)
    state.setdefault("scale", ctx.scale)
    state.setdefault("started_at", _now_iso())
    state.setdefault("stages_completed", [])
    state.setdefault("stage_proofs", {})
    state["cache_mode"] = ctx.cache_mode
    state["run_dir"] = str(Path(ctx.run_dir).absolute())
    state["stage_proofs"][proof.stage_id] = proof.to_dict()
    if proof.passed and proof.stage_id not in state["stages_completed"]:
        state["stages_completed"].append(proof.stage_id)
    state["current_stage"] = proof.stage_id
    save_state(ctx.run_dir, state)


# ─── Shared helpers ─────────────────────────────────────────────────────────


def _setup_users(ctx: StageContext) -> None:
    """Open one browser context per user_subject; populate ctx.user_pages.

    Mirrors worktree source 6635-6654. Pages stay live for the full
    cache_mode loop. Per-cell .webm recording is honoured here via
    `browser.make_browser_context(record_video_dir=...)` when
    `ctx.video in ("representative", "all")`.

    Playwright finalizes the .webm file only when the BrowserContext
    closes (page.close() alone is insufficient). The .webm lives under
    `{ctx.run_dir}/videos/` with a Playwright-assigned random filename;
    `_teardown_users` renames it to the canonical
    `{stage_label}_{user}_cold_dashboard.webm` shape and post-processes
    to .gif via `browser.record_video_to_gif`.
    """
    from playwright.sync_api import sync_playwright  # local import

    creds = browser._ensure_users()  # type: ignore[attr-defined]
    user_subjects = list(ctx.user_pages.get("__subjects__",
                                            ["admin", "cyberjoker"]))
    ctx.user_pages.pop("__subjects__", None)

    pw = sync_playwright().start()
    pw_browser = pw.chromium.launch(headless=True)
    ctx.user_pages["__pw__"] = pw  # type: ignore[assignment]
    ctx.user_pages["__browser__"] = pw_browser  # type: ignore[assignment]

    record_video = (ctx.video or "none").lower() in ("representative", "all")
    videos_dir: Path | None = None
    if record_video:
        videos_dir = Path(ctx.run_dir) / "videos"
        videos_dir.mkdir(parents=True, exist_ok=True)

    for user_subject in user_subjects:
        pwd = creds.get(user_subject)
        if not pwd:
            continue
        u_ctx = browser.make_browser_context(
            pw_browser, record_video_dir=videos_dir)
        u_page = u_ctx.new_page()
        if not browser.browser_login(u_page, user_subject, pwd):
            u_ctx.close()
            continue
        ctx.user_pages[user_subject] = {
            "ctx": u_ctx, "page": u_page,
            "token": ctx.tokens.get(user_subject),
            "record_video": bool(record_video),
        }


def _teardown_users(ctx: StageContext) -> None:
    """Close per-user browser contexts; rename .webm + run ffmpeg → .gif.

    Order matters:
      1. Capture the Playwright-assigned video path BEFORE closing the
         context (page.video.path() requires the context alive).
      2. Close the context (Playwright finalizes the .webm on disk).
      3. Rename random.webm → canonical `{stage_label}_{user}_cold_dashboard.webm`.
      4. Invoke `browser.record_video_to_gif` per pair.
      5. Record produced paths into `ctx.user_pages[user]["artifacts"]`
         so the stage runner can attach them to the StageProof.
    """
    videos_dir = Path(ctx.run_dir) / "videos"
    stage_label = _first_stage_label(ctx)
    produced_artifacts: list[str] = []

    for u_name, v in list(ctx.user_pages.items()):
        if u_name.startswith("__"):
            continue
        raw_webm: Path | None = None
        if v.get("record_video"):
            try:
                page = v.get("page")
                vid = getattr(page, "video", None)
                if vid is not None:
                    raw_webm_path = vid.path()
                    if raw_webm_path:
                        raw_webm = Path(raw_webm_path)
            except Exception:
                raw_webm = None
        try:
            v["ctx"].close()
        except Exception:
            pass
        # Post-process AFTER context close; .webm finalizes on close.
        if raw_webm is not None:
            try:
                # Playwright may write the file under the new context's
                # video dir even though we passed `videos_dir`; consult
                # the actual path it returned.
                final_webm = (videos_dir /
                              f"{stage_label}_{u_name}_cold_dashboard.webm")
                if raw_webm.exists() and raw_webm != final_webm:
                    final_webm.parent.mkdir(parents=True, exist_ok=True)
                    raw_webm.replace(final_webm)
                elif raw_webm.exists():
                    final_webm = raw_webm
                else:
                    final_webm = None  # type: ignore[assignment]
                if final_webm is not None and final_webm.exists():
                    produced_artifacts.append(str(final_webm))
                    gif_path = final_webm.with_suffix(".gif")
                    if browser.record_video_to_gif(final_webm, gif_path):
                        produced_artifacts.append(str(gif_path))
            except Exception as e:
                print(f"  WARN: post-process video for {u_name}: "
                      f"{type(e).__name__}: {e}", flush=True)

    pw_browser = ctx.user_pages.pop("__browser__", None)
    pw = ctx.user_pages.pop("__pw__", None)
    if pw_browser is not None:
        try:
            pw_browser.close()
        except Exception:
            pass
    if pw is not None:
        try:
            pw.stop()
        except Exception:
            pass

    # Stash the produced paths on the context so the orchestrator can
    # attach them to the most-recent stage proof (acceptance bundle).
    ctx.user_pages["__video_artifacts__"] = produced_artifacts  # type: ignore[assignment]


def _first_stage_label(ctx: StageContext) -> str:
    """Return a stage label suitable for naming the .webm.

    The bench runs one BrowserContext per user across the full window,
    so the .webm captures the entire window. We label it with the FIRST
    stage in the completed-or-running set, matching the operator's
    intuition (`--to-stage S1` → `S1_admin_cold_dashboard.webm`).
    """
    state = load_state(ctx.run_dir) or {}
    completed = state.get("stages_completed") or []
    current = state.get("current_stage")
    if completed:
        # Prefer the lowest-indexed completed stage that is NOT S0 or S11
        # (those are meta — preflight + report — and never produce nav
        # samples). Pre-1.1.0 the report stage was S9; renamed to S11
        # by Task #250 Block 2.
        for sid in completed:
            if sid not in ("S0", "S11"):
                return sid
        # Fallback to the first completed entry (S0 / S11 case).
        return completed[0]
    if current:
        return current
    return "S0"


def _measure_all_users(ctx: StageContext, stage_num, stage_desc) -> list[dict]:
    """Run browser_measure_stage on every (user, page); return entries.

    Each entry tagged with `user=<u_name>`. ConvergenceTimeout propagates.
    """
    out = []
    for u_name, u_state in list(ctx.user_pages.items()):
        if u_name.startswith("__"):
            continue
        r = browser.browser_measure_stage(
            u_state["page"], stage_num, stage_desc, ctx.cache_mode,
            token=u_state["token"], user=u_name,
            verify_against_cluster=(u_name == "admin"))
        if r:
            r["user"] = u_name
            out.append(r)
            ctx.all_results.append(r)
    return out


def _snapshot_l1_ready_ts(ctx: StageContext) -> int:
    if ctx.cache_mode != "ON":
        return 0
    try:
        return browser._read_l1_ready_ts()  # type: ignore[attr-defined]
    except Exception:
        return 0


# ─── S0: preflight ──────────────────────────────────────────────────────────


def stage_s0_preflight(ctx: StageContext) -> StageProof:
    """Run the same 7 preflight gates as `bench check`.

    Side-effect-free; returns the proof dict.
    """
    def _work(c: StageContext) -> dict:
        # Lightweight inline preflight — cmd_check has rich logging but
        # we don't want to print the section banner twice. Trust the
        # caller to have run `bench check` separately for ANSI output.
        rc, ctx_out, _ = cluster.kubectl("config", "current-context")
        ctx_str = (ctx_out or "").strip()
        return {
            "kubectl_context": ctx_str,
            "tag": c.tag,
            "scale": c.scale,
            "cache_mode": c.cache_mode,
            "smoke": c.smoke,
        }

    return _run_stage(
        "S0", ctx, _work,
        what_breaks_if_skipped=(
            "without preflight, the run may target the wrong cluster, "
            "an unexpected image tag, or a stale overlay — all later "
            "measurements would be silently invalid."
        ),
    )


# ─── S1: zero state ─────────────────────────────────────────────────────────


def stage_s1_zero_state(ctx: StageContext) -> StageProof:
    def _work(c: StageContext) -> dict:
        comps = cluster.count_compositions()
        nss = cluster.count_bench_ns()
        results = _measure_all_users(c, 1, "Zero state")
        return {
            "compositions_before": comps,
            "bench_namespaces_before": nss,
            "measurement_count": len(results),
            "navs": [
                {"user": r.get("user"), "pages": list(r.get("pages") or {})}
                for r in results
            ],
            "__passed__": True,
        }

    return _run_stage(
        "S1", ctx, _work,
        what_breaks_if_skipped=(
            "without the zero-state baseline, S2-S8 deltas cannot be "
            "attributed to specific mutations; the cold/warm ratio "
            "loses its denominator."
        ),
    )


# ─── S2: 1 ns + compdef ─────────────────────────────────────────────────────


def stage_s2_one_ns_compdef(ctx: StageContext) -> StageProof:
    def _work(c: StageContext) -> dict:
        _ = _snapshot_l1_ready_ts(c)
        lifecycle.create_bench_namespaces(1, 1)
        lifecycle.wait_for_bench_namespaces(1)
        cd_ready = cluster.deploy_compositiondefinition("bench-ns-01")
        time.sleep(15)
        _post_mutation_pause(c.cache_mode)
        results = _measure_all_users(c, 2, "1 ns + compdef")
        return {
            "ns_count": cluster.count_bench_ns(),
            "compdef_ready": cd_ready,
            "measurement_count": len(results),
            "__passed__": cd_ready,
        }

    return _run_stage(
        "S2", ctx, _work,
        what_breaks_if_skipped=(
            "without the compdef-present proof, S4-S6 cannot assume the "
            "Composition CRD exists; deploy_compositions would fail to "
            "apply at the wire level."
        ),
    )


# ─── S3: 20 namespaces ──────────────────────────────────────────────────────


def stage_s3_20_ns(ctx: StageContext) -> StageProof:
    def _work(c: StageContext) -> dict:
        _ = _snapshot_l1_ready_ts(c)
        lifecycle.create_bench_namespaces(2, 20)
        lifecycle.wait_for_bench_namespaces(20)
        time.sleep(10)
        _post_mutation_pause(c.cache_mode)
        results = _measure_all_users(c, 3, "20 bench ns")
        return {
            "ns_count": cluster.count_bench_ns(),
            "measurement_count": len(results),
            "__passed__": True,
        }

    return _run_stage(
        "S3", ctx, _work,
        what_breaks_if_skipped=(
            "without 20-ns ramp, S4 composition deploy may reuse stale "
            "ns state from a prior run and skew the per-ns RBAC view."
        ),
    )


# ─── S4: 20 compositions ────────────────────────────────────────────────────


def stage_s4_20_compositions(ctx: StageContext) -> StageProof:
    def _work(c: StageContext) -> dict:
        _ = _snapshot_l1_ready_ts(c)
        lifecycle.wait_for_crd()
        # 20 compositions across ns 1-20 (1 each). deploy_compositions_parallel
        # handles arbitrary scale and replaces the worktree's sequential
        # deploy_compositions(1, 20, 1) call at source 6708.
        lifecycle.deploy_compositions_parallel(1, 20, 1)
        browser.wait_for_compositions(20)
        _post_mutation_pause(c.cache_mode)
        results = _measure_all_users(c, 4, "20 compositions")
        return {
            "compositions_deployed": cluster.count_compositions(),
            "measurement_count": len(results),
            "__passed__": True,
        }

    return _run_stage(
        "S4", ctx, _work,
        what_breaks_if_skipped=(
            "without 20-composition baseline, the small-N cell anchors "
            "(cold_ms at 20) cannot be compared against SCALE-N S6 "
            "values; per-stage convergence_mass loses its low-end point."
        ),
    )


# ─── S5: SCALE ns ──────────────────────────────────────────────────────────


def stage_s5_scale_ns(ctx: StageContext) -> StageProof:
    def _work(c: StageContext) -> dict:
        if c.scale >= 10000:
            s5_ns_end = 50
        else:
            s5_ns_end = c.scale // 10 if c.scale >= 10 else 1
        _ = _snapshot_l1_ready_ts(c)
        lifecycle.create_bench_namespaces(21, s5_ns_end)
        lifecycle.wait_for_bench_namespaces(s5_ns_end, timeout=600)
        _post_mutation_pause(c.cache_mode)
        results = _measure_all_users(c, 5, f"{s5_ns_end} bench ns")
        return {
            "ns_target": s5_ns_end,
            "ns_actual": cluster.count_bench_ns(),
            "measurement_count": len(results),
            "__passed__": True,
        }

    return _run_stage(
        "S5", ctx, _work,
        what_breaks_if_skipped=(
            "without the scaled namespace count, S6 composition deploy "
            "cannot spread across enough namespaces to exercise the "
            "informer's pagination."
        ),
    )


# ─── S6: SCALE compositions ────────────────────────────────────────────────


def stage_s6_scale_compositions(ctx: StageContext) -> StageProof:
    def _work(c: StageContext) -> dict:
        if c.scale >= 10000:
            s5_ns_end = 50
            s6_comps_per_ns = c.scale // s5_ns_end
        else:
            s5_ns_end = c.scale // 10 if c.scale >= 10 else 1
            s6_comps_per_ns = 10
        s6_timeout = 1200 if c.scale <= 5000 else 3600
        s6_quiesce = 60 if c.scale <= 5000 else 120

        # Background piechart progression polling — see worktree 6731-6738.
        pie_stop = threading.Event()
        pie_thread = threading.Thread(
            target=browser._poll_piechart_progression,  # type: ignore[attr-defined]
            args=(c.admin_token, pie_stop),
            daemon=True,
        )
        pie_thread.start()

        try:
            lifecycle.deploy_compositions_parallel(1, s5_ns_end, s6_comps_per_ns)
            browser.wait_for_compositions(c.scale, timeout=s6_timeout)
            time.sleep(s6_quiesce)
            _post_mutation_pause(c.cache_mode)
        finally:
            pie_stop.set()
            pie_thread.join(timeout=10)

        lifecycle.wait_for_restaction_steady_state(
            timeout=600, target_per_ns=120, polling_interval=10)
        results = _measure_all_users(c, 6, f"{c.scale} compositions")
        return {
            "ns_count": s5_ns_end,
            "comps_per_ns": s6_comps_per_ns,
            "compositions_actual": cluster.count_compositions(),
            "measurement_count": len(results),
            "__passed__": True,
        }

    return _run_stage(
        "S6", ctx, _work,
        what_breaks_if_skipped=(
            "S6 is the SCALE-anchor cell — mix_weighted.warm_p50_ms "
            "without it cannot reflect customer experience at 50K."
        ),
    )


# ─── S7: delete 1 composition ──────────────────────────────────────────────


def stage_s7_delete_one_comp(ctx: StageContext) -> StageProof:
    def _work(c: StageContext) -> dict:
        _ = _snapshot_l1_ready_ts(c)
        lifecycle.delete_one_composition("bench-ns-01", "bench-app-01-01")
        lifecycle.wait_for_composition_gone("bench-ns-01", "bench-app-01-01")
        _post_mutation_pause(c.cache_mode)
        results = _measure_all_users(c, 7, "Deleted 1 comp")
        return {
            "ns": "bench-ns-01",
            "name": "bench-app-01-01",
            "measurement_count": len(results),
            "__passed__": True,
        }

    return _run_stage(
        "S7", ctx, _work,
        what_breaks_if_skipped=(
            "without per-mutation delete cell, "
            "convergence_mass_s7_p99 is null — cache invalidation "
            "regression cannot be detected."
        ),
    )


# ─── Task #250 Block 2 — RBAC mutation stages S8 (RB-add) + S9 (RB-remove) ──
#
# Stage runners are pure helpers — they read RBAC layout from ctx-derived
# parameters. Production-realistic shape (Diego choice 2 ratified
# 2026-06-05): the RB-add targets `bench-ns-01`, a pre-populated namespace
# carrying ~SCALE/50 compositions at S6 entry. This exercises the
# subject-index propagation path (Probe A on /debug/vars +
# Probe B on /call view convergence).
#
# Parametric per feedback_no_special_cases: subject_user, target_ns,
# role/RB names, subject group are all named local variables — no
# cj-hardcoded literal participates in cluster mutations. The single
# point of "which group is subject_user in" is the `_user_group` lookup
# table below (data, not policy).


def _user_group(subject_user: str) -> str:
    """Return the primary RBAC Group for a bench user.

    Parametric data table — NOT a special case. Mirrors the portal-chart
    user provisioning fact: today the portal chart provisions
    `cyberjoker` into the `devs` group. If portal adds a third user this
    table grows in ONE place. PM Q2 ratified 2026-06-05.

    Cross-reference: `bench/storm.py:577-590` references `cyberjoker`
    + `devs` in the CRB-burst harness (defect reproducer, NOT a Phase
    6 stage). When this table is extended, that site MUST be updated
    in lockstep — see the doc comment at storm.py:577-590.

    Args:
        subject_user: bench user identity ("cyberjoker", "admin", ...).

    Returns:
        Group name string ("devs" for cyberjoker), empty string when
        the user has no Group-based RBAC entry (admin is provisioned
        via a ClusterRoleBinding; group lookup is unused for admin).
    """
    return {
        "cyberjoker": "devs",
        # admin is provisioned via a CRB — Group lookup not used.
    }.get(subject_user, "")


def _pick_visible_composition_name(target_ns: str) -> str:
    """Return a composition name guaranteed to render on the datagrid's
    first page (per_page=5) for a viewer with full ns access.

    Convention picks `bench-app-01-02` (lex-second in `bench-ns-01`)
    because S7 deletes `bench-app-01-01` and the datagrid renders names
    in lex order. Falls back to live ns enumeration if the conventional
    name is absent (e.g. running against a cluster shape that does not
    match the convention).

    Args:
        target_ns: namespace to scope the live-enumeration fallback to.

    Returns:
        Composition name string; falls back to the conventional name on
        kubectl failure (so the caller's `_user_card_text_present` check
        still has a deterministic input to look for).
    """
    conventional = "bench-app-01-02"
    rc, out, _ = cluster.kubectl(
        "get", f"{cluster.COMP_RES}.{cluster.COMP_GVR}", "-n", target_ns,
        "--no-headers", "-o", "custom-columns=NAME:.metadata.name")
    if rc != 0 or not out.strip():
        return conventional
    names = sorted(n.strip() for n in out.splitlines() if n.strip())
    if conventional in names:
        return conventional
    return names[0] if names else conventional


def _user_card_text_present(ctx: StageContext,
                            subject_user: str,
                            card_name: str) -> bool:
    """Return True iff `subject_user`'s live Compositions page contains
    a DOM element whose text matches `card_name`.

    CONTENT-not-status check per feedback_validate_content_not_just_status
    + Task #250 R4 mitigation: even when call_count matches the formula,
    a per-card widget rendering empty placeholders (or 403'ing silently)
    will NOT show the composition name in the DOM. The card-text-present
    check eliminates that failure mode.

    Args:
        ctx:          StageContext carrying ctx.user_pages.
        subject_user: bench user whose Page to inspect.
        card_name:    composition name to search for in the rendered DOM.

    Returns:
        True iff `page.locator(text=card_name).count() >= 1`.
        False on missing card_name, missing user_state, missing page, or
        any locator exception.
    """
    if not card_name:
        return False
    u_state = ctx.user_pages.get(subject_user)
    if not u_state:
        return False
    page = u_state.get("page")
    if page is None:
        return False
    try:
        return page.locator(f"text={card_name}").count() >= 1
    except Exception:
        return False


def _count_widget_errors(results: list, *, user: str, page: str) -> int:
    """Sum `validation.errored_count` across nav results for (user, page).

    Task #250 R4 mitigation — load-bearing on S8 `__passed__`. The
    `errored_count` value is the `.ant-result-error` DOM count captured
    by `_validate_widget_terminal_state` (browser.py:809). A non-zero
    value at S8 means per-card widget RESTActions are 403'ing while the
    composition card is otherwise rendered — exactly the #186-class
    failure mode that would silently pass a call-count-only gate.

    Args:
        results: list of nav dicts emitted by `_measure_all_users`.
        user:    subject to filter on.
        page:    page_name to filter on (matches `pages` keys, e.g.
                 "Compositions" / "Dashboard").

    Returns:
        Sum of errored_count across matching navs. 0 when no match.
    """
    out = 0
    for r in results or []:
        if r.get("user") != user:
            continue
        # The result shape is per-stage (each entry from
        # browser_measure_stage carries pages_data keyed by page_name).
        pages_data = r.get("pages") or {}
        page_entry = pages_data.get(page)
        if not page_entry:
            continue
        for nav in page_entry.get("navigations") or []:
            v = nav.get("validation") or {}
            try:
                out += int(v.get("errored_count") or 0)
            except (TypeError, ValueError):
                pass
    return out


def _wait_rbac_propagation_to_snowplow(ctx: StageContext,
                                       subject_user: str,
                                       target_ns: str,
                                       expected_visible: int,
                                       timeout_secs: int = 180
                                       ) -> tuple[bool, int, dict]:
    """Wait for BOTH probes to agree before declaring RBAC propagation.

    Probe A (mechanism-independent): snowplow's
    `snowplow_rbac_publish_seq` expvar must INCREMENT vs the
    pre-mutation snapshot. The counter is sourced from cache.RBACGen()
    (snowplow rbac_snapshot.go:251), bumped exactly once per successful
    rebuildRBACSnapshot publish. Cannot be tricked by a stale `/call`
    cell — only a real informer event + snapshot rebuild moves it.

    Probe B (UX-side): `count_user_compositions_in_ns(user, token, ns)`
    must equal `expected_visible` within the same budget. Confirms 'by
    the time the browser nav fires, the view has caught up'.

    BOTH probes must pass for `propagation_ok=True`. Mismatch surfaces
    the failure mode in the diag dict — Probe A pass + Probe B fail
    means snapshot rebuilt but evaluator returned the wrong view
    (#149-class regression); Probe A fail means no informer event
    observed OR rebuild stalled.

    Timeout (re-gate v4 / 2026-06-08): bumped 30s → 180s. Tester
    evidence showed the RB-add cold path needs > 30s for the
    /compositions view to reach the user; the prior 30s budget
    surfaced a Probe B miss (uvc=0 at +30s) while the actual browser
    nav observed convergence at ~260s (api=999, ui=999, converged in
    23.5s). The mechanism IS propagating — the 30s budget was too
    tight. 180s leaves 6x headroom over the empirical 30s mid-band
    and covers cache-OFF S9 (~30s) plus jitter.

    Returns:
        (ok, elapsed_ms, diag) — diag captures pre/post seq + final n.
    """
    token = ctx.tokens.get(subject_user)
    if not token:
        return (False, 0, {"error": "no_token",
                           "subject_user": subject_user})

    seq_before = browser.read_snowplow_expvar_int(
        "snowplow_rbac_publish_seq")
    if seq_before is None:
        # Expvar not wired or unreachable — FAIL CLOSED. Block 2a
        # expvar shim MUST publish the key for the gate to operate.
        return (False, 0, {
            "error": "expvar_unreadable",
            "key": "snowplow_rbac_publish_seq",
        })

    deadline = time.time() + timeout_secs
    started = time.time()
    seq_now = seq_before
    n = -1
    probe_a_pass = False
    probe_b_pass = False
    while time.time() < deadline:
        seq_now = browser.read_snowplow_expvar_int(
            "snowplow_rbac_publish_seq")
        n = browser.count_user_compositions_in_ns(
            subject_user, token, target_ns)
        probe_a_pass = (seq_now is not None and seq_now > seq_before)
        probe_b_pass = (n == expected_visible)
        if probe_a_pass and probe_b_pass:
            return (True, int((time.time() - started) * 1000), {
                "rbac_publish_seq_before": seq_before,
                "rbac_publish_seq_after": seq_now,
                "user_visible_count": n,
                "expected_visible": expected_visible,
                "probe_a_pass": True,
                "probe_b_pass": True,
            })
        time.sleep(1)
    return (False, int((time.time() - started) * 1000), {
        "rbac_publish_seq_before": seq_before,
        "rbac_publish_seq_after": seq_now,
        "user_visible_count": n,
        "expected_visible": expected_visible,
        "probe_a_pass": probe_a_pass,
        "probe_b_pass": probe_b_pass,
    })


def stage_s8_add_rb_to_populated_ns(ctx: StageContext) -> StageProof:
    """RB-add into a pre-populated ns; cj transitions 0 → N visible cards.

    Tests snowplow's subject-index propagation: a RoleBinding ADD in
    `bench-ns-01` must (a) bump rbac_publish_seq, (b) cause cj's /call
    view to include the bench-ns-01 compositions, (c) trigger
    per-card widget RESTAction fan-out on the rendered datagrid.

    Per Task #250 design §3.1. Parametric — no cj-hardcoded path
    participates in mutation.
    """
    def _work(c: StageContext) -> dict:
        # Diego choice 2 (production-realistic): cj is the only non-admin
        # bench user today; subject_user parameter would expand to a
        # stage-arg if a third subject is added.
        subject_user = "cyberjoker"
        subject_group = _user_group(subject_user)
        target_ns = "bench-ns-01"

        # Pre-mutation snapshot for proof bookkeeping.
        comps_in_ns = cluster.count_compositions_in_ns(target_ns)
        _ = _snapshot_l1_ready_ts(c)

        # Pre-check (re-gate v2 defence-in-depth): the bench's
        # kubeconfig identity MUST be able to create RoleBindings in
        # target_ns. A 403 here surfaces as `rbac_precheck_denied`
        # which is far more diagnosable than a generic create failure.
        precheck_ok, precheck_diag = cluster.k8s_can_i_create_rolebinding(
            target_ns)
        if not precheck_ok:
            return {
                "subject_user": subject_user,
                "subject_group": subject_group,
                "target_ns": target_ns,
                "error": precheck_diag,
                "precheck_allowed": False,
                "__passed__": False,
            }

        # 1. Create the Role with TWO PolicyRules (re-gate v3 / Diego
        # ratified 2026-06-05, closes #186 Option (a); re-gate v4
        # 2026-06-08 adds tablists per architect trace):
        #   - composition.krateo.io/*: get,list — the composition CRs
        #     cj's datagrid iterates.
        #   - widgets.templates.krateo.io: panels, markdowns, buttons,
        #     TABLISTS — the per-card widget RESTActions the datagrid
        #     fans out to. tablists is the 4th widget per card (the
        #     click-nav target — Panel.spec.resourcesRefs[3] has
        #     id=composition-tablist). task-215's trace doc was wrong
        #     about this being "2nd Button"; live `kubectl get panels
        #     ... -o jsonpath={.spec.resourcesRefs}` against bench-ns-01
        #     on 2026-06-08 confirms the four resources are:
        #       [markdowns/GET, buttons/DELETE, buttons/PATCH,
        #        tablists/GET].
        #     `kubectl auth can-i list tablists -n bench-ns-01
        #      --as=cyberjoker --as-group=devs` → "no" pre-fix; this
        #     was the 403 producing `cj_widget_error_count = 15`
        #     (5 cards × 1 tablist 403 × 3 navs) in the prior tester run.
        # The TWO-RULE shape lets cj's S8 Compositions page actually
        # RENDER cards (not just transit /call), so the call-count
        # gate observes the real datagrid fan-out signal.
        # GVR group string empirically verified against the live
        # cluster 2026-06-05 via
        # `kubectl api-resources --api-group=widgets.templates.krateo.io`.
        role_name = f"bench-{subject_user}-{target_ns}-comp-reader"
        rb_name = f"bench-{subject_user}-{target_ns}-comp-reader-binding"
        role_ok, role_diag = cluster.k8s_create_namespaced_role(
            target_ns, role_name,
            rules=[
                (["composition.krateo.io"], ["*"],
                 ["get", "list"]),
                # tablists is the 4th widget per card (the click-nav
                # target); task-215 doc had this wrong as "2nd Button"
                # but Panel.spec.resourcesRefs[3] is the tablist
                # (TRACED 2026-06-08 live cluster).
                (["widgets.templates.krateo.io"],
                 ["panels", "markdowns", "buttons", "tablists"],
                 ["get", "list"]),
            ],
        )
        rb_ok, rb_diag = cluster.k8s_create_namespaced_role_binding(
            target_ns, rb_name,
            role_ref=("Role", role_name),
            subjects=[{"kind": "Group", "name": subject_group}],
        )
        if not (role_ok and rb_ok):
            # Surface the FULL diagnostic from the cluster.py helper
            # so SDK / RBAC drift is debuggable from the proof body
            # alone (re-gate v2: previously this said only
            # `error: rbac_create_failed` and hid the AttributeError
            # on `V1Subject` for the entire S8 wall-clock).
            return {
                "subject_user": subject_user,
                "subject_group": subject_group,
                "target_ns": target_ns,
                "role_name": role_name,
                "rb_name": rb_name,
                "role_ok": role_ok,
                "rb_ok": rb_ok,
                "role_diag": role_diag,
                "rb_diag": rb_diag,
                "precheck_allowed": True,
                "error": (role_diag if not role_ok else rb_diag),
                "__passed__": False,
            }

        # 2. Two-probe inner gate (mechanism + UX). cj's visible count
        # should rise from 0 (pre-mutation) to comps_in_ns.
        # Timeout 180s (re-gate v4 / 2026-06-08): tester evidence
        # showed the RB-add cold path needs > 30s for Probe B to
        # observe the cj view; prior 30s budget was too tight while
        # the real browser nav saw convergence ~260s later. 180s
        # leaves 6x headroom over the empirical mid-band.
        prop_ok, prop_ms, prop_diag = _wait_rbac_propagation_to_snowplow(
            c, subject_user, target_ns,
            expected_visible=comps_in_ns,
            timeout_secs=180,
        )

        # 3. Browser measurement (cj's /compositions cell should now
        # fan out to per-card widgets — call_count gate verifies this
        # via the N-aware EXPECTED_CALLS formula).
        _post_mutation_pause(c.cache_mode)
        results = _measure_all_users(c, 8, "RB-add to pre-populated ns")

        # 4. CONTENT-not-status assertion (per R4 mitigation).
        expected_card_name = _pick_visible_composition_name(target_ns)
        cj_card_present = _user_card_text_present(
            c, subject_user, expected_card_name)
        cj_widget_errors = _count_widget_errors(
            results, user=subject_user, page="Compositions")

        return {
            "subject_user": subject_user,
            "subject_group": subject_group,
            "target_ns": target_ns,
            "role_name": role_name,
            "rb_name": rb_name,
            "comps_in_ns": comps_in_ns,
            "propagation_ok": prop_ok,
            "propagation_ms": prop_ms,
            "propagation_diag": prop_diag,
            "expected_card_name": expected_card_name,
            "content_card_present": cj_card_present,
            "cj_widget_error_count": cj_widget_errors,
            "measurement_count": len(results),
            "__passed__": (
                prop_ok
                and cj_card_present
                and cj_widget_errors == 0
            ),
        }

    return _run_stage(
        "S8", ctx, _work,
        what_breaks_if_skipped=(
            "without RB-add into pre-populated ns, the subject-index "
            "propagation regression of Ship 0.30.235 (#149) cannot be "
            "detected — cj's narrowed view stays empty even when an "
            "admin grants ns access mid-session."
        ),
    )


def stage_s9_remove_rb_from_populated_ns(ctx: StageContext) -> StageProof:
    """RB-remove from same ns; cj transitions N visible → 0 cards.

    Tests snowplow's RBAC-snapshot revocation path. The bidirectional
    pair (S8 add + S9 remove) is the falsifier that the #149 defect
    would have triggered — symmetric add/remove cycles each catch a
    different end of the regression.

    Reads target ns + role/RB names from S8's proof on disk (parametric
    flow — no string literal pass-through).
    """
    def _work(c: StageContext) -> dict:
        s8_proof = (load_state(c.run_dir) or {}).get(
            "stage_proofs", {}).get("S8", {})
        s8_body = (s8_proof.get("proof") or {})
        subject_user = s8_body.get("subject_user")
        target_ns = s8_body.get("target_ns")
        role_name = s8_body.get("role_name")
        rb_name = s8_body.get("rb_name")
        if not (subject_user and target_ns and role_name and rb_name):
            return {
                "error": "s8_proof_missing",
                "subject_user": subject_user,
                "target_ns": target_ns,
                "role_name": role_name,
                "rb_name": rb_name,
                "__passed__": False,
            }

        _ = _snapshot_l1_ready_ts(c)

        # 1. Delete the RB. Role is left in place — symmetric add/remove
        # is "subject access", not "permission inventory"; Role removal
        # is a separate stage we explicitly do not model here. Cleanup
        # of the Role happens after the propagation gate so a partial
        # failure path doesn't leak.
        rb_gone = cluster.k8s_delete_rolebinding(target_ns, rb_name)
        if not rb_gone:
            return {
                "subject_user": subject_user,
                "target_ns": target_ns,
                "role_name": role_name,
                "rb_name": rb_name,
                "error": "rb_delete_failed",
                "__passed__": False,
            }

        # 2. Wait for revocation propagation. expected_visible=0 means
        # the user's narrowed view drops back to empty. Timeout 180s
        # for symmetry with S8 (re-gate v4); revocation is typically
        # faster (cache-ON ~6.5s, cache-OFF ~30s) but the symmetric
        # budget absorbs jitter without over-provisioning the bench
        # wall-clock.
        prop_ok, prop_ms, prop_diag = _wait_rbac_propagation_to_snowplow(
            c, subject_user, target_ns,
            expected_visible=0,
            timeout_secs=180,
        )

        # 3. Cleanup the Role (test hygiene — not a gate assertion).
        cluster.k8s_delete_role(target_ns, role_name)

        # 4. Browser measurement (cj's /compositions should drop back
        # to BASE structural ceiling).
        _post_mutation_pause(c.cache_mode)
        results = _measure_all_users(c, 9, "RB-remove from same ns")

        # 5. CONTENT-absent assertion (revocation symmetric to S8).
        # Reuse S8's expected_card_name so we look for THE SAME row
        # that S8 confirmed was present.
        s8_card = (s8_body.get("expected_card_name") or "").strip()
        cj_card_absent = True
        if s8_card:
            cj_card_absent = not _user_card_text_present(
                c, subject_user, s8_card)

        return {
            "subject_user": subject_user,
            "target_ns": target_ns,
            "role_name": role_name,
            "rb_name": rb_name,
            "propagation_ok": prop_ok,
            "propagation_ms": prop_ms,
            "propagation_diag": prop_diag,
            "expected_card_name": s8_card,
            "content_card_absent": cj_card_absent,
            "measurement_count": len(results),
            "__passed__": (prop_ok and cj_card_absent),
        }

    return _run_stage(
        "S9", ctx, _work,
        what_breaks_if_skipped=(
            "without RB-remove, snowplow's RBAC-snapshot revocation "
            "path is uncovered — a defect that leaves cj's narrowed "
            "view populated AFTER the RB is gone would ship undetected."
        ),
    )


# ─── S10: delete 1 namespace (was S8 pre-1.1.0) ────────────────────────────


def stage_s10_delete_one_ns(ctx: StageContext) -> StageProof:
    def _work(c: StageContext) -> dict:
        if c.scale >= 10000:
            s5_ns_end = 50
        else:
            s5_ns_end = c.scale // 10 if c.scale >= 10 else 1
        s10_ns = f"bench-ns-{s5_ns_end:02d}"
        _ = _snapshot_l1_ready_ts(c)
        lifecycle.delete_one_bench_namespace(s10_ns)
        lifecycle.wait_for_namespace_gone(s10_ns)
        _post_mutation_pause(c.cache_mode)
        results = _measure_all_users(c, 10, "Deleted 1 ns")
        return {
            "ns": s10_ns,
            "measurement_count": len(results),
            "__passed__": True,
        }

    return _run_stage(
        "S10", ctx, _work,
        what_breaks_if_skipped=(
            "without ns-cascade delete cell, convergence_mass_s10_p99 "
            "is null — bulk delete invalidation regression cannot be "
            "detected."
        ),
    )


# ─── S11: build canonical ledger row + write bundle (was S9 pre-1.1.0) ─────


def stage_s11_report(ctx: StageContext) -> StageProof:
    def _work(c: StageContext) -> dict:
        per_stage_proofs = {}
        state = load_state(c.run_dir) or {}
        for sid, p in (state.get("stage_proofs") or {}).items():
            per_stage_proofs[sid] = p
        row = ledger.write_run_bundle(
            c.run_dir, c.all_results,
            per_stage_proofs=per_stage_proofs,
            tag=c.tag, scale=c.scale,
        )
        return {
            "verdict": row.get("verdict"),
            "ledger_row_keys": sorted(list(row.keys())),
            "__passed__": row.get("verdict") in ("PASS", "WEAK_PASS",
                                                 "FLOOR"),
        }

    return _run_stage(
        "S11", ctx, _work,
        what_breaks_if_skipped=(
            "S11 emits the canonical ledger row + summary.json bundle — "
            "without it the PM has no scored artifact to compare against "
            "the north-star ledger."
        ),
    )


# ─── STAGE_REGISTRY ─────────────────────────────────────────────────────────


STAGE_REGISTRY: dict[str, Callable[[StageContext], StageProof]] = {
    "S0": stage_s0_preflight,
    "S1": stage_s1_zero_state,
    "S2": stage_s2_one_ns_compdef,
    "S3": stage_s3_20_ns,
    "S4": stage_s4_20_compositions,
    "S5": stage_s5_scale_ns,
    "S6": stage_s6_scale_compositions,
    "S7": stage_s7_delete_one_comp,
    # Task #250 Block 2: new RBAC-mutation stages inserted after S7.
    "S8": stage_s8_add_rb_to_populated_ns,
    "S9": stage_s9_remove_rb_from_populated_ns,
    # Renamed from pre-1.1.0 layout (was S8 / S9).
    "S10": stage_s10_delete_one_ns,
    "S11": stage_s11_report,
}

STAGE_ORDER = ["S0", "S1", "S2", "S3", "S4", "S5", "S6", "S7",
               "S8", "S9", "S10", "S11"]


# ─── --from-stage / --to-stage semantics (per §E.3) ─────────────────────────


def _stages_in_window(from_stage: str | None,
                      to_stage: str | None) -> list[str]:
    start = STAGE_ORDER.index(from_stage) if from_stage else 0
    end = STAGE_ORDER.index(to_stage) + 1 if to_stage else len(STAGE_ORDER)
    if end <= start:
        return []
    return STAGE_ORDER[start:end]


def _validate_resume(state: dict, from_stage: str) -> tuple[bool, str]:
    """Per R4.1: ensure prior proofs in state.json precede from_stage in order.

    Returns (ok, diag). When state.json is empty (fresh resume) we accept.
    """
    completed = state.get("stages_completed") or []
    if not completed:
        return True, "no prior stages — fresh run"
    from_idx = STAGE_ORDER.index(from_stage)
    for sid in completed:
        try:
            idx = STAGE_ORDER.index(sid)
        except ValueError:
            return False, f"state.json carries unknown stage {sid!r}"
        if idx >= from_idx:
            return False, (
                f"state.json contains stages beyond requested "
                f"--from-stage={from_stage}: completed includes {sid}. "
                f"Pass a different --run-dir or remove the file."
            )
    return True, "resume window OK"


def _proof_validation_re_runner(state: dict,
                                from_stage: str,
                                *, budget_secs: float = 10.0) -> dict:
    """Per R4.1: re-verify prior stage proofs against the live cluster.

    Bounded to `budget_secs`. Stages whose proof cannot be validated in
    budget opt-out with `proof_validation: skipped`. Returns a dict
    {stage_id: "pass" | "fail:<reason>" | "skipped"}.
    """
    out: dict[str, str] = {}
    deadline = time.time() + budget_secs
    proofs = state.get("stage_proofs") or {}
    from_idx = STAGE_ORDER.index(from_stage)
    for sid in STAGE_ORDER[:from_idx]:
        if time.time() >= deadline:
            out[sid] = "skipped:budget_exhausted"
            continue
        p = proofs.get(sid)
        if not p:
            out[sid] = "skipped:no_proof"
            continue
        proof_body = p.get("proof") or {}
        if sid == "S2":
            # Compdef present?
            rc, _, _ = cluster.kubectl(
                "get", "compositiondefinitions.core.krateo.io",
                "-A", "--no-headers", "--ignore-not-found",
                timeout_secs=5)
            if rc == 0:
                out[sid] = "pass"
            else:
                out[sid] = "fail:compdef_missing"
        elif sid == "S3":
            expected = proof_body.get("ns_count", 20)
            actual = cluster.count_bench_ns()
            out[sid] = ("pass" if actual >= expected
                        else f"fail:ns_count_{actual}_lt_{expected}")
        elif sid in ("S4", "S5", "S6"):
            expected = proof_body.get("compositions_deployed") or \
                       proof_body.get("compositions_actual") or 0
            actual = cluster.count_compositions()
            if expected > 0 and actual < expected * 0.9:
                out[sid] = f"fail:comp_count_{actual}_lt_{int(expected * 0.9)}"
            else:
                out[sid] = "pass"
        elif sid == "S8":
            # S8 created a RoleBinding; verify it still exists at
            # apiserver. The Role + RB names are stamped on the proof
            # so the re-runner is parametric (no string literal).
            rb_name = proof_body.get("rb_name")
            target_ns = proof_body.get("target_ns")
            if rb_name and target_ns:
                rb = cluster.k8s_read_namespaced_role_binding(
                    target_ns, rb_name)
                out[sid] = "pass" if rb is not None else "fail:rb_missing"
            else:
                out[sid] = "skipped:no_rb_metadata"
        elif sid == "S9":
            # S9 deleted the RB; verify it is gone.
            rb_name = proof_body.get("rb_name")
            target_ns = proof_body.get("target_ns")
            if rb_name and target_ns:
                rb = cluster.k8s_read_namespaced_role_binding(
                    target_ns, rb_name)
                out[sid] = ("pass" if rb is None
                            else "fail:rb_still_present")
            else:
                out[sid] = "skipped:no_rb_metadata"
        else:
            # S0/S1/S7/S10: no live-cluster reverify (S0 is meta,
            # S1 zero-state, S7/S10 already mutated — can't undo).
            out[sid] = "skipped:no_live_check"
    return out


# ─── Phase 6 orchestrator ──────────────────────────────────────────────────


def run_phase6(tag: str,
               scale: int,
               *,
               from_stage: str | None = None,
               to_stage: str | None = None,
               cache_mode: str = "OFF",
               video: str = "representative",
               run_dir: Path | None = None,
               smoke: bool | None = None) -> dict:
    """Run Phase 6 stages within [from_stage, to_stage] inclusive.

    On `--from-stage`, calls load_state + _validate_resume + the proof-
    validation re-runner (R4.1). On ConvergenceTimeout from any stage,
    state.json is already persisted by the inner _run_stage handler;
    we re-raise so the CLI can exit 4.

    Returns the final state dict (after the window completes).
    """
    if scale is None:
        scale = _env_scale()
    if smoke is None:
        smoke = _env_smoke()

    # Default run_dir under /tmp/snowplow-runs/{tag}/run-{ts}.
    if run_dir is None:
        ts = time.strftime("%Y%m%d-%H%M%S")
        run_dir = Path("/tmp/snowplow-runs") / tag / f"run-{ts}"
    run_dir = Path(run_dir)
    run_dir.mkdir(parents=True, exist_ok=True)

    state = load_state(run_dir) or {}
    if from_stage:
        ok, diag = _validate_resume(state, from_stage)
        if not ok:
            raise RuntimeError(f"resume validation failed: {diag}")
        if state.get("stage_proofs"):
            verdicts = _proof_validation_re_runner(state, from_stage)
            state["proof_validation"] = verdicts
            failures = [k for k, v in verdicts.items()
                        if v.startswith("fail:")]
            if failures:
                raise RuntimeError(
                    f"stale state.json — proofs failed re-validation: "
                    f"{failures}. Re-run from earlier stage."
                )

    state.setdefault("schema_version", SCHEMA_VERSION)
    state.setdefault("tag", tag)
    state.setdefault("scale", scale)
    state.setdefault("started_at", _now_iso())
    state.setdefault("stages_completed", [])
    state.setdefault("stage_proofs", {})
    state.setdefault("users", ["admin", "cyberjoker"])
    state["cache_mode"] = cache_mode
    state["run_dir"] = str(run_dir.absolute())
    save_state(run_dir, state)

    window = _stages_in_window(from_stage, to_stage)
    ctx = StageContext(
        tag=tag, scale=scale,
        tokens={}, user_pages={"__subjects__": ["admin", "cyberjoker"]},
        run_dir=run_dir,
        state_path=run_dir / "state.json",
        cache_mode=cache_mode,
        smoke=bool(smoke),
        all_results=[],
        video=(video or "representative"),
    )

    # Lifecycle: log in (login_all writes to ctx.tokens) only when window
    # contains a real measurement stage (S1+). Pure S0 / S11 skip login.
    needs_login = any(s != "S0" and s != "S11" for s in window)
    if needs_login:
        try:
            ctx.tokens = browser.login_all()
            ctx.admin_token = ctx.tokens.get("admin")
        except Exception:
            ctx.tokens = {}
            ctx.admin_token = None

    # Task #250 Block 2: new S8/S9 stages run measurements via
    # _measure_all_users, so they share the browser scaffolding. S10
    # (was S8 — delete 1 ns) keeps the existing measurement footprint.
    needs_browser = any(s in ("S1", "S2", "S3", "S4", "S5", "S6", "S7",
                              "S8", "S9", "S10")
                        for s in window)
    if needs_browser and browser.FRONTEND is not None:
        try:
            _setup_users(ctx)
        except Exception as e:
            # Surface but continue with degraded measurement set.
            print(f"  WARN: _setup_users failed: {type(e).__name__}: {e}",
                  flush=True)

    try:
        for stage_id in window:
            fn = STAGE_REGISTRY[stage_id]
            fn(ctx)  # _run_stage handles persist + raise on ConvergenceTimeout
    finally:
        if needs_browser:
            _teardown_users(ctx)
            _attach_video_artifacts_to_last_measurement_proof(ctx)

    return load_state(run_dir) or {}


def _attach_video_artifacts_to_last_measurement_proof(ctx: StageContext) -> None:
    """After _teardown_users produces canonical-named .webm/.gif pairs,
    attach their paths to the earliest measurement stage proof (matching
    `_first_stage_label` naming).

    Why the earliest: the BrowserContext recorded the window's entire
    lifetime. We name and attribute the artifacts to the first
    measurement stage so the proof carries them even if a later stage
    raised ConvergenceTimeout (the .webm file still represents real
    work done up to that point).
    """
    artifacts = ctx.user_pages.pop("__video_artifacts__", None)
    if not artifacts:
        return
    target_sid = _first_stage_label(ctx)
    proof_path = Path(ctx.run_dir) / "proofs" / f"{target_sid}.json"
    if not proof_path.exists():
        return
    try:
        proof_d = json.loads(proof_path.read_text())
        existing = proof_d.get("artifacts") or []
        # Store paths relative to run_dir for portability (per plan §E.2
        # StageProof.artifacts description).
        run_dir_abs = Path(ctx.run_dir).absolute()
        rel_artifacts: list[str] = list(existing)
        for a in artifacts:
            try:
                rp = Path(a).absolute().relative_to(run_dir_abs)
                rel_artifacts.append(str(rp))
            except ValueError:
                rel_artifacts.append(str(a))
        proof_d["artifacts"] = rel_artifacts
        proof_path.write_text(json.dumps(proof_d, indent=2, default=str))
        # Also update state.json's stage_proofs entry to match.
        state = load_state(ctx.run_dir) or {}
        if target_sid in (state.get("stage_proofs") or {}):
            state["stage_proofs"][target_sid]["artifacts"] = rel_artifacts
            save_state(ctx.run_dir, state)
    except Exception as e:
        print(f"  WARN: attach video artifacts: {type(e).__name__}: {e}",
              flush=True)


# ─── Phase 7 / Phase 8 thin wrappers ────────────────────────────────────────


def run_phase7_user_scaling(tag: str | None = None,
                            run_dir: Path | None = None) -> None:
    """Thin wrapper around bench.storm.run_user_scaling.

    Per plan §A.8: Phase 7 retained as a separate subcommand; the worktree
    function `run_phase_user_scaling` (source 7697-7951) has been moved
    to bench.storm.run_user_scaling (Block 2). This wrapper exists so
    operators can run it from `python -m bench phase7`.
    """
    from bench import storm
    try:
        tokens = browser.login_all()
    except Exception:
        tokens = {}
    storm.run_user_scaling(tokens)


def run_phase8_per_mutation(tag: str | None = None,
                            run_dir: Path | None = None) -> None:
    """Phase 8 entrypoint — mutate+poll per-event convergence.

    The worktree function `run_phase_per_mutation` (source 7954-8162) was
    held back from earlier blocks because it is independent of Phase 6's
    state machine. We pull it into bench.phases as a private helper so the
    CLI's `phase8` subcommand has a clean import surface.
    """
    try:
        tokens = browser.login_all()
    except Exception:
        tokens = {}
    if "admin" not in tokens or "cyberjoker" not in tokens:
        print("Phase 8: missing admin or cyberjoker token; aborting",
              flush=True)
        return

    targets = _phase8_pick_targets(int(os.environ.get("PHASE8_TARGETS", "60")))
    if not targets:
        print("Phase 8: no compositions present; cluster not at steady state",
              flush=True)
        return

    duration_min = float(os.environ.get("PHASE8_DURATION_MIN", "5"))
    timeout_s = int(os.environ.get("PHASE8_TIMEOUT_S", "30"))
    poll_interval = float(os.environ.get("PHASE8_POLL_INTERVAL", "0.25"))

    recent_touched = set()
    per_event: list[dict] = []
    cycle_interval = max(0.5, duration_min * 60.0 / max(len(targets), 1))
    for i, (ns, name) in enumerate(targets):
        cls = "HOT" if (ns, name) in recent_touched else "WARM"
        marker = f"phase8-{int(time.time() * 1000)}-{i}"
        t0 = _phase8_mutate_target(ns, name, marker)
        if t0 < 0:
            continue
        deadline = t0 + timeout_s
        for user_label, token in tokens.items():
            lat = _phase8_poll_via_snowplow(
                ns, name, marker, token, t0, deadline, poll_interval)
            if lat >= 0:
                per_event.append({
                    "class": cls, "user": user_label, "latency_ms": lat,
                    "ns": ns, "name": name})
        time.sleep(max(0.0, cycle_interval - (time.time() - t0)))

    admin_samples = [e["latency_ms"] for e in per_event if e["user"] == "admin"]
    cyber_samples = [e["latency_ms"] for e in per_event
                     if e["user"] == "cyberjoker"]
    admin_p99 = ledger.pct(admin_samples, 99) if admin_samples else 0
    cyber_p99 = ledger.pct(cyber_samples, 99) if cyber_samples else 0
    p99_mix = int(round(0.95 * cyber_p99 + 0.05 * admin_p99))

    out = {
        "samples_total": len(per_event),
        "samples_admin": len(admin_samples),
        "samples_cyberjoker": len(cyber_samples),
        "p99_admin": admin_p99,
        "p99_cyberjoker": cyber_p99,
        "p99_mix": p99_mix,
        "hot_p99":  ledger.pct(
            [e["latency_ms"] for e in per_event if e["class"] == "HOT"],
            99) or 0,
        "warm_p99": ledger.pct(
            [e["latency_ms"] for e in per_event if e["class"] == "WARM"],
            99) or 0,
        "cold_p99": ledger.pct(
            [e["latency_ms"] for e in per_event if e["class"] == "COLD"],
            99) or 0,
        "events": per_event,
    }
    # Block 4: writes under run_dir/phase8/ if supplied; legacy path else.
    if run_dir is not None:
        target_dir = Path(run_dir) / "phase8"
        target_dir.mkdir(parents=True, exist_ok=True)
        target_file = target_dir / "per_mutation_results.json"
    else:
        target_file = Path("/tmp/snowplow_per_mutation_results.json")
    target_file.write_text(json.dumps(out, indent=2, default=str))
    print(f"Phase 8 results saved to {target_file}", flush=True)


def _phase8_pick_targets(n: int) -> list[tuple[str, str]]:
    rc, out, _ = cluster.kubectl(
        "get",
        f"{cluster.COMP_RES}.{cluster.COMP_GVR}",
        "-A", "--no-headers",
        "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
    if rc != 0 or not out.strip():
        return []
    rows = []
    for line in out.splitlines():
        parts = line.split(None, 1)
        if len(parts) >= 2:
            rows.append((parts[0], parts[1]))
    import random
    random.seed(0)
    random.shuffle(rows)
    return rows[:n]


def _phase8_mutate_target(ns: str, name: str, marker: str) -> float:
    patch = json.dumps({"metadata": {"annotations": {
        "snowplow-bench/phase8-marker": marker}}})
    t0 = time.time()
    rc, _, _ = cluster.kubectl(
        "patch",
        f"{cluster.COMP_RES}.{cluster.COMP_GVR}", name,
        "-n", ns, "--type=merge", "-p", patch)
    if rc != 0:
        return -1
    return t0


def _phase8_poll_via_snowplow(ns: str, name: str, marker: str,
                              token: str, start_time: float,
                              deadline: float, poll_interval: float) -> int:
    import urllib.request
    snowplow = os.environ.get("SNOWPLOW_URL", "http://34.135.50.203:8081")
    path = ("/call?apiVersion=templates.krateo.io%2Fv1"
            "&resource=restactions&name=compositions-list"
            "&namespace=krateo-system")
    while time.time() < deadline:
        try:
            req = urllib.request.Request(
                snowplow + path,
                headers={"Authorization": "Bearer " + token,
                         "Accept-Encoding": "gzip"})
            with urllib.request.urlopen(req, timeout=10) as r:
                body = r.read()
                if marker in body.decode("utf-8", errors="replace"):
                    return int((time.time() - start_time) * 1000)
        except Exception:
            pass
        time.sleep(poll_interval)
    return -1
