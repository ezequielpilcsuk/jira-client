package jiraclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSearch_PagesUntilLast(t *testing.T) {
	var requestedTokens []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedTokens = append(requestedTokens, r.URL.Query().Get("nextPageToken"))
		if r.URL.Query().Get("nextPageToken") == "" {
			_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{"summary":"one"}}],
				"isLast":false,"nextPageToken":"page2"}`)
			return
		}
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-2","fields":{"summary":"two"}}],"isLast":true}`)
	})

	issues, err := client.SearchIssues(context.Background(), "project = ABC", nil)

	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	if issues[0].Key != "ABC-1" || issues[1].Key != "ABC-2" {
		t.Fatalf("unexpected keys: %v", issues)
	}
	if len(requestedTokens) != 2 || requestedTokens[1] != "page2" {
		t.Fatalf("pagination tokens: %v", requestedTokens)
	}
}

func TestSearch_ReconcilesTheIssuesNamed(t *testing.T) {
	t.Run("passes the ids on every page", func(t *testing.T) {
		var perPageIDs [][]string
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			perPageIDs = append(perPageIDs, r.URL.Query()["reconcileIssues"])
			if r.URL.Query().Get("nextPageToken") == "" {
				_, _ = io.WriteString(w, `{"issues":[],"isLast":false,"nextPageToken":"p2"}`)
				return
			}
			_, _ = io.WriteString(w, `{"issues":[],"isLast":true}`)
		})

		if _, err := client.Search(context.Background(), SearchQuery{JQL: "project = ABC", ReconcileIssues: []string{"10001", "10002"}}); err != nil {
			t.Fatalf("search: %v", err)
		}

		if len(perPageIDs) != 2 {
			t.Fatalf("expected two pages, got %d", len(perPageIDs))
		}
		for page, ids := range perPageIDs {
			if len(ids) != 2 || ids[0] != "10001" {
				t.Errorf("page %d lost the reconcile ids: %v", page, ids)
			}
		}
	})

	// Jira accepts at most 50, so a larger set is rejected before a request is sent.
	t.Run("rejects more than fifty ids", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("no request should have been made")
		})

		ids := make([]string, 51)
		_, err := client.Search(context.Background(), SearchQuery{JQL: "project = ABC", ReconcileIssues: ids})
		if errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("want ErrInvalidArgument, got %v", err)
		}
	})
}

