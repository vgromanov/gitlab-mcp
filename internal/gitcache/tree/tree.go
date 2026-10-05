// Package tree reads commit and tree objects and compares two trees by path,
// hash, and mode. A rename is reported as a delete plus an add. It does not
// pair renames and does not read GitLab diff anchors.
//
// Commit parsing accepts only this subset: one tree, up to 128 parent lines,
// one author, one committer, an optional encoding of UTF-8, and an optional
// gpgsig block. The signature is not verified. Any other encoding, mergetag,
// or unknown header is rejected. The message after the blank line is ignored.
//
// Walk caps recursion at 64 trees, each full path at 4096 bytes, and the
// number of visited entries across the whole walk at bounds.MaxObjects. Those
// checks happen before a path string is concatenated and before the result
// map is updated. Names containing '/', NUL, or a space, and the names "."
// and "..", are rejected, as are duplicate names in one tree.
package tree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

const (
	ModeTree   = "40000"
	ModeFile   = "100644"
	ModeExec   = "100755"
	ModeSym    = "120000"
	ModeCommit = "160000"

	// MaxDepth is the number of nested trees Walk will enter, including the root.
	MaxDepth = 64

	// MaxPathBytes is the maximum length of one full path, including separators.
	MaxPathBytes = 4096

	maxParents    = 128
	maxSigLines   = 4096
	maxCommitLine = 65536
)

var (
	ErrMissing   = errors.New("required object is missing; refusing to infer it")
	ErrShallow   = errors.New("parent commit is missing; refusing to infer a shallow snapshot")
	ErrWrongType = errors.New("object type does not match the tree entry")
	ErrParent    = errors.New("pinned base is not a parent of the fetched head")
	ErrTree      = errors.New("malformed tree or commit")
	ErrDepth     = errors.New("tree recursion exceeds 64")
	ErrEntries   = errors.New("tree walk exceeds 20000 entries")
	ErrPath      = errors.New("tree path exceeds 4096 bytes")
	ErrName      = errors.New("tree entry name is rejected")
)

// Getter loads one object. The bool is false when the object is absent.
// A present object with the wrong hash or type is an error, not a miss.
type Getter interface {
	Get(ctx context.Context, id plumbing.Hash) (pack.Object, bool, error)
}

// Map is an in-memory object source.
type Map map[plumbing.Hash]pack.Object

// Get implements Getter.
func (m Map) Get(ctx context.Context, id plumbing.Hash) (pack.Object, bool, error) {
	if err := ctx.Err(); err != nil {
		return pack.Object{}, false, err
	}
	obj, ok := m[id]
	return obj, ok, nil
}

// Entry is one tree path. Gitlink entries do not require a target object.
type Entry struct {
	Mode   string
	Hash   plumbing.Hash
	Binary bool
}

// Summary counts changes. It does not list paths or file contents.
// Added, deleted, modified, and mode-changed may describe the same path more
// than once. Changed counts each differing path once. Binary counts differing
// paths whose blob is binary, including deletions.
type Summary struct {
	BasePaths   int
	HeadPaths   int
	Added       int
	Deleted     int
	Modified    int
	ModeChanged int
	Binary      int
	Symlink     int
	Executable  int
	Gitlink     int
	changed     int
}

// Changed is the number of paths that differ. It is not a rename count and
// it does not add overlapping category counters together.
func (s Summary) Changed() int {
	return s.changed
}

// Commit is a parsed commit object. Signature bytes are not retained.
type Commit struct {
	Tree    plumbing.Hash
	Parents []plumbing.Hash
}

// ParseCommit reads the supported commit subset. It does not fetch parents
// and does not verify gpgsig.
func ParseCommit(data []byte) (Commit, error) {
	var c Commit
	sawTree := false
	sawAuthor := false
	sawCommitter := false
	for {
		if len(data) == 0 || data[0] == '\n' {
			break
		}
		line, rest, err := oneLine(data)
		if err != nil {
			return Commit{}, err
		}
		data = rest
		key, val, ok := bytes.Cut(line, []byte(" "))
		if !ok {
			return Commit{}, ErrTree
		}
		switch string(key) {
		case "tree":
			if sawTree {
				return Commit{}, ErrTree
			}
			h, err := parseHash(val)
			if err != nil {
				return Commit{}, err
			}
			c.Tree = h
			sawTree = true
		case "parent":
			if len(c.Parents) >= maxParents {
				return Commit{}, ErrTree
			}
			h, err := parseHash(val)
			if err != nil {
				return Commit{}, err
			}
			c.Parents = append(c.Parents, h)
		case "author":
			if sawAuthor || len(val) == 0 {
				return Commit{}, ErrTree
			}
			sawAuthor = true
		case "committer":
			if sawCommitter || len(val) == 0 {
				return Commit{}, ErrTree
			}
			sawCommitter = true
		case "encoding":
			if string(val) != "UTF-8" {
				return Commit{}, ErrTree
			}
		case "gpgsig":
			data, err = skipSignature(data)
			if err != nil {
				return Commit{}, err
			}
		default:
			return Commit{}, ErrTree
		}
	}
	if !sawTree || !sawAuthor || !sawCommitter {
		return Commit{}, ErrTree
	}
	return c, nil
}

