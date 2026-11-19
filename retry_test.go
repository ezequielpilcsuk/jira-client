package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// fastRetry is a policy whose waits are too short to slow a test down.
func fastRetry() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, RetryServerErrors: true}
}

// A 5xx does not prove the write did not happen: a gateway can time out after Jira created the issue.
// Replaying the POST would file the ticket twice.
func TestRetry_DoesNotReplayACreateOnServerError(t *testing.T) {
	attempts := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadGateway)
	}, WithRetryPolicy(fastRetry()))

	_, err := client.CreateIssue(context.Background(),
		CreateIssueInput{ProjectKey: "ABC", IssueType: "Task", Summary: "s"})

	var apiErr *APIError
	if errors.As(err, &apiErr) == false || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("want the 502 back, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("the create was sent %d times, want exactly 1", attempts)
	}
}

// A 429 is refused before Jira executes anything, so replaying even a create is safe.
func TestRetry_ReplaysACreateOnRateLimit(t *testing.T) {
	attempts := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"1","key":"ABC-1"}`)
	}, WithRetryPolicy(fastRetry()))

	key, err := client.CreateIssue(context.Background(),
		CreateIssueInput{ProjectKey: "ABC", IssueType: "Task", Summary: "s"})

	if err != nil || key != "ABC-1" {
		t.Fatalf("key = %q, err = %v", key, err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

// A PUT sets a state rather than adding to it, so sending it twice leaves the issue as one send would.
func TestRetry_ReplaysAnIdempotentWriteOnServerError(t *testing.T) {
	attempts := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}, WithRetryPolicy(fastRetry()))

	if err := client.UpdateSummary(context.Background(), "ABC-1", "new"); err != nil {
		t.Fatalf("should have recovered: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

// Jira models some reads as a POST. They change nothing, so they stay as retryable as a GET.
func TestRetry_ReplaysAReadExpressedAsPostOnServerError(t *testing.T) {
	attempts := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{"summary":"one"}}]}`)
	}, WithRetryPolicy(fastRetry()))

	issues, err := client.GetIssues(context.Background(), []string{"ABC-1"}, nil)

	if err != nil || len(issues) != 1 {
		t.Fatalf("issues = %v, err = %v", issues, err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

// A caller that would rather risk a duplicate than lose a write can say so.
func TestRetry_RetryNonIdempotentReplaysACreateOnServerError(t *testing.T) {
	attempts := 0
	policy := fastRetry()
	policy.RetryNonIdempotent = true
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"1","key":"ABC-1"}`)
	}, WithRetryPolicy(policy))

	key, err := client.CreateIssue(context.Background(),
		CreateIssueInput{ProjectKey: "ABC", IssueType: "Task", Summary: "s"})

	if err != nil || key != "ABC-1" {
		t.Fatalf("key = %q, err = %v", key, err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

// A wait longer than the policy tolerates is handed back at once, with the rate limit intact, so the
// caller can reschedule instead of holding a goroutine for an hour.
func TestRetry_ReturnsTheRateLimitWhenRetryAfterExceedsTheCap(t *testing.T) {
	attempts := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}, WithRetryPolicy(fastRetry()))

	started := time.Now()
	_, err := client.GetIssue(context.Background(), "ABC-1", nil)

	if errors.Is(err, ErrRateLimited) == false {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) == false || apiErr.RetryAfter != time.Hour {
		t.Fatalf("the requested wait must survive on the error, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("returned after %s, want immediately", elapsed)
	}
}

// A wait the context cannot outlive is pointless to start: the caller gets the rate limit, not a
// deadline error that hides why the call failed.
func TestRetry_ReturnsTheRateLimitWhenTheDeadlineComesFirst(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}, WithRetryPolicy(fastRetry()))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, err := client.GetIssue(ctx, "ABC-1", nil)

	if errors.Is(err, ErrRateLimited) == false {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("returned after %s, want before the deadline", elapsed)
	}
}

// Cancelling mid-wait reports the cancellation without losing the failure that caused the wait.
func TestRetry_CancellationKeepsTheErrorBeingRetried(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}, WithRetryPolicy(fastRetry()))

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := client.GetIssue(ctx, "ABC-1", nil)

	if errors.Is(err, context.Canceled) == false {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if errors.Is(err, ErrRateLimited) == false {
		t.Fatalf("the rate limit being waited out was lost: %v", err)
	}
}

// Doubling by bit shift overflows after a few dozen attempts and yields no wait at all.
func TestBackoff_StaysAtTheCapForLargeAttemptCounts(t *testing.T) {
	client := NewClient("http://example.invalid", "e", "t",
		WithRetryPolicy(RetryPolicy{MaxAttempts: 100, BaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second}))

	for _, attempt := range []int{33, 34, 64, 65, 99} {
		delay := client.backoff(attempt, 0)
		if delay < 21*time.Second || delay > 39*time.Second {
			t.Errorf("attempt %d waits %s, want the 30s cap give or take jitter", attempt, delay)
		}
	}
}

// Retry-After is seconds in everything Jira documents, but HTTP also allows a date.
func TestParseRetryAfter_AcceptsAnHTTPDate(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))

	wait := parseRetryAfter(header)

	if wait < 80*time.Second || wait > 100*time.Second {
		t.Fatalf("wait = %s, want about 90s", wait)
	}
}
