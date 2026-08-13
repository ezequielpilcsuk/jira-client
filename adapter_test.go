package jiraclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// This file pins the library against a real consumer's needs.
//
// The first consumer is a ticket-triage automation that collapses duplicate issues: it searches an
// open queue, widens the surviving issue's title, links each duplicate, comments, labels, and
// transitions the duplicate to a terminal status. Its own code depends on a narrow interface rather
// than on a concrete client, so the test below defines that interface, implements it with a thin
// adapter over this library, and asserts the resulting HTTP calls are exactly what a hand-rolled
// integration produced — same endpoints, same methods, same payload shapes, same ordering.
//
// If a future change to this library alters any of those, this test fails rather than the consumer
// silently behaving differently after migrating.

// triageClient is the consumer-side interface, reproduced verbatim.
type triageClient interface {
	SearchIssues(jql string) ([]Issue, error)
	UpdateSummary(key, summary string) error
	CreateIssueLink(linkType, inwardKey, outwardKey string) error
	SetWontDo(key string) error
	UpdateAssignee(key, accountID string) error
	AddLabel(key, label string) error
	AddComment(key, comment string) error
}

// adapter maps the consumer's interface onto the library. It is ~30 lines, which is the point:
// migrating a service is writing this, not rewriting the service.
type adapter struct {
	client *Client
	ctx    context.Context
	// wontDoTransitionID avoids a transition lookup per issue. Resolve it once via Transitions, or
	// use TransitionByName when the extra call is acceptable.
	wontDoTransitionID string
}

func (a adapter) SearchIssues(jql string) ([]Issue, error) {
	return a.client.SearchIssues(a.ctx, jql, nil)
}

func (a adapter) UpdateSummary(key, summary string) error {
	return a.client.UpdateSummary(a.ctx, key, summary)
}

func (a adapter) CreateIssueLink(linkType, inwardKey, outwardKey string) error {
	return a.client.LinkIssues(a.ctx, linkType, inwardKey, outwardKey)
}

func (a adapter) SetWontDo(key string) error {
	return a.client.Transition(a.ctx, key, a.wontDoTransitionID)
}

func (a adapter) UpdateAssignee(key, accountID string) error {
	return a.client.SetAssignee(a.ctx, key, accountID)
}

func (a adapter) AddLabel(key, label string) error {
	return a.client.AddLabel(a.ctx, key, label)
}

// The consumer does not want the created comment back, so the adapter drops it. That one line is the
// entire migration cost of AddComment/AddTextComment returning what they created.
func (a adapter) AddComment(key, comment string) error {
	_, err := a.client.AddTextComment(a.ctx, key, comment)
	return err
}

// Compile-time proof that the library satisfies the consumer's interface through the adapter.
var _ triageClient = adapter{}

// call is one observed HTTP request.
type call struct {
	method string
	path   string
	body   map[string]any
}