func oneLine(data []byte) ([]byte, []byte, error) {
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 || nl > maxCommitLine {
		return nil, nil, ErrTree
	}
	return data[:nl], data[nl+1:], nil
}

func skipSignature(data []byte) ([]byte, error) {
	for n := 0; n < maxSigLines && len(data) > 0 && data[0] == ' '; n++ {
		_, rest, err := oneLine(data)
		if err != nil {
			return nil, err
		}
		data = rest
	}
	if len(data) > 0 && data[0] == ' ' {
		return nil, ErrTree
	}
	return data, nil
}

func parseHash(b []byte) (plumbing.Hash, error) {
	if len(b) != 40 {
		return plumbing.ZeroHash, ErrTree
	}
	var h plumbing.Hash
	for i := 0; i < 20; i++ {
		n, err := strconv.ParseUint(string(b[i*2:i*2+2]), 16, 8)
		if err != nil {
			return plumbing.ZeroHash, ErrTree
		}
		h[i] = byte(n)
	}
	return h, nil
}

// TreeEntry is one line of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	Hash plumbing.Hash
}

// EncodeTree sorts entries with git's directory rule and returns the object body.
func EncodeTree(entries []TreeEntry) ([]byte, error) {
	sorted := append([]TreeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return treeKey(sorted[i]) < treeKey(sorted[j])
	})
	seen := make(map[string]struct{}, len(sorted))
	var buf bytes.Buffer
	for _, e := range sorted {
		mode := normalizeMode(e.Mode)
		if mode == "" || !validName(e.Name) {
			return nil, ErrName
		}
		if _, ok := seen[e.Name]; ok {
			return nil, ErrName
		}
		seen[e.Name] = struct{}{}
		fmt.Fprintf(&buf, "%s %s\x00", mode, e.Name)
		_, _ = buf.Write(e.Hash[:])
	}
	return buf.Bytes(), nil
}

func treeKey(e TreeEntry) string {
	if normalizeMode(e.Mode) == ModeTree {
		return e.Name + "/"
	}
	return e.Name
}

func normalizeMode(mode string) string {
	switch mode {
	case "40000", "040000":
		return ModeTree
	case "100644", "100755", "120000", "160000":
		return mode
	default:
		return ""
	}
}

func validName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return strings.IndexByte(name, 0) < 0 && strings.IndexByte(name, '/') < 0 && strings.IndexByte(name, ' ') < 0
}

// ParseTree decodes a tree body.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	seen := make(map[string]struct{})
	for len(data) > 0 {
		if len(out) >= bounds.MaxObjects {
			return nil, ErrEntries
		}
		sp := bytes.IndexByte(data, ' ')
		nul := bytes.IndexByte(data, 0)
		if sp <= 0 || nul <= sp+1 || nul+21 > len(data) {
			return nil, ErrTree
		}
		mode := normalizeMode(string(data[:sp]))
		name := string(data[sp+1 : nul])
		if mode == "" || !validName(name) {
			return nil, ErrName
		}
		if _, ok := seen[name]; ok {
			return nil, ErrName
		}
		seen[name] = struct{}{}
		var h plumbing.Hash
		copy(h[:], data[nul+1:nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: name, Hash: h})
		data = data[nul+21:]
	}
	return out, nil
}

// Walk lists every path under root. Gitlinks are leaves and are not opened.
// Entry count, depth, and path length are checked before any path concatenation.
func Walk(ctx context.Context, src Getter, root plumbing.Hash) (map[string]Entry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if src == nil {
		return nil, ErrMissing
	}
	out := make(map[string]Entry)
	visited := 0
	if err := walk(ctx, src, root, "", out, map[plumbing.Hash]bool{}, 0, &visited); err != nil {
		return nil, err
	}
	return out, nil
}

