package tools

import (
	"net/http"
	"strconv"
	"strings"
)

// queuePageObs is the queue-only pagination contract. Shared readmeta.ObservePaging
// stays unchanged for legacy list/diff adapters.
type queuePageObs struct {
	exhausted bool
	next      int64
	ambiguous bool
}

// observeQueuePage accepts only explicit exhaustion or exactly current+1, and
// requires the raw X-Next-Page value to match the SDK next page. Duplicate,
// missing, blank-vs-positive, repeated, backward, and jumping headers are
// ambiguous. An empty page that still advertises a next page does not progress.
func observeQueuePage(current int, hdr http.Header, sdkNext int64, entryCount int) queuePageObs {
	if current < 1 {
		return queuePageObs{ambiguous: true}
	}
	vals, present := queueNextPageValues(hdr)
	if !present {
		return queuePageObs{ambiguous: true}
	}
	if len(vals) != 1 {
		return queuePageObs{ambiguous: true}
	}
	raw := vals[0]
	if strings.TrimSpace(raw) != raw {
		return queuePageObs{ambiguous: true}
	}
	if raw == "" || raw == "0" {
		if sdkNext > 0 {
			return queuePageObs{ambiguous: true}
		}
		return queuePageObs{exhausted: true}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != raw {
		return queuePageObs{ambiguous: true}
	}
	if sdkNext != n || int(n) != current+1 {
		return queuePageObs{ambiguous: true}
	}
	if entryCount == 0 {
		return queuePageObs{ambiguous: true}
	}
	return queuePageObs{next: n}
}

func queueNextPageValues(hdr http.Header) ([]string, bool) {
	if hdr == nil {
		return nil, false
	}
	canon := http.CanonicalHeaderKey("X-Next-Page")
	if vals, ok := hdr[canon]; ok {
		return vals, true
	}
	if vals, ok := hdr["X-Next-Page"]; ok {
		return vals, true
	}
	return nil, false
}
