package jiraclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

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

func TestSearch_ReconcilesTheIssuesNamed(t *testing.T) {
	t.Run("passes the ids on every page", func(t *testing.T) {
		var perPageIDs [][]string
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			perPageIDs = append(perPageIDs, r.URL.Query()["reconcileIssues"])
			if r.URL.Query().Get("nextPageToken") == "" {
				_, _ = io.WriteString(w, `{"issues":[],"isLast":false,"nextPageToken":"p2"}`)
				return
			}
			_, _ = io.WriteString(w, `{"issues":[],"isLast":true}`)
		})

		if _, err := client.Search(context.Background(), SearchQuery{JQL: "project = ABC", ReconcileIssues: []string{"10001", "10002"}}); err != nil {
			t.Fatalf("search: %v", err)
		}

		if len(perPageIDs) != 2 {
			t.Fatalf("expected two pages, got %d", len(perPageIDs))
		}
		for page, ids := range perPageIDs {
			if len(ids) != 2 || ids[0] != "10001" {
				t.Errorf("page %d lost the reconcile ids: %v", page, ids)
			}
		}
	})

	// Jira accepts at most 50, so a larger set is rejected before a request is sent.
	t.Run("rejects more than fifty ids", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("no request should have been made")
		})

		ids := make([]string, 51)
		_, err := client.Search(context.Background(), SearchQuery{JQL: "project = ABC", ReconcileIssues: ids})
		if errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("want ErrInvalidArgument, got %v", err)
		}
	})
}

// A warned query returns 200 with no issues, so without the warnings a wrong JQL is indistinguishable
// from one that legitimately matched nothing.
func TestSearch_SurfacesWarnings(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[],"isLast":true,
			"warnings":[{"message":"The value 'nope' does not exist for the field 'status'."}]}`)
	})

	result, err := client.Search(context.Background(), SearchQuery{JQL: "status = nope"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Issues) != 0 {
		t.Errorf("expected no issues, got %d", len(result.Issues))
	}
	if len(result.Warnings) != 1 || strings.Contains(result.Warnings[0], "does not exist") == false {
		t.Errorf("warning not surfaced: %v", result.Warnings)
	}
}

// The retired /search endpoint used a different warning key; both are decoded.
func TestSearch_DecodesLegacyWarningShape(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[],"isLast":true,"warningMessages":["legacy warning"]}`)
	})

	result, err := client.Search(context.Background(), SearchQuery{JQL: "project = ABC"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != "legacy warning" {
		t.Errorf("legacy warning not decoded: %v", result.Warnings)
	}
}

func TestAccountIDByEmail_PrefersAnExactEmailMatch(t *testing.T) {
	// Jira matches the query as a prefix, so the wrong account can come back first.
	t.Run("an exact match wins over an earlier prefix match", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `[
				{"accountId":"wrong","emailAddress":"ada@example.com.au"},
				{"accountId":"right","emailAddress":"ada@example.com"}
			]`)
		})

		id, err := client.AccountIDByEmail(context.Background(), "ada@example.com")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if id != "right" {
			t.Errorf("got %q, want the exactly-matching account", id)
		}
	})

	t.Run("matching is case-insensitive", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `[{"accountId":"acct-1","emailAddress":"Ada@Example.COM"}]`)
		})

		id, err := client.AccountIDByEmail(context.Background(), "ada@example.com")
		if err != nil || id != "acct-1" {
			t.Errorf("got (%q, %v), want acct-1", id, err)
		}
	})

	// Emails are visible but none matches exactly — every candidate is a longer address.
	t.Run("a visible but non-matching email resolves to nothing", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `[{"accountId":"wrong","emailAddress":"ada@example.com.au"}]`)
		})

		id, err := client.AccountIDByEmail(context.Background(), "ada@example.com")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if id != "" {
			t.Errorf("got %q, want no match rather than a wrong one", id)
		}
	})

	// Under restrictive profile visibility there is nothing to verify against, so the top match is
	// the best available guess.
	t.Run("falls back when every email is hidden", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `[{"accountId":"acct-1","displayName":"Ada"}]`)
		})

		id, err := client.AccountIDByEmail(context.Background(), "ada@example.com")
		if err != nil || id != "acct-1" {
			t.Errorf("got (%q, %v), want acct-1", id, err)
		}
	})
}

// A silently-unparsed timestamp is worse than a wrong one: it decodes to the zero time with no error.
func TestParseJiraTime_AcceptsTheFormatsJiraActuallySends(t *testing.T) {
	cases := map[string]string{
		"jira default":        "2026-08-11T13:00:32.478-0700",
		"rfc3339 with offset": "2026-08-11T13:00:32-07:00",
		"rfc3339 zulu":        "2026-08-11T13:00:32Z",
		"nanoseconds":         "2026-08-11T13:00:32.123456789Z",
		"no fraction":         "2026-08-11T13:00:32-0700",
		"date only":           "2026-08-11",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if parseJiraTime(raw).IsZero() == true {
				t.Errorf("%q did not parse", raw)
			}
		})
	}

	for _, raw := range []string{"", "not a time"} {
		if parseJiraTime(raw).IsZero() == false {
			t.Errorf("%q should not have parsed", raw)
		}
	}

	// The offset must survive, not be flattened to UTC.
	parsed := parseJiraTime("2026-08-11T13:00:32.478-0700")
	if _, offset := parsed.Zone(); offset != -7*60*60 {
		t.Errorf("offset lost: got %d seconds", offset)
	}
	if parsed.Year() != 2026 || parsed.Month() != time.August || parsed.Day() != 11 {
		t.Errorf("date decoded wrong: %s", parsed)
	}
}

// 413 is a permanent per-issue ceiling, not a rate limit — retrying never clears it.
func TestAPIError_EntityLimitMapsToItsOwnSentinel(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"errorMessages":["Issue has too many comments"]}`)
	})

	_, err := client.GetIssue(context.Background(), "ABC-1", nil)
	if errors.Is(err, ErrLimitExceeded) == false {
		t.Errorf("want ErrLimitExceeded, got %v", err)
	}
	if errors.Is(err, ErrRateLimited) == true {
		t.Error("an entity limit must not be confused with a rate limit")
	}
}
