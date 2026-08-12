package jiraclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient points a client at an httptest server.
func newTestClient(t *testing.T, handler http.HandlerFunc, opts ...ClientOption) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient(server.URL, "bot@example.com", "token", opts...)
}

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

	issues, err := client.Search(context.Background(), "project = ABC", nil)

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

// Jira omits unset fields entirely. Decoding must not assume any of them are present.
func TestSearch_DecodesIssueMissingEveryOptionalField(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{"summary":"bare"}}],"isLast":true}`)
	})

	issues, err := client.Search(context.Background(), "project = ABC", nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	issue := issues[0]
	if issue.Description != "" || issue.Status != "" || issue.AssigneeID != "" || issue.ReporterID != "" {
		t.Fatalf("absent fields should be zero: %+v", issue)
	}
	if issue.Created.IsZero() == false {
		t.Fatalf("absent created should be zero, got %v", issue.Created)
	}
}

func TestSearch_MapsPopulatedFields(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[{"id":"1","key":"ABC-1","fields":{
			"summary":"s","labels":["a"],"created":"2026-08-05T09:30:00.000-0700",
			"status":{"id":"3","name":"In Progress"},
			"assignee":{"accountId":"acc","displayName":"Dev"},
			"reporter":{"accountId":"rep","displayName":"Reporter"},
			"priority":{"name":"High"},"issuetype":{"name":"Bug"},"resolution":{"name":"Done"},
			"description":{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[{"type":"text","text":"hello"}]}]}
		}}],"isLast":true}`)
	})

	issues, _ := client.Search(context.Background(), "project = ABC", nil)
	issue := issues[0]

	if issue.Status != "In Progress" || issue.StatusID != "3" {
		t.Fatalf("status: %+v", issue)
	}
	if issue.AssigneeID != "acc" || issue.AssigneeName != "Dev" || issue.ReporterID != "rep" {
		t.Fatalf("people: %+v", issue)
	}
	if issue.Priority != "High" || issue.IssueType != "Bug" || issue.Resolution != "Done" {
		t.Fatalf("named fields: %+v", issue)
	}
	if issue.Description != "hello" {
		t.Fatalf("description: %q", issue.Description)
	}
	if issue.Created.Year() != 2026 {
		t.Fatalf("created: %v", issue.Created)
	}
	if issue.HasLabel("A") == false {
		t.Fatal("label matching should be case-insensitive")
	}
}

// A dry client must read normally and write nothing.
func TestDryRun_SuppressesEveryMutationButNotReads(t *testing.T) {
	var writes []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes = append(writes, r.Method+" "+r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"issues":[],"isLast":true,"transitions":[]}`)
	}, WithDryRun(true))

	ctx := context.Background()
	if _, err := client.Search(ctx, "project = ABC", nil); err != nil {
		t.Fatalf("read must still work: %v", err)
	}

	mutations := map[string]error{
		"SetAssignee":   client.SetAssignee(ctx, "ABC-1", "acc"),
		"AddLabel":      client.AddLabel(ctx, "ABC-1", "x"),
		"RemoveLabel":   client.RemoveLabel(ctx, "ABC-1", "x"),
		"UpdateSummary": client.UpdateSummary(ctx, "ABC-1", "new"),
		"AddComment":    client.AddTextComment(ctx, "ABC-1", "hi"),
		"Transition":    client.Transition(ctx, "ABC-1", "71"),
		"LinkIssues":    client.LinkIssues(ctx, LinkDuplicate, "ABC-1", "ABC-2"),
		"CustomField":   client.UpdateCustomField(ctx, "ABC-1", "customfield_1", 3),
	}
	for name, err := range mutations {
		if err != nil {
			t.Fatalf("%s should be a no-op in dry run, got %v", name, err)
		}
	}

	key, err := client.CreateIssue(ctx, CreateIssueInput{ProjectKey: "ABC", IssueType: "Bug", Summary: "s"})
	if err != nil || key != "DRY-RUN" {
		t.Fatalf("CreateIssue dry run: key=%q err=%v", key, err)
	}
	if len(writes) != 0 {
		t.Fatalf("dry run performed writes: %v", writes)
	}
}

func TestLinkIssues_SendsDirectionAndType(t *testing.T) {
	var payload map[string]any
	var path string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusCreated)
	})

	if err := client.LinkIssues(context.Background(), LinkDuplicate, "ABC-1", "ABC-2"); err != nil {
		t.Fatalf("link: %v", err)
	}
	if path != "/rest/api/3/issueLink" {
		t.Fatalf("path: %s", path)
	}
	if payload["type"].(map[string]any)["name"] != "Duplicate" {
		t.Fatalf("type: %v", payload["type"])
	}
	if payload["inwardIssue"].(map[string]any)["key"] != "ABC-1" {
		t.Fatalf("inward: %v", payload["inwardIssue"])
	}
	if payload["outwardIssue"].(map[string]any)["key"] != "ABC-2" {
		t.Fatalf("outward: %v", payload["outwardIssue"])
	}
}

func TestTransitionByName_ResolvesID(t *testing.T) {
	var transitioned string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"transitions":[
				{"id":"11","name":"To Do","to":{"id":"10002","name":"To Do"}},
				{"id":"71","name":"Won't Do","to":{"id":"10100","name":"Won't Do"}}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]map[string]string
		_ = json.Unmarshal(body, &payload)
		transitioned = payload["transition"]["id"]
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.TransitionByName(context.Background(), "ABC-1", "won't do"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if transitioned != "71" {
		t.Fatalf("resolved to %q, want 71", transitioned)
	}
}

func TestTransitionByName_UnavailableTransition(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"transitions":[{"id":"11","name":"To Do","to":{}}]}`)
	})

	err := client.TransitionByName(context.Background(), "ABC-1", "Won't Do")
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}

