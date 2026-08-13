package jiraclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
)

func TestChangelog_PagesUntilIsLast(t *testing.T) {
	var requestedStartAt []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		startAt := r.URL.Query().Get("startAt")
		requestedStartAt = append(requestedStartAt, startAt)
		if startAt == "0" {
			_, _ = io.WriteString(w, `{"values":[{"id":"1"},{"id":"2"}],"isLast":false,
				"total":3,"startAt":0,"maxResults":2}`)
			return
		}
		_, _ = io.WriteString(w, `{"values":[{"id":"3"}],"isLast":true,"total":3,"startAt":2}`)
	})

	entries, err := client.Changelog(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("changelog: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	if entries[0].ID != "1" || entries[2].ID != "3" {
		t.Errorf("entries out of order: %+v", entries)
	}
	if len(requestedStartAt) != 2 || requestedStartAt[1] != "2" {
		t.Errorf("startAt progression: %v", requestedStartAt)
	}
}

// total is an estimate Jira warns can change between pages, so isLast is the only stopping signal.
func TestChangelog_StopsOnIsLastRatherThanTotal(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = io.WriteString(w, `{"values":[{"id":"1"}],"isLast":true,"total":9999,"maxResults":100}`)
	})

	entries, err := client.Changelog(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("changelog: %v", err)
	}
	if requests != 1 {
		t.Errorf("a lying total should not drive paging, made %d requests", requests)
	}
	if len(entries) != 1 {
		t.Errorf("got %d entries, want 1", len(entries))
	}
}

// A page that returns nothing without setting isLast must not be re-requested forever.
func TestChangelog_StopsOnAnEmptyPageMissingIsLast(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > 5 {
			t.Fatal("paging did not terminate")
		}
		_, _ = io.WriteString(w, `{"values":[]}`)
	})

	entries, err := client.Changelog(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("changelog: %v", err)
	}
	if requests != 1 || len(entries) != 0 {
		t.Errorf("requests=%d entries=%d, want 1 and 0", requests, len(entries))
	}
}

func TestChangelog_MapsEveryChangeItemField(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"values":[{"id":"10100",
			"author":{"accountId":"acc-1","displayName":"Ada"},
			"created":"2026-08-11T13:00:32.478-0700",
			"items":[{"field":"status","fieldId":"status","fieldtype":"jira",
				"from":"10002","fromString":"To Do","to":"10100","toString":"Done"}]}],
			"isLast":true}`)
	})

	entries, err := client.Changelog(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("changelog: %v", err)
	}

	entry := entries[0]
	if entry.ID != "10100" || entry.AuthorID != "acc-1" || entry.AuthorName != "Ada" {
		t.Errorf("entry: %+v", entry)
	}
	if entry.Created.Year() != 2026 || entry.Created.Month() != 8 {
		t.Errorf("created: %v", entry.Created)
	}
	if len(entry.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(entry.Items))
	}

	item := entry.Items[0]
	// Jira spells the type key lowercase; decoding it as "fieldType" silently yields "".
	if item.FieldType != "jira" {
		t.Errorf("fieldtype not decoded: %+v", item)
	}
	if item.Field != "status" || item.FieldID != "status" {
		t.Errorf("field: %+v", item)
	}
	if item.From != "10002" || item.FromString != "To Do" || item.To != "10100" || item.ToString != "Done" {
		t.Errorf("transition values: %+v", item)
	}
}

// An automation rule or a deleted account leaves a change with no author, and a cleared field sends
// nulls. Neither may panic or fail the decode.
func TestChangelog_DecodesEntriesWithNoAuthorAndNullValues(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"values":[{"id":"1","items":[
			{"field":"assignee","fieldId":"assignee","fieldtype":"jira",
			 "from":"acc-1","fromString":"Ada","to":null,"toString":null}]}],"isLast":true}`)
	})

	entries, err := client.Changelog(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("changelog: %v", err)
	}

	entry := entries[0]
	if entry.AuthorID != "" || entry.AuthorName != "" {
		t.Errorf("absent author should be empty: %+v", entry)
	}
	if entry.Created.IsZero() == false {
		t.Errorf("absent created should be zero, got %v", entry.Created)
	}
	if entry.Items[0].To != "" || entry.Items[0].ToString != "" {
		t.Errorf("a cleared field should be empty: %+v", entry.Items[0])
	}
	if entry.Items[0].FromString != "Ada" {
		t.Errorf("the from side should survive: %+v", entry.Items[0])
	}
}

