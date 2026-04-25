package cmd

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHarness13_BareClose_Reason replays the oc-8wj pattern: closes with
// content-less reasons must be rejected with exit code 2 (Rule 1).
//
// This is the unit-level contract — runCloseGate is invoked with synthetic
// inputs, no bd lookup. Rule 1 runs before bd is ever called.
func TestHarness13_BareClose_Reason(t *testing.T) {
	cases := []struct {
		name     string
		reason   string
		hasFlag  bool // true = --reason was passed at all
		wantCode int
	}{
		{name: "missing-reason", reason: "", hasFlag: false, wantCode: closeGateExitReason},
		{name: "empty-string", reason: "", hasFlag: true, wantCode: closeGateExitReason},
		{name: "whitespace-only", reason: "   \t\n  ", hasFlag: true, wantCode: closeGateExitReason},
		{name: "literal-Closed", reason: "Closed", hasFlag: true, wantCode: closeGateExitReason},
		{name: "literal-closed", reason: "closed", hasFlag: true, wantCode: closeGateExitReason},
		{name: "literal-done", reason: "done", hasFlag: true, wantCode: closeGateExitReason},
		{name: "literal-fixed", reason: "fixed", hasFlag: true, wantCode: closeGateExitReason},
		{name: "single-emoji", reason: "✅", hasFlag: true, wantCode: closeGateExitReason},
		{name: "punct-only", reason: "...", hasFlag: true, wantCode: closeGateExitReason},
		{name: "too-short", reason: "ok looks fine", hasFlag: true, wantCode: closeGateExitReason},
		{name: "deliverable-reason", reason: "added idempotency key to webhook handler, covered by TestRefund", hasFlag: true, wantCode: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gateInputs{reason: tc.reason, hasReason: tc.hasFlag}
			// No beadIDs — Rule 1 should resolve before bead lookup.
			code, err := runCloseGate(g)
			if code != tc.wantCode {
				t.Fatalf("exit code: got %d, want %d (err=%v)", code, tc.wantCode, err)
			}
			if tc.wantCode != 0 && err == nil {
				t.Fatal("expected non-nil error on rejection")
			}
		})
	}
}

// TestHarness13_BareReason_PureFunction tests validateCloseReason directly
// for finer-grained Rule 1 coverage (single-rune surrogates, mixed scripts).
func TestHarness13_BareReason_PureFunction(t *testing.T) {
	good := []string{
		"added idempotency key to webhook handler",
		"리팩토링: convoy launch path 의 deadlock 제거 — TestConvoyDeadlock 추가",
		"reverted 51e28794d, blocked downstream regression",
	}
	for _, g := range good {
		if err := validateCloseReason(g); err != nil {
			t.Errorf("expected accept, got reject: %q -> %v", g, err)
		}
	}
	bad := []string{
		"", "   ", "\t\n", "Closed", "DONE", "lgtm", "✅", "...",
		"ok cool", "fix done", "x",
	}
	for _, b := range bad {
		if err := validateCloseReason(b); err == nil {
			t.Errorf("expected reject, got accept: %q", b)
		}
	}
}

// TestHarness13_SkipPath verifies Rule 3: explicit --skip-gate bypasses the
// generic-reason rejection but still requires non-empty content.
func TestHarness13_SkipPath(t *testing.T) {
	t.Run("skip-allows-generic-reason", func(t *testing.T) {
		g := gateInputs{
			reason:    "hotfix: outage triage",
			hasReason: true,
			skipGate:  true,
		}
		code, err := runCloseGate(g)
		if code != 0 {
			t.Fatalf("skip path should accept generic-but-nonempty reason: got code=%d err=%v", code, err)
		}
	})
	t.Run("skip-still-rejects-empty", func(t *testing.T) {
		g := gateInputs{reason: "", hasReason: true, skipGate: true}
		code, _ := runCloseGate(g)
		if code != closeGateExitReason {
			t.Fatalf("skip path must still reject empty reason: got code=%d", code)
		}
	})
	t.Run("skip-still-rejects-whitespace", func(t *testing.T) {
		g := gateInputs{reason: "   ", hasReason: true, skipGate: true}
		code, _ := runCloseGate(g)
		if code != closeGateExitReason {
			t.Fatalf("skip path must still reject whitespace-only reason: got code=%d", code)
		}
	})
}

// TestHarness13_ShouldSkipGate covers Rule 3 trigger detection.
func TestHarness13_ShouldSkipGate(t *testing.T) {
	cases := []struct {
		name     string
		info     *gateBeadInfo
		hasFlag  bool
		wantSkip bool
	}{
		{name: "no-info-no-flag", info: nil, hasFlag: false, wantSkip: false},
		{name: "explicit-flag", info: nil, hasFlag: true, wantSkip: true},
		{name: "skip-label", info: &gateBeadInfo{Labels: []string{"close-gate:skip"}}, wantSkip: true},
		{name: "advisory-type", info: &gateBeadInfo{Type: "advisory"}, wantSkip: true},
		{name: "routing-type", info: &gateBeadInfo{Type: "routing"}, wantSkip: true},
		{name: "note-type", info: &gateBeadInfo{Type: "note"}, wantSkip: true},
		{name: "case-insensitive-type", info: &gateBeadInfo{Type: "Advisory"}, wantSkip: true},
		{name: "unrelated-label", info: &gateBeadInfo{Labels: []string{"role:polecat"}}, wantSkip: false},
		{name: "task-no-flag", info: &gateBeadInfo{Type: "task"}, wantSkip: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSkipGate(tc.info, tc.hasFlag); got != tc.wantSkip {
				t.Fatalf("shouldSkipGate: got %v, want %v", got, tc.wantSkip)
			}
		})
	}
}

