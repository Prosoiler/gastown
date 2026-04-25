package closegate

import "testing"

// TestValidateReason covers Rule 1 in isolation. The same cases are also
// reached transitively from internal/cmd/close_validation_test.go and
// internal/witness/false_close_test.go — duplicated here so the shared
// package has its own self-contained contract test.
func TestValidateReason(t *testing.T) {
	pass := []string{
		"added idempotency key to webhook handler, covered by TestRefund",
		"리팩토링: convoy launch path 의 deadlock 제거 — TestConvoyDeadlock 추가",
		"reverted 51e28794d, blocked downstream regression",
	}
	for _, r := range pass {
		if err := ValidateReason(r); err != nil {
			t.Errorf("expected pass for %q, got %v", r, err)
		}
	}

	reject := []string{
		"", "   ", "\t\n", "Closed", "DONE", "lgtm", "merged", "✅", "...",
		"ok", "x", "fix done",
	}
	for _, r := range reject {
		if err := ValidateReason(r); err == nil {
			t.Errorf("expected reject for %q, got nil", r)
		}
	}
}

func TestValidateReason_BoundaryLength(t *testing.T) {
	// Exactly 19 chars (rune count) → reject.
	short := "1234567890123456789"
	if err := ValidateReason(short); err == nil {
		t.Errorf("19-char reason should reject")
	}
	// Exactly 20 → accept.
	long := "12345678901234567890"
	if err := ValidateReason(long); err != nil {
		t.Errorf("20-char reason should accept; got %v", err)
	}
}
