package tools

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

func TestSearchCode_queryParams(t *testing.T) {
	t.Parallel()
	var sawPath string
	var sawQuery url.Values
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
	cli, _ := testutil.NewGitLabClient(t, h)
	ref := "main"
	st := "zoekt"
	_, out, err := searchCode(context.Background(), nil, searchCodeIn{
		Query:      "filename:*.go WithSearchType",
		Ref:        &ref,
		SearchType: &st,
	}, Deps{Config: &config.Config{}, Client: cli})
	if err != nil {
		t.Fatal(err)
	}
	if sawPath != "/api/v4/search" {
		t.Fatalf("path %q", sawPath)
	}
	if got := sawQuery.Get("scope"); got != "blobs" {
		t.Fatalf("scope=%q", got)
	}
	if got := sawQuery.Get("search"); got != "filename:*.go WithSearchType" {
		t.Fatalf("search=%q", got)
	}
	if got := sawQuery.Get("ref"); got != "main" {
		t.Fatalf("ref=%q", got)
	}
	if got := sawQuery.Get("search_type"); got != "zoekt" {
		t.Fatalf("search_type=%q", got)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("out type %T", out)
	}
	if _, ok := m["blobs"]; !ok {
		t.Fatalf("missing blobs: %#v", m)
	}
}

func TestSearchProjectCode_queryParams(t *testing.T) {
	t.Parallel()
	var sawPath string
	var sawQuery url.Values
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
	cli, _ := testutil.NewGitLabClient(t, h)
	ref := "feature/x"
	st := "advanced"
	_, _, err := searchProjectCode(context.Background(), nil, searchProjectCodeIn{
		ProjectID:  "42",
		Query:      "path:internal/ foo",
		Ref:        &ref,
		SearchType: &st,
	}, Deps{Config: &config.Config{}, Client: cli})
	if err != nil {
		t.Fatal(err)
	}
	if sawPath != "/api/v4/projects/42/-/search" {
		t.Fatalf("path %q", sawPath)
	}
	if sawQuery.Get("scope") != "blobs" {
		t.Fatalf("scope=%q", sawQuery.Get("scope"))
	}
	if sawQuery.Get("ref") != "feature/x" {
		t.Fatalf("ref=%q", sawQuery.Get("ref"))
	}
	if sawQuery.Get("search_type") != "advanced" {
		t.Fatalf("search_type=%q", sawQuery.Get("search_type"))
	}
}

func TestSearchGroupCode_queryParams(t *testing.T) {
	t.Parallel()
	var sawPath string
	var sawQuery url.Values
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
	cli, _ := testutil.NewGitLabClient(t, h)
	st := "basic"
	_, _, err := searchGroupCode(context.Background(), nil, searchGroupCodeIn{
		GroupID:    "10",
		Query:      "extension:go Ptr",
		SearchType: &st,
	}, Deps{Config: &config.Config{}, Client: cli})
	if err != nil {
		t.Fatal(err)
	}
	if sawPath != "/api/v4/groups/10/-/search" {
		t.Fatalf("path %q", sawPath)
	}
	if sawQuery.Get("scope") != "blobs" {
		t.Fatalf("scope=%q", sawQuery.Get("scope"))
	}
	if sawQuery.Get("search_type") != "basic" {
		t.Fatalf("search_type=%q", sawQuery.Get("search_type"))
	}
	if sawQuery.Get("ref") != "" {
		t.Fatalf("unexpected ref=%q", sawQuery.Get("ref"))
	}
}

func TestSearchCode_invalidSearchType(t *testing.T) {
	t.Parallel()
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("should not call API")
		w.WriteHeader(http.StatusNoContent)
	}))
	bad := "lucene"
	_, _, err := searchCode(context.Background(), nil, searchCodeIn{
		Query:      "x",
		SearchType: &bad,
	}, Deps{Config: &config.Config{}, Client: cli})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSearchProjectCode_invalidSearchType(t *testing.T) {
	t.Parallel()
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("should not call API")
		w.WriteHeader(http.StatusNoContent)
	}))
	bad := "nope"
	_, _, err := searchProjectCode(context.Background(), nil, searchProjectCodeIn{
		ProjectID:  "1",
		Query:      "x",
		SearchType: &bad,
	}, Deps{Config: &config.Config{}, Client: cli})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSearchGroupCode_invalidSearchType(t *testing.T) {
	t.Parallel()
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("should not call API")
		w.WriteHeader(http.StatusNoContent)
	}))
	bad := "nope"
	_, _, err := searchGroupCode(context.Background(), nil, searchGroupCodeIn{
		GroupID:    "1",
		Query:      "x",
		SearchType: &bad,
	}, Deps{Config: &config.Config{}, Client: cli})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSearchProjectCode_emptyProjectID(t *testing.T) {
	t.Parallel()
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("should not call API")
		w.WriteHeader(http.StatusNoContent)
	}))
	_, _, err := searchProjectCode(context.Background(), nil, searchProjectCodeIn{
		Query: "x",
	}, Deps{Config: &config.Config{}, Client: cli})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSearchCode_apiError(t *testing.T) {
	t.Parallel()
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})
	cli, _ := testutil.NewGitLabClient(t, h)
	_, _, err := searchCode(context.Background(), nil, searchCodeIn{Query: "x"}, Deps{
		Config: &config.Config{},
		Client: cli,
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSearchProjectCode_apiError(t *testing.T) {
	t.Parallel()
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})
	cli, _ := testutil.NewGitLabClient(t, h)
	_, _, err := searchProjectCode(context.Background(), nil, searchProjectCodeIn{
		ProjectID: "1",
		Query:     "x",
	}, Deps{Config: &config.Config{}, Client: cli})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSearchGroupCode_apiError(t *testing.T) {
	t.Parallel()
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})
	cli, _ := testutil.NewGitLabClient(t, h)
	_, _, err := searchGroupCode(context.Background(), nil, searchGroupCodeIn{
		GroupID: "1",
		Query:   "x",
	}, Deps{Config: &config.Config{}, Client: cli})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSearchCode_omitsOptionalParams(t *testing.T) {
	t.Parallel()
	var sawQuery url.Values
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
	cli, _ := testutil.NewGitLabClient(t, h)
	_, _, err := searchCode(context.Background(), nil, searchCodeIn{Query: "hello"}, Deps{
		Config: &config.Config{},
		Client: cli,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawQuery.Get("scope") != "blobs" {
		t.Fatalf("scope=%q", sawQuery.Get("scope"))
	}
	if _, ok := sawQuery["search_type"]; ok {
		t.Fatalf("search_type present: %v", sawQuery["search_type"])
	}
	if _, ok := sawQuery["ref"]; ok {
		t.Fatalf("ref present: %v", sawQuery["ref"])
	}
}
