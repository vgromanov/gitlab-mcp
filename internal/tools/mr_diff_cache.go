package tools

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/gitdiff"
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
	if manifestByteCap(ctx) == 0 {
		return diffWindowOut{}, false
	}
	hold, from, to, sem, prov, ok := prepareCacheCompare(ctx, d, q, proved)
	if !ok {
		return diffWindowOut{}, false
	}
	defer hold.Release()
	lim := gitdiff.Limits{MaxBytes: manifestByteCap(ctx), Timeout: remainingOrDefault(ctx)}
	cmp, err := gitdiff.Compare(ctx, from, to, sem, hold.Objects, lim)
	res := cmp.Result
	if err != nil {
		if res.Partial {
			sec.AddLimitation(readmeta.CodePartial, res.Reason)
			sec.ContentComplete = readmeta.ContentCompleteFalse
		}
		return diffWindowOut{}, false
	}
	if !chargeRawComparison(ctx, res.RawBytes) {
		return diffWindowOut{}, false
	}
	sec.Limitations = []readmeta.Limitation{}
	all := make([]diffManifestEntry, 0, len(res.Files))
	for _, f := range res.Files {
		all = append(all, entryFromGitFile(f))
	}
	sortManifestEntries(all)
	already := proved.Charged
	entries := make([]diffManifestEntry, 0, len(all))
	full := true
	for i, e := range all {
		if b := igl.BudgetFromContext(ctx); b != nil && i >= already {
			if err := b.AddItem(); err != nil {
				full = false
				sec.AddLimitation(readmeta.CodePartial, "item budget")
				break
			}
		}
		entries = append(entries, e)
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
	if manifestByteCap(ctx) == 0 {
		return diffContentOut{}, false
	}
	hold, from, to, sem, prov, ok := prepareCacheCompare(ctx, d, q, proved)
	if !ok {
		return diffContentOut{}, false
	}
	defer hold.Release()
	rawLim := gitdiff.Limits{MaxBytes: manifestByteCap(ctx), Timeout: remainingOrDefault(ctx)}
	cmp, err := gitdiff.Compare(ctx, from, to, sem, hold.Objects, rawLim)
	if err != nil {
		return diffContentOut{}, false
	}
	res := cmp.Result
	if !chargeRawComparison(ctx, res.RawBytes) {
		return diffContentOut{}, false
	}
	sec.Limitations = []readmeta.Limitation{}
	all := make([]retainedDiffFile, 0, len(res.Files))
	for _, f := range res.Files {
		all = append(all, retainedDiffFile{entry: entryFromGitFile(f)})
	}
	sort.SliceStable(all, func(i, j int) bool {
		return manifestOrderLess(all[i].entry, all[j].entry)
	})
	already := proved.Charged
	files := make([]retainedDiffFile, 0, len(all))
	truncated := false
	for i, f := range all {
		if b := igl.BudgetFromContext(ctx); b != nil && i >= already {
			if err := b.AddItem(); err != nil {
				truncated = true
				sec.AddLimitation(readmeta.CodePartial, "item budget")
				break
			}
		}
		files = append(files, f)
	}
	patchLim := gitdiff.Limits{MaxBytes: patchByteCap(ctx, opts), Timeout: remainingOrDefault(ctx)}
	pres, err := cmp.Patches(ctx, patchPathspec(opts.Paths, files), patchLim)
	partial := pres.Partial
	if err != nil && !partial {
		return diffContentOut{}, false
	}
	if b := igl.BudgetFromContext(ctx); b != nil {
		if err := b.ChargeBytes(int64(pres.Bytes)); err != nil {
			partial = true
			sec.AddLimitation(readmeta.CodePartial, "byte budget")
		}
	}
	command := pres.Command
	byPath := make(map[string]string, 2*len(pres.Patches))
	for _, p := range pres.Patches {
		byPath[p.OldPath] = p.Text
		byPath[p.NewPath] = p.Text
	}
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
	outcomes, selected := matchSelectors(opts.Paths, files, !truncated)
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
	if cropped || knownOmit || partial || truncated {
		sec.ContentComplete = readmeta.ContentCompleteFalse
		if !truncated {
			sec.AddLimitation(readmeta.CodePartial, "content cropped or bounded")
		}
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

func patchPathspec(requested []string, files []retainedDiffFile) []string {
	if len(requested) == 0 {
		return requested
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, f := range files {
		if !pathSelected(requested, f.entry) {
			continue
		}
		add(pathStr(f.entry.OldPath))
		add(pathStr(f.entry.NewPath))
	}
	for _, p := range requested {
		add(p)
	}
	return out
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

// chargeRawComparison charges the raw comparison listing against max_bytes and
// reports false when that exhausts the byte budget, so no patch work follows.
func chargeRawComparison(ctx context.Context, n int) bool {
	b := igl.BudgetFromContext(ctx)
	if b == nil {
		return true
	}
	if err := b.ChargeBytes(int64(n)); err != nil {
		return false
	}
	return b.RemainingBytes() != 0
}

func manifestByteCap(ctx context.Context) int {
	if b := igl.BudgetFromContext(ctx); b != nil {
		left := b.RemainingBytes()
		if left == 0 {
			return 0
		}
		if left > 0 && left < 8<<20 {
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
