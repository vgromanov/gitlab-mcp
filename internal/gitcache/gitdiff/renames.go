package gitdiff

import (
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
	if !partial {
		changes = mergeExecutableRenames(ctx, changes, objs, int(score), int(limit))
	}
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

func mergeExecutableRenames(ctx context.Context, changes object.Changes, objs map[plumbing.Hash]pack.Object, minScore int, limit int) object.Changes {
	if ctx != nil && ctx.Err() != nil {
		return changes
	}
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
	exeAdds, exeDels := 0, 0
	for _, ch := range adds {
		if ch.To.TreeEntry.Mode == filemode.Executable {
			exeAdds++
		}
	}
	for _, ch := range dels {
		if ch.From.TreeEntry.Mode == filemode.Executable {
			exeDels++
		}
	}
	if limit > 0 && exeAdds > 0 && exeDels > 0 && maxInt(exeAdds, exeDels) > limit {
		return changes
	}
	usedAdd := make(map[int]bool)
	usedDel := make(map[int]bool)
	for di, del := range dels {
		if ctx != nil && ctx.Err() != nil {
			return changes
		}
		if usedDel[di] || del.From.TreeEntry.Mode != filemode.Executable {
			continue
		}
		bestAI, bestScore := -1, minScore-1
		for ai, add := range adds {
			if ctx != nil && ctx.Err() != nil {
				return changes
			}
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
	var totalA, totalB, common int64
	seen := map[string]int{}
	for _, ln := range la {
		totalA += lineRegionBytes(ln)
		seen[ln]++
	}
	for _, ln := range lb {
		totalB += lineRegionBytes(ln)
		if seen[ln] > 0 {
			common += lineRegionBytes(ln)
			seen[ln]--
		}
	}
	maxTotal := totalA
	if totalB > maxTotal {
		maxTotal = totalB
	}
	if maxTotal == 0 {
		return 100
	}
	return int(common * 100 / maxTotal)
}

// lineRegionBytes is the byte weight of one text region, matching Git/JGit line
// hashing (content bytes plus the terminating newline when present).
func lineRegionBytes(ln string) int64 {
	if ln == "" {
		return 1
	}
	return int64(len(ln) + 1)
}

func blobLines(objs map[plumbing.Hash]pack.Object, h plumbing.Hash, mode filemode.FileMode) (lines []string, ok bool) {
	obj, exist := objs[h]
	if !exist || obj.Type != "blob" {
		return nil, false
	}
	if blobBinary(objs, object.TreeEntry{Hash: h, Mode: mode}) {
		return nil, false
	}
	return blobContentLines(obj.Data), true
}

func blobContentLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := string(data)
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" && strings.HasSuffix(s, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
