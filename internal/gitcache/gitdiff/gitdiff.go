// Package gitdiff compares two commits held in memory using in-process go-git.
// It never invokes the git executable, never touches the file system, does not
// acquire remotes, and does not check out a work tree.
package gitdiff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	godiff "github.com/go-git/go-git/v5/utils/diff"
	"github.com/sergi/go-diff/diffmatchpatch"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

const (
	SemanticsFullMR   = "full_mr_base_head"
	SemanticsStraight = "incremental_straight"

	// RawCommand and PatchCommand are provenance labels for the in-process
	// comparison; no external program is involved.
	RawCommand   = "go-git object.DiffTreeWithOptions rename-score=50 (in-process)"
	PatchCommand = "go-git unified diff context=3 (in-process)"

	defaultTimeout = 15 * time.Second
	defaultMaxRaw  = 8 << 20
	renameScore    = 50
	renameLimit    = 200
	contextLines   = 3
	binarySniff    = 8000
	gitModeMissing = "0"
	rawModeMissing = "000000"
	rawModeWidth   = 6
)

var (
	ErrObject  = errors.New("gitdiff: required object is missing or mismatched")
	ErrPartial = errors.New("gitdiff: comparison output or time was bounded")
)

// Limits bound one comparison step. Manifest and patch callers pass
// independent caps.
type Limits struct {
	MaxBytes int
	Timeout  time.Duration
}

// File is one comparison row with GitLab-shaped flags.
type File struct {
	OldPath     string
	NewPath     string
	AMode       string
	BMode       string
	Status      string
	NewFile     bool
	DeletedFile bool
	RenamedFile bool
	Binary      bool
	Submodule   bool
}

// Result is a manifest comparison. Partial means a cap or deadline stopped it.
type Result struct {
	Files []File
	// RawBytes is the size of the equivalent raw -z listing. Callers charge it
	// against the invocation byte budget.
	RawBytes  int
	Partial   bool
	Reason    string
	Command   string
	From      string
	To        string
	Semantics string
}

// Patch is one rendered unified diff for a file.
type Patch struct {
	OldPath string
	NewPath string
	Text    string
}

// PatchResult is a bounded set of rendered patches.
type PatchResult struct {
	Patches []Patch
	// Bytes is the total size of the rendered patches.
	Bytes   int
	Partial bool
	Reason  string
	Command string
}

// Comparison is one in-process tree comparison that can render patches.
type Comparison struct {
	Result
	changes []*object.Change
	objs    map[plumbing.Hash]pack.Object
}

func (l Limits) apply(fallbackBytes int) Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = fallbackBytes
	}
	if l.Timeout <= 0 {
		l.Timeout = defaultTimeout
	}
	return l
}

// RequireCommits fails closed when a comparison SHA is missing or not a commit.
func RequireCommits(objs map[plumbing.Hash]pack.Object, shas ...string) error {
	for _, raw := range shas {
		if strings.TrimSpace(raw) == "" {
			return ErrObject
		}
		h := plumbing.NewHash(raw)
		obj, ok := objs[h]
		if !ok || obj.Type != "commit" || obj.Hash != h {
			return ErrObject
		}
	}
	return nil
}

// Compare diffs the trees of from and to, straight, with rename detection. The
// returned Comparison renders patches on demand without recomputing the diff.
func Compare(ctx context.Context, from, to, semantics string, objs map[plumbing.Hash]pack.Object, lim Limits) (*Comparison, error) {
	c := &Comparison{objs: objs}
	c.From, c.To, c.Semantics, c.Command = from, to, semantics, RawCommand
	if err := RequireCommits(objs, from, to); err != nil {
		return c, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lim = lim.apply(defaultMaxRaw)
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	store := &objectStore{objs: objs, live: ctx.Err}
	fromTree, err := commitTree(store, from)
	if err != nil {
		if ctx.Err() != nil {
			return c, c.bound(ctx)
		}
		return c, err
	}
	toTree, err := commitTree(store, to)
	if err != nil {
		if ctx.Err() != nil {
			return c, c.bound(ctx)
		}
		return c, err
	}
	changes, renamePartial, err := diffWithRenames(ctx, fromTree, toTree, renameScore, renameLimit, objs)
	store.live = nil
	if err != nil {
		return c, c.boundOrObject(ctx, err)
	}
	if renamePartial {
		c.Partial, c.Reason = true, "rename limit"
	}
	c.Files = []File{}
	for _, ch := range changes {
		if ctx.Err() != nil {
			return c, c.bound(ctx)
		}
		f, err := fileFromChange(ch, objs)
		if err != nil {
			return c, err
		}
		size := rawRowSize(f)
		if c.RawBytes+size > lim.MaxBytes {
			c.Partial, c.Reason = true, "output limit"
			return c, ErrPartial
		}
		c.RawBytes += size
		c.Files = append(c.Files, f)
		c.changes = append(c.changes, ch)
	}
	if renamePartial {
		return c, ErrPartial
	}
	return c, nil
}

func (c *Comparison) boundOrObject(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, object.ErrCanceled) {
		return c.bound(ctx)
	}
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return fmt.Errorf("%w: %v", ErrObject, err)
	}
	return err
}

