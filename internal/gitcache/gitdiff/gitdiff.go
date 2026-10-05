// Package gitdiff runs a no-checkout, no-external-helper git comparison
// against cached objects. It does not acquire remotes and does not write a
// work tree.
package gitdiff

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

const (
	SemanticsFullMR      = "full_mr_base_head"
	SemanticsStraight    = "incremental_straight"
	defaultTimeout       = 15 * time.Second
	defaultMaxBytes      = 8 << 20
	gitModeMissing       = "0"
	gitModeGitlink       = "160000"
	rawCommandTemplate   = "git --git-dir=<bare> -c core.hooksPath=<empty> -c diff.external= --no-pager diff --no-ext-diff --no-textconv --full-index --abbrev=40 --raw -z -M --no-color <from> <to>"
	patchCommandTemplate = "git --git-dir=<bare> -c core.hooksPath=<empty> -c diff.external= --no-pager diff --no-ext-diff --no-textconv --ignore-submodules=all --full-index -p --binary --no-color <from> <to> -- <paths>"
)

var (
	ErrGitMissing = errors.New("gitdiff: git executable is not available")
	ErrObject     = errors.New("gitdiff: required object is missing or mismatched")
	ErrParse      = errors.New("gitdiff: raw diff output is unusable")
	ErrPartial    = errors.New("gitdiff: comparison output or time was bounded")
)

// Limits bound one subprocess. Manifest and patch callers pass independent caps.
type Limits struct {
	MaxBytes int
	Timeout  time.Duration
}

// File is one git raw comparison row with GitLab-shaped flags.
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

// Result is a parsed comparison. Partial means the process was capped.
type Result struct {
	Files     []File
	Partial   bool
	Reason    string
	Command   string
	From      string
	To        string
	Semantics string
}

func (l Limits) apply() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = defaultMaxBytes
	}
	if l.Timeout <= 0 {
		l.Timeout = defaultTimeout
	}
	return l
}

// WriteBare materializes loose objects into a bare repo with no worktree, hooks,
// attributes, or textconv config. ctx cancellation stops further writes.
func WriteBare(ctx context.Context, dir string, objs map[plumbing.Hash]pack.Object) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if dir == "" || len(objs) == 0 {
		return ErrObject
	}
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o700); err != nil {
		return err
	}
	cfg := "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n\tlogallrefupdates = false\n\thooksPath = hooks\n"
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(cfg), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		return err
	}
	for h, obj := range objs {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if obj.Hash != h || pack.HashObject(obj.Type, obj.Data) != h {
			return ErrObject
		}
		if err := writeLoose(dir, obj); err != nil {
			return err
		}
	}
	return nil
}

