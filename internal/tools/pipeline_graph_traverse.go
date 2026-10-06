package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	graphDefaultMaxDepth = 8
	graphDefaultMaxNodes = 16
	edgeKindBridge       = "bridge"
	edgeKindCycle        = "cycle"
	edgeKindShared       = "shared"
	edgeKindDenied       = "denied"
	edgeKindInaccessible = "inaccessible"
	edgeKindIdentity     = "identity_unresolved"
	edgeKindDepthStop    = "depth_stop"
	edgeKindNodeStop     = "node_stop"
	edgeKindUnsupported  = "unsupported"
	edgeKindMissing      = "missing"
	nodeRoleParent       = "parent"
	nodeRoleDownstream   = "downstream"
)

type graphNodeKey struct {
	Project  string
	Pipeline int64
}

func (k graphNodeKey) String() string {
	return k.Project + ":" + strconv.FormatInt(k.Pipeline, 10)
}

type queuedGraphNode struct {
	Key       graphNodeKey
	Depth     int
	ParentSHA string
	Ancestors []graphNodeKey
	BridgeID  int64
}

type graphNodeView struct {
	ProjectID  string        `json:"project_id"`
	PipelineID int64         `json:"pipeline_id"`
	Depth      int           `json:"depth"`
	Role       string        `json:"role"`
	Pipeline   *pipelineView `json:"pipeline,omitempty"`
	Jobs       []jobView     `json:"jobs"`
	Lineage    []lineageView `json:"lineage"`
}

type graphEdgeView struct {
	FromProject   string   `json:"from_project"`
	FromPipeline  int64    `json:"from_pipeline"`
	ToProject     string   `json:"to_project,omitempty"`
	ToPipeline    int64    `json:"to_pipeline,omitempty"`
	BridgeID      *int64   `json:"bridge_id,omitempty"`
	Kind          string   `json:"kind"`
	Capability    string   `json:"capability,omitempty"`
	SHAComparison string   `json:"sha_comparison,omitempty"`
	Provenance    []string `json:"provenance"`
}

type graphBridge struct {
	Job           graphJob
	ChildProject  int64
	ChildPipeline int64
	ChildSHA      string
	ChildPresent  bool
}

type bridgePage struct {
	Bridges      []graphBridge
	Partial      bool
	Reason       string
	Paging       readmeta.PagingObservation
	Unsupported  bool
	Inaccessible bool
}

type graphWalk struct {
	current          graphNodeKey
	pipe             *pipelineView
	depth            int
	phase            string
	visited          map[string]struct{}
	queue            []queuedGraphNode
	nodes            []graphNodeView
	edges            []graphEdgeView
	block            bool
	partial          bool
	unknown          bool
	reasons          []string
	outcomes         []policyOutcome
	cap              string
	coverage         string
	unseen           bool
	bridgesOn        bool
	nodeCount        int
	maxDepth         int
	maxNodes         int
	ancestors        []graphNodeKey
	authz            map[string]string
	root             graphNodeKey
	relKind          string
	relProven        bool
	relSHA           string
	stickyIncomplete bool
	jobsEv           string
	bridgesEv        string
	evidence         map[string][3]string
	reach            []graphEdgeView
}

var errPipelineForbidden = errors.New("pipeline forbidden")

func parseGraphBounds(in pipelineGraphIn) (int, int, error) {
	if in.MaxDepth < 0 || in.MaxNodes < 0 {
		return 0, 0, fmt.Errorf("invalid pipeline graph input")
	}
	depth := in.MaxDepth
	if depth == 0 {
		depth = graphDefaultMaxDepth
	}
	if depth > 32 {
		return 0, 0, fmt.Errorf("max_depth must be between 1 and 32")
	}
	nodes := in.MaxNodes
	if nodes == 0 {
		nodes = graphDefaultMaxNodes
	}
	if nodes > cursor.MaxGraphVisited {
		return 0, 0, fmt.Errorf("max_nodes must be between 1 and %d", cursor.MaxGraphVisited)
	}
	return depth, nodes, nil
}

