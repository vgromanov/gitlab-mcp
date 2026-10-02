package readmeta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Presence distinguishes absent / null / false / true before SDK bool erasure.
type Presence string

const (
	PresenceAbsent Presence = "absent"
	PresenceNull   Presence = "null"
	PresenceFalse  Presence = "false"
	PresenceTrue   Presence = "true"
)

// DecodeBoolPresence inspects a raw JSON object for a boolean policy field.
//
// Rules:
//   - empty or JSON null input → absent (field not observable on an object)
//   - input must be a single complete JSON object (no trailing tokens)
//   - missing field → absent
//   - field null/true/false → corresponding Presence
//   - field present with any other JSON type, or malformed/truncated/non-object input → error
func DecodeBoolPresence(raw json.RawMessage, field string) (Presence, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return PresenceAbsent, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("presence decode: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return "", fmt.Errorf("presence decode: expected JSON object")
	}

	found := false
	var val json.RawMessage
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("presence decode: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return "", fmt.Errorf("presence decode: expected object key string")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return "", fmt.Errorf("presence decode: %w", err)
		}
		if key == field {
			found = true
			val = append(json.RawMessage(nil), v...)
		}
	}

	// Consume closing '}'.
	closeTok, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("presence decode: %w", err)
	}
	closeDelim, ok := closeTok.(json.Delim)
	if !ok || closeDelim != '}' {
		return "", fmt.Errorf("presence decode: expected end of object")
	}

	// Reject trailing input after the object.
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return "", fmt.Errorf("presence decode: trailing input after object")
		}
		return "", fmt.Errorf("presence decode: %w", err)
	}

	if !found {
		return PresenceAbsent, nil
	}

	s := string(bytes.TrimSpace(val))
	switch s {
	case "null":
		return PresenceNull, nil
	case "true":
		return PresenceTrue, nil
	case "false":
		return PresenceFalse, nil
	default:
		return "", fmt.Errorf("presence decode: field %q has non-bool value", field)
	}
}

// BoolFromPresence maps presence to a Go bool for SDK-shaped projections.
// Absent and null map to false (SDK erasure); true/false keep their values.
func BoolFromPresence(p Presence) bool {
	return p == PresenceTrue
}
