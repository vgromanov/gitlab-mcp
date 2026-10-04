package tools

import (
	"fmt"
	"strconv"
	"strings"
)

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

func canonicalProjectID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != s {
		return "", false
	}
	return s, true
}

func proofLineValid(ln diffContentLine) bool {
	switch ln.Kind {
	case diffLineKindAddition:
		return ln.NewLine != nil && *ln.NewLine > 0 && ln.OldLine == nil
	case diffLineKindDeletion:
		return ln.OldLine != nil && *ln.OldLine > 0 && ln.NewLine == nil
	case diffLineKindContext:
		return ln.OldLine != nil && ln.NewLine != nil && *ln.OldLine > 0 && *ln.NewLine > 0
	default:
		return false
	}
}

func validateProofShape(proof diffContentProof) error {
	if _, ok := canonicalProjectID(proof.ProjectID); !ok {
		return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "project"}
	}
	if proof.MergeRequestIID < 1 {
		return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "mr"}
	}
	switch proof.Kind {
	case diffModeVersion:
		if proof.VersionID < 1 {
			return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "version"}
		}
		if !isFortyHex(proof.HeadSHA) || !isFortyHex(proof.BaseSHA) || !isFortyHex(proof.StartSHA) {
			return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "version refs"}
		}
	case diffModeTuple:
		if !isFortyHex(proof.HeadSHA) || !isFortyHex(proof.BaseSHA) || !isFortyHex(proof.StartSHA) {
			return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "tuple refs"}
		}
	case diffModeIncremental:
		if !isFortyHex(proof.FromSHA) || !isFortyHex(proof.ToSHA) || !proof.Straight {
			return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "incremental refs"}
		}
	default:
		return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "selection kind"}
	}
	if proof.OldPath == "" && proof.NewPath == "" {
		return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "path pair"}
	}
	if len(proof.Windows) == 0 {
		return diffAnchorRejection{Code: diffAnchorRejectUnavailable, Message: "no windows"}
	}
	seenOld := map[int]string{}
	seenNew := map[int]string{}
	for _, w := range proof.Windows {
		for _, ln := range w.Lines {
			if ln.Kind == diffLineKindMarker {
				return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "marker line"}
			}
			if !proofLineValid(ln) {
				return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "line shape"}
			}
			if ln.OldLine != nil {
				if prev, ok := seenOld[*ln.OldLine]; ok && prev != ln.Kind {
					return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "duplicate old mapping"}
				}
				seenOld[*ln.OldLine] = ln.Kind
			}
			if ln.NewLine != nil {
				if prev, ok := seenNew[*ln.NewLine]; ok && prev != ln.Kind {
					return diffAnchorRejection{Code: diffAnchorRejectMalformed, Message: "duplicate new mapping"}
				}
				seenNew[*ln.NewLine] = ln.Kind
			}
		}
	}
	return nil
}

// validateDiffAnchor returns a canonical text position or a typed rejection.
// Markers are never anchors. Context lines return both coordinates.
func validateDiffAnchor(proof diffContentProof, anchor diffAnchor) (diffTextPosition, error) {
	if err := validateProofShape(proof); err != nil {
		return diffTextPosition{}, err
	}
	if !proof.Available {
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectUnavailable, Message: "proof unavailable"}
	}
	if !proof.Consistent {
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectInconsistent, Message: "proof inconsistent"}
	}
	if ap, ok := canonicalProjectID(anchor.ProjectID); !ok || ap != proof.ProjectID || anchor.MergeRequestIID != proof.MergeRequestIID {
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectBinding, Message: "project or mr"}
	}
	if anchor.Kind != proof.Kind {
		return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectBinding, Message: "selection kind"}
	}
	switch proof.Kind {
	case diffModeVersion:
		if anchor.VersionID != proof.VersionID {
			return diffTextPosition{}, diffAnchorRejection{Code: diffAnchorRejectBinding, Message: "version"}
		}
		if (anchor.HeadSHA != "" && anchor.HeadSHA != proof.HeadSHA) ||
			(anchor.BaseSHA != "" && anchor.BaseSHA != proof.BaseSHA) ||
			(anchor.StartSHA != "" && anchor.StartSHA != proof.StartSHA) {
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
	p.Available = file.Status == diffFileStatusText && len(file.Windows) > 0 && validateProofShape(p) == nil
	return p
}
