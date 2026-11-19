package jiraclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGetIssues_ChunksKeys(t *testing.T) {
	var queries []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("jql"))
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{"summary":"s"}}],"isLast":true}`)
	})

	keys := make([]string, 150)
	for i := range keys {
		keys[i] = "ABC-" + string(rune('a'+i%26))
	}

	if _, err := client.GetIssues(context.Background(), keys, nil); err != nil {
		t.Fatalf("get issues: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("150 keys should be 2 requests, got %d", len(queries))
	}
}

// GetIssues must read issues directly rather than searching for them: the search index is eventually
// consistent, so an issue created a moment earlier can be missing from a JQL result entirely.
func TestGetIssues_ReadsDirectlyNotViaSearch(t *testing.T) {
	var requestedPath, requestedBody string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		requestedBody = string(raw)
		_, _ = io.WriteString(w, `{"issues":[
			{"id":"1","key":"ABC-1","fields":{"summary":"one"}},
			{"id":"2","key":"ABC-2","fields":{"summary":"two"}}
		],"issueErrors":[]}`)
	})

	issues, err := client.GetIssues(context.Background(), []string{"ABC-1", "ABC-2"}, nil)
	if err != nil {
		t.Fatalf("get issues: %v", err)
	}

	if requestedPath != "/rest/api/3/issue/bulkfetch" {
		t.Errorf("got %q, want the bulkfetch endpoint", requestedPath)
	}
	if strings.Contains(requestedBody, "key IN") == true {
		t.Errorf("keys should not be turned into JQL: %s", requestedBody)
	}
	if len(issues) != 2 || issues["ABC-1"].Summary != "one" {
		t.Errorf("issues not decoded: %+v", issues)
	}
}

// Jira caps bulkfetch at 100 issues per request.
func TestGetIssues_ChunksAtTheAPILimit(t *testing.T) {
	var batches []int
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			IssueIDsOrKeys []string `json:"issueIdsOrKeys"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		batches = append(batches, len(payload.IssueIDsOrKeys))
		_, _ = io.WriteString(w, `{"issues":[]}`)
	})

	keys := make([]string, 250)
	for i := range keys {
		keys[i] = "ABC-1"
	}
	if _, err := client.GetIssues(context.Background(), keys, nil); err != nil {
		t.Fatalf("get issues: %v", err)
	}

	want := []int{100, 100, 50}
	if len(batches) != len(want) {
		t.Fatalf("got %d requests, want %d: %v", len(batches), len(want), batches)
	}
	for i, size := range want {
		if batches[i] != size {
			t.Errorf("batch %d had %d keys, want %d", i, batches[i], size)
		}
	}
}

