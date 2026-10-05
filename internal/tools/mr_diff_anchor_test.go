package tools

import "testing"

func TestDiffAnchor_tables(t *testing.T) {
	patch := samplePatchAdditionDeletionContext()
	parsed := parseUnifiedDiff(patch)
	budget := &contentEmitBudget{maxLines: 1000, maxBytes: 262144}
	wins, ok, _, _ := selectDiffWindows(parsed, 1, budget)
	if !ok || len(wins) != 1 {
		t.Fatalf("wins=%v", wins)
	}
	sel := diffContentSelectionOut{
		ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: int64Ptr(9),
		HeadSHA: strPtr(shaN(1)), BaseSHA: strPtr(shaN(2)), StartSHA: strPtr(shaN(3)),
	}
	file := diffContentFile{OldPath: strPtr("old.go"), NewPath: strPtr("new.go"), Status: diffFileStatusText, Windows: wins}
	proof := proofFromContentFile(sel, file, true)

	t.Run("addition", func(t *testing.T) {
		pos, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 2,
		})
		if err != nil || pos.Kind != diffLineKindAddition || pos.NewLine == nil || *pos.NewLine != 2 || pos.OldLine != nil {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
	t.Run("deletion", func(t *testing.T) {
		pos, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "old", Line: 2,
		})
		if err != nil || pos.Kind != diffLineKindDeletion || pos.OldLine == nil || *pos.OldLine != 2 || pos.NewLine != nil {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
	t.Run("context", func(t *testing.T) {
		pos, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "old", Line: 1,
		})
		if err != nil || pos.Kind != diffLineKindContext || pos.Side != "context" || pos.OldLine == nil || pos.NewLine == nil {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
	t.Run("rename paths", func(t *testing.T) {
		_, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "wrong", NewPath: "new.go", Side: "new", Line: 2,
		})
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectPath {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("wrong version", func(t *testing.T) {
		_, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 8,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 2,
		})
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectBinding {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("wrong side for addition", func(t *testing.T) {
		// Addition at new line 2 must reject old-side request for that addition coordinate.
		_, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "old", Line: 2,
		})
		// Line 2 old is the deletion; requesting old side at addition's new-line is off-window or side.
		// Use addition-only patch for true side rejection:
		addOnly := "@@ -0,0 +1 @@\n+only\n"
		parsed := parseUnifiedDiff(addOnly)
		budget := &contentEmitBudget{maxLines: 100, maxBytes: 10000}
		wins, ok, _, _ := selectDiffWindows(parsed, 0, budget)
		if !ok || len(wins) != 1 {
			t.Fatalf("setup wins=%v", wins)
		}
		f := diffContentFile{OldPath: strPtr("f"), NewPath: strPtr("f"), Status: diffFileStatusText, Windows: wins}
		pr := proofFromContentFile(sel, f, true)
		pr.HeadSHA, pr.BaseSHA, pr.StartSHA = shaN(1), shaN(2), shaN(3)
		_, err = validateDiffAnchor(pr, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "f", NewPath: "f", Side: "old", Line: 1,
		})
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectSide && err.(diffAnchorRejection).Code != diffAnchorRejectLine {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("zero line", func(t *testing.T) {
		_, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 0,
		})
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectLine {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		bad := proof
		bad.Available = false
		_, err := validateDiffAnchor(bad, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 2,
		})
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectUnavailable {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("marker not anchor", func(t *testing.T) {
		p := "@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file\n"
		parsed := parseUnifiedDiff(p)
		budget := &contentEmitBudget{maxLines: 100, maxBytes: 10000}
		wins, ok, _, _ := selectDiffWindows(parsed, 0, budget)
		if !ok {
			t.Fatal("parse")
		}
		f := diffContentFile{OldPath: strPtr("f"), NewPath: strPtr("f"), Status: diffFileStatusText, Windows: wins}
		pr := proofFromContentFile(sel, f, true)
		pr.HeadSHA, pr.BaseSHA, pr.StartSHA = shaN(1), shaN(2), shaN(3)
		_, err := validateDiffAnchor(pr, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "f", NewPath: "f", Side: "new", Line: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("delete file old path", func(t *testing.T) {
		delPatch := "@@ -1 +0,0 @@\n-gone\n"
		parsed := parseUnifiedDiff(delPatch)
		budget := &contentEmitBudget{maxLines: 100, maxBytes: 10000}
		wins, ok, _, _ := selectDiffWindows(parsed, 0, budget)
		if !ok || len(wins) != 1 {
			t.Fatalf("wins=%v", wins)
		}
		f := diffContentFile{OldPath: strPtr("gone.go"), NewPath: strPtr("gone.go"), Status: diffFileStatusText, Windows: wins, DeletedFile: boolPtr(true)}
		pr := proofFromContentFile(sel, f, true)
		pr.OldPath, pr.NewPath = "gone.go", "gone.go"
		pr.HeadSHA, pr.BaseSHA, pr.StartSHA = shaN(1), shaN(2), shaN(3)
		pos, err := validateDiffAnchor(pr, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "gone.go", NewPath: "gone.go", Side: "old", Line: 1,
		})
		if err != nil || pos.Kind != diffLineKindDeletion {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
}

func TestDiffAnchorRepair_F4_malformedProofAndTables(t *testing.T) {
	patch := samplePatchAdditionDeletionContext()
	parsed := parseUnifiedDiff(patch)
	budget := &contentEmitBudget{maxLines: 1000, maxBytes: 262144}
	wins, ok, _, _ := selectDiffWindows(parsed, 1, budget)
	if !ok || len(wins) != 1 {
		t.Fatalf("setup wins=%v ok=%v", wins, ok)
	}
	baseProof := diffContentProof{
		ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
		HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
		OldPath: "old.go", NewPath: "new.go", Windows: wins, Consistent: true, Available: true,
	}
	good := diffAnchor{
		ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
		HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
		OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 2,
	}
	t.Run("positive_version", func(t *testing.T) {
		pos, err := validateDiffAnchor(baseProof, good)
		if err != nil || pos.Kind != diffLineKindAddition {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
	t.Run("missing_proof_refs", func(t *testing.T) {
		p := baseProof
		p.HeadSHA, p.BaseSHA, p.StartSHA = "", "", ""
		_, err := validateDiffAnchor(p, good)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectMalformed {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("unknown_line_kind", func(t *testing.T) {
		p := baseProof
		w := p.Windows[0]
		lines := append([]diffContentLine(nil), w.Lines...)
		n := 2
		lines[2] = diffContentLine{Kind: "unsupported", Text: "+added", NewLine: &n}
		w.Lines = lines
		p.Windows = []diffContentWindow{w}
		_, err := validateDiffAnchor(p, good)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectMalformed {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("addition_with_old_coord", func(t *testing.T) {
		p := baseProof
		w := p.Windows[0]
		lines := append([]diffContentLine(nil), w.Lines...)
		o, n := 9, 2
		lines[2] = diffContentLine{Kind: diffLineKindAddition, Text: "+added", OldLine: &o, NewLine: &n}
		w.Lines = lines
		p.Windows = []diffContentWindow{w}
		_, err := validateDiffAnchor(p, good)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectMalformed {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("available_true_insufficient", func(t *testing.T) {
		p := baseProof
		p.ProjectID = ""
		p.Available = true
		_, err := validateDiffAnchor(p, good)
		if err == nil {
			t.Fatal("trusted Available alone")
		}
	})
	t.Run("tuple_positive", func(t *testing.T) {
		p := baseProof
		p.Kind = diffModeTuple
		p.VersionID = 0
		a := good
		a.Kind = diffModeTuple
		a.VersionID = 0
		pos, err := validateDiffAnchor(p, a)
		if err != nil || pos.Kind != diffLineKindAddition {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
	t.Run("straight_positive", func(t *testing.T) {
		p := baseProof
		p.Kind = diffModeIncremental
		p.FromSHA, p.ToSHA, p.Straight = shaN(4), shaN(5), true
		p.HeadSHA, p.BaseSHA, p.StartSHA, p.VersionID = "", "", "", 0
		a := diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeIncremental,
			FromSHA: shaN(4), ToSHA: shaN(5), Straight: true,
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 2,
		}
		pos, err := validateDiffAnchor(p, a)
		if err != nil || pos.Kind != diffLineKindAddition {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
	t.Run("wrong_project", func(t *testing.T) {
		a := good
		a.ProjectID = "99"
		_, err := validateDiffAnchor(baseProof, a)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectBinding {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("wrong_mr", func(t *testing.T) {
		a := good
		a.MergeRequestIID = 2
		_, err := validateDiffAnchor(baseProof, a)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectBinding {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("wrong_kind", func(t *testing.T) {
		a := good
		a.Kind = diffModeTuple
		_, err := validateDiffAnchor(baseProof, a)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectBinding {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("wrong_ref_head", func(t *testing.T) {
		a := good
		a.HeadSHA = shaN(9)
		_, err := validateDiffAnchor(baseProof, a)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectBinding {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("noncanonical_ref", func(t *testing.T) {
		a := good
		a.HeadSHA = "not-a-sha"
		_, err := validateDiffAnchor(baseProof, a)
		if err == nil {
			t.Fatal("accepted noncanonical ref")
		}
	})
	t.Run("unknown_kind", func(t *testing.T) {
		a := good
		a.Kind = "nope"
		_, err := validateDiffAnchor(baseProof, a)
		if err == nil {
			t.Fatal("accepted unknown kind")
		}
	})
	t.Run("deletion_with_new_coord", func(t *testing.T) {
		p := baseProof
		w := p.Windows[0]
		lines := append([]diffContentLine(nil), w.Lines...)
		o, n := 2, 9
		lines[1] = diffContentLine{Kind: diffLineKindDeletion, Text: "-deleted", OldLine: &o, NewLine: &n}
		w.Lines = lines
		p.Windows = []diffContentWindow{w}
		_, err := validateDiffAnchor(p, good)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectMalformed {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("context_missing_new", func(t *testing.T) {
		p := baseProof
		w := p.Windows[0]
		lines := append([]diffContentLine(nil), w.Lines...)
		o := 1
		lines[0] = diffContentLine{Kind: diffLineKindContext, Text: " context-a", OldLine: &o}
		w.Lines = lines
		p.Windows = []diffContentWindow{w}
		a := good
		a.Side, a.Line = "old", 1
		_, err := validateDiffAnchor(p, a)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectMalformed {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("inconsistent_flag", func(t *testing.T) {
		p := baseProof
		p.Consistent = false
		_, err := validateDiffAnchor(p, good)
		if err == nil {
			t.Fatal("trusted inconsistent proof")
		}
	})
	t.Run("version_id_only_selector_allowed_when_refs_match_proof", func(t *testing.T) {
		// Version-ID-only selector remains allowed; trusted proof still carries exact observed refs.
		a := good
		a.HeadSHA, a.BaseSHA, a.StartSHA = "", "", ""
		pos, err := validateDiffAnchor(baseProof, a)
		if err != nil || pos.Kind != diffLineKindAddition {
			t.Fatalf("version-id-only against proved refs should bind: pos=%#v err=%v", pos, err)
		}
	})
	t.Run("version_id_only_rejected_when_proof_refs_missing", func(t *testing.T) {
		p := baseProof
		p.HeadSHA, p.BaseSHA, p.StartSHA = "", "", ""
		a := good
		a.HeadSHA, a.BaseSHA, a.StartSHA = "", "", ""
		_, err := validateDiffAnchor(p, a)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectMalformed {
			t.Fatalf("proof without refs must not trust version-id-only: err=%v", err)
		}
	})
	t.Run("off_window_line", func(t *testing.T) {
		a := good
		a.Line = 99
		_, err := validateDiffAnchor(baseProof, a)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectLine {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("duplicate_coordinate_in_proof_shape", func(t *testing.T) {
		p := baseProof
		w := p.Windows[0]
		lines := append([]diffContentLine(nil), w.Lines...)
		n := 2
		lines = append(lines, diffContentLine{Kind: diffLineKindAddition, Text: "+dup", NewLine: &n})
		w.Lines = lines
		p.Windows = []diffContentWindow{w}
		_, err := validateDiffAnchor(p, good)
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectMalformed {
			t.Fatalf("duplicate new mapping must fail proof shape: err=%v", err)
		}
	})
}

func TestDiffAnchorRepair_R2_windowBinding(t *testing.T) {
	patch := samplePatchAdditionDeletionContext()
	parsed := parseUnifiedDiff(patch)
	budget := &contentEmitBudget{maxLines: 1000, maxBytes: 262144}
	wins, ok, _, _ := selectDiffWindows(parsed, 1, budget)
	if !ok || len(wins) != 1 {
		t.Fatalf("setup wins=%v", wins)
	}
	baseProof := diffContentProof{
		ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
		HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
		OldPath: "old.go", NewPath: "new.go", Windows: wins, Consistent: true, Available: true,
	}
	good := diffAnchor{
		ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
		HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
		OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 2,
	}
	t.Run("control_positive_still_binds", func(t *testing.T) {
		pos, err := validateDiffAnchor(baseProof, good)
		if err != nil || pos.Kind != diffLineKindAddition || pos.NewLine == nil || *pos.NewLine != 2 {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
	// Old-source RED (immutable receipt): out-of-range NewLine=99 kept text/header/hash.
	t.Run("mutate_newline_out_of_range", func(t *testing.T) {
		p := baseProof
		w := p.Windows[0]
		lines := append([]diffContentLine(nil), w.Lines...)
		n := 99
		lines[2] = diffContentLine{Kind: diffLineKindAddition, Text: "+added", NewLine: &n}
		w.Lines = lines
		p.Windows = []diffContentWindow{w}
		_, err := validateDiffAnchor(p, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 99,
		})
		if err == nil || (err.(diffAnchorRejection).Code != diffAnchorRejectMalformed && err.(diffAnchorRejection).Code != diffAnchorRejectLine) {
			t.Fatalf("out-of-range detached coord accepted: err=%v", err)
		}
	})
	// CONTROL (already green on old source via duplicate-coordinate shortcut — not an old-source red).
	t.Run("control_in_range_collides_duplicate_shortcut", func(t *testing.T) {
		p := baseProof
		w := p.Windows[0]
		lines := append([]diffContentLine(nil), w.Lines...)
		n := 3 // collides with context-b's new line
		lines[2] = diffContentLine{Kind: diffLineKindAddition, Text: "+added", NewLine: &n}
		w.Lines = lines
		p.Windows = []diffContentWindow{w}
		_, err := validateDiffAnchor(p, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 3,
		})
		if err == nil {
			t.Fatal("duplicate in-range collision should still reject")
		}
	})
	// Required UNIQUE in-range alternate on FIRST represented new-side coord:
	// cropped mid-hunk (context=0) begins on deletion/addition; addition is the only
	// new-side line — relative-origin binders would accept mutating it to any unused
	// in-range value. Parser-retained offsets must reject.
	t.Run("unique_in_range_alternate_no_duplicate", func(t *testing.T) {
		multi := "@@ -1,5 +1,5 @@\n" +
			" a\n-b\n+B\n" +
			" c\n-d\n+D\n" +
			" e\n"
		parsed := parseUnifiedDiff(multi)
		if !parsed.ok {
			t.Fatal("setup parse")
		}
		// context=0 → fragment begins mid-hunk on deletion/addition without leading context.
		budget := &contentEmitBudget{maxLines: 3, maxBytes: 262144}
		wins, ok, trunc, _ := selectDiffWindows(parsed, 0, budget)
		if !ok || !trunc || len(wins) != 1 {
			t.Fatalf("setup crop wins=%d trunc=%v", len(wins), trunc)
		}
		w := wins[0]
		if w.NewCount != 5 {
			t.Fatalf("setup: want original new_count=5 got %d", w.NewCount)
		}
		body := w.Text[len(w.Header):]
		if len(body) == 0 || (body[0] != '-' && body[0] != '+') {
			t.Fatalf("setup: want mid-hunk crop without leading context, text=%q", w.Text)
		}
		var addIdx = -1
		usedNew := map[int]bool{}
		newSideLines := 0
		for i, ln := range w.Lines {
			if ln.NewLine != nil {
				usedNew[*ln.NewLine] = true
				newSideLines++
			}
			if ln.Kind == diffLineKindAddition && ln.NewLine != nil {
				addIdx = i
			}
		}
		if addIdx < 0 || newSideLines != 1 {
			t.Fatalf("setup: need single new-side addition as first-represented new coord; lines=%#v", w.Lines)
		}
		alt := 0
		for cand := w.NewStart; cand < w.NewStart+w.NewCount; cand++ {
			if !usedNew[cand] {
				alt = cand
				break
			}
		}
		if alt == 0 {
			t.Fatalf("setup: no unique unused in-range new coord; used=%v", usedNew)
		}
		base := baseProof
		base.Windows = []diffContentWindow{w}
		trueLine := *w.Lines[addIdx].NewLine
		if _, err := validateDiffAnchor(base, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: trueLine,
		}); err != nil {
			t.Fatalf("setup baseline cropped mid-hunk anchor failed: %v", err)
		}
		lines := append([]diffContentLine(nil), w.Lines...)
		n := alt
		lines[addIdx] = diffContentLine{Kind: diffLineKindAddition, Text: lines[addIdx].Text, NewLine: &n}
		w.Lines = lines
		p := baseProof
		p.Windows = []diffContentWindow{w}
		_, err := validateDiffAnchor(p, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: alt,
		})
		if err == nil {
			t.Fatalf("unique in-range first new-side coord %d (true=%d) accepted without matching parser bind", alt, trueLine)
		}
		if err.(diffAnchorRejection).Code != diffAnchorRejectMalformed && err.(diffAnchorRejection).Code != diffAnchorRejectLine {
			t.Fatalf("want malformed/off-window, got %v", err)
		}
	})
	// Independently mutate FIRST represented old-side coord on a shifted single-side fragment.
	t.Run("unique_in_range_first_old_coord_single_side", func(t *testing.T) {
		multi := "@@ -1,5 +1,5 @@\n" +
			" a\n-b\n+B\n" +
			" c\n-d\n+D\n" +
			" e\n"
		parsed := parseUnifiedDiff(multi)
		budget := &contentEmitBudget{maxLines: 3, maxBytes: 262144}
		wins, ok, trunc, _ := selectDiffWindows(parsed, 0, budget)
		if !ok || !trunc || len(wins) != 1 {
			t.Fatalf("setup crop wins=%d trunc=%v", len(wins), trunc)
		}
		w := wins[0]
		var delIdx = -1
		usedOld := map[int]bool{}
		oldSideLines := 0
		for i, ln := range w.Lines {
			if ln.OldLine != nil {
				usedOld[*ln.OldLine] = true
				oldSideLines++
			}
			if ln.Kind == diffLineKindDeletion && ln.OldLine != nil && delIdx < 0 {
				delIdx = i
			}
		}
		if delIdx != 0 || oldSideLines != 1 {
			t.Fatalf("setup: want first line deletion as sole old-side coord; lines=%#v", w.Lines)
		}
		alt := 0
		for cand := w.OldStart; cand < w.OldStart+w.OldCount; cand++ {
			if !usedOld[cand] {
				alt = cand
				break
			}
		}
		if alt == 0 {
			t.Fatalf("setup: no unique unused in-range old coord; used=%v", usedOld)
		}
		trueLine := *w.Lines[delIdx].OldLine
		lines := append([]diffContentLine(nil), w.Lines...)
		n := alt
		lines[delIdx] = diffContentLine{Kind: diffLineKindDeletion, Text: lines[delIdx].Text, OldLine: &n}
		w.Lines = lines
		p := baseProof
		p.Windows = []diffContentWindow{w}
		_, err := validateDiffAnchor(p, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "old", Line: alt,
		})
		if err == nil {
			t.Fatalf("unique in-range first old-side coord %d (true=%d) accepted", alt, trueLine)
		}
	})
	t.Run("mutate_text_without_coord_change", func(t *testing.T) {
		p := baseProof
		w := p.Windows[0]
		lines := append([]diffContentLine(nil), w.Lines...)
		n := 2
		lines[2] = diffContentLine{Kind: diffLineKindAddition, Text: "+DIFFERENT", NewLine: &n}
		w.Lines = lines
		p.Windows = []diffContentWindow{w}
		_, err := validateDiffAnchor(p, good)
		if err == nil {
			t.Fatal("text association drift accepted")
		}
	})
	t.Run("mutate_side_association", func(t *testing.T) {
		// Addition at new:2 requested on old side.
		_, err := validateDiffAnchor(baseProof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "old", Line: 2,
		})
		// Old line 2 is deletion — different kind. Use addition-only for pure side reject:
		addOnly := "@@ -0,0 +1 @@\n+only\n"
		parsed := parseUnifiedDiff(addOnly)
		wins, ok, _, _ := selectDiffWindows(parsed, 0, &contentEmitBudget{maxLines: 100, maxBytes: 10000})
		if !ok || len(wins) != 1 {
			t.Fatalf("setup %#v", wins)
		}
		p := baseProof
		p.Windows = wins
		_, err = validateDiffAnchor(p, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "old", Line: 1,
		})
		if err == nil || (err.(diffAnchorRejection).Code != diffAnchorRejectSide && err.(diffAnchorRejection).Code != diffAnchorRejectLine) {
			t.Fatalf("side association err=%v", err)
		}
	})
	// CONTROL (already green on old source — kind reshape fails line shape / binding).
	t.Run("control_mutate_kind_without_matching_prefix", func(t *testing.T) {
		p := baseProof
		w := p.Windows[0]
		lines := append([]diffContentLine(nil), w.Lines...)
		n := 2
		o := 2
		lines[2] = diffContentLine{Kind: diffLineKindContext, Text: "+added", OldLine: &o, NewLine: &n}
		w.Lines = lines
		p.Windows = []diffContentWindow{w}
		_, err := validateDiffAnchor(p, good)
		if err == nil {
			t.Fatal("kind/prefix association drift accepted")
		}
	})
	t.Run("empty_window", func(t *testing.T) {
		p := baseProof
		p.Windows = []diffContentWindow{{}}
		_, err := validateDiffAnchor(p, good)
		if err == nil {
			t.Fatal("empty window accepted")
		}
	})
	// Old-source RED: hash mismatch with otherwise valid lines.
	t.Run("hash_mismatch", func(t *testing.T) {
		p := baseProof
		w := p.Windows[0]
		w.WindowHash.Value = "deadbeef"
		p.Windows = []diffContentWindow{w}
		_, err := validateDiffAnchor(p, good)
		if err == nil {
			t.Fatal("hash mismatch accepted")
		}
	})
	t.Run("cropped_fragment_positive_exact_anchor", func(t *testing.T) {
		multi := "@@ -1,5 +1,5 @@\n" +
			" a\n-b\n+B\n" +
			" c\n-d\n+D\n" +
			" e\n"
		parsed := parseUnifiedDiff(multi)
		// context=0: legitimate mid-hunk crop beginning on deletion/addition without context.
		budget := &contentEmitBudget{maxLines: 3, maxBytes: 262144}
		wins, ok, trunc, _ := selectDiffWindows(parsed, 0, budget)
		if !ok || !trunc || len(wins) != 1 {
			t.Fatalf("setup crop wins=%d trunc=%v ok=%v", len(wins), trunc, ok)
		}
		w := wins[0]
		if len(w.Text) >= len(multi) {
			t.Fatalf("setup: emitted text not a fragment: %q", w.Text)
		}
		body := w.Text[len(w.Header):]
		if len(body) == 0 || (body[0] != '-' && body[0] != '+') {
			t.Fatalf("setup: mid-hunk without context required, text=%q", w.Text)
		}
		var addLine int
		found := false
		for _, ln := range w.Lines {
			if ln.Kind == diffLineKindAddition && ln.NewLine != nil {
				addLine = *ln.NewLine
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("cropped fragment must contain addition for exact anchor; lines=%#v text=%q", w.Lines, w.Text)
		}
		p := baseProof
		p.Windows = wins
		p.Available = true
		pos, err := validateDiffAnchor(p, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: addLine,
		})
		if err != nil {
			t.Fatalf("cropped mid-hunk exact anchor failed: %v", err)
		}
		if pos.Kind != diffLineKindAddition || pos.NewLine == nil || *pos.NewLine != addLine || pos.OldLine != nil {
			t.Fatalf("exact position mismatch pos=%#v want new=%d", pos, addLine)
		}
	})
	t.Run("disjoint_windows_positive", func(t *testing.T) {
		multi := "@@ -1 +1 @@\n-a\n+b\n@@ -5 +5 @@\n-c\n+d\n"
		parsed := parseUnifiedDiff(multi)
		wins, ok, _, _ := selectDiffWindows(parsed, 0, &contentEmitBudget{maxLines: 1000, maxBytes: 262144})
		if !ok || len(wins) != 2 {
			t.Fatalf("setup disjoint wins=%d", len(wins))
		}
		p := baseProof
		p.Windows = wins
		pos, err := validateDiffAnchor(p, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 5,
		})
		if err != nil || pos.Kind != diffLineKindAddition || pos.NewLine == nil || *pos.NewLine != 5 {
			t.Fatalf("disjoint second window pos=%#v err=%v", pos, err)
		}
	})
	t.Run("context_dual_coords_positive", func(t *testing.T) {
		pos, err := validateDiffAnchor(baseProof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "old", Line: 1,
		})
		if err != nil || pos.Kind != diffLineKindContext || pos.OldLine == nil || pos.NewLine == nil {
			t.Fatalf("context dual pos=%#v err=%v", pos, err)
		}
	})
	t.Run("eof_marker_attached_positive", func(t *testing.T) {
		eof := "@@ -0,0 +1 @@\n+new\n\\ No newline at end of file\n"
		parsed := parseUnifiedDiff(eof)
		wins, ok, _, _ := selectDiffWindows(parsed, 0, &contentEmitBudget{maxLines: 100, maxBytes: 10000})
		if !ok || len(wins) != 1 {
			t.Fatalf("setup eof wins=%v", wins)
		}
		if !wins[0].Lines[0].NoNewline {
			t.Fatal("setup: marker not attached")
		}
		p := baseProof
		p.Windows = wins
		pos, err := validateDiffAnchor(p, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			HeadSHA: shaN(1), BaseSHA: shaN(2), StartSHA: shaN(3),
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 1,
		})
		if err != nil || pos.Kind != diffLineKindAddition {
			t.Fatalf("eof attached pos=%#v err=%v", pos, err)
		}
	})
}