func (c *Comparison) bound(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	c.Partial, c.Reason = true, "time limit"
	return ErrPartial
}

// Patches renders unified diffs for the changes whose old or new path equals
// one of paths exactly (no globbing); empty paths selects every non-submodule
// change. Output is bounded per file: a file whose patch would not fit is
// omitted and the result is marked partial.
func (c *Comparison) Patches(ctx context.Context, paths []string, lim Limits) (PatchResult, error) {
	out := PatchResult{Command: PatchCommand, Patches: []Patch{}}
	if ctx == nil {
		ctx = context.Background()
	}
	lim = lim.apply(1 << 20)
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	want := map[string]struct{}{}
	for _, p := range paths {
		want[p] = struct{}{}
	}
	for i, ch := range c.changes {
		f := c.Files[i]
		if f.Submodule {
			continue
		}
		if len(want) > 0 {
			_, okOld := want[f.OldPath]
			_, okNew := want[f.NewPath]
			if !okOld && !okNew {
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return c.patchBound(out, ctx)
		}
		text, err := renderPatch(ctx, ch, f, lim.MaxBytes-out.Bytes, c.objs)
		if err != nil {
			if errors.Is(err, errCapped) {
				out.Partial, out.Reason = true, "output limit"
				return out, ErrPartial
			}
			if ctx.Err() != nil || errors.Is(err, object.ErrCanceled) || errors.Is(err, context.DeadlineExceeded) {
				return c.patchBound(out, ctx)
			}
			return out, err
		}
		out.Bytes += len(text)
		out.Patches = append(out.Patches, Patch{OldPath: f.OldPath, NewPath: f.NewPath, Text: text})
	}
	return out, nil
}

func (c *Comparison) patchBound(out PatchResult, ctx context.Context) (PatchResult, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return out, context.Canceled
	}
	out.Partial, out.Reason = true, "time limit"
	return out, ErrPartial
}

var errCapped = errors.New("gitdiff: output capped")

func renderPatch(ctx context.Context, ch *object.Change, f File, room int, objs map[plumbing.Hash]pack.Object) (string, error) {
	if room <= 0 {
		return "", errCapped
	}
	p, err := changePatch(ctx, ch, objs)
	if err != nil {
		return "", err
	}
	if !f.Binary && (f.NewFile || f.DeletedFile) && !hasChunks(p) {
		// go-git frames a patch without chunks as binary, but an empty text
		// file added or deleted has no patch body, like the API's empty diff.
		return "", nil
	}
	var w capWriter
	w.max = room
	if err := fdiff.NewUnifiedEncoder(&w, contextLines).Encode(p); err != nil {
		if w.hit {
			return "", errCapped
		}
		return "", err
	}
	return w.buf.String(), nil
}

// changePatch is object.Change.PatchContext with a deadline-bounded line diff.
// go-git's version diffs with a fixed one-hour timeout and never consults the
// context while doing so. Here the diff library gets the time left on ctx, and a
// diff that used it all is discarded instead of returned as a degraded
// delete-plus-insert.
func changePatch(ctx context.Context, ch *object.Change, objs map[plumbing.Hash]pack.Object) (fdiff.Patch, error) {
	fp := &filePatch{from: ch.From, to: ch.To}
	fromBytes, fromOK := blobTextBytes(objs, ch.From.TreeEntry)
	toBytes, toOK := blobTextBytes(objs, ch.To.TreeEntry)
	if !fromOK || !toOK {
		return &patch{[]fdiff.FilePatch{fp}}, nil
	}
	fromContent := string(fromBytes)
	toContent := string(toBytes)

	budget := time.Hour
	if dl, ok := ctx.Deadline(); ok {
		budget = time.Until(dl)
	}
	if err := ctx.Err(); err != nil || budget <= 0 {
		return nil, context.DeadlineExceeded
	}
	start := time.Now()
	diffs := godiff.DoWithTimeout(fromContent, toContent, budget)
	if err := ctx.Err(); err != nil || time.Since(start) >= budget {
		return nil, context.DeadlineExceeded
	}
	for _, d := range diffs {
		var op fdiff.Operation
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			op = fdiff.Equal
		case diffmatchpatch.DiffDelete:
			op = fdiff.Delete
		case diffmatchpatch.DiffInsert:
			op = fdiff.Add
		}
		fp.chunks = append(fp.chunks, chunk{d.Text, op})
	}
	return &patch{[]fdiff.FilePatch{fp}}, nil
}

