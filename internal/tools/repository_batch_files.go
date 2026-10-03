package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	capabilityBatchFilesV1     = "readmeta.batch_get_file_contents.v1"
	batchMaxPaths              = 20
	batchHardMaxReturnedBytes  = int64(1 << 20) // 1 MiB
	batchLocalMaxScannedBytes  = int64(8 << 20) // 8 MiB
	batchLocalMaxRequests      = 16
	batchLocalMaxItems         = 20
	batchMaxBase64EncodedBytes = (batchHardMaxReturnedBytes/3)*4 + 8
)

// batchInvocationMaxElapsed is the local wall-clock cap for one batch invocation.
// Tests shorten via setBatchInvocationMaxElapsedForTest.
var batchInvocationMaxElapsed = 30 * time.Second

func setBatchInvocationMaxElapsedForTest(d time.Duration) (restore func()) {
	prev := batchInvocationMaxElapsed
	batchInvocationMaxElapsed = d
	return func() { batchInvocationMaxElapsed = prev }
}

type batchByteRange struct {
	StartByte int64  `json:"start_byte"`
	EndByte   *int64 `json:"end_byte,omitempty" jsonschema:"Exclusive end byte offset"`
}

type batchPathSpec struct {
	Path  string          `json:"path" jsonschema:"Repository-relative file path"`
	Range *batchByteRange `json:"range,omitempty" jsonschema:"Optional zero-based raw byte range (end exclusive)"`
}

type batchGetFileContentsIn struct {
	ProjectID string          `json:"project_id"`
	CommitSHA string          `json:"commit_sha" jsonschema:"Full 40-hex commit SHA"`
	Paths     []batchPathSpec `json:"paths" jsonschema:"1..20 path objects"`
	MaxBytes  *int64          `json:"max_bytes,omitempty" jsonschema:"Optional returned raw-byte cap (1..1048576)"`
}

type batchItemError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type batchFileItem struct {
	Path                string          `json:"path"`
	Error               *batchItemError `json:"error,omitempty"`
	BlobID              *string         `json:"blob_id"`
	FullContentDigest   *string         `json:"full_content_digest"`
	ReturnedRangeHash   *string         `json:"returned_range_hash,omitempty"`
	ContentEncoding     string          `json:"content_encoding,omitempty"` // utf-8 | base64
	Content             string          `json:"content,omitempty"`
	BytesReturned       *int64          `json:"bytes_returned,omitempty"`
	IsBinary            *bool           `json:"is_binary,omitempty"`
	UTF8Valid           *bool           `json:"utf8_valid,omitempty"`
	RequestedStart      *int64          `json:"requested_start_byte,omitempty"`
	RequestedEndExcl    *int64          `json:"requested_end_byte,omitempty"`
	ObservedStart       *int64          `json:"observed_start_byte,omitempty"`
	ObservedEndExcl     *int64          `json:"observed_end_byte,omitempty"`
	ObservedTotalSize   *int64          `json:"observed_total_size"`
	RangeHonored        *bool           `json:"range_honored,omitempty"`
	WindowComplete      *bool           `json:"window_complete,omitempty"`
	FullContentComplete *bool           `json:"full_content_complete,omitempty"`
	Symlink             *string         `json:"symlink,omitempty"`   // unknown omitted
	Submodule           *string         `json:"submodule,omitempty"` // unknown omitted
	Visited             bool            `json:"visited"`
}

type batchGetFileContentsOut struct {
	ProjectID string           `json:"project_id"`
	CommitSHA string           `json:"commit_sha"`
	Items     []batchFileItem  `json:"items"`
	Section   readmeta.Section `json:"section"`
}