func writeLoose(gitDir string, obj pack.Object) error {
	var raw bytes.Buffer
	zw := zlib.NewWriter(&raw)
	if _, err := fmt.Fprintf(zw, "%s %d\x00", obj.Type, len(obj.Data)); err != nil {
		_ = zw.Close()
		return err
	}
	if _, err := zw.Write(obj.Data); err != nil {
		_ = zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	hex := obj.Hash.String()
	odir := filepath.Join(gitDir, "objects", hex[:2])
	if err := os.MkdirAll(odir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(odir, hex[2:]), raw.Bytes(), 0o400)
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

// Raw runs a straight no-checkout git diff --raw -z between from and to.
func Raw(ctx context.Context, gitDir, from, to, semantics string, objs map[plumbing.Hash]pack.Object, lim Limits) (Result, error) {
	out := Result{From: from, To: to, Semantics: semantics, Command: rawCommandTemplate}
	if err := RequireCommits(objs, from, to); err != nil {
		return out, err
	}
	data, partial, reason, err := runGit(ctx, gitDir, lim, []string{
		"--no-pager", "diff",
		"--no-ext-diff", "--no-textconv",
		"--full-index", "--abbrev=40", "--raw", "-z", "-M", "--no-color",
		from, to,
	})
	if err != nil && !errors.Is(err, ErrPartial) {
		return out, err
	}
	files, parseErr := parseRaw(data, objs)
	capped := partial || errors.Is(err, ErrPartial)
	if parseErr != nil && !capped {
		return out, parseErr
	}
	out.Files = files
	out.Partial = capped || parseErr != nil
	out.Reason = reason
	if out.Partial {
		if out.Reason == "" {
			out.Reason = "output limit"
		}
		return out, ErrPartial
	}
	return out, nil
}

// Patch runs a bounded straight git diff -p for the selected paths.
func Patch(ctx context.Context, gitDir, from, to string, paths []string, lim Limits) (text string, partial bool, command string, err error) {
	command = patchCommandTemplate
	args := []string{
		"--no-pager", "diff",
		"--no-ext-diff", "--no-textconv", "--ignore-submodules=all",
		"--full-index", "-p", "--binary", "--no-color",
		from, to, "--",
	}
	args = append(args, paths...)
	data, hit, reason, runErr := runGit(ctx, gitDir, lim, args)
	if runErr != nil && !errors.Is(runErr, ErrPartial) {
		return "", false, command, runErr
	}
	if hit || errors.Is(runErr, ErrPartial) {
		return string(data), true, command, fmt.Errorf("%w: %s", ErrPartial, reason)
	}
	return string(data), false, command, nil
}

func runGit(ctx context.Context, gitDir string, lim Limits, args []string) ([]byte, bool, string, error) {
	lim = lim.apply()
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, false, "", ErrGitMissing
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()
	hooks := filepath.Join(gitDir, "hooks")
	cmdArgs := append([]string{
		"--git-dir=" + gitDir,
		"-c", "core.hooksPath=" + hooks,
		"-c", "diff.external=",
		"-c", "diff.renames=true",
		"-c", "core.quotepath=false",
		"-c", "core.bare=true",
	}, args...)
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	cmd.Dir = gitDir
	tmp := filepath.Join(gitDir, "tmp")
	_ = os.MkdirAll(tmp, 0o700)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + gitDir,
		"TMPDIR=" + tmp,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_DIR=" + gitDir,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LITERAL_PATHSPECS=1",
		"LC_ALL=C",
	}
	var stdout capBuffer
	stdout.max = lim.MaxBytes
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if stdout.hit {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return stdout.bytes(), true, "output limit", ErrPartial
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stdout.bytes(), true, "time limit", ErrPartial
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return stdout.bytes(), true, "canceled", ctx.Err()
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok && ee.ExitCode() == 1 && stderr.Len() == 0 {
			// git diff exits 1 when differences exist.
			return stdout.bytes(), false, "", nil
		}
		if stderr.Len() > 0 {
			return stdout.bytes(), false, "", fmt.Errorf("gitdiff: git: %w", runErr)
		}
		return stdout.bytes(), false, "", runErr
	}
	return stdout.bytes(), false, "", nil
}

// capBuffer is a hard-capped writer. It must not embed bytes.Buffer: io.Copy
// would then use the promoted ReadFrom and skip Write.
type capBuffer struct {
	buf bytes.Buffer
	max int
	hit bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.hit {
		return 0, errCapped
	}
	remain := c.max - c.buf.Len()
	if remain <= 0 {
		c.hit = true
		return 0, errCapped
	}
	if len(p) > remain {
		_, _ = c.buf.Write(p[:remain])
		c.hit = true
		return remain, errCapped
	}
	return c.buf.Write(p)
}

func (c *capBuffer) bytes() []byte { return c.buf.Bytes() }

var errCapped = errors.New("gitdiff: output capped")

func parseRaw(data []byte, objs map[plumbing.Hash]pack.Object) ([]File, error) {
	if len(data) == 0 {
		return []File{}, nil
	}
	var files []File
	rest := data
	for len(rest) > 0 {
		if rest[0] == 0 {
			rest = rest[1:]
			continue
		}
		if rest[0] != ':' {
			return files, ErrParse
		}
		rest = rest[1:]
		hdr, next, ok := bytes.Cut(rest, []byte{0})
		if !ok {
			return files, ErrParse
		}
		fields := strings.Fields(string(hdr))
		if len(fields) != 5 {
			return files, ErrParse
		}
		aMode, bMode, aSHA, bSHA, status := fields[0], fields[1], fields[2], fields[3], fields[4]
		rest = next
		var oldPath, newPath string
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			oldPath, rest, ok = cutCString(rest)
			if !ok {
				return files, ErrParse
			}
			newPath, rest, ok = cutCString(rest)
			if !ok {
				return files, ErrParse
			}
		} else {
			p, nrest, ok := cutCString(rest)
			if !ok {
				return files, ErrParse
			}
			oldPath, newPath, rest = p, p, nrest
		}
		f := File{
			OldPath:     oldPath,
			NewPath:     newPath,
			AMode:       normalizeMode(aMode),
			BMode:       normalizeMode(bMode),
			Status:      status,
			NewFile:     strings.HasPrefix(status, "A"),
			DeletedFile: strings.HasPrefix(status, "D"),
			RenamedFile: strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C"),
			Submodule:   aMode == gitModeGitlink || bMode == gitModeGitlink,
			Binary:      blobBinary(objs, aSHA) || blobBinary(objs, bSHA),
		}
		files = append(files, f)
	}
	return files, nil
}

