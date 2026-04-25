package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/closegate"
)

// HARNESS-13: bd-close validation gate. Blocks FALSE-CLOSE patterns observed in
// oc-zf2 / oc-8wj (closes with no deliverable) before they reach the database.
//
// See: ~/gt/occultfusion/crew/atlas/reports/2026-04-25_harness-13_bd-close-validation-gate-design.md

// closeGateExitReason  = exit code 2 (Rule 1 violation: bad reason).
// closeGateExitNoWork  = exit code 3 (Rule 2 violation: polecat with 0 commits).
const (
	closeGateExitReason = 2
	closeGateExitNoWork = 3
)

// validateCloseReason delegates to the shared closegate.ValidateReason
// (Rule 1). It is a thin wrapper kept here so test code in this package
// can reach it without importing closegate.
func validateCloseReason(reason string) error { return closegate.ValidateReason(reason) }

// gateBeadInfo is the subset of `bd show --json` output the gate cares about.
// Distinct from the broader beadInfo used by sling/scheduler code.
type gateBeadInfo struct {
	ID     string   `json:"id"`
	Type   string   `json:"issue_type"`
	Status string   `json:"status"`
	Labels []string `json:"labels"`
}

// fetchBeadInfo runs `bd show <id> --json` against the rig that owns the bead
// and returns the parsed info. Failure to fetch (e.g., bd offline, unknown ID)
// returns nil with a non-nil error so callers can fail open or escalate.
func fetchBeadInfo(beadID string) (*gateBeadInfo, error) {
	dir := resolveBeadDir(beadID)
	if dir == "" || dir == "." {
		dir = "."
	}
	bd := BdCmd("show", beadID, "--json").Dir(dir).StripBeadsDir()
	out, err := bd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd show %s: %w", beadID, err)
	}
	// `bd show <id> --json` may return either a single object or a list.
	trimmed := strings.TrimSpace(string(out))
	if strings.HasPrefix(trimmed, "[") {
		var arr []gateBeadInfo
		if err := json.Unmarshal(out, &arr); err != nil {
			return nil, fmt.Errorf("parse bd show output: %w", err)
		}
		if len(arr) == 0 {
			return nil, fmt.Errorf("bead %s not found", beadID)
		}
		return &arr[0], nil
	}
	var info gateBeadInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parse bd show output: %w", err)
	}
	return &info, nil
}

// gateHasLabel reports whether labels contains the given label (case-sensitive).
// Local to the close gate to avoid collision with internal/cmd's existing helper
// (which uses a different label-shape elsewhere).
func gateHasLabel(labels []string, target string) bool {
	for _, l := range labels {
		if l == target {
			return true
		}
	}
	return false
}

// shouldSkipGate decides whether to bypass Rule 2 entirely.
// Rule 1 is partially bypassed: empty reason is still rejected even on skip
// path (a content-less close is never informative — Atlas design §3).
//
// Skip triggers (any one):
//  1. --skip-gate flag (caller asserts skip; auditable via gt:close-gate-skip label)
//  2. close-gate:skip label on the bead
//  3. issue_type in {advisory, routing, note}
func shouldSkipGate(info *gateBeadInfo, hasSkipFlag bool) bool {
	if hasSkipFlag {
		return true
	}
	if info == nil {
		return false
	}
	if gateHasLabel(info.Labels, "close-gate:skip") {
		return true
	}
	switch strings.ToLower(info.Type) {
	case "advisory", "routing", "note":
		return true
	}
	return false
}

// detectPolecatBranch returns the polecat branch name owning beadID, or "" if
// no such branch exists. We probe `polecat/<assignee-slug>/<beadID>@*` and the
// older `polecat/<anyone>/<beadID>` shape used by some rigs.
func detectPolecatBranch(beadID string) string {
	// Search by bead-id substring across all polecat/* branches.
	out, err := exec.Command("git", "branch", "--list", "--format=%(refname:short)",
		fmt.Sprintf("polecat/*%s*", beadID)).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Match either polecat/<x>/<bead>@<id> or polecat/<x>/<bead>
		if strings.Contains(line, beadID) {
			return line
		}
	}
	return ""
}

// branchCommitCount returns the number of commits on `branch` not yet on `main`.
// Used by Rule 2 to detect zero-deliverable polecat closes.
func branchCommitCount(branch string) (int, error) {
	out, err := exec.Command("git", "rev-list", "--count", "main.."+branch).Output()
	if err != nil {
		return 0, fmt.Errorf("git rev-list main..%s: %w", branch, err)
	}
	n := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n); err != nil {
		return 0, fmt.Errorf("parse rev-list count %q: %w", string(out), err)
	}
	return n, nil
}

// isPolecatClose decides whether Rule 2 applies. Trigger conditions per Atlas
// design: bead has role:polecat label OR caller is a polecat process
// (GT_ROLE matches "*polecat*") OR a matching polecat branch exists.
func isPolecatClose(info *gateBeadInfo, branch string) bool {
	if info != nil && gateHasLabel(info.Labels, "role:polecat") {
		return true
	}
	if envRole := os.Getenv(EnvGTRole); strings.Contains(strings.ToLower(envRole), "polecat") {
		return true
	}
	if branch != "" {
		return true
	}
	return false
}