func batchGetFileContents(ctx context.Context, _ *mcp.CallToolRequest, in batchGetFileContentsIn, d Deps) (*mcp.CallToolResult, any, error) {
	// Pre-transport validation (no identity/blob I/O).
	wantSHA, ok := readmeta.ObservedHeadSHA(in.CommitSHA)
	if !ok {
		return nil, nil, fmt.Errorf("%s: commit_sha must be a full 40-hex SHA", readmeta.CodeHTTPError)
	}
	if err := validateBatchPaths(in.Paths); err != nil {
		return nil, nil, fmt.Errorf("%s: %v", readmeta.CodeHTTPError, err)
	}
	maxReturn := batchHardMaxReturnedBytes
	if in.MaxBytes != nil {
		if *in.MaxBytes <= 0 || *in.MaxBytes > batchHardMaxReturnedBytes {
			return nil, nil, fmt.Errorf("%s: max_bytes must be in 1..%d", readmeta.CodeHTTPError, batchHardMaxReturnedBytes)
		}
		maxReturn = *in.MaxBytes
	}

	ctx, budget, release := ensureBatchBudget(ctx)
	defer release()

	section := newBatchFilesSection(d.now())
	section.PaginationExhausted = true // fixed request list; no continuation
	section.NextCursor = nil

	owner, err := AuthorizeCanonicalProject(ctx, d, in.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	pid := strconv.FormatInt(owner.ID, 10)

	commit, _, err := d.Client.Commits.GetCommit(pid, wantSHA, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: verify commit", readmeta.CodeHTTPError)
	}
	gotSHA, ok := readmeta.ObservedHeadSHA("")
	if commit != nil {
		gotSHA, ok = readmeta.ObservedHeadSHA(commit.ID)
	}
	if !ok || gotSHA != wantSHA {
		return nil, nil, fmt.Errorf("%s: commit identity mismatch", readmeta.CodeHTTPError)
	}
	section.HeadSHA = &wantSHA

	items := make([]batchFileItem, len(in.Paths))
	for i, spec := range in.Paths {
		items[i] = batchFileItem{
			Path:    spec.Path,
			Visited: false,
			BlobID:  nil, FullContentDigest: nil, ObservedTotalSize: nil,
		}
	}

	returnedUsed := int64(0)
	globalStop := false
	var globalCode, globalMsg string

	for i, spec := range in.Paths {
		if err := ctx.Err(); err != nil {
			globalStop = true
			globalCode, globalMsg = readmeta.CodeCancelled, "request cancelled"
			markUnvisited(items[i:], globalCode, globalMsg)
			break
		}
		if globalStop {
			break
		}
		if returnedUsed >= maxReturn {
			globalStop = true
			globalCode, globalMsg = readmeta.CodeTooLarge, "returned raw-byte aggregate cap reached"
			markUnvisited(items[i:], globalCode, globalMsg)
			break
		}
		if err := budget.AddItem(); err != nil {
			globalStop = true
			globalCode, globalMsg = budgetCode(err), "item budget exhausted"
			markUnvisited(items[i:], globalCode, globalMsg)
			break
		}

		item := fetchBatchPath(ctx, d, pid, wantSHA, spec, maxReturn-returnedUsed)
		items[i] = item
		if item.BytesReturned != nil {
			returnedUsed += *item.BytesReturned
		}
		if item.Error != nil {
			switch item.Error.Code {
			case readmeta.CodeBudgetBytes, readmeta.CodeBudgetRequests, readmeta.CodeBudgetElapsed, readmeta.CodeBudgetItems:
				globalStop = true
				globalCode, globalMsg = item.Error.Code, item.Error.Message
				if i+1 < len(items) {
					markUnvisited(items[i+1:], globalCode, globalMsg)
				}
			case readmeta.CodeCancelled:
				globalStop = true
				globalCode, globalMsg = item.Error.Code, item.Error.Message
				if i+1 < len(items) {
					markUnvisited(items[i+1:], globalCode, globalMsg)
				}
			}
		}
	}

	finalizeBatchSection(&section, items, budget, globalStop, globalCode, globalMsg)

	out := batchGetFileContentsOut{
		ProjectID: pid,
		CommitSHA: wantSHA,
		Items:     items,
		Section:   section,
	}
	return nil, Out(out), nil
}

func fetchBatchPath(ctx context.Context, d Deps, pid, sha string, spec batchPathSpec, remainReturn int64) batchFileItem {
	item := batchFileItem{
		Path:              spec.Path,
		Visited:           true,
		BlobID:            nil,
		FullContentDigest: nil,
		ObservedTotalSize: nil,
	}
	var startPtr *int64
	var endPtr *int64
	if spec.Range != nil {
		s := spec.Range.StartByte
		startPtr = &s
		item.RequestedStart = &s
		if spec.Range.EndByte != nil {
			e := *spec.Range.EndByte
			endPtr = &e
			item.RequestedEndExcl = &e
		}
	}
	if remainReturn <= 0 {
		item.Error = &batchItemError{Code: readmeta.CodeTooLarge, Message: "no returned-byte budget remaining"}
		wc, fc := false, false
		item.WindowComplete = &wc
		item.FullContentComplete = &fc
		return item
	}

	res := igl.StreamRawFile(ctx, d.Client, igl.RawStreamRequest{
		ProjectID:      pid,
		FilePath:       spec.Path,
		Ref:            sha,
		RangeStart:     startPtr,
		RangeEnd:       endPtr,
		MaxReturnBytes: remainReturn,
	})

	if res.Err != nil {
		item.Error = mapStreamErr(res.Err)
		if item.Error != nil && item.Error.Code == readmeta.CodeInconsistent {
			// Provenance break: never emit unattested provider identity/size.
			item.BlobID = nil
			item.FullContentDigest = nil
			item.ObservedTotalSize = nil
			item.ObservedStart = nil
			item.ObservedEndExcl = nil
			item.ReturnedRangeHash = nil
			item.RangeHonored = nil
			f := false
			item.WindowComplete = &f
			item.FullContentComplete = &f
			return item
		}
		// Preserve any partial bytes only when useful; on hard errors drop content.
		if res.BlobID != "" {
			b := res.BlobID
			item.BlobID = &b
		}
		if res.ContentSHA256 != "" {
			dgst := res.ContentSHA256
			item.FullContentDigest = &dgst
		}
		if res.SizeKnown {
			sz := res.Size
			item.ObservedTotalSize = &sz
		}
		item.ObservedStart = res.ObservedStart
		item.ObservedEndExcl = res.ObservedEndExcl
		if res.RangeRequested {
			h := res.RangeHonored
			item.RangeHonored = &h
		}
		wc := res.WindowComplete
		fc := res.FullContentKnown
		item.WindowComplete = &wc
		item.FullContentComplete = &fc
		if res.Data != nil && len(res.Data) > 0 && item.Error.Code == readmeta.CodePartial {
			attachContent(&item, res.Data, res.ReturnedRangeSHA)
		}
		return item
	}
	if res.BlobID != "" {
		b := res.BlobID
		item.BlobID = &b
	}
	if res.ContentSHA256 != "" {
		dgst := res.ContentSHA256
		item.FullContentDigest = &dgst
	}
	if res.SizeKnown {
		sz := res.Size
		item.ObservedTotalSize = &sz
	}
	item.ObservedStart = res.ObservedStart
	item.ObservedEndExcl = res.ObservedEndExcl
	if res.RangeRequested {
		h := res.RangeHonored
		item.RangeHonored = &h
	}
	wc := res.WindowComplete
	fc := res.FullContentKnown
	item.WindowComplete = &wc
	item.FullContentComplete = &fc
	if res.Status == 404 {
		item.Error = &batchItemError{Code: readmeta.CodeInaccessible, Message: "file not found"}
		return item
	}
	if res.Status == 403 {
		item.Error = &batchItemError{Code: readmeta.CodeAuthzDenied, Message: "path denied"}
		return item
	}
	if res.Status != 200 && res.Status != 206 {
		item.Error = &batchItemError{Code: readmeta.CodeHTTPError, Message: fmt.Sprintf("status %d", res.Status)}
		return item
	}

	attachContent(&item, res.Data, res.ReturnedRangeSHA)

	// Full digest: only attested header or verified full-content retention.
	if item.FullContentDigest == nil && res.FullContentKnown && len(res.Data) > 0 && res.ContentSHA256 == "" {
		// Body retained equals full file but provider did not attest — leave unknown (nil).
		item.FullContentDigest = nil
	}
	if !res.WindowComplete {
		falseVal := false
		item.WindowComplete = &falseVal
	}
	return item
}

func attachContent(item *batchFileItem, data []byte, rangeHash string) {
	n := int64(len(data))
	item.BytesReturned = &n
	if rangeHash != "" {
		h := rangeHash
		item.ReturnedRangeHash = &h
	}
	// UTF-8 validity is independent of the binary/NUL presentation heuristic.
	utf8ok := utf8.Valid(data)
	isBin := !utf8ok || strings.Contains(string(data), "\x00")
	item.UTF8Valid = &utf8ok
	item.IsBinary = &isBin
	if !isBin {
		item.ContentEncoding = "utf-8"
		item.Content = string(data)
		return
	}
	// Bound base64 presentation allocation.
	enc := base64.StdEncoding.EncodeToString(data)
	if int64(len(enc)) > batchMaxBase64EncodedBytes {
		item.Error = &batchItemError{Code: readmeta.CodeTooLarge, Message: "base64 presentation cap"}
		item.Content = ""
		item.ContentEncoding = ""
		return
	}
	item.ContentEncoding = "base64"
	item.Content = enc
}

func mapStreamErr(err error) *batchItemError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return &batchItemError{Code: readmeta.CodeCancelled, Message: "cancelled"}
	case errors.Is(err, igl.ErrBudgetBytes):
		return &batchItemError{Code: readmeta.CodeBudgetBytes, Message: "byte budget exhausted"}
	case errors.Is(err, igl.ErrBudgetRequests):
		return &batchItemError{Code: readmeta.CodeBudgetRequests, Message: "request budget exhausted"}
	case errors.Is(err, igl.ErrBudgetElapsed):
		return &batchItemError{Code: readmeta.CodeBudgetElapsed, Message: "elapsed budget exhausted"}
	case errors.Is(err, igl.ErrBudgetItems):
		return &batchItemError{Code: readmeta.CodeBudgetItems, Message: "item budget exhausted"}
	case errors.Is(err, igl.ErrRangeIgnored):
		return &batchItemError{Code: readmeta.CodePartial, Message: "range ignored by provider"}
	case errors.Is(err, igl.ErrRawProvenance):
		return &batchItemError{Code: readmeta.CodeInconsistent, Message: "raw redirect broke immutable commit binding"}
	case errors.Is(err, gitlab.ErrNotFound):
		return &batchItemError{Code: readmeta.CodeInaccessible, Message: "file not found"}
	default:
		msg := err.Error()
		if strings.Contains(msg, "404") {
			return &batchItemError{Code: readmeta.CodeInaccessible, Message: "file not found"}
		}
		if strings.Contains(msg, "403") {
			return &batchItemError{Code: readmeta.CodeAuthzDenied, Message: "path denied"}
		}
		if strings.Contains(msg, "raw_provenance") || strings.Contains(msg, "ref changed") {
			return &batchItemError{Code: readmeta.CodeInconsistent, Message: "raw redirect broke immutable commit binding"}
		}
		if strings.Contains(msg, "malformed Content-Range") || strings.Contains(msg, "mismatch") {
			return &batchItemError{Code: readmeta.CodePartial, Message: msg}
		}
		return &batchItemError{Code: readmeta.CodeHTTPError, Message: "raw fetch failed"}
	}
}