type patch struct{ files []fdiff.FilePatch }

func (p *patch) FilePatches() []fdiff.FilePatch { return p.files }
func (p *patch) Message() string                { return "" }

// filePatch mirrors go-git's text file patch: a patch without chunks is
// reported as binary.
type filePatch struct {
	chunks   []fdiff.Chunk
	from, to object.ChangeEntry
}

func (p *filePatch) IsBinary() bool        { return len(p.chunks) == 0 }
func (p *filePatch) Chunks() []fdiff.Chunk { return p.chunks }

func (p *filePatch) Files() (fdiff.File, fdiff.File) {
	var from, to fdiff.File
	if p.from.TreeEntry.Mode.IsFile() {
		from = entryFile{p.from}
	}
	if p.to.TreeEntry.Mode.IsFile() {
		to = entryFile{p.to}
	}
	return from, to
}

type entryFile struct{ e object.ChangeEntry }

func (f entryFile) Hash() plumbing.Hash     { return f.e.TreeEntry.Hash }
func (f entryFile) Mode() filemode.FileMode { return f.e.TreeEntry.Mode }
func (f entryFile) Path() string            { return f.e.Name }

type chunk struct {
	text string
	op   fdiff.Operation
}

func (c chunk) Content() string       { return c.text }
func (c chunk) Type() fdiff.Operation { return c.op }

func hasChunks(p fdiff.Patch) bool {
	for _, fp := range p.FilePatches() {
		if len(fp.Chunks()) > 0 {
			return true
		}
	}
	return false
}

// capWriter is a hard-capped writer.
type capWriter struct {
	buf bytes.Buffer
	max int
	hit bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		c.hit = true
		return 0, errCapped
	}
	return c.buf.Write(p)
}

func commitTree(store storer.EncodedObjectStorer, sha string) (*object.Tree, error) {
	commit, err := object.GetCommit(store, plumbing.NewHash(sha))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrObject, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrObject, err)
	}
	return tree, nil
}

func fileFromChange(ch *object.Change, objs map[plumbing.Hash]pack.Object) (File, error) {
	if _, err := ch.Action(); err != nil {
		return File{}, err
	}
	f := File{
		OldPath: ch.From.Name,
		NewPath: ch.To.Name,
		AMode:   modeString(ch.From.TreeEntry.Mode),
		BMode:   modeString(ch.To.TreeEntry.Mode),
	}
	switch {
	case ch.From.Name == "":
		f.OldPath = ch.To.Name
		f.NewFile, f.Status = true, "A"
	case ch.To.Name == "":
		f.NewPath = ch.From.Name
		f.DeletedFile, f.Status = true, "D"
	case ch.From.Name != ch.To.Name:
		f.RenamedFile, f.Status = true, "R"
	default:
		f.Status = "M"
	}
	f.Submodule = ch.From.TreeEntry.Mode == filemode.Submodule || ch.To.TreeEntry.Mode == filemode.Submodule
	f.Binary = !blobTextOK(objs, ch.From.TreeEntry) || !blobTextOK(objs, ch.To.TreeEntry)
	return f, nil
}

func modeString(m filemode.FileMode) string {
	if m == filemode.Empty {
		return gitModeMissing
	}
	return strconv.FormatUint(uint64(m), 8)
}

func blobBinary(objs map[plumbing.Hash]pack.Object, e object.TreeEntry) bool {
	data := blobBytes(objs, e)
	if len(data) == 0 {
		return false
	}
	sniff := data
	if len(sniff) > binarySniff {
		sniff = sniff[:binarySniff]
	}
	return bytes.IndexByte(sniff, 0) >= 0
}

func blobBytes(objs map[plumbing.Hash]pack.Object, e object.TreeEntry) []byte {
	if e.Hash.IsZero() || !e.Mode.IsFile() {
		return nil
	}
	obj, ok := objs[e.Hash]
	if !ok || obj.Type != "blob" {
		return nil
	}
	return obj.Data
}

