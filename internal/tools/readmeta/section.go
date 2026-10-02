package readmeta

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Fixed source/provider/capability for the MR diffs reference adapter.
const (
	SourceGitLabREST        = "gitlab_rest"
	ProviderGitLab          = "gitlab"
	CapabilityMRDiffsV1     = "readmeta.mr_diffs.v1"
	ContentCompleteTrue     = "true"
	ContentCompleteFalse    = "false"
	ContentCompleteUnknown  = "unknown"
	ConsistencyConsistent   = "consistent"
	ConsistencyInconsistent = "inconsistent"
	ConsistencyUnknown      = "unknown"
	CoverageFull            = "full"
	CoveragePartial         = "partial"
	CoverageUnknown         = "unknown"
)

// ObservedHeadSHA returns a valid 40-char hex git SHA observation, or ("", false).
// Locked contract: section.head_sha is JSON null unless a real 40-hex observation is known.
func ObservedHeadSHA(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if len(s) != 40 {
		return "", false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return "", false
		}
	}
	return strings.ToLower(s), true
}

// Limitation is a safe per-section limitation entry.
type Limitation struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Counts holds nullable counters; JSON null means unknown (never coerce to 0).
type Counts struct {
	Items *int   `json:"items"`
	Bytes *int64 `json:"bytes"`
	Files *int   `json:"files"`
}

// Section is the locked read-section envelope sibling object.
type Section struct {
	RetrievedAt         string       `json:"retrieved_at"`
	Source              string       `json:"source"`
	Provider            string       `json:"provider"`
	CapabilityVersion   string       `json:"capability_version"`
	HeadSHA             *string      `json:"head_sha"`
	PaginationExhausted bool         `json:"pagination_exhausted"`
	ContentComplete     string       `json:"content_complete"`
	Consistency         string       `json:"consistency"`
	Limitations         []Limitation `json:"limitations"`
	NextCursor          *string      `json:"next_cursor"`
	Counts              Counts       `json:"counts"`
	ManifestCoverage    string       `json:"manifest_coverage"`
	PatchCoverage       string       `json:"patch_coverage"`
}

// NewMRDiffsSection returns a section skeleton with locked fixed fields.
func NewMRDiffsSection(now time.Time) Section {
	return Section{
		RetrievedAt:       now.UTC().Format(time.RFC3339),
		Source:            SourceGitLabREST,
		Provider:          ProviderGitLab,
		CapabilityVersion: CapabilityMRDiffsV1,
		HeadSHA:           nil,
		Limitations:       []Limitation{},
		Counts:            Counts{},
		ContentComplete:   ContentCompleteUnknown,
		Consistency:       ConsistencyUnknown,
		ManifestCoverage:  CoverageUnknown,
		PatchCoverage:     CoverageUnknown,
	}
}

// AddLimitation appends a safe limitation (no-op on empty code).
func (s *Section) AddLimitation(code, message string) {
	if code == "" {
		return
	}
	s.Limitations = append(s.Limitations, Limitation{Code: code, Message: message})
}

// PagingObservation captures raw X-Next-Page header presence vs SDK NextPage.
type PagingObservation struct {
	SDKNextPage       int64
	HeaderPresent     bool
	HeaderValue       string
	ExhaustedObserved bool // true only when header present and no further page
	PagingKnown       bool // true when header present
}

// ObservePaging inspects response headers for explicit pagination exhaustion.
// Missing X-Next-Page must NOT be treated as exhausted (SDK NextPage may default to 0).
func ObservePaging(hdr http.Header, sdkNextPage int64) PagingObservation {
	obs := PagingObservation{SDKNextPage: sdkNextPage}
	if hdr == nil {
		return obs
	}
	const key = "X-Next-Page"
	canon := http.CanonicalHeaderKey(key)
	vals, ok := hdr[canon]
	if !ok {
		vals, ok = hdr[key]
	}
	if !ok {
		return obs
	}
	obs.HeaderPresent = true
	obs.PagingKnown = true
	if len(vals) > 0 {
		obs.HeaderValue = vals[0]
	}
	if obs.HeaderValue == "" || obs.HeaderValue == "0" {
		obs.ExhaustedObserved = true
	}
	return obs
}

// ApplyPaging sets pagination_exhausted and next_cursor from observation.
// pagination_exhausted is true only when ExhaustedObserved; never inferred from SDK 0 alone.
func (s *Section) ApplyPaging(obs PagingObservation) {
	s.PaginationExhausted = obs.ExhaustedObserved
	if obs.SDKNextPage > 0 {
		c := strconv.FormatInt(obs.SDKNextPage, 10)
		s.NextCursor = &c
	} else {
		s.NextCursor = nil
	}
	if !obs.PagingKnown {
		s.AddLimitation(CodeUnknownCount, "paging metadata unavailable")
		if s.ContentComplete == ContentCompleteTrue {
			s.ContentComplete = ContentCompleteUnknown
		}
	}
}

// InformationalNextCursor returns the unsigned decimal page token or nil.
func InformationalNextCursor(sdkNextPage int64) *string {
	if sdkNextPage <= 0 {
		return nil
	}
	c := strconv.FormatInt(sdkNextPage, 10)
	return &c
}
