package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

// RegisterPipelines registers CI pipeline and job tools (gated by USE_PIPELINE).
func RegisterPipelines(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "list_pipelines", Description: "List pipelines in a project"}, listPipelines)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "get_pipeline", Description: "Get a pipeline by id"}, getPipeline)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "list_pipeline_jobs", Description: "List jobs in a pipeline"}, listPipelineJobs)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "list_pipeline_trigger_jobs", Description: "List bridge/trigger jobs in a pipeline"}, listPipelineTriggerJobs)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "get_pipeline_job", Description: "Get a pipeline job"}, getPipelineJob)
	AddTool(s, d, false, "pipeline", &mcp.Tool{Name: "get_pipeline_job_output", Description: "Get a bounded redacted job trace window (prefix, tail, error region, or byte range). Credential patterns are always redacted. This tool does not download an unlimited trace."}, getPipelineJobOutput)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "create_pipeline", Description: "Create a pipeline for a ref"}, createPipeline)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "retry_pipeline", Description: "Retry failed/canceled jobs in a pipeline"}, retryPipeline)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "cancel_pipeline", Description: "Cancel a pipeline"}, cancelPipeline)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "play_pipeline_job", Description: "Run a manual job"}, playPipelineJob)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "retry_pipeline_job", Description: "Retry a single job"}, retryPipelineJob)
	AddTool(s, d, true, "pipeline", &mcp.Tool{Name: "cancel_pipeline_job", Description: "Cancel a running job"}, cancelPipelineJob)
}

func pidOnly(_ context.Context, projectID string, d Deps) (string, error) {
	// Legacy string-match allowlist for out-of-matrix families (wiki/releases/…).
	// Pipeline handlers must use resolvePipelineProject instead.
	def := ""
	if d.Config != nil {
		def = d.Config.DefaultProjectID
	}
	pid, err := ResolveProjectID(projectID, def)
	if err != nil {
		return "", err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return "", err
	}
	return pid, nil
}

// resolvePipelineProject applies canonical project policy for the locked pipeline matrix.
func resolvePipelineProject(ctx context.Context, projectID string, d Deps) (string, error) {
	return resolveProjectAuthz(ctx, d, projectID)
}

type listPipelinesIn struct {
	ProjectID string `json:"project_id"`
	Pagination
	Ref    *string `json:"ref,omitempty"`
	Status *string `json:"status,omitempty"`
}

