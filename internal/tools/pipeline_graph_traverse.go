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
	current   graphNodeKey
	pipe      *pipelineView
	depth     int
	phase     string
	visited   map[string]struct{}
	queue     []queuedGraphNode
	nodes     []graphNodeView
	edges     []graphEdgeView
	block     bool
	partial   bool
	unknown   bool
	reasons   []string
	outcomes  []policyOutcome
	cap       string
	coverage  string
	unseen    bool
	bridgesOn bool
	nodeCount int
	maxDepth  int
	maxNodes  int
	ancestors []graphNodeKey
	authz     map[string]string
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
	}
}

func restoreGraphWalk(pipe *pipelineView, gc *cursor.GraphCont, depth, nodes int) (*graphWalk, error) {
	if gc == nil {
		return nil, fmt.Errorf("%s: graph continuation missing", cursor.ResyncRequired)
	}
	w := &graphWalk{
		current:   graphNodeKey{Project: gc.NP, Pipeline: gc.NI},
		pipe:      pipe,
		depth:     gc.D,
		phase:     gc.Phase,
		visited:   map[string]struct{}{},
		block:     gc.Block,
		partial:   gc.Part,
		unknown:   gc.Unk,
		coverage:  gc.Cov,
		unseen:    gc.Cov != downstreamCoverageComplete,
		cap:       gc.Cap,
		bridgesOn: gc.Cap == bridgeCapabilityBridges,
		nodeCount: gc.N,
		maxDepth:  depth,
		maxNodes:  nodes,
		authz:     map[string]string{},
	}
	if w.current.Project != pidOf(pipe) && pipe != nil {
		// current node identity is the continuation NP, not necessarily root.
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
	return w, nil
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
	i := strings.LastIndexByte(item, ':')
	if i <= 0 {
		return queuedGraphNode{}, false
	}
	depth, err := strconv.Atoi(item[i+1:])
	if err != nil || depth < 1 {
		return queuedGraphNode{}, false
	}
	proj, id, ok := splitVisitKey(item[:i])
	if !ok {
		return queuedGraphNode{}, false
	}
	return queuedGraphNode{Key: graphNodeKey{Project: proj, Pipeline: id}, Depth: depth}, true
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
		q = append(q, n.Key.String()+":"+strconv.Itoa(n.Depth))
	}
	cov := w.coverage
	if cov == "" {
		cov = downstreamCoverageUnknown
	}
	return &cursor.GraphCont{
		V:     cursor.GraphContSchemaG1,
		Phase: w.phase,
		NP:    w.current.Project,
		NI:    w.current.Pipeline,
		D:     w.depth,
		Vis:   vis,
		Q:     q,
		N:     w.nodeCount,
		Block: w.block,
		Part:  w.partial,
		Unk:   w.unknown,
		Cov:   cov,
		Cap:   w.cap,
	}
}