// Arguments Jira would certainly reject are refused locally, without spending a request.
func TestLocalValidation_RejectsBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	ctx := context.Background()

	cases := map[string]error{
		"empty jql":       mustErr(func() error { _, err := client.Search(ctx, "  ", nil); return err }),
		"long summary":    client.UpdateSummary(ctx, "ABC-1", strings.Repeat("x", SummaryMaxChars+1)),
		"empty summary":   client.UpdateSummary(ctx, "ABC-1", "  "),
		"label w/ space":  client.AddLabel(ctx, "ABC-1", "two words"),
		"self link":       client.LinkIssues(ctx, LinkDuplicate, "ABC-1", "ABC-1"),
		"empty link type": client.LinkIssues(ctx, "", "ABC-1", "ABC-2"),
		"empty comment":   client.AddTextComment(ctx, "ABC-1", "   "),
	}
	for name, err := range cases {
		if errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("%s: want ErrInvalidArgument, got %v", name, err)
		}
	}
	if called == true {
		t.Fatal("no request should have been sent")
	}
}

func TestAPIError_MapsStatusToSentinel(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, ErrNotFound},
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrUnauthorized},
		{http.StatusTooManyRequests, ErrRateLimited},
	}

	for _, tc := range cases {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"errorMessages":["nope"]}`)
		})
		_, err := client.GetIssue(context.Background(), "ABC-1", nil)
		if errors.Is(err, tc.want) == false {
			t.Errorf("status %d: want %v, got %v", tc.status, tc.want, err)
		}
	}
}

// Jira puts the actionable detail in the body; it must survive into the error.
func TestAPIError_PreservesJiraMessages(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errorMessages":["bad doc"],"errors":{"summary":"too long"}}`)
	})

	_, err := client.GetIssue(context.Background(), "ABC-1", nil)

	var apiErr *APIError
	if errors.As(err, &apiErr) == false {
		t.Fatalf("want *APIError, got %v", err)
	}
	joined := strings.Join(apiErr.Messages, "|")
	if strings.Contains(joined, "bad doc") == false || strings.Contains(joined, "summary: too long") == false {
		t.Fatalf("messages lost: %v", apiErr.Messages)
	}
}

func TestRetry_RetriesRateLimitThenSucceeds(t *testing.T) {
	attempts := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("RateLimit-Reason", "jira-per-issue-on-write")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}, WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}))

	if err := client.AddLabel(context.Background(), "ABC-1", "x"); err != nil {
		t.Fatalf("should have recovered: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestRetry_GivesUpAndDoesNotRetryClientErrors(t *testing.T) {
	attempts := 0
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusTooManyRequests)
	}, WithRetryPolicy(policy))
	if err := client.AddLabel(context.Background(), "ABC-1", "x"); err == nil {
		t.Fatal("want error after exhausting attempts")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}

	attempts = 0
	badRequest := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
	}, WithRetryPolicy(policy))
	if err := badRequest.AddLabel(context.Background(), "ABC-1", "x"); err == nil {
		t.Fatal("want error")
	}
	if attempts != 1 {
		t.Fatalf("400 must not be retried, attempts = %d", attempts)
	}
}

// Without an explicit policy the client does not retry, so a caller with its own retry wrapper is
// not silently double-retrying.
func TestRetry_DisabledByDefault(t *testing.T) {
	attempts := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_ = client.AddLabel(context.Background(), "ABC-1", "x")
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

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

func mustErr(fn func() error) error { return fn() }
