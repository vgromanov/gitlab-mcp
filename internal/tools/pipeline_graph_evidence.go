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
// with the evidence recorded at completion. Reads draw from the caller budget.
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
	budget := igl.BudgetFromContext(ctx)
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
		jobs, err := readJobEvidence(ctx, d, budget, a.Project, a.Pipeline, perPage, "", 1, 0)
		if err != nil {
			return err
		}
		bridges, err := readBridgeEvidence(ctx, d, budget, a.Project, a.Pipeline, perPage, "", 1, 0)
		if err != nil {
			return err
		}
		if jobs != want[0] || bridges != want[1] {
			return drift
		}
	}
	return nil
}

// readJobEvidence chains guard tokens for job pages from..until-1, or from..end
// when until is 0. A bounded read requires every page to continue to the next.
func readJobEvidence(ctx context.Context, d Deps, budget *igl.Budget, project string, pipelineID int64, perPage int, state string, from, until int) (string, error) {
	drift := fmt.Errorf("%s: completed node evidence drift", cursor.ResyncRequired)
	for page := from; until == 0 || page < until; {
		gp, err := collectJobPage(ctx, d, budget, project, pipelineID, page, perPage, nil)
		if be := evidenceBudgetErr(gp.Reason, err); be != nil {
			return "", be
		}
		if err != nil {
			return "", err
		}
		if gp.Partial || !gp.Paging.PagingKnown {
			return "", drift
		}
		state = chainEvidence(state, jobGuardTokens(gp.Jobs)...)
		next, more := pagingContinues(gp.Paging, page)
		if !more {
			if until > 0 || !gp.Paging.ExhaustedObserved {
				return "", drift
			}
			break
		}
		page = int(next)
	}
	return state, nil
}

// readBridgeEvidence is the bridge-list counterpart of readJobEvidence.
func readBridgeEvidence(ctx context.Context, d Deps, budget *igl.Budget, project string, pipelineID int64, perPage int, state string, from, until int) (string, error) {
	drift := fmt.Errorf("%s: completed node evidence drift", cursor.ResyncRequired)
	for page := from; until == 0 || page < until; {
		bp, err := collectBridgePage(ctx, d, budget, project, pipelineID, page, perPage, nil)
		if be := evidenceBudgetErr(bp.Reason, err); be != nil {
			return "", be
		}
		if err != nil {
			return "", err
		}
		if bp.Partial {
			return "", drift
		}
		state = chainEvidence(state, bridgePageTokens(bp)...)
		if bp.Unsupported || bp.Inaccessible {
			if until > 0 {
				return "", drift
			}
			break
		}
		next, more := pagingContinues(bp.Paging, page)
		if !more {
			if until > 0 || !bridgePagingExhausted(bp) {
				return "", drift
			}
			break
		}
		page = int(next)
	}
	return state, nil
}

// revalidateActiveJobs re-reads job pages 1..through-1 of the active node and
// checks that, with the already-guarded page through, they reproduce the
// signed running digest.
func (w *graphWalk) revalidateActiveJobs(ctx context.Context, d Deps, project string, pipelineID int64, perPage, through int, guard graphPage) error {
	drift := fmt.Errorf("%s: active node evidence drift", cursor.ResyncRequired)
	budget := igl.BudgetFromContext(ctx)
	state, err := readJobEvidence(ctx, d, budget, project, pipelineID, perPage, "", 1, through)
	if err != nil {
		return err
	}
	if chainEvidence(state, jobGuardTokens(guard.Jobs)...) != w.jobsEv {
		return drift
	}
	return nil
}

// revalidateActiveBridges re-reads every job page of the active node plus bridge
// pages 1..through-1, and checks them against the signed running digests.
func (w *graphWalk) revalidateActiveBridges(ctx context.Context, d Deps, project string, pipelineID int64, perPage, through int, guard bridgePage) error {
	drift := fmt.Errorf("%s: active node evidence drift", cursor.ResyncRequired)
	budget := igl.BudgetFromContext(ctx)
	jobs, err := readJobEvidence(ctx, d, budget, project, pipelineID, perPage, "", 1, 0)
	if err != nil {
		return err
	}
	if jobs != w.jobsEv {
		return drift
	}
	state, err := readBridgeEvidence(ctx, d, budget, project, pipelineID, perPage, "", 1, through)
	if err != nil {
		return err
	}
	if chainEvidence(state, bridgePageTokens(guard)...) != w.bridgesEv {
		return drift
	}
	return nil
}

func evidenceBudgetErr(reason string, err error) error {
	if err != nil {
		if errors.Is(err, igl.ErrBudgetItems) || errors.Is(err, igl.ErrBudgetBytes) || errors.Is(err, igl.ErrBudgetRequests) || errors.Is(err, igl.ErrBudgetElapsed) {
			return err
		}
	}
	switch reason {
	case "budget_items", "budget":
		return igl.ErrBudgetItems
	}
	return nil
}