func collectBridgePage(ctx context.Context, d Deps, budget *igl.Budget, pid string, pipelineID int64, page, perPage int, prior map[int64]struct{}) (bridgePage, error) {
	path := fmt.Sprintf("projects/%s/pipelines/%d/bridges", gitlab.PathEscape(pid), pipelineID)
	opt := &gitlab.ListJobsOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	out := bridgePage{}
	seen := map[int64]struct{}{}
	resp, err := igl.StreamJSONArray(ctx, d.Client, http.MethodGet, path, opt, func(raw json.RawMessage) error {
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
		if err := budget.AddItem(); err != nil {
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

func (w *graphWalk) ingestBridges(parent graphNodeKey, parentSHA string, page bridgePage) {
	w.bridgesOn = true
	if page.Unsupported {
		w.cap = bridgeCapabilityUnknown
		w.unseen = true
		w.coverage = downstreamCoverageUnknown
		w.edges = append(w.edges, graphEdgeView{
			FromProject:  parent.Project,
			FromPipeline: parent.Pipeline,
			Kind:         edgeKindUnsupported,
			Capability:   bridgeCapabilityUnknown,
			Provenance:   []string{"bridges_404"},
		})
		return
	}
	if page.Inaccessible {
		w.cap = bridgeCapabilityBridges
		w.unseen = true
		w.coverage = downstreamCoveragePartial
		w.edges = append(w.edges, graphEdgeView{
			FromProject:  parent.Project,
			FromPipeline: parent.Pipeline,
			Kind:         edgeKindInaccessible,
			Capability:   bridgeCapabilityBridges,
			Provenance:   []string{"bridges_403"},
		})
		return
	}
	w.cap = bridgeCapabilityBridges
	if !w.hasIncompleteEdges() {
		w.unseen = false
	}
	groups := buildLineage(bridgeJobs(page.Bridges), lineageCarry{})
	w.recordOutcomes(groups)
	for _, br := range page.Bridges {
		w.addBridgeEdge(parent, parentSHA, br)
	}
}

func (w *graphWalk) hasIncompleteEdges() bool {
	if w == nil {
		return false
	}
	for _, e := range w.edges {
		switch e.Kind {
		case edgeKindMissing, edgeKindDenied, edgeKindIdentity, edgeKindDepthStop, edgeKindNodeStop, edgeKindUnsupported, edgeKindInaccessible:
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

func (w *graphWalk) addBridgeEdge(parent graphNodeKey, parentSHA string, br graphBridge) {
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
		w.edges = append(w.edges, base)
		return
	}
	if br.ChildProject < 1 {
		base.Kind = edgeKindIdentity
		w.unseen = true
		w.edges = append(w.edges, base)
		return
	}
	if br.ChildPipeline < 1 {
		base.Kind = edgeKindMissing
		base.ToProject = strconv.FormatInt(br.ChildProject, 10)
		w.unseen = true
		w.edges = append(w.edges, base)
		return
	}
	child := graphNodeKey{Project: strconv.FormatInt(br.ChildProject, 10), Pipeline: br.ChildPipeline}
	base.ToProject = child.Project
	base.ToPipeline = child.Pipeline
	base.SHAComparison = compareSHA(br.ChildSHA, parentSHA)
	if w.depth+1 > w.maxDepth {
		base.Kind = edgeKindDepthStop
		w.unseen = true
		w.coverage = downstreamCoveragePartial
		w.edges = append(w.edges, base)
		return
	}
	if _, seen := w.visited[child.String()]; seen || w.queued(child) || w.isAncestor(child) {
		if w.isAncestor(child) || child == w.current {
			base.Kind = edgeKindCycle
		} else {
			base.Kind = edgeKindShared
		}
		base.Provenance = append(base.Provenance, base.Kind)
		w.edges = append(w.edges, base)
		return
	}
	if w.nodeCount >= w.maxNodes {
		base.Kind = edgeKindNodeStop
		w.unseen = true
		w.coverage = downstreamCoveragePartial
		w.edges = append(w.edges, base)
		return
	}
	base.Kind = edgeKindBridge
	w.edges = append(w.edges, base)
	w.nodeCount++
	w.queue = append(w.queue, queuedGraphNode{
		Key:       child,
		Depth:     w.depth + 1,
		ParentSHA: parentSHA,
		Ancestors: append(append([]graphNodeKey{}, w.currentAncestors()...), parent),
	})
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

func encodeGraphDigest(nodes []graphNodeView, edges []graphEdgeView, assessment, coverage string) string {
	type row struct {
		Nodes      []string `json:"nodes"`
		Edges      []string `json:"edges"`
		Assessment string   `json:"assessment"`
		Coverage   string   `json:"coverage"`
	}
	nkeys := make([]string, 0, len(nodes))
	for _, n := range nodes {
		nkeys = append(nkeys, n.ProjectID+":"+strconv.FormatInt(n.PipelineID, 10)+":"+n.Role)
	}
	sort.Strings(nkeys)
	ekeys := make([]string, 0, len(edges))
	for _, e := range edges {
		ekeys = append(ekeys, strings.Join([]string{e.FromProject, strconv.FormatInt(e.FromPipeline, 10), e.Kind, e.ToProject, strconv.FormatInt(e.ToPipeline, 10)}, ":"))
	}
	sort.Strings(ekeys)
	raw, err := json.Marshal(row{Nodes: nkeys, Edges: ekeys, Assessment: assessment, Coverage: coverage})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
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
