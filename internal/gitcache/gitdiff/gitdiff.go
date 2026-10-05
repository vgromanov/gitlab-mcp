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

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

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

	store := memStore(objs)
	fromTree, err := commitTree(store, from)
	if err != nil {
		return c, err
	}
	toTree, err := commitTree(store, to)
	if err != nil {
		return c, err
	}
	changes, err := object.DiffTreeWithOptions(ctx, fromTree, toTree, &object.DiffTreeOptions{
		DetectRenames: true,
		RenameScore:   renameScore,
		RenameLimit:   renameLimit,
	})
	if err != nil {
		return c, c.boundOrObject(ctx, err)
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
		text, err := renderPatch(ctx, ch, lim.MaxBytes-out.Bytes)
		if err != nil {
			if errors.Is(err, errCapped) {
				out.Partial, out.Reason = true, "output limit"
				return out, ErrPartial
			}
			if ctx.Err() != nil || errors.Is(err, object.ErrCanceled) {
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

func renderPatch(ctx context.Context, ch *object.Change, room int) (string, error) {
	if room <= 0 {
		return "", errCapped
	}
	p, err := ch.PatchContext(ctx)
	if err != nil {
		return "", err
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
	f.Binary = blobBinary(objs, ch.From.TreeEntry) || blobBinary(objs, ch.To.TreeEntry)
	return f, nil
}

func modeString(m filemode.FileMode) string {
	if m == filemode.Empty {
		return gitModeMissing
	}
	return strconv.FormatUint(uint64(m), 8)
}

func blobBinary(objs map[plumbing.Hash]pack.Object, e object.TreeEntry) bool {
	if e.Hash.IsZero() || !e.Mode.IsFile() {
		return false
	}
	obj, ok := objs[e.Hash]
	if !ok || obj.Type != "blob" {
		return false
	}
	data := obj.Data
	if len(data) > binarySniff {
		data = data[:binarySniff]
	}
	return bytes.IndexByte(data, 0) >= 0
}

// rawRowSize is the size of the equivalent `git diff --raw -z` record, so the
// charge is comparable to what a subprocess would have emitted.
func rawRowSize(f File) int {
	// ":" amode " " bmode " " 40 " " 40 " " status NUL path NUL [path NUL]
	n := 1 + len(f.AMode) + 1 + len(f.BMode) + 1 + 40 + 1 + 40 + 1 + len(f.Status) + 1 + len(f.NewPath) + 1
	if f.RenamedFile {
		n += len(f.OldPath) + 1
	}
	return n
}

// memStore adapts the held objects to go-git's read-only storer.
func memStore(objs map[plumbing.Hash]pack.Object) storer.EncodedObjectStorer {
	return &objectStore{objs: objs}
}

type objectStore struct{ objs map[plumbing.Hash]pack.Object }

func (s *objectStore) NewEncodedObject() plumbing.EncodedObject { return &plumbing.MemoryObject{} }

func (s *objectStore) SetEncodedObject(plumbing.EncodedObject) (plumbing.Hash, error) {
	return plumbing.ZeroHash, errors.New("gitdiff: object store is read-only")
}

func (s *objectStore) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
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