func newGraphWalk(pipe *pipelineView, pid string, depth, nodes int) *graphWalk {
	key := graphNodeKey{Project: pid, Pipeline: pipe.ID}
	return &graphWalk{
		current:   key,
		pipe:      pipe,
		depth:     0,
		phase:     cursor.GraphPhaseJobs,
		visited:   map[string]struct{}{},
		coverage:  downstreamCoverageUnknown,
		unseen:    true,
		nodeCount: 1,
		maxDepth:  depth,
		maxNodes:  nodes,
		authz:     map[string]string{},
		root:      key,
	}
}

func restoreGraphWalk(pipe *pipelineView, gc *cursor.GraphCont, depth, nodes int) (*graphWalk, error) {
	if gc == nil {
		return nil, fmt.Errorf("%s: graph continuation missing", cursor.ResyncRequired)
	}
	w := &graphWalk{
		current:          graphNodeKey{Project: gc.NP, Pipeline: gc.NI},
		pipe:             pipe,
		depth:            gc.D,
		phase:            gc.Phase,
		visited:          map[string]struct{}{},
		block:            gc.Block,
		partial:          gc.Part,
		unknown:          gc.Unk,
		coverage:         gc.Cov,
		unseen:           gc.Cov != downstreamCoverageComplete || gc.Inc,
		cap:              gc.Cap,
		bridgesOn:        gc.Cap == bridgeCapabilityBridges,
		nodeCount:        gc.N,
		maxDepth:         depth,
		maxNodes:         nodes,
		authz:            map[string]string{},
		root:             graphNodeKey{Project: gc.RP, Pipeline: gc.RI},
		relKind:          gc.RK,
		relProven:        gc.Prv,
		relSHA:           gc.RS,
		stickyIncomplete: gc.Inc,
		jobsEv:           gc.JD,
		bridgesEv:        gc.BD,
		evidence:         map[string][3]string{},
	}
	for _, item := range gc.Ev {
		key, jd, bd, md, ok := cursor.ParseGraphEvidence(item)
		if !ok {
			return nil, fmt.Errorf("%s: graph continuation missing", cursor.ResyncRequired)
		}
		w.evidence[key] = [3]string{jd, bd, md}
	}
	for _, a := range gc.Anc {
		p, id, ok := splitVisitKey(a)
		if !ok {
			return nil, fmt.Errorf("%s: graph continuation missing", cursor.ResyncRequired)
		}
		w.ancestors = append(w.ancestors, graphNodeKey{Project: p, Pipeline: id})
	}
	for _, v := range gc.Vis {
		w.visited[v] = struct{}{}
	}
	for _, item := range gc.Q {
		qn, ok := parseQueuedNode(item)
		if !ok {
			return nil, fmt.Errorf("%s: graph continuation missing", cursor.ResyncRequired)
		}
		w.queue = append(w.queue, qn)
	}
	for _, item := range gc.Rg {
		from, to, kind, ok := cursor.ParseGraphReachEdge(item)
		if !ok {
			return nil, fmt.Errorf("%s: graph continuation missing", cursor.ResyncRequired)
		}
		fp, fpi, fok := splitVisitKey(from)
		tp, tpi, tok := splitVisitKey(to)
		if !fok || !tok {
			return nil, fmt.Errorf("%s: graph continuation missing", cursor.ResyncRequired)
		}
		w.recordReachEdge(graphEdgeView{
			FromProject:  fp,
			FromPipeline: fpi,
			ToProject:    tp,
			ToPipeline:   tpi,
			Kind:         kind,
		})
	}
	restoreGraphReasons(w, gc.Rsn)
	return w, nil
}

func restoreGraphReasons(w *graphWalk, reasons []string) {
	if w == nil {
		return
	}
	for _, r := range reasons {
		w.reasons = append(w.reasons, r)
		switch r {
		case "failed_required", "required_manual":
			w.block = true
			w.outcomes = append(w.outcomes, policyOutcome{Outcome: policyBlock, Reason: r})
		case "in_progress":
			w.partial = true
			w.outcomes = append(w.outcomes, policyOutcome{Outcome: policyPartial, Reason: r})
		default:
			w.unknown = true
			w.outcomes = append(w.outcomes, policyOutcome{Outcome: policyUnknown, Reason: r})
		}
	}
}

