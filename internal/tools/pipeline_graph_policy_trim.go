package tools

import (
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
)

func trimGraphPolicyState(key []byte, p *cursor.Payload) error {
	if p == nil || p.GraphCont == nil {
		return nil
	}
	gc := p.GraphCont
	origOc := append([]string(nil), gc.Oc...)
	origLM := append([]string(nil), p.PageState.LineageMax...)
	try := func(oc, lm []string) error {
		gc.Oc = oc
		p.PageState.LineageMax = lm
		_, err := cursor.Encode(key, *p)
		return err
	}
	if try(origOc, origLM) == nil {
		return nil
	}
	bestOc := maxPrefixThatFits(len(origOc), func(n int) bool { return try(origOc[:n], origLM) == nil })
	if try(origOc[:bestOc], origLM) == nil {
		markOutcomeStateTruncated(gc, p, len(origOc), len(origLM))
		return nil
	}
	bestLM := maxPrefixThatFits(len(origLM), func(n int) bool {
		n = lineageMaxTrimLen(origLM, n)
		return try(origOc[:bestOc], origLM[:n]) == nil
	})
	bestLM = lineageMaxTrimLen(origLM, bestLM)
	gc.Oc = origOc[:bestOc]
	p.PageState.LineageMax = origLM[:bestLM]
	markOutcomeStateTruncated(gc, p, len(origOc), len(origLM))
	return nil
}

// lineageMaxTrimLen never drops the carry saturation marker alone.
func lineageMaxTrimLen(orig []string, n int) int {
	if n <= 0 && len(orig) > 0 && orig[0] == "*" {
		return 1
	}
	return n
}

func markOutcomeStateTruncated(gc *cursor.GraphCont, p *cursor.Payload, origOc, origLM int) {
	if gc == nil {
		return
	}
	if len(gc.Oc) < origOc {
		gc.Ocx = true
	}
	if p != nil && len(p.PageState.LineageMax) < origLM {
		gc.Lmx = true
	}
}
