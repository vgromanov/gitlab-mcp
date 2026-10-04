package tools

import "fmt"

const (
	diffAnchorRejectUnavailable  = "unavailable"
	diffAnchorRejectMalformed    = "malformed"
	diffAnchorRejectInconsistent = "inconsistent"
	diffAnchorRejectBinding      = "binding"
	diffAnchorRejectPath         = "path"
	diffAnchorRejectSide         = "side"
	diffAnchorRejectLine         = "line"
	diffAnchorRejectMarker       = "marker"
)

// diffContentProof is a provider-constructed trusted binding for returned
// diff content. It is never built from MCP input.
type diffContentProof struct {
	ProjectID       string
	MergeRequestIID int64
	Kind            string
	VersionID       int64
	BaseSHA         string
	StartSHA        string
	HeadSHA         string
	FromSHA         string
	ToSHA           string
	Straight        bool
	OldPath         string
	NewPath         string
	Windows         []diffContentWindow
	Consistent      bool
	Available       bool
}

// diffAnchor is a caller-requested coordinate to validate against a proof.
type diffAnchor struct {
	ProjectID       string
	MergeRequestIID int64
	Kind            string
	VersionID       int64
	BaseSHA         string
	StartSHA        string
	HeadSHA         string
	FromSHA         string
	ToSHA           string
	Straight        bool
	OldPath         string
	NewPath         string
	Side            string // "old" | "new"
	Line            int
}

// diffTextPosition is a validated text coordinate in returned content.
type diffTextPosition struct {
	OldPath string
	NewPath string
	OldLine *int
	NewLine *int
	Side    string
	Line    int
	Kind    string
}

type diffAnchorRejection struct {
	Code    string
	Message string
}

func (r diffAnchorRejection) Error() string {
	return fmt.Sprintf("%s: %s", r.Code, r.Message)
}

// validateDiffAnchor returns a canonical text position or a typed rejection.
// Markers are never anchors. Context lines return both coordinates.
func validateDiffAnchor(proof diffContentProof, anchor diffAnchor) (diffTextPosition, error) {
	if !proof.Available {
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectUnavailable, Message: "proof unavailable"}
	}
	if !proof.Consistent {
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectInconsistent, Message: "proof inconsistent"}
	}
	if len(proof.Windows) == 0 {
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectUnavailable, Message: "no windows"}
	}
	if anchor.ProjectID != proof.ProjectID || anchor.MergeRequestIID != proof.MergeRequestIID {
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectBinding, Message: "project or mr"}
	}
	if anchor.Kind != proof.Kind {
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectBinding, Message: "selection kind"}
	}
	switch proof.Kind {
	case diffModeVersion:
		if anchor.VersionID != proof.VersionID || proof.VersionID < 1 {
			return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectBinding, Message: "version"}
		}
		if proof.HeadSHA != "" && (anchor.HeadSHA != "" && anchor.HeadSHA != proof.HeadSHA ||
			anchor.BaseSHA != "" && anchor.BaseSHA != proof.BaseSHA ||
			anchor.StartSHA != "" && anchor.StartSHA != proof.StartSHA) {
			return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectBinding, Message: "refs"}
		}
	case diffModeTuple:
		if anchor.BaseSHA != proof.BaseSHA || anchor.StartSHA != proof.StartSHA || anchor.HeadSHA != proof.HeadSHA {
			return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectBinding, Message: "tuple refs"}
		}
		if proof.VersionID > 0 && anchor.VersionID != 0 && anchor.VersionID != proof.VersionID {
			return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectBinding, Message: "version"}
		}
	case diffModeIncremental:
		if anchor.FromSHA != proof.FromSHA || anchor.ToSHA != proof.ToSHA || anchor.Straight != proof.Straight || !proof.Straight {
			return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectBinding, Message: "incremental refs"}
		}
	default:
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "selection kind"}
	}
	if anchor.OldPath != proof.OldPath || anchor.NewPath != proof.NewPath {
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectPath, Message: "path pair"}
	}
	if anchor.Line < 1 {
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectLine, Message: "line must be positive"}
	}
	side := anchor.Side
	switch side {
	case "old", "new":
	default:
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectSide, Message: "side"}
	}
	for _, w := range proof.Windows {
		for _, ln := range w.Lines {
			if ln.Kind == diffLineKindMarker {
				continue
			}
			switch side {
			case "old":
				if ln.OldLine == nil || *ln.OldLine != anchor.Line {
					continue
				}
				if ln.Kind == diffLineKindAddition {
					return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectSide, Message: "addition has no old line"}
				}
				return canonicalPosition(proof, ln, side, anchor.Line), nil
			case "new":
				if ln.NewLine == nil || *ln.NewLine != anchor.Line {
					continue
				}
				if ln.Kind == diffLineKindDeletion {
					return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectSide, Message: "deletion has no new line"}
				}
				return canonicalPosition(proof, ln, side, anchor.Line), nil
			}
		}
	}
	return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectLine, Message: "off-window"}
}

func canonicalPosition(proof diffContentProof, ln diffContentLine, side string, line int) diffTextPosition {
	pos := diffTextPosition{
		OldPath: proof.OldPath,
		NewPath: proof.NewPath,
		Side:    side,
		Line:    line,
		Kind:    ln.Kind,
		OldLine: cloneIntPtr(ln.OldLine),
		NewLine: cloneIntPtr(ln.NewLine),
	}
	if ln.Kind == diffLineKindContext {
		// Canonical context position exposes both coordinates.
		pos.Side = "context"
	}
	return pos
}

// proofFromContentFile builds a trusted proof from provider-owned content output.
func proofFromContentFile(sel diffContentSelectionOut, file diffContentFile, consistent bool) diffContentProof {
	p := diffContentProof{
		ProjectID:       sel.ProjectID,
		MergeRequestIID: sel.MergeRequestIID,
		Kind:            sel.Kind,
		OldPath:         pathStr(file.OldPath),
		NewPath:         pathStr(file.NewPath),
		Windows:         file.Windows,
		Consistent:      consistent,
		Available:       file.Status == diffFileStatusText && len(file.Windows) > 0,
	}
	if sel.VersionID != nil {
		p.VersionID = *sel.VersionID
	}
	if sel.BaseSHA != nil {
		p.BaseSHA = *sel.BaseSHA
	}
	if sel.StartSHA != nil {
		p.StartSHA = *sel.StartSHA
	}
	if sel.HeadSHA != nil {
		p.HeadSHA = *sel.HeadSHA
	}
	if sel.FromSHA != nil {
		p.FromSHA = *sel.FromSHA
	}
	if sel.ToSHA != nil {
		p.ToSHA = *sel.ToSHA
	}
	if sel.Straight != nil {
		p.Straight = *sel.Straight
	}
	return p
}
