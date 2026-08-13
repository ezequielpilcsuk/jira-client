package jiraclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
)

// commentPage renders one page of the comment list.
func commentPage(startAt, total int, ids ...string) string {
	comments := make([]string, 0, len(ids))
	for _, id := range ids {
		comments = append(comments, `{"id":"`+id+`","created":"2026-08-12T09:00:00.000-0300",
			"author":{"accountId":"acct-`+id+`"},
			"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
				{"type":"text","text":"note `+id+`"}]}]}}`)
	}
	page := `{"startAt":` + strconv.Itoa(startAt) + `,"maxResults":100,"total":` + strconv.Itoa(total) + `,"comments":[`
	for i, comment := range comments {
		if i > 0 {
			page += ","
		}
		page += comment
	}
	return page + `]}`
}

func TestComments_PagesUntilTotal(t *testing.T) {
	var requestedStarts []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("startAt")
		requestedStarts = append(requestedStarts, start)
		if start == "0" {
			_, _ = io.WriteString(w, commentPage(0, 3, "10", "11"))
			return
		}
		_, _ = io.WriteString(w, commentPage(2, 3, "12"))
	})

	comments, err := client.Comments(context.Background(), "ABC-1")

	if err != nil {
		t.Fatalf("comments: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("got %d comments, want 3", len(comments))
	}
	if comments[0].ID != "10" || comments[2].ID != "12" {
		t.Fatalf("unexpected ids: %+v", comments)
	}
	if len(requestedStarts) != 2 || requestedStarts[1] != "2" {
		t.Fatalf("pagination cursors: %v", requestedStarts)
	}
}

// A page advancing past the total must stop even when Jira's total disagrees with what it sent.
func TestComments_StopsOnAnEmptyPage(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > 5 {
			t.Fatal("pagination did not terminate")
		}
		if requests == 1 {
			_, _ = io.WriteString(w, commentPage(0, 99, "10"))
			return
		}
		_, _ = io.WriteString(w, commentPage(1, 99))
	})

	comments, err := client.Comments(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("comments: %v", err)
	}
	if len(comments) != 1 || requests != 2 {
		t.Fatalf("got %d comments over %d requests, want 1 over 2", len(comments), requests)
	}
}

func TestComments_FlattensBodyAndAuthor(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, commentPage(0, 1, "10"))
	})

	comments, err := client.Comments(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("comments: %v", err)
	}
	if comments[0].Body != "note 10" {
		t.Errorf("body not flattened: %q", comments[0].Body)
	}
	if comments[0].AuthorID != "acct-10" {
		t.Errorf("author: %q", comments[0].AuthorID)
	}
	if comments[0].Created.IsZero() == true {
		t.Error("created timestamp not parsed")
	}
}

// Jira omits the author of a comment left by a deleted account, and the body of one it will not show.
func TestComments_DecodesCommentMissingAuthorAndBody(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"total":1,"comments":[{"id":"10"}]}`)
	})

	comments, err := client.Comments(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("comments: %v", err)
	}
	if comments[0].Body != "" || comments[0].AuthorID != "" {
		t.Errorf("absent fields should be zero: %+v", comments[0])
	}
	if comments[0].Created.IsZero() == false {
		t.Errorf("absent created should be zero, got %v", comments[0].Created)
	}
}

func TestComments_SendsOrderBy(t *testing.T) {
	var ordered string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		ordered = r.URL.Query().Get("orderBy")
		_, _ = io.WriteString(w, commentPage(0, 0))
	})

	if _, err := client.Comments(context.Background(), "ABC-1", CommentOrderNewestFirst); err != nil {
		t.Fatalf("comments: %v", err)
	}
	if ordered != "-created" {
		t.Fatalf("orderBy = %q, want -created", ordered)
	}
}

// orderBy accepts only created/+created/-created; anything else is a certain 400.
func TestComments_RejectsAnUnsortableOrder(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should have been sent")
	})

	_, err := client.Comments(context.Background(), "ABC-1", CommentOrder("updated"))
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}

	for _, order := range []CommentOrder{CommentOrderOldestFirst, CommentOrderNewestFirst, "+created"} {
		if validCommentOrders[order] == false {
			t.Errorf("%q should be accepted", order)
		}
	}
}

// AddComment throws the 201 body away, so the created comment's id is unrecoverable. This is the
// whole reason AddCommentReturning exists.
func TestAddCommentReturning_ReturnsTheCreatedComment(t *testing.T) {
	var payload map[string]any
	var method, path string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"10042","created":"2026-08-12T09:00:00.000-0300",
			"author":{"accountId":"acct-1","displayName":"Bot"},
			"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
				{"type":"text","text":"posted"}]}]}}`)
	})

	doc, _ := TextDoc("posted")
	comment, err := client.AddCommentReturning(context.Background(), "ABC-1", doc)

	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if method != http.MethodPost || path != "/rest/api/3/issue/ABC-1/comment" {
		t.Fatalf("request: %s %s", method, path)
	}
	if comment.ID != "10042" {
		t.Fatalf("id = %q, want 10042", comment.ID)
	}
	if comment.Body != "posted" || comment.AuthorID != "acct-1" {
		t.Fatalf("comment not flattened: %+v", comment)
	}
	if _, restricted := payload["visibility"]; restricted == true {
		t.Errorf("an ordinary comment must not carry a visibility: %v", payload)
	}
}