func walk(ctx context.Context, src Getter, id plumbing.Hash, prefix string, out map[string]Entry, stack map[plumbing.Hash]bool, depth int, visited *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth >= MaxDepth {
		return ErrDepth
	}
	if stack[id] {
		return ErrTree
	}
	obj, ok, err := src.Get(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrMissing
	}
	if obj.Type != "tree" || obj.Hash != id {
		return ErrWrongType
	}
	stack[id] = true
	defer delete(stack, id)
	entries, err := ParseTree(obj.Data)
	if err != nil {
		return err
	}
	names := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validName(e.Name) {
			return ErrName
		}
		if _, dup := names[e.Name]; dup {
			return ErrName
		}
		names[e.Name] = struct{}{}
		if *visited >= bounds.MaxObjects {
			return ErrEntries
		}
		*visited++
		if e.Mode == ModeTree && depth+1 >= MaxDepth {
			return ErrDepth
		}
		n := len(e.Name)
		if prefix != "" {
			n += len(prefix) + 1
		}
		if n > MaxPathBytes {
			return ErrPath
		}
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		switch e.Mode {
		case ModeCommit:
			out[path] = Entry{Mode: e.Mode, Hash: e.Hash}
		case ModeTree:
			if err := walk(ctx, src, e.Hash, path, out, stack, depth+1, visited); err != nil {
				return err
			}
		case ModeFile, ModeExec, ModeSym:
			blob, found, err := src.Get(ctx, e.Hash)
			if err != nil {
				return err
			}
			if !found {
				return ErrMissing
			}
			if blob.Type != "blob" || blob.Hash != e.Hash {
				return ErrWrongType
			}
			out[path] = Entry{Mode: e.Mode, Hash: e.Hash, Binary: isBinary(blob.Data)}
		default:
			return ErrTree
		}
	}
	return nil
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

// Compare reports path, hash, and mode differences. It never pairs renames.
func Compare(base, head map[string]Entry) Summary {
	var s Summary
	s.BasePaths = len(base)
	s.HeadPaths = len(head)
	for path, b := range base {
		h, ok := head[path]
		if !ok {
			s.Deleted++
			s.changed++
			if b.Binary {
				s.Binary++
			}
			note(&s, b)
			continue
		}
		mode := b.Mode != h.Mode
		content := b.Hash != h.Hash
		if mode {
			s.ModeChanged++
		}
		if content {
			s.Modified++
		}
		if mode || content {
			s.changed++
			if b.Binary || h.Binary {
				s.Binary++
			}
			note(&s, h)
		}
	}
	for path, h := range head {
		if _, ok := base[path]; ok {
			continue
		}
		s.Added++
		s.changed++
		if h.Binary {
			s.Binary++
		}
		note(&s, h)
	}
	return s
}

func note(s *Summary, e Entry) {
	switch e.Mode {
	case ModeSym:
		s.Symlink++
	case ModeExec:
		s.Executable++
	case ModeCommit:
		s.Gitlink++
	}
}

// ProveHead checks the pinned head commit and its full tree. Depth 2 also
// requires the independent authorized base commit object and its full tree.
// Base need not be a direct parent of head (merge-base / multi-commit MRs).
// A missing base object is a shallow snapshot. History beyond the materialized
// roots is not inferred or fetched.
func ProveHead(ctx context.Context, src Getter, head, base plumbing.Hash, depth int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := proveCommitTree(ctx, src, head); err != nil {
		return err
	}
	if depth == 1 {
		return nil
	}
	if depth != 2 {
		return ErrTree
	}
	return proveCommitTree(ctx, src, base)
}

// ProveCommitRoot validates one authorized root: exact hash/type, parseable
// commit body, and a full bounded tree walk.
func ProveCommitRoot(ctx context.Context, src Getter, root plumbing.Hash) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return proveCommitTree(ctx, src, root)
}

func proveCommitTree(ctx context.Context, src Getter, id plumbing.Hash) error {
	obj, ok, err := src.Get(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrShallow
	}
	if obj.Type != "commit" || obj.Hash != id {
		return ErrWrongType
	}
	c, err := ParseCommit(obj.Data)
	if err != nil {
		return err
	}
	if _, err := Walk(ctx, src, c.Tree); err != nil {
		return err
	}
	return nil
}