func listPipelines(ctx context.Context, _ *mcp.CallToolRequest, in listPipelinesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListProjectPipelinesOptions{ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)}}
	if in.Ref != nil {
		opt.Ref = in.Ref
	}
	if in.Status != nil {
		v := gitlab.BuildStateValue(*in.Status)
		opt.Status = &v
	}
	pipes, resp, err := d.Client.Pipelines.ListProjectPipelines(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"pipelines": pipes, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getPipelineIn struct {
	ProjectID  string `json:"project_id"`
	PipelineID int64  `json:"pipeline_id"`
}

func getPipeline(ctx context.Context, _ *mcp.CallToolRequest, in getPipelineIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	p, _, err := d.Client.Pipelines.GetPipeline(pid, in.PipelineID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(p), nil
}

type listPipelineJobsIn struct {
	ProjectID  string `json:"project_id"`
	PipelineID int64  `json:"pipeline_id"`
	Pagination
}

func listPipelineJobs(ctx context.Context, _ *mcp.CallToolRequest, in listPipelineJobsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	jobs, resp, err := d.Client.Jobs.ListPipelineJobs(pid, in.PipelineID, &gitlab.ListJobsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"jobs": jobs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type listPipelineTriggerJobsIn struct {
	ProjectID  string `json:"project_id"`
	PipelineID int64  `json:"pipeline_id"`
	Pagination
}

func listPipelineTriggerJobs(ctx context.Context, _ *mcp.CallToolRequest, in listPipelineTriggerJobsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	bridges, resp, err := d.Client.Jobs.ListPipelineBridges(pid, in.PipelineID, &gitlab.ListJobsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	if err := redactOutOfPolicyDownstreamBridges(ctx, d, bridges); err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"bridges": bridges, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

// redactOutOfPolicyDownstreamBridges nulls DownstreamPipeline when its ProjectID is
// out of policy, non-positive, or otherwise unproven under an active policy.
func redactOutOfPolicyDownstreamBridges(ctx context.Context, d Deps, bridges []*gitlab.Bridge) error {
	if !policyActive(d.Config) || len(bridges) == 0 {
		return nil
	}
	for _, b := range bridges {
		if b == nil || b.DownstreamPipeline == nil {
			continue
		}
		childID := b.DownstreamPipeline.ProjectID
		if childID <= 0 {
			b.DownstreamPipeline = nil
			continue
		}
		_, err := AuthorizeCanonicalProject(ctx, d, strconv.FormatInt(childID, 10))
		if err == nil {
			continue
		}
		msg := err.Error()
		if strings.HasPrefix(msg, readmeta.CodeAuthzDenied) || strings.HasPrefix(msg, readmeta.CodeIdentityUnresolved) {
			b.DownstreamPipeline = nil
			continue
		}
		return err
	}
	return nil
}

type getPipelineJobIn struct {
	ProjectID string `json:"project_id"`
	JobID     int64  `json:"job_id"`
}

func getPipelineJob(ctx context.Context, _ *mcp.CallToolRequest, in getPipelineJobIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	j, _, err := d.Client.Jobs.GetJob(pid, in.JobID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(j), nil
}

type getPipelineJobOutputIn struct {
	ProjectID     string `json:"project_id"`
	JobID         int64  `json:"job_id"`
	TruncateLines int    `json:"truncate_lines,omitempty"`
	Selector      string `json:"selector,omitempty"`
	ErrorMatch    string `json:"error_match,omitempty"`
	StartByte     *int64 `json:"start_byte,omitempty"`
	EndByte       *int64 `json:"end_byte,omitempty"`
	MaxBytes      int    `json:"max_bytes,omitempty"`
	MaxScanBytes  int    `json:"max_scan_bytes,omitempty"`
	MaxLines      int    `json:"max_lines,omitempty"`
}

func getPipelineJobOutput(ctx context.Context, _ *mcp.CallToolRequest, in getPipelineJobOutputIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	q, err := normalizeTraceQuery(in)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token := ""
	if d.Config != nil {
		token = d.Config.Token
	}
	// Tail and range reads keep a redaction margin outside the retained window,
	// and the copier needs one extra byte to observe EOF. A budget already on
	// the context must be raised to that size: BudgetInterceptor would otherwise
	// cut the suffix back and a long trace would be cleared as an unproven tail.
	need := traceBudgetBytes(q.scan, traceContextMargin(token))
	if b := igl.BudgetFromContext(ctx); b == nil {
		b = igl.DefaultBudget()
		b.MaxBytes = need
		b.MaxRequests = 4
		b.MaxElapsed = 30 * time.Second
		ctx = igl.WithBudget(ctx, b)
	} else {
		b.EnsureMinBytes(need)
	}
	res, meta, err := readJobTrace(ctx, d, pid, in.JobID, q)
	if err != nil {
		return nil, nil, err
	}
	win, piece := buildTraceWindow(q, res, meta, token)
	sec := sectionForTrace(d.now(), win, piece, meta)
	return nil, Out(map[string]any{
		"trace":   piece.text,
		"window":  win,
		"section": sec,
	}), nil
}

type traceReadMeta struct {
	rangeIgnored bool
	unprovenTail bool
	scanStopped  bool
}

// traceBudgetBytes is the body cap installed when the caller has no budget.
// A tail reads scan+margin and then one peek byte. A range also keeps
// lookbehind, so the largest read is scan plus both margins, plus the peek.
func traceBudgetBytes(scan, margin int) int64 {
	n := int64(scan) + 2*int64(margin) + 1
	if n > int64(jobTraceHardScan)+1 {
		n = int64(jobTraceHardScan) + 1
	}
	if n < 1 {
		n = 1
	}
	return n
}

func traceContextMargin(token string) int {
	m := jobTraceLookbehind
	if len(token) > m {
		m = len(token)
	}
	if m > traceTokenHold {
		m = traceTokenHold
	}
	return m
}

func readJobTrace(ctx context.Context, d Deps, pid string, jobID int64, q traceQuery) (igl.JobTraceResult, traceReadMeta, error) {
	var meta traceReadMeta
	token := ""
	if d.Config != nil {
		token = d.Config.Token
	}
	margin := traceContextMargin(token)
	switch q.selector {
	case "tail":
		suffix := int64(q.scan) + int64(margin)
		if suffix > int64(jobTraceHardScan) {
			suffix = int64(jobTraceHardScan)
		}
		first := igl.StreamJobTrace(ctx, d.Client, igl.JobTraceRequest{
			ProjectID: pid, JobID: jobID, SuffixBytes: suffix, MaxScanBytes: suffix,
		})
		if errors.Is(first.Err, igl.ErrRangeIgnored) {
			meta.rangeIgnored = true
			second := igl.StreamJobTrace(ctx, d.Client, igl.JobTraceRequest{
				ProjectID: pid, JobID: jobID, MaxScanBytes: int64(q.scan),
			})
			if err := safeTraceErr(second.Err); err != nil {
				return second, meta, err
			}
			if second.Truncated || !second.EOF {
				meta.unprovenTail = true
				meta.scanStopped = true
				second.Data = []byte{}
			}
			return second, meta, nil
		}
		if err := safeTraceErr(first.Err); err != nil {
			return first, meta, err
		}
		if !first.SuffixAnchored {
			meta.unprovenTail = true
			meta.scanStopped = first.Truncated
			first.Data = []byte{}
		}
		return first, meta, nil
	case "range":
		start := *q.start
		lb := int64(margin)
		reqStart := start - lb
		if reqStart < 0 {
			reqStart = 0
		}
		req := igl.JobTraceRequest{
			ProjectID: pid, JobID: jobID, RangeStart: &reqStart, MaxScanBytes: int64(q.scan),
		}
		if q.end != nil {
			end := *q.end + lb
			req.RangeEndExcl = &end
			// Keep both the lookbehind and the trailing margin inside the read,
			// even when the requested window already fills max_scan_bytes.
			fetch := int64(q.scan) + 2*lb
			if fetch > int64(jobTraceHardScan) {
				fetch = int64(jobTraceHardScan)
			}
			span := end - reqStart
			if span > 0 && span < fetch {
				req.MaxScanBytes = span
			} else if fetch > req.MaxScanBytes {
				req.MaxScanBytes = fetch
			}
		}
		res := igl.StreamJobTrace(ctx, d.Client, req)
		if errors.Is(res.Err, igl.ErrRangeIgnored) {
			meta.rangeIgnored = true
			res.Err = nil
			res.Data = []byte{}
			return res, meta, nil
		}
		if err := safeTraceErr(res.Err); err != nil {
			return res, meta, err
		}
		return res, meta, nil
	default:
		res := igl.StreamJobTrace(ctx, d.Client, igl.JobTraceRequest{
			ProjectID: pid, JobID: jobID, MaxScanBytes: int64(q.scan),
		})
		if err := safeTraceErr(res.Err); err != nil {
			return res, meta, err
		}
		if res.Truncated {
			meta.scanStopped = true
		}
		return res, meta, nil
	}
}

func buildTraceWindow(q traceQuery, res igl.JobTraceResult, meta traceReadMeta, token string) (jobTraceWindow, tracePiece) {
	win := jobTraceWindow{
		Selector:     q.selector,
		ScannedBytes: res.Scanned,
		RangeHonored: res.RangeHonored,
		TotalKnown:   res.SizeKnown,
	}
	if res.SizeKnown {
		sz := res.Size
		win.TotalBytes = &sz
	}
	base := int64(0)
	if res.ObservedStart != nil {
		base = *res.ObservedStart
	}
	var piece tracePiece
	switch {
	case meta.rangeIgnored && q.selector == "range":
		piece = tracePiece{}
	case meta.unprovenTail:
		piece = tracePiece{}
	case q.selector == "tail":
		lead := 0
		if q.scan > 0 && len(res.Data) > q.scan {
			lead = len(res.Data) - q.scan
		}
		piece = selectTail(res.Data, base, lead, q.lines, q.output, q.line, token, res.EOF || res.SuffixAnchored, true)
	case q.selector == "error":
		piece = selectError(res.Data, base, q.errorMatch, q.output, q.line, token, res.EOF)
	case q.selector == "range":
		piece = selectRange(res.Data, base, *q.start, q.end, q.output, q.line, token, res.SizeKnown, res.Size)
	default:
		piece = selectPrefix(res.Data, base, q.lines, q.output, q.line, token, res.EOF)
	}
	win.TailProven = q.selector == "tail" && piece.proven && !meta.unprovenTail
	win.ErrorRegionProven = q.selector == "error" && piece.proven
	win.LineCapped = piece.lineCapped
	if !utf8.ValidString(piece.text) {
		piece.text, _ = normalizeUTF8Spans(piece.text, nil)
	}
	win.OutputBytes = len(piece.text)
	win.RedactionCount = piece.redactions
	win.SourceStart = piece.start
	win.SourceEndExclusive = piece.end
	if meta.rangeIgnored {
		win.RangeHonored = false
	}
	return win, piece
}

func sectionForTrace(now time.Time, win jobTraceWindow, piece tracePiece, meta traceReadMeta) readmeta.Section {
	sec := newJobTraceSection(now)
	items := 0
	if piece.text != "" {
		items = strings.Count(piece.text, "\n") + 1
	}
	bytesN := int64(win.OutputBytes)
	sec.Counts.Items = &items
	sec.Counts.Bytes = &bytesN
	if win.TotalKnown {
		sec.ManifestCoverage = readmeta.CoverageFull
	}
	switch {
	case meta.rangeIgnored && win.Selector == "range":
		sec.ContentComplete = readmeta.ContentCompleteUnknown
		sec.PatchCoverage = readmeta.CoverageUnknown
		sec.AddLimitation(readmeta.CodeUnsupported, "Range ignored; requested window was not read")
	case win.Selector == "range" && win.SourceStart == nil:
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.PatchCoverage = readmeta.CoverageUnknown
		sec.AddLimitation(readmeta.CodeBudgetBytes, "requested range was past the bounded scan")
		if !win.RangeHonored {
			sec.AddLimitation(readmeta.CodeUnsupported, "Range ignored; offset was not reached")
		}
	case win.Selector == "tail" && !win.TailProven:
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.PatchCoverage = readmeta.CoverageUnknown
		sec.AddLimitation(readmeta.CodeBudgetBytes, "scan budget exhausted before a proven tail")
		if meta.rangeIgnored {
			sec.AddLimitation(readmeta.CodeUnsupported, "Range ignored; tail was not read from the end")
		}
	case win.Selector == "error" && !win.ErrorRegionProven && piece.text == "":
		if meta.scanStopped {
			sec.ContentComplete = readmeta.ContentCompleteFalse
			sec.PatchCoverage = readmeta.CoverageUnknown
			sec.AddLimitation(readmeta.CodeBudgetBytes, "scan budget exhausted before an error region")
		} else {
			sec.ContentComplete = readmeta.ContentCompleteTrue
			sec.PatchCoverage = readmeta.CoverageFull
			if win.TotalKnown {
				sec.ManifestCoverage = readmeta.CoverageFull
			}
		}
	case piece.full && win.TotalKnown:
		sec.ContentComplete = readmeta.ContentCompleteTrue
		sec.PatchCoverage = readmeta.CoverageFull
	case win.SourceStart != nil:
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.PatchCoverage = readmeta.CoveragePartial
	default:
		sec.ContentComplete = readmeta.ContentCompleteUnknown
		sec.PatchCoverage = readmeta.CoverageUnknown
	}
	if piece.lineCapped {
		sec.AddLimitation(readmeta.CodeTooLarge, "a line exceeded the retained line cap")
		if sec.ContentComplete == readmeta.ContentCompleteTrue {
			sec.ContentComplete = readmeta.ContentCompleteFalse
			sec.PatchCoverage = readmeta.CoveragePartial
		}
	}
	if piece.outCapped {
		sec.AddLimitation(readmeta.CodePartial, "output byte cap trimmed the window")
		if sec.ContentComplete == readmeta.ContentCompleteTrue {
			sec.ContentComplete = readmeta.ContentCompleteFalse
			sec.PatchCoverage = readmeta.CoveragePartial
		}
	}
	if meta.scanStopped && win.Selector == "prefix" {
		sec.AddLimitation(readmeta.CodeBudgetBytes, "scan budget stopped the prefix")
		if sec.ContentComplete == readmeta.ContentCompleteTrue {
			sec.ContentComplete = readmeta.ContentCompleteFalse
			sec.PatchCoverage = readmeta.CoveragePartial
		}
	}
	return sec
}

func safeTraceErr(err error) error {
	if err == nil || errors.Is(err, igl.ErrRangeIgnored) {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: job trace cancelled", readmeta.CodeCancelled)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, igl.ErrBudgetElapsed) {
		return fmt.Errorf("%s: job trace budget elapsed", readmeta.CodeBudgetElapsed)
	}
	if errors.Is(err, igl.ErrBudgetBytes) || errors.Is(err, igl.ErrBudgetRequests) {
		return fmt.Errorf("%s: job trace budget exhausted", readmeta.CodeBudgetBytes)
	}
	if errors.Is(err, gitlab.ErrNotFound) {
		return fmt.Errorf("%s: job trace not found", readmeta.CodeInaccessible)
	}
	return fmt.Errorf("%s: job trace request failed", readmeta.CodeHTTPError)
}

type createPipelineIn struct {
	ProjectID string            `json:"project_id"`
	Ref       string            `json:"ref"`
	Variables map[string]string `json:"variables,omitempty"`
}

func createPipeline(ctx context.Context, _ *mcp.CallToolRequest, in createPipelineIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreatePipelineOptions{Ref: gitlab.Ptr(in.Ref)}
	if len(in.Variables) > 0 {
		var vars []*gitlab.PipelineVariableOptions
		for k, v := range in.Variables {
			kk, vv := k, v
			vars = append(vars, &gitlab.PipelineVariableOptions{Key: gitlab.Ptr(kk), Value: gitlab.Ptr(vv)})
		}
		opt.Variables = &vars
	}
	p, _, err := d.Client.Pipelines.CreatePipeline(pid, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(p), nil
}

type retryPipelineIn struct {
	ProjectID  string `json:"project_id"`
	PipelineID int64  `json:"pipeline_id"`
}

func retryPipeline(ctx context.Context, _ *mcp.CallToolRequest, in retryPipelineIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	p, _, err := d.Client.Pipelines.RetryPipelineBuild(pid, in.PipelineID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(p), nil
}

type cancelPipelineIn struct {
	ProjectID  string `json:"project_id"`
	PipelineID int64  `json:"pipeline_id"`
}

func cancelPipeline(ctx context.Context, _ *mcp.CallToolRequest, in cancelPipelineIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	p, _, err := d.Client.Pipelines.CancelPipelineBuild(pid, in.PipelineID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(p), nil
}

type playPipelineJobIn struct {
	ProjectID string `json:"project_id"`
	JobID     int64  `json:"job_id"`
}

func playPipelineJob(ctx context.Context, _ *mcp.CallToolRequest, in playPipelineJobIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	j, _, err := d.Client.Jobs.PlayJob(pid, in.JobID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(j), nil
}

type retryPipelineJobIn struct {
	ProjectID string `json:"project_id"`
	JobID     int64  `json:"job_id"`
}

func retryPipelineJob(ctx context.Context, _ *mcp.CallToolRequest, in retryPipelineJobIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	j, _, err := d.Client.Jobs.RetryJob(pid, in.JobID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(j), nil
}

type cancelPipelineJobIn struct {
	ProjectID string `json:"project_id"`
	JobID     int64  `json:"job_id"`
}

func cancelPipelineJob(ctx context.Context, _ *mcp.CallToolRequest, in cancelPipelineJobIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := resolvePipelineProject(ctx, in.ProjectID, d)
	if err != nil {
		return nil, nil, err
	}
	j, _, err := d.Client.Jobs.CancelJob(pid, in.JobID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(j), nil
}