func (w *graphWalk) bindRelation(rel relationResult) {
	if w == nil {
		return
	}
	w.relKind = rel.Kind
	w.relProven = rel.Proven
	w.relSHA = rel.SHAComparison
}

func pidOf(pipe *pipelineView) string {
	if pipe == nil {
		return ""
	}
	if pipe.ScopeProject != "" {
		return pipe.ScopeProject
	}
	if pipe.ProjectID > 0 {
		return strconv.FormatInt(pipe.ProjectID, 10)
	}
	return ""
}

func parseQueuedNode(item string) (queuedGraphNode, bool) {
	proj, pipe, depth, sha, bridge, ancs, ok := cursor.ParseGraphQueueItem(item)
	if !ok {
		return queuedGraphNode{}, false
	}
	n := queuedGraphNode{Key: graphNodeKey{Project: proj, Pipeline: pipe}, Depth: depth, BridgeID: bridge}
	if sha != "-" {
		n.ParentSHA = sha
	}
	for _, a := range ancs {
		p, id, aok := splitVisitKey(a)
		if !aok {
			return queuedGraphNode{}, false
		}
		n.Ancestors = append(n.Ancestors, graphNodeKey{Project: p, Pipeline: id})
	}
	return n, true
}

func encodeQueuedNode(n queuedGraphNode) (string, error) {
	sha := n.ParentSHA
	if sha == "" {
		sha = "-"
	}
	ancs := make([]string, 0, len(n.Ancestors))
	for _, a := range n.Ancestors {
		ancs = append(ancs, a.String())
	}
	return cursor.FormatGraphQueueItem(n.Key.Project, n.Key.Pipeline, n.Depth, sha, n.BridgeID, ancs)
}

func splitVisitKey(item string) (string, int64, bool) {
	i := strings.LastIndexByte(item, ':')
	if i <= 0 || i == len(item)-1 {
		return "", 0, false
	}
	id, err := strconv.ParseInt(item[i+1:], 10, 64)
	if err != nil || id < 1 {
		return "", 0, false
	}
	return item[:i], id, true
}

func (w *graphWalk) recordOutcomes(groups []lineageGroup) {
	for _, g := range groups {
		w.outcomes = append(w.outcomes, g.Outcomes...)
		for _, o := range g.Outcomes {
			switch o.Outcome {
			case policyBlock:
				w.block = true
			case policyPartial:
				w.partial = true
			case policyUnknown:
				w.unknown = true
			}
			if o.Reason != "" {
				w.reasons = append(w.reasons, o.Reason)
			}
		}
	}
}

func (w *graphWalk) snapshotCont() *cursor.GraphCont {
	vis := make([]string, 0, len(w.visited))
	for k := range w.visited {
		vis = append(vis, k)
	}
	sort.Strings(vis)
	q := make([]string, 0, len(w.queue))
	for _, n := range w.queue {
		item, err := encodeQueuedNode(n)
		if err != nil {
			item = ""
		}
		q = append(q, item)
	}
	cov := w.coverage
	if cov == "" {
		cov = downstreamCoverageUnknown
	}
	root := w.root
	if root.Pipeline < 1 {
		if w.depth == 0 {
			root = w.current
		}
	}
	rsn := uniqueGraphReasons(w.reasons)
	ev := make([]string, 0, len(w.evidence))
	for key, pair := range w.evidence {
		if item, err := cursor.FormatGraphEvidence(key, pair[0], pair[1], pair[2]); err == nil {
			ev = append(ev, item)
		}
	}
	sort.Strings(ev)
	rg := graphReachSnapshot(w.reach)
	var anc []string
	for _, a := range w.ancestors {
		anc = append(anc, a.String())
	}
	return &cursor.GraphCont{
		V:     cursor.GraphContSchemaG1,
		Phase: w.phase,
		NP:    w.current.Project,
		NI:    w.current.Pipeline,
		D:     w.depth,
		Vis:   vis,
		Q:     q,
		Anc:   anc,
		Ev:    ev,
		JD:    w.jobsEv,
		BD:    w.bridgesEv,
		N:     w.nodeCount,
		Block: w.block,
		Part:  w.partial,
		Unk:   w.unknown,
		Cov:   cov,
		Cap:   w.cap,
		Inc:   w.stickyIncomplete || w.hasIncompleteEdges(),
		Rsn:   rsn,
		RP:    root.Project,
		RI:    root.Pipeline,
		RK:    w.relKind,
		Prv:   w.relProven,
		RS:    w.relSHA,
		Rg:    rg,
	}
}