func TestChangelog_RejectsAnEmptyKeyBeforeSending(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should have been made")
	})

	_, err := client.Changelog(context.Background(), "")
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("want ErrInvalidArgument, got %v", err)
	}
}

// Jira caps bulkfetch at 1000 issues per request.
func TestChangelogs_ChunksAtTheAPILimit(t *testing.T) {
	var batches []int
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			IssueIDsOrKeys []string `json:"issueIdsOrKeys"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		batches = append(batches, len(payload.IssueIDsOrKeys))
		_, _ = io.WriteString(w, `{"issueChangeLogs":[]}`)
	})

	keys := make([]string, 2500)
	for i := range keys {
		keys[i] = "ABC-1"
	}
	if _, err := client.Changelogs(context.Background(), keys, nil); err != nil {
		t.Fatalf("changelogs: %v", err)
	}

	want := []int{1000, 1000, 500}
	if len(batches) != len(want) {
		t.Fatalf("got %d requests, want %d: %v", len(batches), len(want), batches)
	}
	for i, size := range want {
		if batches[i] != size {
			t.Errorf("batch %d had %d keys, want %d", i, batches[i], size)
		}
	}
}

func TestChangelogs_FollowsNextPageTokenAndKeysByIssueID(t *testing.T) {
	var requestedTokens []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			NextPageToken string `json:"nextPageToken"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		requestedTokens = append(requestedTokens, payload.NextPageToken)

		if payload.NextPageToken == "" {
			_, _ = io.WriteString(w, `{"issueChangeLogs":[
				{"issueId":"10001","changeHistories":[{"id":"1"}]}],"nextPageToken":"page2"}`)
			return
		}
		_, _ = io.WriteString(w, `{"issueChangeLogs":[
			{"issueId":"10001","changeHistories":[{"id":"2"}]},
			{"issueId":"10002","changeHistories":[{"id":"3"}]}]}`)
	})

	histories, err := client.Changelogs(context.Background(), []string{"ABC-1", "ABC-2"}, nil)
	if err != nil {
		t.Fatalf("changelogs: %v", err)
	}
	if len(requestedTokens) != 2 || requestedTokens[1] != "page2" {
		t.Fatalf("pagination tokens: %v", requestedTokens)
	}
	// One issue's history can span pages, and both halves must land in a single entry rather than the
	// issue appearing twice.
	if len(histories) != 2 {
		t.Fatalf("got %d issues, want 2: %+v", len(histories), histories)
	}
	first, second := histories[0], histories[1]
	if first.IssueID != "10001" || len(first.Entries) != 2 {
		t.Fatalf("first issue not accumulated across pages: %+v", first)
	}
	if second.IssueID != "10002" || len(second.Entries) != 1 {
		t.Fatalf("second issue wrong: %+v", second)
	}
	if first.Entries[0].ID != "1" || first.Entries[1].ID != "2" {
		t.Errorf("entries out of order: %+v", first.Entries)
	}
}

func TestChangelogs_SendsFieldIDsWhenGiven(t *testing.T) {
	var payload struct {
		FieldIDs   []string `json:"fieldIds"`
		MaxResults int      `json:"maxResults"`
	}
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		_, _ = io.WriteString(w, `{"issueChangeLogs":[]}`)
	})

	_, err := client.Changelogs(context.Background(), []string{"ABC-1"}, []string{"status", "assignee"})
	if err != nil {
		t.Fatalf("changelogs: %v", err)
	}
	if len(payload.FieldIDs) != 2 || payload.FieldIDs[0] != "status" {
		t.Errorf("field ids: %v", payload.FieldIDs)
	}
	if payload.MaxResults != changelogPageSize {
		t.Errorf("maxResults: %d", payload.MaxResults)
	}
}

// Jira accepts at most 10 field ids, so a larger set is rejected before a request is sent.
func TestChangelogs_RejectsMoreThanTenFieldIDs(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should have been made")
	})

	fieldIDs := make([]string, changelogMaxFields+1)
	_, err := client.Changelogs(context.Background(), []string{"ABC-1"}, fieldIDs)
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("want ErrInvalidArgument, got %v", err)
	}
}

// Nothing to fetch means nothing to ask for.
func TestChangelogs_NoKeysSendsNoRequest(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should have been made")
	})

	histories, err := client.Changelogs(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("changelogs: %v", err)
	}
	if len(histories) != 0 {
		t.Errorf("want an empty map, got %+v", histories)
	}
}