// validatePolecatCommits implements Rule 2 (polecat zero-commit detection).
// Returns nil if the bead is not in scope or the branch has commits; returns
// a non-nil error describing the violation otherwise.
//
// "Not in scope" means the bead has no role:polecat label, the caller is not
// a polecat process, and no polecat branch matches the bead-id. In that case
// we leave Rule 2 silent — Rule 1 still applies.
func validatePolecatCommits(info *gateBeadInfo, beadID string) error {
	branch := detectPolecatBranch(beadID)
	if !isPolecatClose(info, branch) {
		return nil
	}
	// If we marked it polecat by env/label but no branch found, treat as
	// zero-commit — there's no deliverable to point to.
	if branch == "" {
		return fmt.Errorf("polecat close attempted but no matching branch (polecat/.../%s) exists", beadID)
	}
	n, err := branchCommitCount(branch)
	if err != nil {
		// Fail open on git errors — Rule 1 + Witness backstop still apply.
		// Surface the error to stderr so operators can see misconfiguration.
		fmt.Fprintf(os.Stderr, "harness-13: rule-2 git probe failed (%v); allowing close\n", err)
		return nil
	}
	if n == 0 {
		return fmt.Errorf("polecat branch %s has zero commits vs main — no deliverable to close", branch)
	}
	return nil
}

// escalateFalseClose calls `gt escalate -s HIGH` to alert the Witness of an
// attempted FALSE-CLOSE. Errors from the escalate call are logged but never
// block the rejection — the wrapper still exits non-zero. Indirected through
// a package var so tests can swap in a no-op without spawning subprocesses.
var escalateFalseClose = func(beadID, reason string) {
	msg := fmt.Sprintf("FALSE-CLOSE attempt: %s — %s", beadID, reason)
	cmd := exec.Command("gt", "escalate", "-s", "HIGH", msg)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness-13: escalate failed: %v\n", err)
	}
}

// applySkipLabel adds gt:close-gate-skip to the bead so Scrutor's skip-rate
// audit can count it. Best-effort: failure is logged but does not block close.
func applySkipLabel(beadID string) {
	dir := resolveBeadDir(beadID)
	if dir == "" {
		dir = "."
	}
	if err := BdCmd("update", beadID, "--add-labels", "gt:close-gate-skip").
		Dir(dir).StripBeadsDir().Run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness-13: failed to add gt:close-gate-skip to %s: %v\n", beadID, err)
	}
}

// gateInputs is the parsed view of the close command's user input that the
// validation gate needs. We compute it once from the raw args (because close.go
// uses DisableFlagParsing) and pass it through.
type gateInputs struct {
	beadIDs   []string
	reason    string
	skipGate  bool
	hasReason bool
}

// extractGateInputs parses the relevant flags from the raw close args.
// It does not consume args (close.go still passes them through to bd close
// after stripping --skip-gate and --reason injection).
func extractGateInputs(args []string) gateInputs {
	g := gateInputs{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--skip-gate":
			g.skipGate = true
		case a == "--reason" || a == "-r":
			if i+1 < len(args) {
				g.reason = args[i+1]
				g.hasReason = true
				i++
			}
		case strings.HasPrefix(a, "--reason="):
			g.reason = strings.TrimPrefix(a, "--reason=")
			g.hasReason = true
		}
	}
	g.beadIDs = extractBeadIDs(args)
	return g
}

// stripSkipGateFlag removes --skip-gate from args so it isn't passed to bd
// (bd doesn't know the flag). Returns the filtered args.
func stripSkipGateFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--skip-gate" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// runCloseGate is the orchestrator: validates Rule 1 always, looks up bead
// info to decide skip eligibility, then validates Rule 2 unless skipped.
// Returns (exitCode, error). exitCode 0 means proceed; non-zero means abort
// with the given code (caller should os.Exit).
//
// The function writes user-facing guidance to stderr on rejection. The error
// is for caller chaining (e.g., to surface in test output).
func runCloseGate(g gateInputs) (int, error) {
	// Rule 1 applies even when --skip-gate is set: a content-less close is
	// never informative (Atlas design §3, line 45). The skip path bypasses
	// only the strict thresholds that block "ok" / "fixed" / <20-char reasons.
	if g.hasReason {
		if err := validateCloseReason(g.reason); err != nil {
			// On --skip-gate we tolerate generic short reasons (the audit
			// label captures intent) BUT not empty/whitespace.
			if g.skipGate && strings.TrimSpace(g.reason) != "" {
				// allow
			} else {
				fmt.Fprintf(os.Stderr,
					"harness-13: close reason rejected: %s\n"+
						"  Provide a description of the deliverable, e.g.,\n"+
						"  --reason=\"added idempotency key to webhook handler, covered by TestRefundIdempotency\"\n",
					err)
				return closeGateExitReason, err
			}
		}
	} else {
		fmt.Fprintf(os.Stderr,
			"harness-13: --reason is required for close\n"+
				"  Provide a description of the deliverable.\n")
		return closeGateExitReason, fmt.Errorf("--reason is required")
	}

	// One bead-id per gate run is the common case (bd close is variadic but
	// the validations need per-bead context). Loop over each.
	for _, id := range g.beadIDs {
		info, err := fetchBeadInfo(id)
		if err != nil {
			// Fail open on bd lookup errors. Witness backstop covers the case.
			fmt.Fprintf(os.Stderr, "harness-13: bd show %s failed (%v); skipping pre-validation\n", id, err)
			continue
		}

		if shouldSkipGate(info, g.skipGate) {
			applySkipLabel(id)
			continue
		}

		if err := validatePolecatCommits(info, id); err != nil {
			fmt.Fprintf(os.Stderr,
				"harness-13: close rejected for %s: %s\n"+
					"  If this is a no-code close (review, routing), use --skip-gate with a justification reason\n"+
					"  or add the close-gate:skip label to the bead.\n",
				id, err)
			escalateFalseClose(id, err.Error())
			return closeGateExitNoWork, err
		}
	}

	return 0, nil
}