// An unknown key is absent from the map rather than failing the whole batch.
func TestGetIssues_MissingKeysAreSimplyAbsent(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[{"id":"1","key":"ABC-1","fields":{"summary":"one"}}],
			"issueErrors":[{"id":"ABC-9","errorMessage":"Issue does not exist"}]}`)
	})

	issues, err := client.GetIssues(context.Background(), []string{"ABC-1", "ABC-9"}, nil)
	if err != nil {
		t.Fatalf("a per-key error must not fail the batch: %v", err)
	}
	if len(issues) != 1 {
		t.Errorf("got %d issues, want 1: %+v", len(issues), issues)
	}
	if _, present := issues["ABC-9"]; present == true {
		t.Error("a missing key should not appear in the map")
	}
}

// Jira's 255 limit on a summary counts characters. Counting bytes refuses a title of accented or
// non-Latin text at a fraction of the length Jira would accept.
func TestSummaryLimit_CountsCharactersNotBytes(t *testing.T) {
	dry := NewClient("http://example.invalid", "e", "t", WithDryRun(true))
	accented := strings.Repeat("é", 200)

	if _, err := dry.CreateIssue(context.Background(),
		CreateIssueInput{ProjectKey: "ABC", IssueType: "Task", Summary: accented}); err != nil {
		t.Errorf("a 200-character summary was refused on create: %v", err)
	}
	if err := dry.UpdateSummary(context.Background(), "ABC-1", accented); err != nil {
		t.Errorf("a 200-character summary was refused on update: %v", err)
	}

	tooLong := strings.Repeat("é", SummaryMaxChars+1)
	if err := dry.UpdateSummary(context.Background(), "ABC-1", tooLong); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("a %d-character summary must be refused, got %v", SummaryMaxChars+1, err)
	}
}

// Jira limits writes per issue, so five edits to one ticket are five chances to be throttled. One
// request carrying all of them is one.
func TestUpdateIssue_SendsEveryChangeInOneRequest(t *testing.T) {
	var (
		requests int
		method   string
		path     string
		query    string
		body     map[string]any
	)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
		body = decodeBody(t, r)
		w.WriteHeader(http.StatusNoContent)
	})
	quiet := false

	err := client.UpdateIssue(context.Background(), "ABC-1", IssueUpdate{
		Summary:      "New title",
		Fields:       map[string]any{"customfield_10004": 3},
		AddLabels:    []string{"triaged", "bot"},
		RemoveLabels: []string{"stale"},
		NotifyUsers:  &quiet,
	})

	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if requests != 1 || method != http.MethodPut || path != "/rest/api/3/issue/ABC-1" || query != "notifyUsers=false" {
		t.Fatalf("got %d × %s %s?%s", requests, method, path, query)
	}
	encoded, _ := json.Marshal(body)
	want := `{"fields":{"customfield_10004":3,"summary":"New title"},` +
		`"update":{"labels":[{"add":"triaged"},{"add":"bot"},{"remove":"stale"}]}}`
	if string(encoded) != want {
		t.Fatalf("payload = %s\nwant      %s", encoded, want)
	}
}

func TestUpdateIssue_LeavesNotificationToJiraWhenUnstated(t *testing.T) {
	query := "unset"
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.UpdateIssue(context.Background(), "ABC-1", IssueUpdate{AddLabels: []string{"x"}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if query != "" {
		t.Fatalf("query = %q, want none", query)
	}
}

func TestUpdateIssue_RefusesWhatJiraWouldReject(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { requests++ })

	cases := map[string]struct {
		key    string
		update IssueUpdate
	}{
		"no issue key":               {"", IssueUpdate{Summary: "s"}},
		"nothing to change":          {"ABC-1", IssueUpdate{}},
		"blank summary":              {"ABC-1", IssueUpdate{Summary: "   "}},
		"over-long summary":          {"ABC-1", IssueUpdate{Summary: strings.Repeat("x", SummaryMaxChars+1)}},
		"summary given twice":        {"ABC-1", IssueUpdate{Summary: "a", Fields: map[string]any{"summary": "b"}}},
		"label with a space":         {"ABC-1", IssueUpdate{AddLabels: []string{"two words"}}},
		"empty label":                {"ABC-1", IssueUpdate{RemoveLabels: []string{""}}},
		"labels replaced and edited": {"ABC-1", IssueUpdate{Fields: map[string]any{"labels": []string{"a"}}, AddLabels: []string{"b"}}},
	}
	for name, tc := range cases {
		if err := client.UpdateIssue(context.Background(), tc.key, tc.update); errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("%s: want ErrInvalidArgument, got %v", name, err)
		}
	}
	if requests != 0 {
		t.Fatalf("made %d requests for updates that could not succeed", requests)
	}
}

func TestUpdateIssue_DryRunSendsNothing(t *testing.T) {
	requests := 0
	dry := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { requests++ }, WithDryRun(true))

	if err := dry.UpdateIssue(context.Background(), "ABC-1", IssueUpdate{Summary: "s"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if requests != 0 {
		t.Fatalf("a dry run made %d requests", requests)
	}
}

func TestDeleteIssue(t *testing.T) {
	var method, path string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.DeleteIssue(context.Background(), "ABC-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if method != "DELETE" || path != "/rest/api/3/issue/ABC-1" {
		t.Errorf("got %s %s, want DELETE /rest/api/3/issue/ABC-1", method, path)
	}
}

func TestDeleteIssue_RejectsEmptyKey(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not reach the server")
	})
	if err := client.DeleteIssue(context.Background(), ""); err == nil {
		t.Fatal("empty key must be rejected")
	}
}

// Delete is a mutation, so a dry client must refuse it like every other write.
func TestDeleteIssue_DryRunDoesNotCall(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a dry client must not issue a delete")
	}, WithDryRun(true))

	if err := client.DeleteIssue(context.Background(), "ABC-1"); err != nil {
		t.Fatalf("dry delete: %v", err)
	}
}
