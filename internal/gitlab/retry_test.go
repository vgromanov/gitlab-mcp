package gitlab

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"

	gl "gitlab.com/gitlab-org/api/client-go/v2"
)

// flaky answers 502 until okAfter requests have been made, then 200.
func flaky(t *testing.T, okAfter int32) (*gl.Client, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if n.Add(1) <= okAfter {
			w.WriteHeader(http.StatusBadGateway)
		}
		_, _ = w.Write([]byte(`{"id":1,"iid":1}`))
	}))
	t.Cleanup(ts.Close)
	c, err := NewClient(&config.Config{Token: "t", APIURL: ts.URL + "/api/v4"})
	if err != nil {
		t.Fatal(err)
	}
	return c, &n
}

// A write that gets a 502 must reach the server exactly once.
func TestNewClient_writesAreNotRetried(t *testing.T) {
	calls := map[string]func(c *gl.Client) error{
		"POST": func(c *gl.Client) error {
			_, _, err := c.Notes.CreateMergeRequestNote(1, 1, &gl.CreateMergeRequestNoteOptions{Body: gl.Ptr("x")})
			return err
		},
		"PUT": func(c *gl.Client) error {
			_, _, err := c.MergeRequests.UpdateMergeRequest(1, 1, &gl.UpdateMergeRequestOptions{Title: gl.Ptr("x")})
			return err
		},
		"DELETE": func(c *gl.Client) error {
			_, err := c.MergeRequests.DeleteMergeRequest(1, 1)
			return err
		},
	}
	for method, call := range calls {
		t.Run(method, func(t *testing.T) {
			c, n := flaky(t, 100)
			if err := call(c); err == nil {
				t.Fatal("want the 502 as an error")
			}
			if got := n.Load(); got != 1 {
				t.Fatalf("%s made %d requests, want exactly 1", method, got)
			}
		})
	}
}

// Reads keep the SDK's retry policy: a 502 followed by a 200 succeeds.
func TestNewClient_readsStillRetry(t *testing.T) {
	c, n := flaky(t, 1)
	if _, _, err := c.MergeRequests.GetMergeRequest(1, 1, nil); err != nil {
		t.Fatalf("GET should have been retried: %v", err)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("GET made %d requests, want 2 (502 then 200)", got)
	}
}