// TestHarness13_FalseClose_ZeroCommits replays the oc-zf2 pattern: a polecat
// branch with zero commits vs main is detected by branchCommitCount and
// rejected by validatePolecatCommits with a non-nil error.
//
// Uses a real git fixture so the test exercises the same code path the gate
// uses in production (no mocking the boundary).
func TestHarness13_FalseClose_ZeroCommits(t *testing.T) {
	dir := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}

	run("git", "init", "-q", "-b", "main")
	run("git", "config", "user.email", "test@harness13.local")
	run("git", "config", "user.name", "test")
	// Seed a commit on main so rev-list has something to compare against.
	run("sh", "-c", "echo seed > seed && git add seed && git commit -q -m seed")
	// Create a polecat branch with zero new commits vs main.
	run("git", "checkout", "-q", "-b", "polecat/test/hq-fakebead@abcd")

	// chdir to fixture for git rev-list to find the repo.
	t.Chdir(dir)

	branch := "polecat/test/hq-fakebead@abcd"
	n, err := branchCommitCount(branch)
	if err != nil {
		t.Fatalf("branchCommitCount: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 commits on fresh polecat branch, got %d", n)
	}

	// validatePolecatCommits when there's a polecat label and zero commits.
	info := &gateBeadInfo{ID: "hq-fakebead", Labels: []string{"role:polecat"}}
	err = validatePolecatCommits(info, "hq-fakebead")
	if err == nil {
		t.Fatal("expected zero-commit polecat close to be rejected")
	}
	if !strings.Contains(err.Error(), "zero commits") &&
		!strings.Contains(err.Error(), "no matching branch") {
		t.Errorf("rejection should mention zero commits or missing branch; got: %v", err)
	}
}

// TestHarness13_PolecatWithCommits_Allowed verifies the positive case for
// Rule 2: a polecat branch with at least one commit is not rejected by Rule 2.
func TestHarness13_PolecatWithCommits_Allowed(t *testing.T) {
	dir := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main")
	run("git", "config", "user.email", "test@harness13.local")
	run("git", "config", "user.name", "test")
	run("sh", "-c", "echo seed > seed && git add seed && git commit -q -m seed")
	run("git", "checkout", "-q", "-b", "polecat/test/hq-realbead@efgh")
	run("sh", "-c", "echo work > work && git add work && git commit -q -m 'real deliverable'")

	t.Chdir(dir)

	info := &gateBeadInfo{ID: "hq-realbead", Labels: []string{"role:polecat"}}
	err := validatePolecatCommits(info, "hq-realbead")
	if err != nil {
		t.Fatalf("expected polecat close with commits to be allowed; got: %v", err)
	}
}

// TestHarness13_ExtractGateInputs covers the flag-parsing surface the gate
// relies on (DisableFlagParsing means we hand-parse).
func TestHarness13_ExtractGateInputs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantBead []string
		wantR    string
		wantHasR bool
		wantSkip bool
	}{
		{name: "reason-equals", args: []string{"hq-1", "--reason=fixed it well"}, wantBead: []string{"hq-1"}, wantR: "fixed it well", wantHasR: true},
		{name: "reason-space", args: []string{"hq-1", "--reason", "fixed it well"}, wantBead: []string{"hq-1"}, wantR: "fixed it well", wantHasR: true},
		{name: "skip-flag", args: []string{"hq-1", "--skip-gate", "--reason=hotfix outage"}, wantBead: []string{"hq-1"}, wantR: "hotfix outage", wantHasR: true, wantSkip: true},
		{name: "no-reason", args: []string{"hq-1"}, wantBead: []string{"hq-1"}},
		{name: "multi-bead", args: []string{"hq-1", "hq-2", "--reason=batch close, see PR #123"}, wantBead: []string{"hq-1", "hq-2"}, wantR: "batch close, see PR #123", wantHasR: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := extractGateInputs(tc.args)
			if g.reason != tc.wantR {
				t.Errorf("reason: got %q want %q", g.reason, tc.wantR)
			}
			if g.hasReason != tc.wantHasR {
				t.Errorf("hasReason: got %v want %v", g.hasReason, tc.wantHasR)
			}
			if g.skipGate != tc.wantSkip {
				t.Errorf("skipGate: got %v want %v", g.skipGate, tc.wantSkip)
			}
			if len(g.beadIDs) != len(tc.wantBead) {
				t.Errorf("beadIDs: got %v want %v", g.beadIDs, tc.wantBead)
			} else {
				for i := range g.beadIDs {
					if g.beadIDs[i] != tc.wantBead[i] {
						t.Errorf("beadIDs[%d]: got %q want %q", i, g.beadIDs[i], tc.wantBead[i])
					}
				}
			}
		})
	}
}

// TestHarness13_StripSkipGate ensures the gt-only flag isn't passed to bd.
func TestHarness13_StripSkipGate(t *testing.T) {
	in := []string{"hq-1", "--skip-gate", "--reason=hotfix"}
	out := stripSkipGateFlag(in)
	for _, a := range out {
		if a == "--skip-gate" {
			t.Fatalf("--skip-gate should be stripped before bd delegation, got: %v", out)
		}
	}
	if len(out) != 2 {
		t.Errorf("expected 2 args after strip, got %d: %v", len(out), out)
	}
}

// withinSourceRoot is a minimal sanity guard so test fixtures don't accidentally
// touch the repo. Used by the regression replay tests below.
func withinSourceRoot(t *testing.T, dir string) bool {
	t.Helper()
	abs, _ := filepath.Abs(dir)
	return strings.Contains(abs, "/T/") || strings.Contains(abs, "/tmp/")
}
