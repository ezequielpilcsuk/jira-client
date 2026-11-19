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
)

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

func TestJQLQuote(t *testing.T) {
	for value, want := range map[string]string{
		`plain`:             `"plain"`,
		``:                  `""`,
		`say "hi"`:          `"say \"hi\""`,
		`C:\temp`:           `"C:\\temp"`,
		"two\nlines\tand\r": `"two\nlines\tand\r"`,
		"bell\x07":          `"bell\u0007"`,
		`préço €`:           `"préço €"`,
		`" OR project = X`:  `"\" OR project = X"`,
	} {
		if got := JQLQuote(value); got != want {
			t.Errorf("JQLQuote(%q) = %s, want %s", value, got, want)
		}
	}
}

func TestJQLList(t *testing.T) {
	if got, want := JQLList("triaged", `b"c`), `("triaged", "b\"c")`; got != want {
		t.Errorf("JQLList = %s, want %s", got, want)
	}
	if got := JQLList(); got != "()" {
		t.Errorf("empty list = %s, want ()", got)
	}
}
