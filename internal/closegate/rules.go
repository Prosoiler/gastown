// Package closegate exposes the shared HARNESS-13 validation rules used by
// both the `gt close` wrapper (Layer A, pre-write) and the Witness backstop
// (Layer B, post-write reopen). Keeping the rules in one place prevents the
// two layers from drifting in what counts as a FALSE-CLOSE.
//
// See: ~/gt/occultfusion/crew/atlas/reports/2026-04-25_harness-13_bd-close-validation-gate-design.md
package closegate

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// MinReasonLen is the minimum length for a non-skip close reason.
// Threshold per Atlas design — median of recent non-regression closes.
const MinReasonLen = 20

// GenericReasons are content-less close reasons rejected by Rule 1.
// Closing a bead with one of these communicates nothing about the deliverable,
// matching the oc-zf2 / oc-8wj regression patterns from 2026-04-22.
var GenericReasons = map[string]struct{}{
	"closed":    {},
	"close":     {},
	"done":      {},
	"fixed":     {},
	"ok":        {},
	"complete":  {},
	"completed": {},
	"lgtm":      {},
	"merged":    {},
}

var allPunctOrEmoji = regexp.MustCompile(`^[\p{P}\p{S}\p{Z}\p{C}]+$`)

// ValidateReason implements Rule 1 (close-reason content validation).
// Returns nil if the reason describes a deliverable, or a descriptive error
// otherwise. Used by Layer A (reject pre-write) and Layer B (Witness reopen).
func ValidateReason(reason string) error {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return fmt.Errorf("close reason is empty or whitespace-only")
	}
	lower := strings.ToLower(trimmed)
	if _, ok := GenericReasons[lower]; ok {
		return fmt.Errorf("close reason %q is too generic", trimmed)
	}
	if len([]rune(trimmed)) < MinReasonLen {
		return fmt.Errorf("close reason is %d chars, minimum %d", len([]rune(trimmed)), MinReasonLen)
	}
	if allPunctOrEmoji.MatchString(trimmed) {
		return fmt.Errorf("close reason is punctuation/emoji-only")
	}
	if isSingleGrapheme(trimmed) {
		return fmt.Errorf("close reason appears to be a single character/emoji")
	}
	return nil
}

func isSingleGrapheme(s string) bool {
	runes := []rune(s)
	if len(runes) != 1 {
		return false
	}
	r := runes[0]
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}