func markUnvisited(items []batchFileItem, code, msg string) {
	for i := range items {
		if items[i].Visited {
			continue
		}
		items[i].Visited = false
		items[i].Error = &batchItemError{Code: code, Message: msg}
		items[i].BlobID = nil
		items[i].FullContentDigest = nil
		items[i].ObservedTotalSize = nil
		f := false
		items[i].WindowComplete = &f
		items[i].FullContentComplete = &f
	}
}

func finalizeBatchSection(section *readmeta.Section, items []batchFileItem, budget *igl.Budget, globalStop bool, globalCode, globalMsg string) {
	n := len(items)
	section.Counts.Items = &n
	visited := 0
	files := 0
	unknown := false
	anyIncomplete := false
	anySuccess := false
	anyInconsistent := false
	for _, it := range items {
		if it.Visited {
			visited++
		} else {
			anyIncomplete = true
		}
		if it.Error == nil && it.Visited {
			anySuccess = true
			files++
		}
		if it.WindowComplete != nil && !*it.WindowComplete {
			anyIncomplete = true
		}
		if it.FullContentComplete != nil && !*it.FullContentComplete {
			// requested-window complete can still leave full content incomplete — not section-unknown alone
		}
		if it.BlobID == nil && it.Error == nil && it.Visited {
			unknown = true
		}
		if it.Error != nil && (it.Error.Code == readmeta.CodePartial || it.Error.Code == readmeta.CodeUnknownCount) {
			unknown = true
		}
		if it.Error != nil && it.Error.Code == readmeta.CodeInconsistent {
			anyInconsistent = true
		}
	}
	section.Counts.Files = &files
	if budget != nil {
		_, bytesRead, _ := budget.Stats()
		b := bytesRead
		section.Counts.Bytes = &b
	}
	if globalStop && globalCode != "" {
		section.AddLimitation(globalCode, globalMsg)
	}
	if anyInconsistent {
		section.AddLimitation(readmeta.CodeInconsistent, "raw redirect broke immutable commit binding")
	}
	section.ManifestCoverage = readmeta.CoverageFull
	if anyIncomplete || visited < len(items) {
		section.ManifestCoverage = readmeta.CoveragePartial
	}
	section.PatchCoverage = readmeta.CoverageUnknown

	switch {
	case globalCode == readmeta.CodeCancelled:
		section.ContentComplete = readmeta.ContentCompleteFalse
		section.Consistency = readmeta.ConsistencyUnknown
	case anyInconsistent:
		// Cannot attest completeness/consistency after provenance break.
		section.ContentComplete = readmeta.ContentCompleteUnknown
		section.Consistency = readmeta.ConsistencyUnknown
	case anyIncomplete:
		section.ContentComplete = readmeta.ContentCompleteFalse
		if unknown {
			section.ContentComplete = readmeta.ContentCompleteUnknown
		}
		section.Consistency = readmeta.ConsistencyUnknown
	case unknown:
		section.ContentComplete = readmeta.ContentCompleteUnknown
		section.Consistency = readmeta.ConsistencyUnknown
	case anySuccess || len(items) == 0:
		section.ContentComplete = readmeta.ContentCompleteTrue
		if section.HeadSHA != nil {
			section.Consistency = readmeta.ConsistencyConsistent
		} else {
			section.Consistency = readmeta.ConsistencyUnknown
		}
	default:
		// all visited with only per-item errors
		section.ContentComplete = readmeta.ContentCompleteTrue
		section.Consistency = readmeta.ConsistencyConsistent
	}
}

