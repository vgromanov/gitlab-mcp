package tools

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/gitdiff"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

type diffCacheProvenance struct {
	Projects     []string `json:"projects"`
	HeadSHA      string   `json:"head_sha,omitempty"`
	BaseSHA      string   `json:"base_sha,omitempty"`
	StartSHA     string   `json:"start_sha,omitempty"`
	FromSHA      string   `json:"from_sha,omitempty"`
	ToSHA        string   `json:"to_sha,omitempty"`
	Semantics    string   `json:"semantics"`
	Command      string   `json:"command"`
	Provider     string   `json:"provider"`
	GenerationID string   `json:"generation_id,omitempty"`
}

func (d Deps) cacheAvailable() bool {
	if d.cacheHold != nil {
		return d.cacheHold.Enabled()
	}
	return d.GitCache != nil && d.GitCache.Enabled()
}

func (d Deps) holdCache(ctx context.Context, intent gitcache.AcquireIntent) (*gitcache.ObjectHold, error) {
	auth := NewGitCacheAuthorizer(d)
	ctx = cacheHoldCtx(ctx)
	if d.cacheHold != nil {
		return d.cacheHold.Hold(ctx, intent, auth)
	}
	if d.GitCache == nil {
		return nil, gitcache.ErrDisabled
	}
	return d.GitCache.Hold(ctx, intent, auth)
}

func cacheHoldCtx(ctx context.Context) context.Context {
	b := igl.DefaultBudget()
	b.MaxRequests = 64
	if parent := igl.BudgetFromContext(ctx); parent != nil && parent.MaxElapsed > 0 {
		left := parent.MaxElapsed
		if !parent.ElapsedExceeded() {
			if dl, ok := parent.OriginalDeadline(); ok {
				if rem := time.Until(dl); rem > 0 && rem < left {
					left = rem
				}
			}
		}
		b.MaxElapsed = left
	}
	return igl.WithBudget(ctx, b)
}

func recoverCacheManifest(ctx context.Context, d Deps, q diffQuery, sec readmeta.Section, proved provedManifest) (diffWindowOut, bool) {
	hold, from, to, sem, prov, ok := prepareCacheCompare(ctx, d, q, proved)
	if !ok {
		return diffWindowOut{}, false
	}
	defer hold.Release()
	dir, cleanup, err := d.openCompareDir(ctx, hold.Objects)
	if err != nil {
		return diffWindowOut{}, false
	}
	defer cleanup()
	if err := gitdiff.WriteBare(ctx, dir, hold.Objects); err != nil {
		return diffWindowOut{}, false
	}
	lim := gitdiff.Limits{MaxBytes: manifestByteCap(ctx), Timeout: remainingOrDefault(ctx)}
	res, err := gitdiff.Raw(ctx, dir, from, to, sem, hold.Objects, lim)
	if err != nil {
		if res.Partial {
			sec.AddLimitation(readmeta.CodePartial, res.Reason)
			sec.ContentComplete = readmeta.ContentCompleteFalse
		}
		return diffWindowOut{}, false
	}
	sec.Limitations = []readmeta.Limitation{}
	entries := make([]diffManifestEntry, 0, len(res.Files))
	full := true
	for _, f := range res.Files {
		if b := igl.BudgetFromContext(ctx); b != nil {
			if err := b.AddItem(); err != nil {
				full = false
				sec.AddLimitation(readmeta.CodePartial, "item budget")
				break
			}
		}
		entries = append(entries, entryFromGitFile(f))
	}
	proved = cacheProved(q, proved, from, to, len(entries))
	if !full {
		proved.Full = false
	}
	sec.Source = readmeta.SourceGitCache
	sec.Provider = readmeta.ProviderGit
	out, err := finishManifestWindow(q, sec, proved, entries)
	if err != nil {
		return diffWindowOut{}, false
	}
	prov.Command = res.Command
	out.Provenance = &prov
	return out, true
}