// A warned query returns 200 with no issues, so without the warnings a wrong JQL is indistinguishable
// from one that legitimately matched nothing.
func TestSearch_SurfacesWarnings(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[],"isLast":true,
			"warnings":[{"message":"The value 'nope' does not exist for the field 'status'."}]}`)
	})

	result, err := client.Search(context.Background(), SearchQuery{JQL: "status = nope"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Issues) != 0 {
		t.Errorf("expected no issues, got %d", len(result.Issues))
	}
	if len(result.Warnings) != 1 || strings.Contains(result.Warnings[0], "does not exist") == false {
		t.Errorf("warning not surfaced: %v", result.Warnings)
	}
}

// The retired /search endpoint used a different warning key; both are decoded.
func TestSearch_DecodesLegacyWarningShape(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[],"isLast":true,"warningMessages":["legacy warning"]}`)
	})

	result, err := client.Search(context.Background(), SearchQuery{JQL: "project = ABC"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != "legacy warning" {
		t.Errorf("legacy warning not decoded: %v", result.Warnings)
	}
}

// A server that hands back the token it was just given would be asked for the same page for ever.
func TestSearch_FailsWhenThePageTokenRepeats(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{"summary":"one"}}],"nextPageToken":"same"}`)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.SearchIssues(ctx, "project = ABC", nil)

	if errors.Is(err, ErrPaginationStalled) == false {
		t.Fatalf("want ErrPaginationStalled, got %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2: the first page and the one that repeated", requests)
	}
}

// Tokens that cycle through several values are the same fault one step removed.
func TestSearch_FailsWhenPageTokensCycle(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		next := "a"
		if r.URL.Query().Get("nextPageToken") == "a" {
			next = "b"
		}
		_, _ = io.WriteString(w, `{"issues":[],"nextPageToken":"`+next+`"}`)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.SearchIssues(ctx, "project = ABC", nil)

	if errors.Is(err, ErrPaginationStalled) == false {
		t.Fatalf("want ErrPaginationStalled, got %v", err)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
}

// twoIssuePages answers every search request with two issues and a fresh page token, recording the
// page size each request asked for.
func twoIssuePages(requestedSizes *[]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*requestedSizes = append(*requestedSizes, r.URL.Query().Get("maxResults"))
		page := len(*requestedSizes)
		_, _ = fmt.Fprintf(w, `{"issues":[{"key":"ABC-%d"},{"key":"ABC-%d"}],"nextPageToken":"p%d"}`,
			page*2-1, page*2, page+1)
	}
}

// "The five newest" should cost the requests for five issues, not for every match.
func TestSearch_LimitStopsOnceEnoughIssuesAreIn(t *testing.T) {
	var sizes []string
	client := newTestClient(t, twoIssuePages(&sizes))

	result, err := client.Search(context.Background(), SearchQuery{JQL: "project = ABC ORDER BY created DESC", Limit: 3})

	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Issues) != 3 || result.Issues[2].Key != "ABC-3" {
		t.Fatalf("issues = %+v, want the first three", result.Issues)
	}
	if slices.Equal(sizes, []string{"3", "1"}) == false {
		t.Fatalf("page sizes asked for = %v, want [3 1]", sizes)
	}
}

func TestSearch_NegativeLimitIsRefused(t *testing.T) {
	var sizes []string
	client := newTestClient(t, twoIssuePages(&sizes))

	_, err := client.Search(context.Background(), SearchQuery{JQL: "project = ABC", Limit: -1})

	if errors.Is(err, ErrInvalidArgument) == false || len(sizes) != 0 {
		t.Fatalf("err = %v after %d requests", err, len(sizes))
	}
}

// A long key list makes a URL of several kilobytes, which a gateway turns away before Jira sees it.
// Jira serves the same search from a POST body.
func TestSearch_SendsALongQueryInTheBody(t *testing.T) {
	keys := make([]string, 0, 900)
	for number := 0; number < 900; number++ {
		keys = append(keys, fmt.Sprintf("ABC-%d", 100000+number))
	}
	jql := "key IN (" + strings.Join(keys, ", ") + ")"

	var bodies []map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rest/api/3/search/jql" || r.URL.RawQuery != "" {
			t.Errorf("got %s %s?%s, want a bare POST to /rest/api/3/search/jql", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		bodies = append(bodies, decodeBody(t, r))
		if len(bodies) == 1 {
			_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1"}],"nextPageToken":"p2"}`)
			return
		}
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-2"}],"isLast":true}`)
	})

	result, err := client.Search(context.Background(), SearchQuery{
		JQL: jql, Fields: []string{"summary", "status"}, ReconcileIssues: []string{"10001"},
	})

	if err != nil || len(result.Issues) != 2 {
		t.Fatalf("issues = %d, err = %v", len(result.Issues), err)
	}
	first, second := bodies[0], bodies[1]
	if first["jql"] != jql || first["maxResults"] != float64(100) {
		t.Errorf("first body: maxResults %v, jql intact %v", first["maxResults"], first["jql"] == jql)
	}
	if fields, _ := first["fields"].([]any); len(fields) != 2 || fields[0] != "summary" || fields[1] != "status" {
		t.Errorf("fields must be a JSON array of ids, got %v", first["fields"])
	}
	if ids, _ := first["reconcileIssues"].([]any); len(ids) != 1 || ids[0] != float64(10001) {
		t.Errorf("reconcile ids must be JSON numbers, got %v", first["reconcileIssues"])
	}
	if _, sent := first["nextPageToken"]; sent == true {
		t.Errorf("the first page must not carry a page token: %v", first["nextPageToken"])
	}
	if second["nextPageToken"] != "p2" {
		t.Errorf("second page token = %v, want p2", second["nextPageToken"])
	}
}

// A search is a read on either verb, so it is retried on either.
func TestSearch_ALongQueryIsStillRetriedOnServerError(t *testing.T) {
	attempts := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1"}],"isLast":true}`)
	}, WithRetryPolicy(fastRetry()))

	issues, err := client.SearchIssues(context.Background(), `summary ~ "`+strings.Repeat("x", 7000)+`"`, nil)

	if err != nil || len(issues) != 1 || attempts != 2 {
		t.Fatalf("issues = %d, err = %v, attempts = %d", len(issues), err, attempts)
	}
}

// Jira reconciles by numeric issue id. A key in that list can never work, so it is caught here.
func TestSearch_RefusesAReconcileIDThatIsNotNumeric(t *testing.T) {
	var sizes []string
	client := newTestClient(t, twoIssuePages(&sizes))

	_, err := client.Search(context.Background(), SearchQuery{JQL: "project = ABC", ReconcileIssues: []string{"ABC-1"}})

	if errors.Is(err, ErrInvalidArgument) == false || len(sizes) != 0 {
		t.Fatalf("err = %v after %d requests", err, len(sizes))
	}
}
