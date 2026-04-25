package witness

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/closegate"
)

// HARNESS-13 Layer B: post-write FALSE-CLOSE detection.
//
// Layer A (gt close wrapper) blocks regressions before bd writes — but agents
// can still bypass by calling `bd close` directly. This handler runs on the
// Witness patrol cadence, re-applies Rule 1 to recently-closed beads, and
// reopens any that violate it. The reopen is annotated with a comment so
// the offending agent sees feedback on the next interaction.
//
// Design ref: ~/gt/occultfusion/crew/atlas/reports/2026-04-25_harness-13_bd-close-validation-gate-design.md §"Layer B"

// DefaultFalseCloseLookback is the default window for "recently closed" — the
// patrol re-checks closes within this many minutes back.
const DefaultFalseCloseLookback = 5 * time.Minute

// FalseCloseAction is the action the Witness took on a detected violation.
type FalseCloseAction string

const (
	FalseCloseReopened FalseCloseAction = "reopened"
	// FalseCloseSkippedIdempotent: already has gt:close-gate-reopened label.
	FalseCloseSkippedIdempotent FalseCloseAction = "skipped-idempotent"
	// FalseCloseSkippedAllowed: bead has close-gate:skip label or skip-eligible type.
	FalseCloseSkippedAllowed FalseCloseAction = "skipped-allowed"
	// FalseCloseError: detected violation but reopen / comment / escalate failed.
	FalseCloseError FalseCloseAction = "error"
)

// FalseCloseResult describes one bead inspected by the patrol.
type FalseCloseResult struct {
	BeadID      string
	CloseReason string
	Violation   string // human-readable rule violation, e.g. "close reason 'Closed' is too generic"
	Action      FalseCloseAction
	Err         error
}

// DetectFalseClosesResult is the patrol cycle's output.
type DetectFalseClosesResult struct {
	Inspected int
	Reopened  int
	Skipped   int
	Errors    int
	Items     []FalseCloseResult
}

// closedBead is the subset of `bd list` JSON the patrol consumes.
// Local to this file — handlers.go has its own beadInfo for unrelated work.
type closedBead struct {
	ID          string   `json:"id"`
	Status      string   `json:"status"`
	IssueType   string   `json:"issue_type"`
	ClosedAt    string   `json:"closed_at"`
	CloseReason string   `json:"close_reason"`
	Labels      []string `json:"labels"`
}

// reopenLabel is the idempotency marker. A bead carrying this label has
// already been reopened by Layer B once; skip on subsequent passes so we
// don't enter a reopen-loop with an agent that re-closes immediately.
const reopenLabel = "gt:close-gate-reopened"

// skipLabel matches the Layer A skip path. Layer B honours it identically.
const skipLabel = "close-gate:skip"

// DetectFalseCloses inspects beads closed within `lookback` and reopens any
// whose close_reason fails Rule 1. It calls bd in `workDir` (the rig dir).
//
// This intentionally only re-applies Rule 1. Rule 2 (polecat zero-commit
// detection) requires a git repo and live branches; by the time a Witness
// patrol runs, the polecat branch may have been pruned. Layer A is the
// place to enforce Rule 2; Layer B is the safety net for content rules.
func DetectFalseCloses(bd *BdCli, workDir string, lookback time.Duration) *DetectFalseClosesResult {
	result := &DetectFalseClosesResult{}

	cutoff := time.Now().UTC().Add(-lookback)

	// `bd list --status=closed --json` returns recent closes. We rely on the
	// natural ordering (recent first) and stop scanning once we cross cutoff.
	out, err := bd.Exec(workDir, "list", "--status=closed", "--limit", "200", "--json")
	if err != nil {
		result.Errors++
		return result
	}

	var beads []closedBead
	if err := json.Unmarshal([]byte(out), &beads); err != nil {
		result.Errors++
		return result
	}

	for _, b := range beads {
		closedAt, err := time.Parse(time.RFC3339, b.ClosedAt)
		if err != nil {
			// Unparsable timestamp — skip rather than misclassify.
			continue
		}
		if closedAt.Before(cutoff) {
			// bd list returns most-recent-first; we can stop here.
			break
		}
		result.Inspected++

		item := FalseCloseResult{BeadID: b.ID, CloseReason: b.CloseReason}

		if hasLabelStr(b.Labels, reopenLabel) {
			item.Action = FalseCloseSkippedIdempotent
			result.Skipped++
			result.Items = append(result.Items, item)
			continue
		}
		if hasLabelStr(b.Labels, skipLabel) || isSkipType(b.IssueType) {
			item.Action = FalseCloseSkippedAllowed
			result.Skipped++
			result.Items = append(result.Items, item)
			continue
		}

		if vErr := closegate.ValidateReason(b.CloseReason); vErr != nil {
			item.Violation = vErr.Error()
			if err := reopenWithMarker(bd, workDir, b.ID, vErr.Error()); err != nil {
				item.Action = FalseCloseError
				item.Err = err
				result.Errors++
			} else {
				item.Action = FalseCloseReopened
				result.Reopened++
			}
		}
		result.Items = append(result.Items, item)
	}

	return result
}

func isSkipType(t string) bool {
	switch strings.ToLower(t) {
	case "advisory", "routing", "note":
		return true
	}
	return false
}

func hasLabelStr(labels []string, target string) bool {
	for _, l := range labels {
		if l == target {
			return true
		}
	}
	return false
}

// reopenWithMarker reopens the bead, attaches the idempotency label, appends
// a comment naming the violation, and escalates to the mayor. Any single
// step that fails returns an error but we attempt the rest of the chain so
// observability isn't lost on partial failure.
func reopenWithMarker(bd *BdCli, workDir, beadID, violation string) error {
	var firstErr error

	if err := bd.Run(workDir, "reopen", beadID); err != nil {
		firstErr = fmt.Errorf("bd reopen %s: %w", beadID, err)
	}

	if err := bd.Run(workDir, "label", "add", beadID, reopenLabel); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("bd label add %s %s: %w", beadID, reopenLabel, err)
	}

	comment := fmt.Sprintf("[HARNESS-13 gate] reopened by Witness backstop: %s", violation)
	if err := bd.Run(workDir, "comment", beadID, "--body", comment); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("bd comment %s: %w", beadID, err)
	}

	escalateFalseCloseToMayor(beadID, violation)

	return firstErr
}

// escalateFalseCloseToMayor mirrors the Layer A escalation. Best-effort —
// this never blocks the reopen flow. Indirected through a package var so
// tests can swap in a no-op without spawning real `gt escalate` subprocesses.
var escalateFalseCloseToMayor = func(beadID, violation string) {
	msg := fmt.Sprintf("HARNESS-13 backstop reopened %s — %s", beadID, violation)
	cmd := exec.Command("gt", "escalate", "-s", "HIGH", msg)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness-13: escalate failed for %s: %v\n", beadID, err)
	}
}
