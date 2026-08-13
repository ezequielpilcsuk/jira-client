package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

func TestWatchers_DecodesTheList(t *testing.T) {
	var path string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = io.WriteString(w, `{"isWatching":true,"watchCount":2,"watchers":[
			{"accountId":"acct-1","displayName":"Ada","active":true},
			{"accountId":"acct-2","displayName":"Grace","active":false}]}`)
	})

	list, err := client.Watchers(context.Background(), "ABC-1")

	if err != nil {
		t.Fatalf("watchers: %v", err)
	}
	if path != "/rest/api/3/issue/ABC-1/watchers" {
		t.Fatalf("path: %s", path)
	}
	if list.Count != 2 || list.IsWatching == false {
		t.Fatalf("counts: %+v", list)
	}
	if len(list.Watchers) != 2 || list.Watchers[0].AccountID != "acct-1" {
		t.Fatalf("watchers: %+v", list.Watchers)
	}
	if list.Watchers[0].DisplayName != "Ada" || list.Watchers[1].Active == true {
		t.Fatalf("watcher fields: %+v", list.Watchers)
	}
}

// Without the View voters and watchers permission Jira returns the count but omits the roster, so
// the two must not be conflated.
func TestWatchers_CountSurvivesAnOmittedRoster(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"isWatching":false,"watchCount":7}`)
	})

	list, err := client.Watchers(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("watchers: %v", err)
	}
	if list.Count != 7 {
		t.Errorf("count = %d, want 7", list.Count)
	}
	if list.Watchers == nil || len(list.Watchers) != 0 {
		t.Errorf("an absent roster should be an empty slice, got %+v", list.Watchers)
	}
}

// The POST body is a bare JSON string, not an object — the one endpoint in this library shaped that
// way, and an object here is a silent 400.
func TestAddWatcher_SendsABareJSONString(t *testing.T) {
	var body, contentType, method, path string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		contentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.AddWatcher(context.Background(), "ABC-1", "5b10ac8d82e05b22cc7d4ef5"); err != nil {
		t.Fatalf("add watcher: %v", err)
	}

	if method != http.MethodPost || path != "/rest/api/3/issue/ABC-1/watchers" {
		t.Fatalf("request: %s %s", method, path)
	}
	if body != `"5b10ac8d82e05b22cc7d4ef5"` {
		t.Fatalf("body = %s, want a bare quoted string", body)
	}
	if contentType != "application/json" {
		t.Fatalf("content type: %s", contentType)
	}
}

// An empty account id means "add me", which Jira expresses as no body at all — an empty JSON string
// is not the same thing.
func TestAddWatcher_EmptyAccountIDSendsNoBody(t *testing.T) {
	var body string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.AddWatcher(context.Background(), "ABC-1", ""); err != nil {
		t.Fatalf("add watcher: %v", err)
	}
	if body != "" {
		t.Fatalf("body = %q, want nothing sent", body)
	}
}

// The account id moves to the query string on delete, the reverse of the POST.
func TestRemoveWatcher_PutsTheAccountIDInTheQuery(t *testing.T) {
	var method, path, accountID, body string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		accountID = r.URL.Query().Get("accountId")
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.RemoveWatcher(context.Background(), "ABC-1", "5b10ac8d82e05b22cc7d4ef5"); err != nil {
		t.Fatalf("remove watcher: %v", err)
	}

	if method != http.MethodDelete || path != "/rest/api/3/issue/ABC-1/watchers" {
		t.Fatalf("request: %s %s", method, path)
	}
	if accountID != "5b10ac8d82e05b22cc7d4ef5" {
		t.Fatalf("accountId query = %q", accountID)
	}
	if body != "" {
		t.Fatalf("delete must not carry a body, got %q", body)
	}
}

func TestWatchers_DryRunSuppressesWritesButNotReads(t *testing.T) {
	var writes []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes = append(writes, r.Method+" "+r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"isWatching":false,"watchCount":0}`)
	}, WithDryRun(true))

	ctx := context.Background()
	if _, err := client.Watchers(ctx, "ABC-1"); err != nil {
		t.Fatalf("read must still work: %v", err)
	}
	if err := client.AddWatcher(ctx, "ABC-1", "acct-1"); err != nil {
		t.Fatalf("add watcher: %v", err)
	}
	if err := client.RemoveWatcher(ctx, "ABC-1", "acct-1"); err != nil {
		t.Fatalf("remove watcher: %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("dry run performed writes: %v", writes)
	}
}

func TestWatchers_RejectsBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	ctx := context.Background()

	cases := map[string]error{
		"list without key":       mustErr(func() error { _, err := client.Watchers(ctx, ""); return err }),
		"add without key":        client.AddWatcher(ctx, "", "acct-1"),
		"remove without key":     client.RemoveWatcher(ctx, "", "acct-1"),
		"remove without account": client.RemoveWatcher(ctx, "ABC-1", ""),
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