func recoverCacheContent(ctx context.Context, d Deps, q diffQuery, sec readmeta.Section, proved provedManifest, opts diffContentOpts, versionID int64) (diffContentOut, bool) {
	hold, from, to, sem, prov, ok := prepareCacheCompare(ctx, d, q, proved)
	if !ok {
		return diffContentOut{}, false
	}
	defer hold.Release()
	dir, cleanup, err := d.openCompareDir(ctx, hold.Objects)
	if err != nil {
		return diffContentOut{}, false
	}
	defer cleanup()
	if err := gitdiff.WriteBare(ctx, dir, hold.Objects); err != nil {
		return diffContentOut{}, false
	}
	rawLim := gitdiff.Limits{MaxBytes: manifestByteCap(ctx), Timeout: remainingOrDefault(ctx)}
	res, err := gitdiff.Raw(ctx, dir, from, to, sem, hold.Objects, rawLim)
	if err != nil {
		return diffContentOut{}, false
	}
	sec.Limitations = []readmeta.Limitation{}
	files := make([]retainedDiffFile, 0, len(res.Files))
	for _, f := range res.Files {
		if b := igl.BudgetFromContext(ctx); b != nil {
			if err := b.AddItem(); err != nil {
				sec.AddLimitation(readmeta.CodePartial, "item budget")
				break
			}
		}
		files = append(files, retainedDiffFile{entry: entryFromGitFile(f)})
	}
	patchLim := gitdiff.Limits{MaxBytes: patchByteCap(ctx, opts), Timeout: remainingOrDefault(ctx)}
	text, partial, command, err := gitdiff.Patch(ctx, dir, from, to, opts.Paths, patchLim)
	if err != nil && !partial {
		return diffContentOut{}, false
	}
	if b := igl.BudgetFromContext(ctx); b != nil {
		if err := b.ChargeBytes(int64(len(text))); err != nil {
			partial = true
			sec.AddLimitation(readmeta.CodePartial, "byte budget")
		}
	}
	byPath := gitdiff.SplitPatches(text)
	for i := range files {
		e := files[i].entry
		p := firstNonEmpty(pathStr(e.NewPath), pathStr(e.OldPath))
		if patch, ok := byPath[p]; ok {
			files[i].patch = patch
			files[i].patchOK = true
			files[i].sourceHash = hashDiffContentText(diffContentWindowHashScope, patch).Value
		} else if pathSelected(opts.Paths, e) {
			files[i].patchOK = false
		}
	}
	proved = cacheProved(q, proved, from, to, len(files))
	outcomes, selected := matchSelectors(opts.Paths, files, true)
	built, retHash, cropped, knownOmit := buildDiffContentFiles(files, selected, opts)
	sec.Source = readmeta.SourceGitCache
	sec.Provider = readmeta.ProviderGit
	sec.CapabilityVersion = capabilityDiffContentV1
	head := proved.Head
	if head == "" {
		head = to
	}
	sec.HeadSHA = &head
	sec.Consistency = readmeta.ConsistencyConsistent
	sec.ManifestCoverage = readmeta.CoverageFull
	sec.PatchCoverage = readmeta.CoveragePartial
	sec.PaginationExhausted = true
	sec.NextCursor = nil
	if cropped || knownOmit || partial {
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.AddLimitation(readmeta.CodePartial, "content cropped or bounded")
	} else {
		sec.ContentComplete = readmeta.ContentCompleteUnknown
	}
	for _, o := range outcomes {
		if o.Status == diffSelectorAbsent || o.Status == diffSelectorAmbiguous {
			sec.ContentComplete = readmeta.ContentCompleteFalse
			sec.AddLimitation(readmeta.CodePartial, "selector "+o.Status)
		}
	}
	items := len(built)
	sec.Counts.Items = &items
	sec.Counts.Files = &items
	prov.Command = command
	return diffContentOut{
		Section: sec, Selection: selectionOutFrom(q, proved, versionID),
		Files: built, Selectors: outcomes, ReturnedContentHash: retHash, FullPatchHash: nil,
		Provenance: &prov,
	}, true
}

