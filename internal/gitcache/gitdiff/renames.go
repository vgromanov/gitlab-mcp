package gitdiff

import (
	"bytes"
	"context"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
)

// diffWithRenames diffs two trees in-process. When the rename candidate count
// exceeds renameLimit, content similarity is skipped and partial is true.
func diffWithRenames(ctx context.Context, from, to *object.Tree, score uint, limit uint, objs map[plumbing.Hash]pack.Object) (object.Changes, bool, error) {
	raw, err := object.DiffTreeWithOptions(ctx, from, to, &object.DiffTreeOptions{DetectRenames: false})
	if err != nil {
		return nil, false, err
	}
	adds, dels := countAddsDeletes(raw)
	partial := limit > 0 && len(adds) > 0 && len(dels) > 0 && maxInt(len(adds), len(dels)) > int(limit)
	opts := &object.DiffTreeOptions{
		DetectRenames:    true,
		RenameScore:      score,
		RenameLimit:      limit,
		OnlyExactRenames: partial,
	}
	changes, err := object.DetectRenames(raw, opts)
	if err != nil {
		return nil, partial, err
	}
	changes = mergeExecutableRenames(changes, objs, int(score))
	return changes, partial, nil
}

func countAddsDeletes(changes object.Changes) (added, deleted []*object.Change) {
	for _, ch := range changes {
		action, err := ch.Action()
		if err != nil {
			continue
		}
		switch action {
		case merkletrie.Insert:
			added = append(added, ch)
		case merkletrie.Delete:
			deleted = append(deleted, ch)
		}
	}
	return added, deleted
}

func mergeExecutableRenames(changes object.Changes, objs map[plumbing.Hash]pack.Object, minScore int) object.Changes {
	var adds, dels, mods []*object.Change
	for _, ch := range changes {
		action, err := ch.Action()
		if err != nil {
			mods = append(mods, ch)
			continue
		}
		switch action {
		case merkletrie.Insert:
			adds = append(adds, ch)
		case merkletrie.Delete:
			dels = append(dels, ch)
		default:
			mods = append(mods, ch)
		}
	}
	usedAdd := make(map[int]bool)
	usedDel := make(map[int]bool)
	for di, del := range dels {
		if usedDel[di] || del.From.TreeEntry.Mode != filemode.Executable {
			continue
		}
		bestAI, bestScore := -1, minScore-1
		for ai, add := range adds {
			if usedAdd[ai] || add.To.TreeEntry.Mode != filemode.Executable {
				continue
			}
			score := blobSimilarityScore(objs, del.From.TreeEntry, add.To.TreeEntry)
			if score > bestScore {
				bestScore, bestAI = score, ai
			}
		}
		if bestAI < 0 {
			continue
		}
		usedAdd[bestAI], usedDel[di] = true, true
		mods = append(mods, &object.Change{From: del.From, To: adds[bestAI].To})
	}
	out := make(object.Changes, 0, len(mods)+len(adds)+len(dels))
	out = append(out, mods...)
	for i, ch := range adds {
		if !usedAdd[i] {
			out = append(out, ch)
		}
	}
	for i, ch := range dels {
		if !usedDel[i] {
			out = append(out, ch)
		}
	}
	return out
}

func renameComparableMode(m filemode.FileMode) bool {
	return m == filemode.Regular || m == filemode.Executable
}

func blobSimilarityScore(objs map[plumbing.Hash]pack.Object, a, b object.TreeEntry) int {
	la, _ := blobLines(objs, a.Hash, a.Mode)
	lb, _ := blobLines(objs, b.Hash, b.Mode)
	if la == nil || lb == nil {
		return 0
	}
	common := 0
	seen := map[string]int{}
	for _, ln := range la {
		seen[ln]++
	}
	for _, ln := range lb {
		if seen[ln] > 0 {
			common++
			seen[ln]--
		}
	}
	max := len(la)
	if len(lb) > max {
		max = len(lb)
	}
	if max == 0 {
		return 100
	}
	return common * 100 / max
}

func blobLines(objs map[plumbing.Hash]pack.Object, h plumbing.Hash, mode filemode.FileMode) (lines []string, ok bool) {
	obj, exist := objs[h]
	if !exist || obj.Type != "blob" {
		return nil, false
	}
	if blobBinary(objs, object.TreeEntry{Hash: h, Mode: mode}) {
		return nil, false
	}
	data := obj.Data
	if len(data) > 0 && !bytes.Contains(data, []byte{0}) {
		// invalid UTF-8 is handled elsewhere; line split is for similarity only.
	}
	return strings.Split(string(data), "\n"), true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