func newBatchFilesSection(now time.Time) readmeta.Section {
	return readmeta.Section{
		RetrievedAt:         now.UTC().Format(time.RFC3339),
		Source:              readmeta.SourceGitLabREST,
		Provider:            readmeta.ProviderGitLab,
		CapabilityVersion:   capabilityBatchFilesV1,
		HeadSHA:             nil,
		PaginationExhausted: true,
		ContentComplete:     readmeta.ContentCompleteUnknown,
		Consistency:         readmeta.ConsistencyUnknown,
		Limitations:         []readmeta.Limitation{},
		NextCursor:          nil,
		Counts:              readmeta.Counts{},
		ManifestCoverage:    readmeta.CoverageUnknown,
		PatchCoverage:       readmeta.CoverageUnknown,
	}
}

// ensureBatchBudget reuses an upstream budget object (counters/cancel preserved)
// or attaches DefaultBudget. Local item/byte/request caps are applied via the
// mutex-safe CapLimits seam (never unsynchronized field writes). A child
// context deadline of min(parent, now+batchInvocationMaxElapsed) is always
// composed so a long/absent upstream MaxElapsed cannot block past the local cap.
func ensureBatchBudget(ctx context.Context) (context.Context, *igl.Budget, func()) {
	return ensureBatchBudgetWithElapsed(ctx, batchInvocationMaxElapsed)
}