func (w *graphWalk) recordReachEdge(e graphEdgeView) {
	if w == nil {
		return
	}
	if e.Kind != edgeKindBridge && e.Kind != edgeKindShared {
		return
	}
	if e.ToPipeline < 1 {
		return
	}
	for _, got := range w.reach {
		if got.FromProject == e.FromProject && got.FromPipeline == e.FromPipeline &&
			got.ToProject == e.ToProject && got.ToPipeline == e.ToPipeline && got.Kind == e.Kind {
			return
		}
	}
	w.reach = append(w.reach, e)
}

func graphReachSnapshot(edges []graphEdgeView) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e.Kind != edgeKindBridge && e.Kind != edgeKindShared {
			continue
		}
		if e.ToPipeline < 1 {
			continue
		}
		from := graphNodeKey{Project: e.FromProject, Pipeline: e.FromPipeline}.String()
		to := graphNodeKey{Project: e.ToProject, Pipeline: e.ToPipeline}.String()
		item, err := cursor.FormatGraphReachEdge(from, to, e.Kind)
		if err != nil {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

func uniqueGraphReasons(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, r := range in {
		if r == "" {
			continue
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
		if len(out) == cursor.MaxGraphVisited {
			break
		}
	}
	sort.Strings(out)
	return out
}

func collectBridgePage(ctx context.Context, d Deps, budget *igl.Budget, pid string, pipelineID int64, page, perPage int, prior map[int64]struct{}) (bridgePage, error) {
	path := fmt.Sprintf("projects/%s/pipelines/%d/bridges", gitlab.PathEscape(pid), pipelineID)
	opt := &gitlab.ListJobsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	out := bridgePage{}
	seen := map[int64]struct{}{}
	resp, err := igl.StreamJSONArrayQueue(ctx, d.Client, http.MethodGet, path, opt, func(raw json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			out.Partial = true
			out.Reason = "cancelled"
			return err
		}
		br, perr := parseGraphBridge(raw)
		if perr != nil {
			out.Partial = true
			out.Reason = "malformed bridge"
			return fmt.Errorf("%s: malformed bridge", readmeta.CodePartial)
		}
		if _, ok := seen[br.Job.ID]; ok {
			out.Partial = true
			out.Reason = "duplicate bridge"
			return fmt.Errorf("%s: duplicate bridge", readmeta.CodePartial)
		}
		if _, ok := prior[br.Job.ID]; ok {
			out.Partial = true
			out.Reason = "previous-page overlap"
			return fmt.Errorf("%s: previous-page overlap", readmeta.CodePartial)
		}
		if err := igl.ChargeItem(ctx, budget); err != nil {
			out.Partial = true
			out.Reason = "budget_items"
			return err
		}
		seen[br.Job.ID] = struct{}{}
		out.Bridges = append(out.Bridges, br)
		return nil
	})
	if out.Partial {
		if out.Reason == "previous-page overlap" {
			return out, fmt.Errorf("%s: previous-page overlap", cursor.ResyncRequired)
		}
		return out, nil
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return bridgePage{}, err
		}
		if errors.Is(err, igl.ErrBudgetItems) || errors.Is(err, igl.ErrBudgetBytes) || errors.Is(err, igl.ErrBudgetRequests) || errors.Is(err, igl.ErrBudgetElapsed) {
			out.Partial = true
			out.Reason = "budget"
			return out, nil
		}
		switch providerStatus(err) {
		case http.StatusNotFound:
			out.Unsupported = true
			return out, nil
		case http.StatusForbidden:
			out.Inaccessible = true
			return out, nil
		}
		if len(out.Bridges) > 0 && streamDecodeStopped(err) {
			out.Partial = true
			out.Reason = "malformed response"
			return out, nil
		}
		return bridgePage{}, safeProviderErr(err)
	}
	out.Paging = observeResp(resp)
	return out, nil
}