func cutCString(b []byte) (string, []byte, bool) {
	s, rest, ok := bytes.Cut(b, []byte{0})
	if !ok {
		return "", b, false
	}
	return string(s), rest, true
}

func normalizeMode(m string) string {
	if m == "" {
		return gitModeMissing
	}
	n, err := strconv.ParseInt(m, 8, 32)
	if err != nil {
		return m
	}
	if n == 0 {
		return gitModeMissing
	}
	return fmt.Sprintf("%o", n)
}

func blobBinary(objs map[plumbing.Hash]pack.Object, sha string) bool {
	if len(sha) != 40 {
		return false
	}
	obj, ok := objs[plumbing.NewHash(sha)]
	if !ok || obj.Type != "blob" {
		return false
	}
	return bytes.IndexByte(obj.Data, 0) >= 0
}

// SplitPatches splits a multi-file `git diff -p` into path-keyed fragments.
func SplitPatches(text string) map[string]string {
	out := map[string]string{}
	if text == "" {
		return out
	}
	parts := splitDiffGit(text)
	for _, part := range parts {
		oldP, newP := patchPaths(part)
		if oldP != "" {
			out[oldP] = part
		}
		if newP != "" && newP != oldP {
			out[newP] = part
		}
	}
	return out
}

func splitDiffGit(text string) []string {
	const mark = "diff --git "
	if !strings.Contains(text, mark) {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []string{text}
	}
	var parts []string
	start := strings.Index(text, mark)
	if start < 0 {
		return nil
	}
	rest := text[start:]
	for {
		next := strings.Index(rest[len(mark):], "\n"+mark)
		if next < 0 {
			parts = append(parts, rest)
			break
		}
		cut := next + len(mark) + 1
		parts = append(parts, rest[:cut])
		rest = rest[cut:]
	}
	return parts
}

func patchPaths(part string) (oldPath, newPath string) {
	for _, line := range strings.Split(part, "\n") {
		if strings.HasPrefix(line, "--- ") {
			oldPath = headerPath(line[4:])
		}
		if strings.HasPrefix(line, "+++ ") {
			newPath = headerPath(line[4:])
		}
		if strings.HasPrefix(line, "diff --git ") {
			a, b, ok := parseDiffGitPaths(line[len("diff --git "):])
			if ok {
				if oldPath == "" {
					oldPath = a
				}
				if newPath == "" {
					newPath = b
				}
			}
		}
	}
	return oldPath, newPath
}

func headerPath(rest string) string {
	rest = strings.TrimSuffix(rest, "\t")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return ""
	}
	if rest[0] == '"' {
		if u, err := strconv.Unquote(rest); err == nil {
			rest = u
		}
	}
	rest = strings.TrimPrefix(rest, "a/")
	rest = strings.TrimPrefix(rest, "b/")
	if rest == "/dev/null" {
		return ""
	}
	return rest
}

func parseDiffGitPaths(rest string) (oldPath, newPath string, ok bool) {
	oldPath, rest, ok = splitGitPathToken(rest)
	if !ok {
		return "", "", false
	}
	newPath, _, ok = splitGitPathToken(strings.TrimLeft(rest, " "))
	if !ok {
		return "", "", false
	}
	return strings.TrimPrefix(oldPath, "a/"), strings.TrimPrefix(newPath, "b/"), true
}

func splitGitPathToken(s string) (token, rest string, ok bool) {
	s = strings.TrimLeft(s, " ")
	if s == "" {
		return "", s, false
	}
	if s[0] == '"' {
		i := 1
		for i < len(s) {
			if s[i] == '\\' && i+1 < len(s) {
				i += 2
				continue
			}
			if s[i] == '"' {
				raw := s[:i+1]
				u, err := strconv.Unquote(raw)
				if err != nil {
					return "", s, false
				}
				return u, s[i+1:], true
			}
			i++
		}
		return "", s, false
	}
	tok, rest, _ := strings.Cut(s, " ")
	return tok, rest, tok != ""
}

// LookPath reports whether a git binary exists. Tests skip when it does not.
func LookPath() error {
	_, err := exec.LookPath("git")
	if err != nil {
		return ErrGitMissing
	}
	return nil
}