func prepareCacheCompare(ctx context.Context, d Deps, q diffQuery, proved provedManifest) (*gitcache.ObjectHold, string, string, string, diffCacheProvenance, bool) {
	if !d.cacheAvailable() || ctx.Err() != nil {
		return nil, "", "", "", diffCacheProvenance{}, false
	}
	from, to, sem := comparisonSHAs(q, proved)
	if from == "" || to == "" {
		return nil, "", "", "", diffCacheProvenance{}, false
	}
	intent := gitcache.AcquireIntent{
		ProjectID: cacheProjectID(q),
		MRIID:     int(q.IID),
		Depth:     2,
		Transport: "https",
	}
	if d.Config != nil {
		intent.Token = d.Config.Token
	}
	if h, ok := readmeta.ObservedHeadSHA(proved.Head); ok {
		intent.ExpectedHead = plumbing.NewHash(h)
	}
	if b, ok := readmeta.ObservedHeadSHA(proved.Base); ok {
		intent.ExpectedBase = plumbing.NewHash(b)
	}
	if s, ok := readmeta.ObservedHeadSHA(proved.Start); ok {
		intent.ExpectedStart = plumbing.NewHash(s)
	}
	if q.Selection.Mode != diffModeIncremental {
		if intent.ExpectedHead == plumbing.ZeroHash {
			if h, ok := readmeta.ObservedHeadSHA(q.Selection.Head); ok {
				intent.ExpectedHead = plumbing.NewHash(h)
			}
		}
		if intent.ExpectedBase == plumbing.ZeroHash {
			if b, ok := readmeta.ObservedHeadSHA(q.Selection.Base); ok {
				intent.ExpectedBase = plumbing.NewHash(b)
			}
		}
		if intent.ExpectedStart == plumbing.ZeroHash {
			if s, ok := readmeta.ObservedHeadSHA(q.Selection.Start); ok {
				intent.ExpectedStart = plumbing.NewHash(s)
			}
		}
	}
	hold, err := d.holdCache(ctx, intent)
	if err != nil || hold == nil || hold.Objects == nil {
		return nil, "", "", "", diffCacheProvenance{}, false
	}
	g := hold.Result.Grant
	if q.Selection.Mode != diffModeIncremental {
		if mismatchGrant(g, proved, q) {
			_ = hold.Release()
			return nil, "", "", "", diffCacheProvenance{}, false
		}
	}
	if err := gitdiff.RequireCommits(hold.Objects, from, to); err != nil {
		_ = hold.Release()
		return nil, "", "", "", diffCacheProvenance{}, false
	}
	if q.Selection.Mode != diffModeIncremental && proved.Start != "" {
		if err := gitdiff.RequireCommits(hold.Objects, proved.Start); err != nil {
			_ = hold.Release()
			return nil, "", "", "", diffCacheProvenance{}, false
		}
	}
	prov := diffCacheProvenance{
		Projects:     cacheProjects(q, g),
		HeadSHA:      shaOr(proved.Head, g.HeadSHA.String()),
		BaseSHA:      shaOr(proved.Base, g.BaseSHA.String()),
		StartSHA:     shaOr(proved.Start, g.StartSHA.String()),
		FromSHA:      from,
		ToSHA:        to,
		Semantics:    sem,
		Provider:     readmeta.ProviderGit,
		GenerationID: hold.Result.GenerationID,
	}
	return hold, from, to, sem, prov, true
}

func comparisonSHAs(q diffQuery, proved provedManifest) (from, to, sem string) {
	if q.Selection.Mode == diffModeIncremental {
		from, _ = readmeta.ObservedHeadSHA(q.Selection.From)
		to, _ = readmeta.ObservedHeadSHA(q.Selection.To)
		return from, to, gitdiff.SemanticsStraight
	}
	from, _ = readmeta.ObservedHeadSHA(proved.Base)
	to, _ = readmeta.ObservedHeadSHA(proved.Head)
	if from == "" {
		from, _ = readmeta.ObservedHeadSHA(q.Selection.Base)
	}
	if to == "" {
		to, _ = readmeta.ObservedHeadSHA(q.Selection.Head)
	}
	return from, to, gitdiff.SemanticsFullMR
}

func mismatchGrant(g gitcache.Grant, proved provedManifest, q diffQuery) bool {
	if h, ok := readmeta.ObservedHeadSHA(proved.Head); ok && g.HeadSHA.String() != h {
		return true
	}
	if b, ok := readmeta.ObservedHeadSHA(proved.Base); ok && g.BaseSHA.String() != b {
		return true
	}
	if s, ok := readmeta.ObservedHeadSHA(proved.Start); ok && g.StartSHA.String() != s {
		return true
	}
	if q.Selection.Head != "" && g.HeadSHA.String() != strings.ToLower(q.Selection.Head) {
		if h, ok := readmeta.ObservedHeadSHA(q.Selection.Head); ok && g.HeadSHA.String() != h {
			return true
		}
	}
	return false
}