func parseGraphBridge(raw json.RawMessage) (graphBridge, error) {
	job, err := parseGraphJob(raw)
	if err != nil {
		return graphBridge{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &fields); err != nil {
		return graphBridge{}, fmt.Errorf("malformed bridge")
	}
	br := graphBridge{Job: job}
	down, ok := fields["downstream_pipeline"]
	if !ok || bytes.Equal(bytes.TrimSpace(down), []byte("null")) || len(bytes.TrimSpace(down)) == 0 {
		return br, nil
	}
	var child map[string]json.RawMessage
	if err := json.Unmarshal(down, &child); err != nil {
		return graphBridge{}, fmt.Errorf("malformed bridge")
	}
	br.ChildPresent = true
	if id, idOK := positiveJSONID(child["id"]); idOK {
		br.ChildPipeline = id
	}
	if pid, pidOK := positiveJSONID(child["project_id"]); pidOK {
		br.ChildProject = pid
	}
	if sha, known, typeOK := jsonStringField(child, "sha"); typeOK && known {
		if got, ok := readmeta.ObservedHeadSHA(sha); ok {
			br.ChildSHA = got
		}
	}
	return br, nil
}

func (w *graphWalk) ingestBridges(ctx context.Context, d Deps, parent graphNodeKey, parentSHA string, page bridgePage, prior lineageCarry) error {
	w.bridgesOn = true
	w.bridgesEv = chainEvidence(w.bridgesEv, bridgePageTokens(page)...)
	if page.Unsupported {
		w.cap = bridgeCapabilityUnknown
		w.unseen = true
		w.stickyIncomplete = true
		w.coverage = downstreamCoverageUnknown
		w.edges = append(w.edges, graphEdgeView{
			FromProject:  parent.Project,
			FromPipeline: parent.Pipeline,
			Kind:         edgeKindUnsupported,
			Capability:   bridgeCapabilityUnknown,
			Provenance:   []string{"bridges_404"},
		})
		return nil
	}
	if page.Inaccessible {
		w.cap = bridgeCapabilityBridges
		w.unseen = true
		w.stickyIncomplete = true
		w.coverage = downstreamCoveragePartial
		w.edges = append(w.edges, graphEdgeView{
			FromProject:  parent.Project,
			FromPipeline: parent.Pipeline,
			Kind:         edgeKindInaccessible,
			Capability:   bridgeCapabilityBridges,
			Provenance:   []string{"bridges_403"},
		})
		return nil
	}
	w.cap = bridgeCapabilityBridges
	if !w.hasIncompleteEdges() && !w.stickyIncomplete {
		w.unseen = false
	}
	groups := buildLineage(bridgeJobs(page.Bridges), prior)
	attempts := lineageAttempts(groups)
	w.recordOutcomes(groups)
	for _, br := range page.Bridges {
		if attempts[br.Job.ID] == attemptHistory {
			continue
		}
		if err := w.addBridgeEdge(ctx, d, parent, parentSHA, br); err != nil {
			return err
		}
	}
	return nil
}

func (w *graphWalk) hasIncompleteEdges() bool {
	if w == nil {
		return false
	}
	for _, e := range w.edges {
		switch e.Kind {
		case edgeKindMissing, edgeKindDenied, edgeKindIdentity, edgeKindDepthStop, edgeKindNodeStop, edgeKindUnsupported, edgeKindInaccessible, edgeKindCycle:
			return true
		}
	}
	return false
}

func bridgeJobs(in []graphBridge) []graphJob {
	out := make([]graphJob, 0, len(in))
	for _, br := range in {
		out = append(out, br.Job)
	}
	return out
}

func (w *graphWalk) addBridgeEdge(ctx context.Context, d Deps, parent graphNodeKey, parentSHA string, br graphBridge) error {
	id := br.Job.ID
	base := graphEdgeView{
		FromProject:  parent.Project,
		FromPipeline: parent.Pipeline,
		BridgeID:     &id,
		Capability:   w.cap,
		Provenance:   []string{"bridge_edge"},
	}
	if !br.ChildPresent {
		base.Kind = edgeKindMissing
		w.unseen = true
		w.stickyIncomplete = true
		w.edges = append(w.edges, base)
		return nil
	}
	if br.ChildProject < 1 {
		base.Kind = edgeKindIdentity
		w.unseen = true
		w.stickyIncomplete = true
		w.edges = append(w.edges, base)
		return nil
	}
	childProject := strconv.FormatInt(br.ChildProject, 10)
	authKind, err := w.authorize(ctx, d, childProject)
	if err != nil {
		return err
	}
	if authKind == edgeKindDenied {
		base.Kind = edgeKindDenied
		w.unseen = true
		w.stickyIncomplete = true
		w.coverage = downstreamCoveragePartial
		w.edges = append(w.edges, base)
		return nil
	}
	if authKind == edgeKindInaccessible {
		base.Kind = edgeKindInaccessible
		w.unseen = true
		w.stickyIncomplete = true
		w.coverage = downstreamCoveragePartial
		w.edges = append(w.edges, base)
		return nil
	}
	if authKind == edgeKindIdentity {
		base.Kind = edgeKindIdentity
		w.unseen = true
		w.stickyIncomplete = true
		w.edges = append(w.edges, base)
		return nil
	}
	if br.ChildPipeline < 1 {
		base.Kind = edgeKindMissing
		base.ToProject = childProject
		w.unseen = true
		w.stickyIncomplete = true
		w.edges = append(w.edges, base)
		return nil
	}
	child := graphNodeKey{Project: childProject, Pipeline: br.ChildPipeline}
	base.ToProject = child.Project
	base.ToPipeline = child.Pipeline
	base.SHAComparison = compareSHA(br.ChildSHA, parentSHA)
	if w.depth+1 > w.maxDepth {
		base.Kind = edgeKindDepthStop
		w.unseen = true
		w.stickyIncomplete = true
		w.coverage = downstreamCoveragePartial
		w.edges = append(w.edges, base)
		return nil
	}
	if _, seen := w.visited[child.String()]; seen || w.queued(child) || w.isAncestor(child) {
		if w.isAncestor(child) || child == w.current || w.reachableViaBridges(child, w.current) {
			base.Kind = edgeKindCycle
			w.unseen = true
			w.stickyIncomplete = true
			w.coverage = downstreamCoveragePartial
		} else {
			base.Kind = edgeKindShared
		}
		base.Provenance = append(base.Provenance, base.Kind)
		w.edges = append(w.edges, base)
		w.recordReachEdge(base)
		return nil
	}
	if w.nodeCount >= w.maxNodes {
		base.Kind = edgeKindNodeStop
		w.unseen = true
		w.stickyIncomplete = true
		w.coverage = downstreamCoveragePartial
		w.edges = append(w.edges, base)
		return nil
	}
	base.Kind = edgeKindBridge
	w.edges = append(w.edges, base)
	w.recordReachEdge(base)
	w.nodeCount++
	w.queue = append(w.queue, queuedGraphNode{
		Key:       child,
		Depth:     w.depth + 1,
		ParentSHA: parentSHA,
		Ancestors: append([]graphNodeKey{}, w.currentAncestors()...),
		BridgeID:  id,
	})
	return nil
}

func (w *graphWalk) reachableViaBridges(from, to graphNodeKey) bool {
	if from == to {
		return true
	}
	seen := map[string]struct{}{from.String(): {}}
	queue := []graphNodeKey{from}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, e := range w.reach {
			if e.FromProject != n.Project || e.FromPipeline != n.Pipeline {
				continue
			}
			if e.ToPipeline < 1 {
				continue
			}
			next := graphNodeKey{Project: e.ToProject, Pipeline: e.ToPipeline}
			if next == to {
				return true
			}
			if _, ok := seen[next.String()]; ok {
				continue
			}
			seen[next.String()] = struct{}{}
			queue = append(queue, next)
		}
	}
	return false
}