// The write is suppressed, so there is no 201 body to decode. It has to degrade like CreateIssue
// rather than fail on an empty response.
func TestAddCommentReturning_DryRunReturnsAPlaceholder(t *testing.T) {
	var writes []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writes = append(writes, r.Method+" "+r.URL.Path)
	}, WithDryRun(true))

	ctx := context.Background()
	doc, _ := TextDoc("posted")

	comment, err := client.AddCommentReturning(ctx, "ABC-1", doc)
	if err != nil {
		t.Fatalf("dry run must not fail: %v", err)
	}
	if comment.ID != "DRY-RUN" {
		t.Fatalf("id = %q, want DRY-RUN", comment.ID)
	}

	restricted, err := client.AddRestrictedComment(ctx, "ABC-1", doc,
		Visibility{Type: VisibilityGroup, Identifier: "grp-1"})
	if err != nil || restricted.ID != "DRY-RUN" {
		t.Fatalf("restricted dry run: id=%q err=%v", restricted.ID, err)
	}

	// The placeholder id must be safe to feed back into the rest of the lifecycle.
	if err := client.UpdateComment(ctx, "ABC-1", comment.ID, doc); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := client.DeleteComment(ctx, "ABC-1", comment.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("dry run performed writes: %v", writes)
	}
}

func TestAddRestrictedComment_SendsVisibility(t *testing.T) {
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"1"}`)
	})

	doc, _ := TextDoc("internal")
	_, err := client.AddRestrictedComment(context.Background(), "ABC-1", doc,
		Visibility{Type: VisibilityRole, Value: "Administrators", Identifier: "10002"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	visibility, ok := payload["visibility"].(map[string]any)
	if ok == false {
		t.Fatalf("no visibility sent: %v", payload)
	}
	if visibility["type"] != "role" || visibility["value"] != "Administrators" {
		t.Errorf("visibility: %v", visibility)
	}
	// The identifier is the stable key; it must survive even though a value was also given.
	if visibility["identifier"] != "10002" {
		t.Errorf("identifier dropped: %v", visibility)
	}
}

// An identifier alone is the recommended form, so it must not require a value alongside it.
func TestAddRestrictedComment_AcceptsAnIdentifierWithoutAValue(t *testing.T) {
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"1"}`)
	})

	doc, _ := TextDoc("internal")
	if _, err := client.AddRestrictedComment(context.Background(), "ABC-1", doc,
		Visibility{Type: VisibilityGroup, Identifier: "grp-1"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	visibility := payload["visibility"].(map[string]any)
	if _, present := visibility["value"]; present == true {
		t.Errorf("an unset value must be omitted, not sent empty: %v", visibility)
	}
}

func TestUpdateComment_PutsToTheCommentPath(t *testing.T) {
	var method, path string
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
	})

	doc, _ := TextDoc("edited")
	if err := client.UpdateComment(context.Background(), "ABC-1", "10042", doc); err != nil {
		t.Fatalf("update: %v", err)
	}
	if method != http.MethodPut || path != "/rest/api/3/issue/ABC-1/comment/10042" {
		t.Fatalf("request: %s %s", method, path)
	}
	if _, present := payload["body"]; present == false {
		t.Fatalf("body not sent: %v", payload)
	}
}

func TestDeleteComment_DeletesTheCommentPath(t *testing.T) {
	var method, path string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.DeleteComment(context.Background(), "ABC-1", "10042"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if method != http.MethodDelete || path != "/rest/api/3/issue/ABC-1/comment/10042" {
		t.Fatalf("request: %s %s", method, path)
	}
}

func TestCommentLifecycle_RejectsBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	ctx := context.Background()
	doc, _ := TextDoc("text")

	cases := map[string]error{
		"list without key":   mustErr(func() error { _, err := client.Comments(ctx, ""); return err }),
		"add without key":    mustErr(func() error { _, err := client.AddCommentReturning(ctx, "", doc); return err }),
		"add without doc":    mustErr(func() error { _, err := client.AddCommentReturning(ctx, "ABC-1", nil); return err }),
		"add an empty doc":   mustErr(func() error { _, err := client.AddCommentReturning(ctx, "ABC-1", &ADFDoc{}); return err }),
		"update without id":  client.UpdateComment(ctx, "ABC-1", "", doc),
		"update without doc": client.UpdateComment(ctx, "ABC-1", "10042", nil),
		"delete without id":  client.DeleteComment(ctx, "ABC-1", ""),
		"delete without key": client.DeleteComment(ctx, "", "10042"),
		"visibility type": mustErr(func() error {
			_, err := client.AddRestrictedComment(ctx, "ABC-1", doc, Visibility{Type: "project", Value: "x"})
			return err
		}),
		"visibility without a target": mustErr(func() error {
			_, err := client.AddRestrictedComment(ctx, "ABC-1", doc, Visibility{Type: VisibilityGroup})
			return err
		}),
		"visibility unset": mustErr(func() error {
			_, err := client.AddRestrictedComment(ctx, "ABC-1", doc, Visibility{})
			return err
		}),
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

// The 5,000-comment ceiling is permanent, not a rate limit — a caller has to stop, not back off.
func TestAddCommentReturning_EntityCeilingIsALimitNotARateLimit(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"errorMessages":["Issue has too many comments"]}`)
	})

	doc, _ := TextDoc("one too many")
	_, err := client.AddCommentReturning(context.Background(), "ABC-1", doc)

	if errors.Is(err, ErrLimitExceeded) == false {
		t.Errorf("want ErrLimitExceeded, got %v", err)
	}
	if errors.Is(err, ErrRateLimited) == true {
		t.Error("an entity limit must not be confused with a rate limit")
	}
}
