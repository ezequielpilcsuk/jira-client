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

// decodeBody reads a request's JSON body into a generic map.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

// A workflow that demands a resolution on the way to done rejects a bare transition. The fields go
// in the same request, and so does the comment explaining the move.
func TestApplyTransition_SendsFieldsAndCommentWithTheTransition(t *testing.T) {
	var posted map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		posted = decodeBody(t, r)
		w.WriteHeader(http.StatusNoContent)
	})
	comment, _ := TextDoc("closing")

	err := client.ApplyTransition(context.Background(), "ABC-1", TransitionInput{
		ID:      "31",
		Comment: comment,
		Fields:  map[string]any{"resolution": map[string]string{"name": "Won't Do"}},
	})

	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	encoded, _ := json.Marshal(posted)
	for _, fragment := range []string{
		`"transition":{"id":"31"}`,
		`"fields":{"resolution":{"name":"Won't Do"}}`,
		`"update":{"comment":[{"add":{"body":{`,
	} {
		if strings.Contains(string(encoded), fragment) == false {
			t.Errorf("payload lacks %s: %s", fragment, encoded)
		}
	}
}

func TestApplyTransition_ResolvesTheTransitionByName(t *testing.T) {
	var postedID any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"transitions":[{"id":"11","name":"Start"},{"id":"31","name":"Won't Do"}]}`)
			return
		}
		postedID = decodeBody(t, r)["transition"].(map[string]any)["id"]
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.ApplyTransition(context.Background(), "ABC-1", TransitionInput{Name: "won't do"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if postedID != "31" {
		t.Fatalf("posted transition id = %v, want 31", postedID)
	}
}

// Naming a transition the issue does not offer is the commonest failure, and the fix is nearly always
// visible in the list of what it does offer.
func TestApplyTransition_AnUnknownNameSaysWhatIsAvailable(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"transitions":[{"id":"11","name":"Start"},{"id":"31","name":"Won't Do"}]}`)
	})

	err := client.ApplyTransition(context.Background(), "ABC-1", TransitionInput{Name: "Done"})

	if errors.Is(err, ErrInvalidArgument) == false {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if strings.Contains(err.Error(), "Start") == false || strings.Contains(err.Error(), "Won't Do") == false {
		t.Fatalf("the error must list the available transitions: %v", err)
	}
}

func TestApplyTransition_NeedsExactlyOneOfIDAndName(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { requests++ })

	for name, input := range map[string]TransitionInput{
		"neither": {},
		"both":    {ID: "31", Name: "Done"},
	} {
		if err := client.ApplyTransition(context.Background(), "ABC-1", input); errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("%s: want ErrInvalidArgument, got %v", name, err)
		}
	}
	if requests != 0 {
		t.Fatalf("made %d requests for inputs that could not succeed", requests)
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
