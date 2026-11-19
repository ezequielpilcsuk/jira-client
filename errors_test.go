package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

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

func TestAPIError_Error(t *testing.T) {
	t.Run("prefers Jira's messages", func(t *testing.T) {
		err := &APIError{
			StatusCode: 400, Operation: "POST /issue",
			Messages: []string{"summary: is required", "bad node"},
			Body:     `{"errorMessages":["bad node"]}`,
		}
		got := err.Error()
		for _, want := range []string{"400", "POST /issue", "summary: is required", "bad node"} {
			if strings.Contains(got, want) == false {
				t.Errorf("error missing %q: %s", want, got)
			}
		}
	})

	// Jira puts the actionable detail in the body, so an unparseable body must still surface.
	t.Run("falls back to the raw body", func(t *testing.T) {
		err := &APIError{StatusCode: 500, Operation: "GET /issue", Body: "upstream exploded"}
		if strings.Contains(err.Error(), "upstream exploded") == false {
			t.Errorf("raw body not surfaced: %s", err.Error())
		}
	})
}

// Go randomises map order. An error whose text changes between identical failures cannot be grouped
// or deduplicated by whatever collects the logs.
func TestAPIError_FieldMessagesComeInAStableOrder(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errorMessages":["top"],"errors":{"e":"5","c":"3","a":"1","d":"4","b":"2"}}`)
	})

	want := "jira GET /rest/api/3/issue/ABC-1?fields=summary: status 400: top; a: 1; b: 2; c: 3; d: 4; e: 5"
	for i := 0; i < 20; i++ {
		_, err := client.GetIssue(context.Background(), "ABC-1", []string{"summary"})
		if err == nil || err.Error() != want {
			t.Fatalf("call %d: error = %v, want %s", i, err, want)
		}
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