func ensureBatchBudgetWithElapsed(ctx context.Context, localMaxElapsed time.Duration) (context.Context, *igl.Budget, func()) {
	if localMaxElapsed <= 0 {
		localMaxElapsed = 30 * time.Second
	}
	var (
		budget *igl.Budget
		owned  bool
	)
	if b := igl.BudgetFromContext(ctx); b != nil {
		budget = b
		budget.CapLimits(batchLocalMaxItems, batchLocalMaxScannedBytes, batchLocalMaxRequests)
	} else {
		budget = igl.DefaultBudget()
		budget.MaxItems = batchLocalMaxItems
		budget.MaxBytes = batchLocalMaxScannedBytes
		budget.MaxRequests = batchLocalMaxRequests
		budget.MaxElapsed = localMaxElapsed
		ctx = igl.WithBudget(ctx, budget)
		owned = true
	}

	// Local deadline: never exceed localMaxElapsed; preserve tighter parent deadline.
	deadline := time.Now().Add(localMaxElapsed)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dctx, dcancel := context.WithDeadline(ctx, deadline)

	release := func() {
		dcancel()
		if owned {
			budget.Cancel()
		}
	}
	return dctx, budget, release
}

func budgetCode(err error) string {
	switch {
	case errors.Is(err, igl.ErrBudgetBytes):
		return readmeta.CodeBudgetBytes
	case errors.Is(err, igl.ErrBudgetRequests):
		return readmeta.CodeBudgetRequests
	case errors.Is(err, igl.ErrBudgetElapsed):
		return readmeta.CodeBudgetElapsed
	case errors.Is(err, igl.ErrBudgetItems):
		return readmeta.CodeBudgetItems
	default:
		return readmeta.CodeHTTPError
	}
}

