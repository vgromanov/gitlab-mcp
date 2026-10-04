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
