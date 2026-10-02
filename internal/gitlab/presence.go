package gitlab

import (
	"encoding/json"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

// Presence helpers for policy fields (decode-only; no aggregate handlers).

// AllowFailurePresence returns presence of job allow_failure in raw JSON.
func AllowFailurePresence(raw json.RawMessage) (readmeta.Presence, error) {
	return readmeta.DecodeBoolPresence(raw, "allow_failure")
}

// ApprovedPresence returns presence of an approval "approved" field.
func ApprovedPresence(raw json.RawMessage) (readmeta.Presence, error) {
	return readmeta.DecodeBoolPresence(raw, "approved")
}

// UserHasApprovedPresence returns presence of user_has_approved.
func UserHasApprovedPresence(raw json.RawMessage) (readmeta.Presence, error) {
	return readmeta.DecodeBoolPresence(raw, "user_has_approved")
}

// DiffCollapsedPresence returns presence of collapsed on a diff object.
func DiffCollapsedPresence(raw json.RawMessage) (readmeta.Presence, error) {
	return readmeta.DecodeBoolPresence(raw, "collapsed")
}

// DiffTooLargePresence returns presence of too_large on a diff object.
func DiffTooLargePresence(raw json.RawMessage) (readmeta.Presence, error) {
	return readmeta.DecodeBoolPresence(raw, "too_large")
}
