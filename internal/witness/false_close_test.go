package witness

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureEscalations swaps escalateFalseCloseToMayor for the duration of the
// test, recording calls in a slice. Restores the original on cleanup.
type escalationCapture struct {
	mu    sync.Mutex
	calls []string
}

func captureEscalations(t *testing.T) *escalationCapture {
	t.Helper()
	cap := &escalationCapture{}
	orig := escalateFalseCloseToMayor
	escalateFalseCloseToMayor = func(beadID, violation string) {
		cap.mu.Lock()
		defer cap.mu.Unlock()
		cap.calls = append(cap.calls, fmt.Sprintf("%s: %s", beadID, violation))
	}
	t.Cleanup(func() { escalateFalseCloseToMayor = orig })
	return cap
}

func (c *escalationCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// fakeBdCli is a test BdCli that records invocations and serves canned `list` output.
type fakeBdCli struct {
	listOutput string
	calls      [][]string
	runErrs    map[string]error // first-arg-of-run -> err to return (e.g., "reopen" -> err)
}

func newFakeBd(listJSON string) *fakeBdCli {
	return &fakeBdCli{listOutput: listJSON, runErrs: map[string]error{}}
}

func (f *fakeBdCli) toBdCli() *BdCli {
	return &BdCli{
		Exec: func(_ string, args ...string) (string, error) {
			f.calls = append(f.calls, append([]string{"exec"}, args...))
			if len(args) > 0 && args[0] == "list" {
				return f.listOutput, nil
			}
			return "", nil
		},
		Run: func(_ string, args ...string) error {
			f.calls = append(f.calls, append([]string{"run"}, args...))
			if len(args) > 0 {
				if err, ok := f.runErrs[args[0]]; ok {
					return err
				}
			}
			return nil
		},
	}
}

func (f *fakeBdCli) ranWithFirstArg(want string) bool {
	for _, c := range f.calls {
		// c = ["run", arg0, arg1, ...]
		if len(c) >= 2 && c[0] == "run" && c[1] == want {
			return true
		}
	}
	return false
}

// makeListJSON marshals a slice of closed beads into the JSON shape `bd list` returns.
func makeListJSON(t *testing.T, items []closedBead) string {
	t.Helper()
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestHarness13_WitnessReopensBypass replays the regression: a direct
// `bd close <bead> --reason=Closed` (bypassing the Layer A wrapper) is
// detected by the Witness within the lookback window, reopened, labeled,
// commented on, and escalated.
func TestHarness13_WitnessReopensBypass(t *testing.T) {
	cap := captureEscalations(t)
	now := time.Now().UTC()
	items := []closedBead{
		{
			ID: "hq-bypass1", Status: "closed", IssueType: "task",
			ClosedAt: now.Add(-30 * time.Second).Format(time.RFC3339),
			CloseReason: "Closed",
			Labels: []string{"role:polecat"},
		},
	}
	fake := newFakeBd(makeListJSON(t, items))

	res := DetectFalseCloses(fake.toBdCli(), "/tmp/test", DefaultFalseCloseLookback)
	if cap.count() != 1 {
		t.Errorf("expected 1 escalation, got %d", cap.count())
	}

	if res.Inspected != 1 {
		t.Fatalf("Inspected: got %d, want 1", res.Inspected)
	}
	if res.Reopened != 1 {
		t.Fatalf("Reopened: got %d, want 1", res.Reopened)
	}
	if !fake.ranWithFirstArg("reopen") {
		t.Errorf("expected bd reopen call; got: %v", fake.calls)
	}
	if !fake.ranWithFirstArg("label") {
		t.Errorf("expected bd label add call; got: %v", fake.calls)
	}
	if !fake.ranWithFirstArg("comment") {
		t.Errorf("expected bd comment call; got: %v", fake.calls)
	}
	if len(res.Items) != 1 || res.Items[0].Action != FalseCloseReopened {
		t.Errorf("expected FalseCloseReopened item, got %+v", res.Items)
	}
	if res.Items[0].Violation == "" {
		t.Error("expected non-empty violation message")
	}
}

// TestHarness13_Witness_Idempotent: a bead already carrying gt:close-gate-reopened
// must not be reopened again (prevents reopen loops).
func TestHarness13_Witness_Idempotent(t *testing.T) {
	now := time.Now().UTC()
	items := []closedBead{
		{
			ID: "hq-loop", Status: "closed", IssueType: "task",
			ClosedAt: now.Add(-1 * time.Minute).Format(time.RFC3339),
			CloseReason: "closed",
			Labels: []string{"gt:close-gate-reopened"},
		},
	}
	fake := newFakeBd(makeListJSON(t, items))

	res := DetectFalseCloses(fake.toBdCli(), "/tmp/test", DefaultFalseCloseLookback)
	if res.Reopened != 0 {
		t.Fatalf("Reopened should be 0 on idempotency skip, got %d", res.Reopened)
	}
	if res.Skipped != 1 {
		t.Fatalf("Skipped should be 1, got %d", res.Skipped)
	}
	if fake.ranWithFirstArg("reopen") {
		t.Error("must not call bd reopen on idempotent skip")
	}
}

// TestHarness13_Witness_SkipLabel: bead with close-gate:skip label is honoured.
func TestHarness13_Witness_SkipLabel(t *testing.T) {
	now := time.Now().UTC()
	items := []closedBead{
		{
			ID: "hq-skip", Status: "closed", IssueType: "task",
			ClosedAt: now.Add(-1 * time.Minute).Format(time.RFC3339),
			CloseReason: "Closed",
			Labels: []string{"close-gate:skip"},
		},
	}
	fake := newFakeBd(makeListJSON(t, items))

	res := DetectFalseCloses(fake.toBdCli(), "/tmp/test", DefaultFalseCloseLookback)
	if res.Reopened != 0 {
		t.Fatalf("skip-labelled bead must not be reopened: got %d reopens", res.Reopened)
	}
	if res.Items[0].Action != FalseCloseSkippedAllowed {
		t.Errorf("expected FalseCloseSkippedAllowed, got %v", res.Items[0].Action)
	}
}

// TestHarness13_Witness_SkipType: advisory / routing / note types skip.
func TestHarness13_Witness_SkipType(t *testing.T) {
	now := time.Now().UTC()
	items := []closedBead{
		{ID: "hq-adv", Status: "closed", IssueType: "advisory",
			ClosedAt: now.Add(-30 * time.Second).Format(time.RFC3339),
			CloseReason: "ok"},
		{ID: "hq-rt", Status: "closed", IssueType: "routing",
			ClosedAt: now.Add(-30 * time.Second).Format(time.RFC3339),
			CloseReason: "done"},
		{ID: "hq-note", Status: "closed", IssueType: "note",
			ClosedAt: now.Add(-30 * time.Second).Format(time.RFC3339),
			CloseReason: ""},
	}
	fake := newFakeBd(makeListJSON(t, items))

	res := DetectFalseCloses(fake.toBdCli(), "/tmp/test", DefaultFalseCloseLookback)
	if res.Reopened != 0 {
		t.Fatalf("skip-eligible types must not be reopened: got %d", res.Reopened)
	}
	if res.Skipped != 3 {
		t.Fatalf("expected 3 skips, got %d", res.Skipped)
	}
}

// TestHarness13_Witness_OutOfWindow: closes older than lookback are not inspected.
func TestHarness13_Witness_OutOfWindow(t *testing.T) {
	now := time.Now().UTC()
	items := []closedBead{
		{ID: "hq-old", Status: "closed", IssueType: "task",
			ClosedAt: now.Add(-10 * time.Minute).Format(time.RFC3339),
			CloseReason: "Closed"},
	}
	fake := newFakeBd(makeListJSON(t, items))

	res := DetectFalseCloses(fake.toBdCli(), "/tmp/test", DefaultFalseCloseLookback)
	if res.Inspected != 0 {
		t.Fatalf("Inspected should be 0 for out-of-window items, got %d", res.Inspected)
	}
}

// TestHarness13_Witness_ValidCloseUnchanged: a close with a real deliverable
// reason passes through untouched.
func TestHarness13_Witness_ValidCloseUnchanged(t *testing.T) {
	now := time.Now().UTC()
	items := []closedBead{
		{ID: "hq-good", Status: "closed", IssueType: "task",
			ClosedAt: now.Add(-30 * time.Second).Format(time.RFC3339),
			CloseReason: "added idempotency key to webhook handler, covered by TestRefund"},
	}
	fake := newFakeBd(makeListJSON(t, items))

	res := DetectFalseCloses(fake.toBdCli(), "/tmp/test", DefaultFalseCloseLookback)
	if res.Reopened != 0 {
		t.Fatalf("valid close must not trigger reopen, got %d", res.Reopened)
	}
	if fake.ranWithFirstArg("reopen") {
		t.Error("must not call bd reopen on valid close")
	}
}

// TestHarness13_Witness_PartialFailure: bd reopen failure is reported as an
// error but the patrol continues to inspect remaining beads.
func TestHarness13_Witness_PartialFailure(t *testing.T) {
	captureEscalations(t)
	now := time.Now().UTC()
	items := []closedBead{
		{ID: "hq-fail1", Status: "closed", IssueType: "task",
			ClosedAt: now.Add(-30 * time.Second).Format(time.RFC3339),
			CloseReason: "Closed"},
		{ID: "hq-fail2", Status: "closed", IssueType: "task",
			ClosedAt: now.Add(-30 * time.Second).Format(time.RFC3339),
			CloseReason: ""},
	}
	fake := newFakeBd(makeListJSON(t, items))
	fake.runErrs["reopen"] = fmt.Errorf("simulated bd outage")

	res := DetectFalseCloses(fake.toBdCli(), "/tmp/test", DefaultFalseCloseLookback)
	if res.Inspected != 2 {
		t.Fatalf("Inspected: got %d, want 2", res.Inspected)
	}
	if res.Errors == 0 {
		t.Error("expected Errors > 0 on simulated bd outage")
	}
	// Each item should have FalseCloseError action and a non-nil Err.
	for _, it := range res.Items {
		if it.Action != FalseCloseError {
			t.Errorf("item %s: action %v, want FalseCloseError", it.BeadID, it.Action)
		}
		if it.Err == nil || !strings.Contains(it.Err.Error(), "simulated") {
			t.Errorf("item %s: err %v, want propagated", it.BeadID, it.Err)
		}
	}
}
