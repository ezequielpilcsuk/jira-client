package jiraclient

import (
	"context"
	"io"
	"net/http"
	"testing"
)

// Jira nests comments under a paginated "comment" object and renders bodies as ADF, so a consumer
// that wants their text should not have to walk the document itself.
func TestSearch_FlattensCommentBodies(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{
			"summary":"one",
			"comment":{"comments":[
				{"id":"10","created":"2026-08-12T09:00:00.000-0300","author":{"accountId":"acct-1"},
				 "body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
				   {"type":"text","text":"first note"}]}]}},
				{"id":"11","body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
				   {"type":"text","text":"second note"}]}]}}
			]}}}],"isLast":true}`)
	})

	issues, err := client.SearchIssues(context.Background(), "project = ABC", []string{"summary", "comment"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}

	comments := issues[0].Comments
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	if comments[0].Body != "first note" || comments[1].Body != "second note" {
		t.Errorf("bodies not flattened: %+v", comments)
	}
	if comments[0].AuthorID != "acct-1" {
		t.Errorf("author: got %q, want acct-1", comments[0].AuthorID)
	}
	if comments[0].Created.IsZero() {
		t.Error("created timestamp not parsed")
	}
}

// An issue fetched without the comment field must report no comments rather than panicking.
func TestSearch_MissingCommentFieldIsNotAnError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{"summary":"one"}}],"isLast":true}`)
	})

	issues, err := client.SearchIssues(context.Background(), "project = ABC", nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(issues[0].Comments) != 0 {
		t.Errorf("expected no comments, got %+v", issues[0].Comments)
	}
}