// recordingServer captures every request the adapter makes.
func recordingServer(t *testing.T, calls *[]call) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		observed := call{method: r.Method, path: r.URL.Path}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &observed.body)
		}
		*calls = append(*calls, observed)

		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/search/jql"):
			_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{"summary":"NS [A] example.com"}}],"isLast":true}`)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// TestAdapter_ReproducesTriageCallSequence drives the exact sequence a duplicate collapse performs
// and asserts each call lands on the endpoint the previous hand-rolled integration used.
func TestAdapter_ReproducesTriageCallSequence(t *testing.T) {
	var calls []call
	client := newTestClient(t, recordingServer(t, &calls))
	triage := adapter{client: client, ctx: context.Background(), wontDoTransitionID: "71"}

	if _, err := triage.SearchIssues(`project = ABC AND statusCategory != Done`); err != nil {
		t.Fatalf("search: %v", err)
	}
	// Order is load-bearing: a terminal issue may refuse edits, so the link, comment and label must
	// all land before the transition.
	if err := triage.UpdateSummary("ABC-1", "NS [A, B] example.com"); err != nil {
		t.Fatalf("summary: %v", err)
	}
	if err := triage.CreateIssueLink(LinkDuplicate, "ABC-1", "ABC-2"); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := triage.AddComment("ABC-2", "Closed as a duplicate."); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if err := triage.AddLabel("ABC-2", "triage_duplicate"); err != nil {
		t.Fatalf("label: %v", err)
	}
	if err := triage.SetWontDo("ABC-2"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := triage.UpdateAssignee("ABC-3", "acc-1"); err != nil {
		t.Fatalf("assignee: %v", err)
	}

	want := []struct{ method, path string }{
		{http.MethodGet, "/rest/api/3/search/jql"},
		{http.MethodPut, "/rest/api/3/issue/ABC-1"},
		{http.MethodPost, "/rest/api/3/issueLink"},
		{http.MethodPost, "/rest/api/3/issue/ABC-2/comment"},
		{http.MethodPut, "/rest/api/3/issue/ABC-2"},
		{http.MethodPost, "/rest/api/3/issue/ABC-2/transitions"},
		{http.MethodPut, "/rest/api/3/issue/ABC-3/assignee"},
	}
	if len(calls) != len(want) {
		t.Fatalf("made %d calls, want %d: %+v", len(calls), len(want), calls)
	}
	for i, expected := range want {
		if calls[i].method != expected.method || calls[i].path != expected.path {
			t.Errorf("call %d = %s %s, want %s %s", i, calls[i].method, calls[i].path,
				expected.method, expected.path)
		}
	}

	// Payload shapes must match what the previous integration sent.
	if calls[1].body["fields"].(map[string]any)["summary"] != "NS [A, B] example.com" {
		t.Errorf("summary payload: %v", calls[1].body)
	}
	if calls[2].body["inwardIssue"].(map[string]any)["key"] != "ABC-1" ||
		calls[2].body["outwardIssue"].(map[string]any)["key"] != "ABC-2" {
		t.Errorf("link direction: %v", calls[2].body)
	}
	labelUpdate := calls[4].body["update"].(map[string]any)["labels"].([]any)
	if labelUpdate[0].(map[string]any)["add"] != "triage_duplicate" {
		t.Errorf("label payload: %v", calls[4].body)
	}
	if calls[5].body["transition"].(map[string]any)["id"] != "71" {
		t.Errorf("transition payload: %v", calls[5].body)
	}
	if calls[6].body["accountId"] != "acc-1" {
		t.Errorf("assignee payload: %v", calls[6].body)
	}
}

// A dry client must satisfy the same interface and perform no writes, which is what lets a consumer
// produce a full plan against production.
func TestAdapter_DryRunPerformsNoWrites(t *testing.T) {
	var calls []call
	client := newTestClient(t, recordingServer(t, &calls), WithDryRun(true))
	triage := adapter{client: client, ctx: context.Background(), wontDoTransitionID: "71"}

	if _, err := triage.SearchIssues("project = ABC"); err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, mutate := range []func() error{
		func() error { return triage.UpdateSummary("ABC-1", "new") },
		func() error { return triage.CreateIssueLink(LinkDuplicate, "ABC-1", "ABC-2") },
		func() error { return triage.AddComment("ABC-2", "text") },
		func() error { return triage.AddLabel("ABC-2", "x") },
		func() error { return triage.SetWontDo("ABC-2") },
		func() error { return triage.UpdateAssignee("ABC-3", "acc") },
	} {
		if err := mutate(); err != nil {
			t.Fatalf("dry-run mutation returned an error: %v", err)
		}
	}

	if len(calls) != 1 || calls[0].method != http.MethodGet {
		t.Fatalf("dry run should have made exactly one read, got: %+v", calls)
	}
}

// The consumer reads a templated description out of issues. Extraction must survive a real ADF
// document, since that is the contract between the service that writes issues and the one that
// reads them.
func TestAdapter_ExtractsTemplatedDescription(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{
			"summary":"NS [A] example.com",
			"description":{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[{"type":"text","text":"Website : Name = example | Id = 42"}]},
				{"type":"paragraph","content":[{"type":"text","text":"Brand : Name = A | Id = 7"}]}]}
		}}],"isLast":true}`)
	})
	triage := adapter{client: client, ctx: context.Background()}

	issues, err := triage.SearchIssues("project = ABC")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if strings.Contains(issues[0].Description, "Id = 42") == false {
		t.Fatalf("description lost its templated fields: %q", issues[0].Description)
	}
}