func (w *graphWalk) queued(k graphNodeKey) bool {
	for _, n := range w.queue {
		if n.Key == k {
			return true
		}
	}
	return false
}

func (w *graphWalk) isAncestor(k graphNodeKey) bool {
	if k == w.current {
		return true
	}
	for _, a := range w.ancestors {
		if a == k {
			return true
		}
	}
	return false
}

func (w *graphWalk) currentAncestors() []graphNodeKey {
	out := append([]graphNodeKey{}, w.ancestors...)
	return append(out, w.current)
}

func (w *graphWalk) popNext() (queuedGraphNode, bool) {
	if len(w.queue) == 0 {
		return queuedGraphNode{}, false
	}
	n := w.queue[0]
	w.queue = w.queue[1:]
	return n, true
}

func encodeGraphDigest(nodes []graphNodeView, edges []graphEdgeView, assessment, coverage string, reasons []string) string {
	type row struct {
		Nodes      []string `json:"nodes"`
		Edges      []string `json:"edges"`
		Assessment string   `json:"assessment"`
		Coverage   string   `json:"coverage"`
		Reasons    []string `json:"reasons"`
	}
	nkeys := make([]string, 0, len(nodes))
	for _, n := range nodes {
		nkeys = append(nkeys, graphNodeDigest(n))
	}
	sort.Strings(nkeys)
	ekeys := make([]string, 0, len(edges))
	for _, e := range edges {
		ekeys = append(ekeys, graphEdgeDigest(e))
	}
	sort.Strings(ekeys)
	rkeys := append([]string(nil), reasons...)
	sort.Strings(rkeys)
	raw, err := json.Marshal(row{Nodes: nkeys, Edges: ekeys, Assessment: assessment, Coverage: coverage, Reasons: rkeys})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func graphNodeDigest(n graphNodeView) string {
	type jobRow struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Stage        string `json:"stage"`
		Status       string `json:"status"`
		AllowFailure string `json:"allow_failure"`
		Attempt      string `json:"attempt"`
		Policy       string `json:"policy"`
	}
	jobs := make([]json.RawMessage, 0, len(n.Jobs))
	for _, j := range n.Jobs {
		raw, err := json.Marshal(jobRow{
			ID:           j.ID,
			Name:         deref(j.Name),
			Stage:        deref(j.Stage),
			Status:       deref(j.Status),
			AllowFailure: j.AllowFailure,
			Attempt:      j.Attempt,
			Policy:       j.Policy,
		})
		if err != nil {
			return ""
		}
		jobs = append(jobs, raw)
	}
	sort.Slice(jobs, func(i, j int) bool {
		return bytes.Compare(jobs[i], jobs[j]) < 0
	})
	lineage := make([]json.RawMessage, 0, len(n.Lineage))
	for _, lv := range n.Lineage {
		latest := append([]int64(nil), lv.LatestIDs...)
		history := append([]int64(nil), lv.HistoryIDs...)
		sort.Slice(latest, func(i, j int) bool { return latest[i] < latest[j] })
		sort.Slice(history, func(i, j int) bool { return history[i] < history[j] })
		raw, err := json.Marshal(struct {
			Name        string  `json:"name"`
			LatestKnown bool    `json:"latest_known"`
			LatestIDs   []int64 `json:"latest_ids"`
			HistoryIDs  []int64 `json:"history_ids"`
		}{
			Name:        deref(lv.Name),
			LatestKnown: lv.LatestKnown,
			LatestIDs:   latest,
			HistoryIDs:  history,
		})
		if err != nil {
			return ""
		}
		lineage = append(lineage, raw)
	}
	sort.Slice(lineage, func(i, j int) bool {
		return bytes.Compare(lineage[i], lineage[j]) < 0
	})
	var pipe json.RawMessage
	if n.Pipeline != nil {
		raw, err := json.Marshal(struct {
			ID          int64  `json:"id"`
			Status      string `json:"status"`
			Source      string `json:"source"`
			Ref         string `json:"ref"`
			SHA         string `json:"sha"`
			StatusKnown bool   `json:"status_known"`
		}{
			ID:          n.Pipeline.ID,
			Status:      deref(n.Pipeline.Status),
			Source:      deref(n.Pipeline.Source),
			Ref:         deref(n.Pipeline.Ref),
			SHA:         deref(n.Pipeline.SHA),
			StatusKnown: n.Pipeline.StatusKnown,
		})
		if err != nil {
			return ""
		}
		pipe = raw
	}
	raw, err := json.Marshal(struct {
		ProjectID  string            `json:"project_id"`
		PipelineID int64             `json:"pipeline_id"`
		Depth      int               `json:"depth"`
		Role       string            `json:"role"`
		Pipeline   json.RawMessage   `json:"pipeline"`
		Jobs       []json.RawMessage `json:"jobs"`
		Lineage    []json.RawMessage `json:"lineage"`
	}{
		ProjectID:  n.ProjectID,
		PipelineID: n.PipelineID,
		Depth:      n.Depth,
		Role:       n.Role,
		Pipeline:   pipe,
		Jobs:       jobs,
		Lineage:    lineage,
	})
	if err != nil {
		return ""
	}
	return string(raw)
}

