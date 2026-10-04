package tools

import "testing"

func TestDiffAnchor_tables(t *testing.T) {
	patch := samplePatchAdditionDeletionContext()
	parsed := parseUnifiedDiff(patch)
	budget := &contentEmitBudget{maxLines: 1000, maxBytes: 262144}
	wins, ok, _ := selectDiffWindows(parsed, 1, budget)
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
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 2,
		})
		if err != nil || pos.Kind != diffLineKindAddition || pos.NewLine == nil || *pos.NewLine != 2 || pos.OldLine != nil {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
	t.Run("deletion", func(t *testing.T) {
		pos, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			OldPath: "old.go", NewPath: "new.go", Side: "old", Line: 2,
		})
		if err != nil || pos.Kind != diffLineKindDeletion || pos.OldLine == nil || *pos.OldLine != 2 || pos.NewLine != nil {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
	t.Run("context", func(t *testing.T) {
		pos, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			OldPath: "old.go", NewPath: "new.go", Side: "old", Line: 1,
		})
		if err != nil || pos.Kind != diffLineKindContext || pos.Side != "context" || pos.OldLine == nil || pos.NewLine == nil {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
	t.Run("rename paths", func(t *testing.T) {
		_, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			OldPath: "wrong", NewPath: "new.go", Side: "new", Line: 2,
		})
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectPath {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("wrong version", func(t *testing.T) {
		_, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 8,
			OldPath: "old.go", NewPath: "new.go", Side: "new", Line: 2,
		})
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectBinding {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("wrong side for addition", func(t *testing.T) {
		_, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			OldPath: "old.go", NewPath: "new.go", Side: "old", Line: 99,
		})
		if err == nil || err.(diffAnchorRejection).Code != diffAnchorRejectLine {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("zero line", func(t *testing.T) {
		_, err := validateDiffAnchor(proof, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
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
		wins, ok, _ := selectDiffWindows(parsed, 0, budget)
		if !ok {
			t.Fatal("parse")
		}
		f := diffContentFile{OldPath: strPtr("f"), NewPath: strPtr("f"), Status: diffFileStatusText, Windows: wins}
		pr := proofFromContentFile(diffContentSelectionOut{ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: int64Ptr(1)}, f, true)
		// Valid addition still works; marker itself has no coordinates.
		_, err := validateDiffAnchor(pr, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 1,
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
		wins, ok, _ := selectDiffWindows(parsed, 0, budget)
		if !ok || len(wins) != 1 {
			t.Fatalf("wins=%v", wins)
		}
		f := diffContentFile{OldPath: strPtr("gone.go"), NewPath: strPtr("gone.go"), Status: diffFileStatusText, Windows: wins, DeletedFile: boolPtr(true)}
		pr := proofFromContentFile(sel, f, true)
		pr.OldPath, pr.NewPath = "gone.go", "gone.go"
		pos, err := validateDiffAnchor(pr, diffAnchor{
			ProjectID: "42", MergeRequestIID: 1, Kind: diffModeVersion, VersionID: 9,
			OldPath: "gone.go", NewPath: "gone.go", Side: "old", Line: 1,
		})
		if err != nil || pos.Kind != diffLineKindDeletion {
			t.Fatalf("pos=%#v err=%v", pos, err)
		}
	})
}
