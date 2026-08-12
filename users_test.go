package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

func TestSearchUsers_DecodesAccounts(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path + "?" + r.URL.RawQuery
		_, _ = io.WriteString(w, `[
			{"accountId":"acct-1","accountType":"atlassian","displayName":"Ada",
			 "emailAddress":"ada@example.com","active":true},
			{"accountId":"acct-2","displayName":"Grace","active":false}
		]`)
	})

	users, err := client.SearchUsers(context.Background(), "ada@example.com")
	if err != nil {
		t.Fatalf("search users: %v", err)
	}

	if len(users) != 2 {
		t.Fatalf("got %d users, want 2", len(users))
	}
	if users[0].AccountID != "acct-1" || users[0].Email != "ada@example.com" || users[0].Active == false {
		t.Errorf("first user decoded wrong: %+v", users[0])
	}
	// A response omitting optional fields must not fail the decode.
	if users[1].AccountID != "acct-2" || users[1].Email != "" || users[1].Active == true {
		t.Errorf("second user decoded wrong: %+v", users[1])
	}
	if requestedPath != "/rest/api/3/user/search?maxResults=10&query=ada%40example.com" {
		t.Errorf("unexpected request: %s", requestedPath)
	}
}

func TestSearchUsers_RejectsAnEmptyQuery(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should have been made")
	})

	if _, err := client.SearchUsers(context.Background(), "  "); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("want ErrInvalidArgument, got %v", err)
	}
}

func TestAccountIDByEmail(t *testing.T) {
	t.Run("resolves the top match", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `[{"accountId":"acct-1"},{"accountId":"acct-2"}]`)
		})

		id, err := client.AccountIDByEmail(context.Background(), "ada@example.com")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if id != "acct-1" {
			t.Errorf("got %q, want acct-1", id)
		}
	})

	// No match is not an error: a caller that cannot resolve a reporter carries on without one.
	t.Run("no match is not an error", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `[]`)
		})

		id, err := client.AccountIDByEmail(context.Background(), "nobody@example.com")
		if err != nil || id != "" {
			t.Errorf("got (%q, %v), want (\"\", nil)", id, err)
		}
	})

	t.Run("an empty address does not call Jira", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("no request should have been made")
		})

		if id, err := client.AccountIDByEmail(context.Background(), ""); id != "" || err != nil {
			t.Errorf("got (%q, %v), want (\"\", nil)", id, err)
		}
	})

	t.Run("a transport failure surfaces", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})

		if _, err := client.AccountIDByEmail(context.Background(), "ada@example.com"); err == nil {
			t.Error("expected the error to surface")
		}
	})
}