func graphEdgeDigest(e graphEdgeView) string {
	bridge := "0"
	if e.BridgeID != nil {
		bridge = strconv.FormatInt(*e.BridgeID, 10)
	}
	prov := append([]string{}, e.Provenance...)
	sort.Strings(prov)
	return strings.Join([]string{
		e.FromProject,
		strconv.FormatInt(e.FromPipeline, 10),
		e.Kind,
		e.ToProject,
		strconv.FormatInt(e.ToPipeline, 10),
		bridge,
		e.Capability,
		e.SHAComparison,
		strings.Join(prov, ","),
	}, ":")
}

func (w *graphWalk) markQueuedFailure(n queuedGraphNode, kind, provenance string) {
	if w == nil {
		return
	}
	w.unseen = true
	w.stickyIncomplete = true
	w.coverage = downstreamCoveragePartial
	e := graphEdgeView{
		ToProject:  n.Key.Project,
		ToPipeline: n.Key.Pipeline,
		Kind:       kind,
		Capability: w.cap,
		Provenance: []string{provenance},
	}
	if len(n.Ancestors) > 0 {
		from := n.Ancestors[len(n.Ancestors)-1]
		e.FromProject = from.Project
		e.FromPipeline = from.Pipeline
	}
	if n.BridgeID > 0 {
		id := n.BridgeID
		e.BridgeID = &id
	}
	w.edges = append(w.edges, e)
}

