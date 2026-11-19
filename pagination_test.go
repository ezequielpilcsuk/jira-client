package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// A server that hands back the token it was just given would be asked for the same page for ever.
func TestSearch_FailsWhenThePageTokenRepeats(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{"summary":"one"}}],"nextPageToken":"same"}`)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.SearchIssues(ctx, "project = ABC", nil)

	if errors.Is(err, ErrPaginationStalled) == false {
		t.Fatalf("want ErrPaginationStalled, got %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2: the first page and the one that repeated", requests)
	}
}

// Tokens that cycle through several values are the same fault one step removed.
func TestSearch_FailsWhenPageTokensCycle(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		next := "a"
		if r.URL.Query().Get("nextPageToken") == "a" {
			next = "b"
		}
		_, _ = io.WriteString(w, `{"issues":[],"nextPageToken":"`+next+`"}`)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.SearchIssues(ctx, "project = ABC", nil)

	if errors.Is(err, ErrPaginationStalled) == false {
		t.Fatalf("want ErrPaginationStalled, got %v", err)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
}

// /project/search may return an empty page before its last one, so an empty page cannot end the scan
// — but a run of them with no end in sight has to.
func TestProjects_FailsWhenIsLastNeverArrives(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = io.WriteString(w, `{"values":[],"maxResults":100}`)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Projects(ctx, ProjectQuery{})

	if errors.Is(err, ErrPaginationStalled) == false {
		t.Fatalf("want ErrPaginationStalled, got %v", err)
	}
	if requests != maxEmptyProjectPages {
		t.Fatalf("requests = %d, want the %d-page allowance", requests, maxEmptyProjectPages)
	}
}

// A page with rows on it resets the allowance: only an unbroken run of empty pages counts.
func TestProjects_AnEmptyPageBetweenFullOnesIsNotAStall(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case requests == maxEmptyProjectPages+10:
			_, _ = io.WriteString(w, `{"values":[{"id":"2","key":"DEF"}],"maxResults":100,"isLast":true}`)
		case requests%2 == 0:
			_, _ = io.WriteString(w, `{"values":[],"maxResults":100}`)
		default:
			_, _ = io.WriteString(w, `{"values":[{"id":"1","key":"ABC"}],"maxResults":100}`)
		}
	})

	projects, err := client.Projects(context.Background(), ProjectQuery{})

	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	if requests != maxEmptyProjectPages+10 || len(projects) == 0 {
		t.Fatalf("requests = %d, projects = %d", requests, len(projects))
	}
}
