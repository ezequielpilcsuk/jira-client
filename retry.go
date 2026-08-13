package jiraclient

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// Retry defaults. Jira documents exponential backoff with jitter, honouring Retry-After as a minimum.
const (
	defaultMaxAttempts = 4
	defaultBaseDelay   = 2 * time.Second
	defaultMaxDelay    = 30 * time.Second
)

// RetryPolicy controls how transient failures are retried.
//
// Jira rate-limits on several axes at once, including a per-issue write limit
// (RateLimit-Reason: jira-per-issue-on-write). Any workflow that performs several writes against the
// same issue — link it, comment on it, label it, transition it — can trip that limit even at modest
// overall volume, so retrying is not optional for a client that mutates issues.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts including the first. Zero disables retrying.
	MaxAttempts int
	// BaseDelay is the initial backoff, doubled each attempt.
	BaseDelay time.Duration
	// MaxDelay caps the backoff.
	MaxDelay time.Duration
	// RetryServerErrors also retries 5xx responses, which Jira returns transiently under load.
	RetryServerErrors bool
}

// DefaultRetryPolicy follows Atlassian's published guidance.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:       defaultMaxAttempts,
		BaseDelay:         defaultBaseDelay,
		MaxDelay:          defaultMaxDelay,
		RetryServerErrors: true,
	}
}

// WithRetryPolicy sets the retry policy. Without it the client does not retry, so a caller that
// already has its own retry wrapper is not silently double-retrying.
func WithRetryPolicy(policy RetryPolicy) ClientOption {
	return func(c *Client) { c.retry = policy }
}

// shouldRetry reports whether an error is worth another attempt, and how long to wait first.
func (c *Client) shouldRetry(err error, attempt int) (time.Duration, bool) {
	if c.retry.MaxAttempts <= 1 || attempt >= c.retry.MaxAttempts {
		return 0, false
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) == false {
		return 0, false
	}

	retryable := apiErr.StatusCode == http.StatusTooManyRequests ||
		(c.retry.RetryServerErrors == true && apiErr.StatusCode >= 500)
	if retryable == false {
		return 0, false
	}

	return c.backoff(attempt, apiErr.RetryAfter), true
}

// backoff computes the wait: exponential from BaseDelay, capped at MaxDelay, jittered by 0.7-1.3 to
// avoid a thundering herd, and never shorter than the server's own Retry-After.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	base := c.retry.BaseDelay
	if base <= 0 {
		base = defaultBaseDelay
	}
	maxDelay := c.retry.MaxDelay
	if maxDelay <= 0 {
		maxDelay = defaultMaxDelay
	}

	delay := base << (attempt - 1)
	if delay > maxDelay {
		delay = maxDelay
	}

	jittered := time.Duration(float64(delay) * (0.7 + rand.Float64()*0.6))
	if jittered > retryAfter {
		return jittered
	}
	return retryAfter
}

// sleep waits for the delay or until the context is cancelled.
func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// parseRateLimitReset reads X-RateLimit-Reset, an ISO 8601 timestamp naming when the quota refills.
// Absent from most responses, so a zero time means "not stated" rather than "now".
func parseRateLimitReset(header http.Header) time.Time {
	raw := header.Get("X-RateLimit-Reset")
	if raw == "" {
		return time.Time{}
	}
	return parseJiraTime(raw)
}

// parseRetryAfter reads the Retry-After header, which Jira sends in seconds.
func parseRetryAfter(header http.Header) time.Duration {
	raw := header.Get("Retry-After")
	if raw == "" {
		return 0
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