func (w *graphWalk) authorize(ctx context.Context, d Deps, projectID string) (string, error) {
	if w != nil {
		if kind, ok := w.authz[projectID]; ok {
			return kind, nil
		}
	}
	kind, err := authorizeDownstream(ctx, d, projectID)
	if err != nil {
		return "", err
	}
	if w != nil {
		if w.authz == nil {
			w.authz = map[string]string{}
		}
		w.authz[projectID] = kind
	}
	return kind, nil
}

func authorizeDownstream(ctx context.Context, d Deps, projectID string) (string, error) {
	if strings.TrimSpace(projectID) == "" {
		return edgeKindIdentity, nil
	}
	_, err := AuthorizeCanonicalProject(ctx, d, projectID)
	if err == nil {
		return "", nil
	}
	if passthroughTypedProviderErr(err) {
		return "", err
	}
	msg := err.Error()
	if strings.HasPrefix(msg, readmeta.CodeAuthzDenied) {
		return edgeKindDenied, nil
	}
	if strings.HasPrefix(msg, readmeta.CodeIdentityUnresolved) {
		return edgeKindIdentity, nil
	}
	if providerStatus(err) == http.StatusForbidden {
		return edgeKindInaccessible, nil
	}
	if providerStatus(err) == http.StatusNotFound {
		return edgeKindIdentity, nil
	}
	return edgeKindInaccessible, nil
}
