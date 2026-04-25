# HARNESS-13: bd-close validation gate

Defense-in-depth gate that blocks FALSE-CLOSE patterns observed in oc-zf2,
oc-8wj, and ~5,000 historical bare-reason closes. Two layers:

| Layer | Where                                            | Code                                                 | When                       |
|-------|--------------------------------------------------|------------------------------------------------------|----------------------------|
| A     | `gt close` wrapper                               | `internal/cmd/close_validation.go`                   | pre-write (rejects)        |
| B     | Witness patrol                                   | `internal/witness/false_close.go`                    | post-write (reopens)       |

Shared rule logic lives in `internal/closegate` so the layers can never drift.

## Rules

**Rule 1 — Reason content (Layer A + B):**
- Reject empty, whitespace-only, or single-emoji reasons.
- Reject case-insensitive matches of `closed`, `close`, `done`, `fixed`, `ok`,
  `complete`, `completed`, `lgtm`, `merged`.
- Reject reasons shorter than 20 runes (Atlas-recommended threshold —
  median of recent non-regression closes).
- Layer A: exit code 2.
- Layer B: reopen the bead, attach `gt:close-gate-reopened` label, append
  comment `[HARNESS-13 gate] reopened by Witness backstop: ...`, escalate.

**Rule 2 — Polecat zero-commit (Layer A only):**

A close is "in scope" for Rule 2 when any of the following is true:
- Bead has the `role:polecat` label.
- Caller's `GT_ROLE` contains `polecat`.
- A branch matching `polecat/*<bead-id>*` exists locally.

When in scope, run `git rev-list --count main..<branch>`. Zero commits =
no deliverable = exit code 3 + `gt escalate -s HIGH`.

Layer B does **not** re-apply Rule 2: the polecat branch is often pruned
before the patrol arrives, and Witness should not be a git client (Atlas
bias #26 callout in design).

**Rule 3 — Skip path (both layers):**

Rule 1 runs even on skip path (a content-less close is never informative).
Rule 2 is bypassed when:
- `--skip-gate` flag is set on `gt close`.
- Bead has `close-gate:skip` label.
- Bead's `issue_type` is `advisory`, `routing`, or `note`.

Skipped closes get the `gt:close-gate-skip` label so Scrutor's weekly
audit can count them. Skip rate >10% triggers an audit flag.

## Use

```bash
# Normal close (must describe deliverable)
gt close hq-abc --reason "added idempotency key to webhook handler, covered by TestRefund"

# Hotfix / outage triage (skip Rule 2, still need non-empty reason)
gt close hq-abc --skip-gate --reason "hotfix: SEV-1 outage triage, see #4321"

# Cascade close (children inherit parent reason)
gt close hq-abc --cascade --reason "..."
```

## Bypass detection

Direct `bd close` calls bypass Layer A. The Witness patrol re-applies
Rule 1 to beads closed within the last 5 minutes (configurable via
`witness.DefaultFalseCloseLookback`). On violation:

1. `bd reopen <bead>`
2. `bd label add <bead> gt:close-gate-reopened` (idempotency marker — a
   second reopen attempt on the same bead is skipped, preventing loops
   when an agent immediately re-closes).
3. `bd comment <bead> --body "[HARNESS-13 gate] reopened by Witness
   backstop: <violation>"`.
4. `gt escalate -s HIGH "HARNESS-13 backstop reopened <bead> — <violation>"`.

## Observable delta

**Before-state baseline** (captured 2026-04-26 pre-merge):
- 198,809 closed beads in HQ.
- **5,012 bare-reason closes**: 2,542 empty + 2,470 literal `closed`.
- ~50% of historical closes have non-informative reasons.

Artifact: `~/gt/deacon/harness-13/baseline-2026-04-26.json` (12MB).

**After-state expectations** (to record 7 days post-merge):
- New bare-reason close count → **0** for `gt close`-routed closes.
- Witness reopens > 0 in first 72h (any agent that bypasses via direct
  `bd close` will trigger Layer B).
- False-positive budget: <5 rejections/week of legitimate closes.

## Acceptance gates (all required for merge)

- [x] Rule 1 unit tests — `TestHarness13_BareClose_Reason`, `TestHarness13_BareReason_PureFunction`, `TestValidateReason`
- [x] Rule 2 fixture test — `TestHarness13_FalseClose_ZeroCommits`, `TestHarness13_PolecatWithCommits_Allowed`
- [x] Rule 3 tests — `TestHarness13_SkipPath`, `TestHarness13_ShouldSkipGate`
- [x] Layer B integration — `TestHarness13_WitnessReopensBypass`, `TestHarness13_Witness_Idempotent`, `TestHarness13_Witness_SkipLabel`, `TestHarness13_Witness_SkipType`, `TestHarness13_Witness_OutOfWindow`, `TestHarness13_Witness_ValidCloseUnchanged`, `TestHarness13_Witness_PartialFailure`
- [x] Before-state baseline captured (artifact above)
- [x] oc-zf2 / oc-8wj replay covered by Tests 1 & 2
- [ ] Post-deploy: 1+ Witness reopen observed in 72h (or zero observed
      with explanation) — verified via `bd list --label
      gt:close-gate-reopened`.

## References

- Atlas design spec: `~/gt/occultfusion/crew/atlas/reports/2026-04-25_harness-13_bd-close-validation-gate-design.md`
- Origin bead: hq-5zln4
- Implementation bead: hq-uv7a2
- Regression cases: oc-zf2, oc-8wj
- Munger deployment-validation discipline (CLAUDE.md, 2026-04-22)
