package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
)

const (
	maxEvidencePages = 64
	// Revalidation re-reads already-certified work, so it must not draw down the
	// caller's per-call item and request budget that paged the graph in the first place.
	evidenceMaxItems    = 2000
	evidenceMaxRequests = 128
)

// chainEvidence folds ordered guard tokens into a running page-size-independent digest.
func chainEvidence(state string, tokens ...string) string {
	for _, tok := range tokens {
		sum := sha256.Sum256([]byte(state + "\x00" + tok))
		state = hex.EncodeToString(sum[:16])
	}
	return state
}

func bridgePageTokens(page bridgePage) []string {
	switch {
	case page.Unsupported:
		return []string{"unsupported"}
	case page.Inaccessible:
		return []string{"inaccessible"}
	}
	out := make([]string, 0, len(page.Bridges))
	for _, br := range page.Bridges {
		out = append(out, bridgeGuardToken(br))
	}
	return out
}

// pipelineMetaDigest pins the pipeline-level fields that the signed graph digest
// also hashes, so a status change with unchanged jobs and bridges is still drift.
func pipelineMetaDigest(p *pipelineView) string {
	if p == nil {
		return ""
	}
	raw, err := json.Marshal(struct {
		ID          int64  `json:"id"`
		Status      string `json:"status"`
		Source      string `json:"source"`
		Ref         string `json:"ref"`
		SHA         string `json:"sha"`
		StatusKnown bool   `json:"status_known"`
	}{p.ID, deref(p.Status), deref(p.Source), deref(p.Ref), deref(p.SHA), p.StatusKnown})
	if err != nil {
		return ""
	}
	return chainEvidence("", string(raw))
}

// completeNode marks a node's jobs and bridges as fully read and pins that
// evidence so later child continuations can detect upstream drift.
func (w *graphWalk) completeNode(k graphNodeKey, pipe *pipelineView) {
	w.visited[k.String()] = struct{}{}
	if w.evidence == nil {
		w.evidence = map[string][3]string{}
	}
	w.evidence[k.String()] = [3]string{w.jobsEv, w.bridgesEv, pipelineMetaDigest(pipe)}
	w.jobsEv, w.bridgesEv = "", ""
}

// revalidateCompleted re-reads every node whose jobs and bridges the walk has
// already completed, ancestors and finished siblings alike, and compares each
// with the evidence recorded at completion.
func (w *graphWalk) revalidateCompleted(ctx context.Context, d Deps, perPage int) error {
	drift := fmt.Errorf("%s: completed node evidence drift", cursor.ResyncRequired)
	for _, a := range w.ancestors {
		if _, ok := w.evidence[a.String()]; !ok {
			return drift
		}
	}
	if len(w.evidence) == 0 {
		return nil
	}
	keys := make([]string, 0, len(w.evidence))
	for k := range w.evidence {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	budget := &igl.Budget{
		MaxItems:    evidenceMaxItems,
		MaxBytes:    igl.DefaultMaxBytes,
		MaxElapsed:  igl.DefaultMaxElapsed,
		MaxRequests: evidenceMaxRequests,
	}
	ctx = igl.WithBudget(ctx, budget)
	defer budget.Cancel()
	for _, key := range keys {
		want := w.evidence[key]
		project, pipeline, ok := splitVisitKey(key)
		if !ok {
			return drift
		}
		a := graphNodeKey{Project: project, Pipeline: pipeline}
		kind, err := w.authorize(ctx, d, a.Project)
		if err != nil {
			return err
		}
		if kind != "" {
			return drift
		}
		current, err := loadPipeline(ctx, d, a.Project, a.Pipeline)
		if err != nil || current == nil || pipelineMetaDigest(current) != want[2] {
			if err != nil && !errors.Is(err, errPipelineForbidden) {
				return err
			}
			return drift
		}
		jobs := ""
		for page, n := 1, 0; ; n++ {
			if n >= maxEvidencePages {
				return drift
			}
			gp, err := collectJobPage(ctx, d, budget, a.Project, a.Pipeline, page, perPage, nil)
			if err != nil {
				return err
			}
			if gp.Partial || !gp.Paging.PagingKnown {
				return drift
			}
			jobs = chainEvidence(jobs, jobGuardTokens(gp.Jobs)...)
			next, more := pagingContinues(gp.Paging, page)
			if !more {
				if !gp.Paging.ExhaustedObserved {
					return drift
				}
				break
			}
			page = int(next)
		}
		bridges := ""
		for page, n := 1, 0; ; n++ {
			if n >= maxEvidencePages {
				return drift
			}
			bp, err := collectBridgePage(ctx, d, budget, a.Project, a.Pipeline, page, perPage, nil)
			if err != nil {
				return err
			}
			if bp.Partial {
				return drift
			}
			bridges = chainEvidence(bridges, bridgePageTokens(bp)...)
			if bp.Unsupported || bp.Inaccessible {
				break
			}
			next, more := pagingContinues(bp.Paging, page)
			if !more {
				if !bridgePagingExhausted(bp) {
					return drift
				}
				break
			}
			page = int(next)
		}
		if jobs != want[0] || bridges != want[1] {
			return drift
		}
	}
	return nil
}