func validateBatchPaths(paths []batchPathSpec) error {
	if len(paths) < 1 || len(paths) > batchMaxPaths {
		return fmt.Errorf("paths length must be 1..%d", batchMaxPaths)
	}
	seen := make(map[string]struct{}, len(paths))
	for i, p := range paths {
		if err := validateRepoRelPath(p.Path); err != nil {
			return fmt.Errorf("paths[%d]: %w", i, err)
		}
		if _, dup := seen[p.Path]; dup {
			return fmt.Errorf("paths[%d]: duplicate path", i)
		}
		seen[p.Path] = struct{}{}
		if p.Range != nil {
			if p.Range.StartByte < 0 {
				return fmt.Errorf("paths[%d]: start_byte must be >= 0", i)
			}
			if p.Range.EndByte != nil {
				if *p.Range.EndByte <= p.Range.StartByte {
					return fmt.Errorf("paths[%d]: end_byte must be > start_byte", i)
				}
			}
		}
	}
	return nil
}

func validateRepoRelPath(p string) error {
	if p == "" || strings.TrimSpace(p) != p {
		return fmt.Errorf("invalid path")
	}
	if strings.Contains(p, "\x00") || strings.HasPrefix(p, "/") || strings.Contains(p, "//") {
		return fmt.Errorf("invalid path")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("path traversal or empty segment")
		}
	}
	return nil
}