// blobTextOK is false for NUL-binary blobs and for text blobs that are not valid UTF-8.
func blobTextOK(objs map[plumbing.Hash]pack.Object, e object.TreeEntry) bool {
	_, ok := blobTextBytes(objs, e)
	return ok
}

func blobTextBytes(objs map[plumbing.Hash]pack.Object, e object.TreeEntry) ([]byte, bool) {
	data := blobBytes(objs, e)
	if len(data) == 0 {
		return data, true
	}
	if blobBinary(objs, e) {
		return data, false
	}
	if !utf8.Valid(data) {
		return data, false
	}
	return data, true
}

// rawRowSize is the size of the equivalent `git diff --raw -z` record, so the
// charge is comparable to what a subprocess would have emitted.
func rawRowSize(f File) int {
	status := rawStatusField(f)
	amode := rawModeField(f, true)
	bmode := rawModeField(f, false)
	// ":" amode " " bmode " " 40 " " 40 " " status NUL path NUL [path NUL]
	n := 1 + len(amode) + 1 + len(bmode) + 1 + 40 + 1 + 40 + 1 + len(status) + 1 + len(f.NewPath) + 1
	if f.RenamedFile {
		n += len(f.OldPath) + 1
	}
	return n
}

func rawModeField(f File, old bool) string {
	if old && f.NewFile {
		return rawModeMissing
	}
	if !old && f.DeletedFile {
		return rawModeMissing
	}
	if old {
		return rawModeString(f.AMode)
	}
	return rawModeString(f.BMode)
}

func rawModeString(mode string) string {
	if mode == gitModeMissing || mode == "" {
		return rawModeMissing
	}
	if len(mode) >= rawModeWidth {
		return mode
	}
	return strings.Repeat("0", rawModeWidth-len(mode)) + mode
}

func rawStatusField(f File) string {
	if f.RenamedFile {
		return fmt.Sprintf("R%03d", renameScore)
	}
	return f.Status
}

// memStore adapts the held objects to go-git's read-only storer.
func memStore(objs map[plumbing.Hash]pack.Object) storer.EncodedObjectStorer {
	return &objectStore{objs: objs}
}

// objectStore serves held objects. While live is set, every object read first
// consults it, which lets the vendored rename detector (it takes a context but
// ignores it while hashing blobs) stop at the request deadline. Compare clears
// live once the diff walk is done, because the changes it returns outlive the
// comparison deadline and Patches applies its own.
type objectStore struct {
	objs map[plumbing.Hash]pack.Object
	live func() error
}

func (s *objectStore) NewEncodedObject() plumbing.EncodedObject { return &plumbing.MemoryObject{} }

func (s *objectStore) SetEncodedObject(plumbing.EncodedObject) (plumbing.Hash, error) {
	return plumbing.ZeroHash, errors.New("gitdiff: object store is read-only")
}

func (s *objectStore) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	if s.live != nil {
		if err := s.live(); err != nil {
			return nil, err
		}
	}
	obj, ok := s.objs[h]
	if !ok || obj.Hash != h || pack.HashObject(obj.Type, obj.Data) != h {
		return nil, plumbing.ErrObjectNotFound
	}
	kind, err := plumbing.ParseObjectType(obj.Type)
	if err != nil {
		return nil, plumbing.ErrObjectNotFound
	}
	if t != plumbing.AnyObject && t != kind {
		return nil, plumbing.ErrObjectNotFound
	}
	mo := &plumbing.MemoryObject{}
	mo.SetType(kind)
	mo.SetSize(int64(len(obj.Data)))
	if _, err := io.Copy(mo, bytes.NewReader(obj.Data)); err != nil {
		return nil, err
	}
	return mo, nil
}

func (s *objectStore) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	var list []plumbing.EncodedObject
	for h := range s.objs {
		o, err := s.EncodedObject(t, h)
		if err == nil {
			list = append(list, o)
		}
	}
	return storer.NewEncodedObjectSliceIter(list), nil
}

func (s *objectStore) HasEncodedObject(h plumbing.Hash) error {
	if _, ok := s.objs[h]; !ok {
		return plumbing.ErrObjectNotFound
	}
	return nil
}

func (s *objectStore) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	obj, ok := s.objs[h]
	if !ok {
		return 0, plumbing.ErrObjectNotFound
	}
	return int64(len(obj.Data)), nil
}

func (s *objectStore) AddAlternate(string) error {
	return errors.New("gitdiff: object store is read-only")
}