func cacheProved(q diffQuery, proved provedManifest, from, to string, n int) provedManifest {
	if proved.Head == "" {
		proved.Head = to
	}
	if proved.Base == "" {
		proved.Base = from
	}
	if proved.Start == "" && q.Selection.Start != "" {
		proved.Start = q.Selection.Start
	}
	proved.Total = n
	proved.Full = true
	return proved
}

func cacheProjects(q diffQuery, g gitcache.Grant) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	add(g.ProjectID)
	add(g.SourceFork)
	add(g.TargetProjectID)
	add(q.Project)
	add(strconv.FormatInt(q.OwnerID, 10))
	add(strconv.FormatInt(q.SourceProjectID, 10))
	add(strconv.FormatInt(q.TargetProjectID, 10))
	return out
}

func entryFromGitFile(f gitdiff.File) diffManifestEntry {
	oldP, newP := f.OldPath, f.NewPath
	aMode, bMode := f.AMode, f.BMode
	nf, df, rf, bin, sub := f.NewFile, f.DeletedFile, f.RenamedFile, f.Binary, f.Submodule
	collapsed, tooLarge := false, false
	return diffManifestEntry{
		OldPath: &oldP, NewPath: &newP, AMode: &aMode, BMode: &bMode,
		NewFile: &nf, DeletedFile: &df, RenamedFile: &rf,
		Binary: &bin, Submodule: &sub, Collapsed: &collapsed, TooLarge: &tooLarge,
	}
}

func pathSelected(paths []string, e diffManifestEntry) bool {
	oldP, newP := pathStr(e.OldPath), pathStr(e.NewPath)
	for _, p := range paths {
		if p == oldP || p == newP {
			return true
		}
	}
	return false
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func shaOr(primary, fallback string) string {
	if h, ok := readmeta.ObservedHeadSHA(primary); ok {
		return h
	}
	if h, ok := readmeta.ObservedHeadSHA(fallback); ok {
		return h
	}
	return ""
}

func manifestByteCap(ctx context.Context) int {
	if b := igl.BudgetFromContext(ctx); b != nil {
		if left := b.RemainingBytes(); left > 0 && left < 8<<20 {
			return int(left)
		}
	}
	return 8 << 20
}

func patchByteCap(ctx context.Context, opts diffContentOpts) int {
	cap := 1 << 20
	if opts.MaxContentBytes > 0 && opts.MaxContentBytes < cap {
		cap = opts.MaxContentBytes
	}
	if b := igl.BudgetFromContext(ctx); b != nil {
		if left := b.RemainingBytes(); left >= 0 && int(left) < cap {
			if left == 0 {
				return 1
			}
			return int(left)
		}
	}
	return cap
}

func cacheProjectID(q diffQuery) string {
	if p := strings.TrimSpace(q.Project); p != "" {
		return p
	}
	if p := strings.TrimSpace(q.Selection.ProjectID); p != "" {
		return p
	}
	if q.OwnerID > 0 {
		return strconv.FormatInt(q.OwnerID, 10)
	}
	return ""
}

func (d Deps) openCompareDir(ctx context.Context, objs map[plumbing.Hash]pack.Object) (string, func(), error) {
	size := objectBytes(objs)
	if d.GitCache != nil {
		if mgr := d.GitCache.Manager(); mgr != nil {
			dir, cleanup, err := mgr.OpenCompareDir(ctx, size)
			if err != nil {
				return "", nil, err
			}
			return dir, func() { _ = cleanup() }, nil
		}
	}
	dir, err := os.MkdirTemp("", "gitlab-mcp-gitdiff-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func objectBytes(objs map[plumbing.Hash]pack.Object) int64 {
	var n int64
	for _, obj := range objs {
		n += int64(len(obj.Data))
	}
	return n
}

func remainingOrDefault(ctx context.Context) time.Duration {
	if ctx == nil {
		return 15 * time.Second
	}
	if dl, ok := ctx.Deadline(); ok {
		left := time.Until(dl)
		if left > 0 {
			return left
		}
	}
	return 15 * time.Second
}
