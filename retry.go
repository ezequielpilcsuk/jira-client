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
	defaultMaxAttempts   = 4
	defaultBaseDelay     = 2 * time.Second
	defaultMaxDelay      = 30 * time.Second
	defaultMaxRetryAfter = time.Minute
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
	// MaxRetryAfter is the longest server-requested wait the client will sit through. A response asking
	// for more is returned at once, still carrying APIError.RetryAfter, so a caller can reschedule the
	// work instead of holding a goroutine for as long as Jira says. Zero means one minute.
	MaxRetryAfter time.Duration
	// RetryServerErrors also retries 5xx responses, which Jira returns transiently under load. It
	// covers requests that are safe to send twice; see RetryNonIdempotent for the rest.
	RetryServerErrors bool
	// RetryNonIdempotent extends RetryServerErrors to the writes that add something each time they
	// run: creating an issue, a comment, a worklog or an issue link, uploading an attachment, applying
	// a transition. It is off by default because a 5xx does not prove the write failed — a gateway can
	// time out after Jira has stored it — and replaying then files the ticket twice. A 429 is refused
	// before anything executes, so those are retried whatever this says.
	RetryNonIdempotent bool
}

// DefaultRetryPolicy follows Atlassian's published guidance.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:       defaultMaxAttempts,
		BaseDelay:         defaultBaseDelay,
		MaxDelay:          defaultMaxDelay,
		MaxRetryAfter:     defaultMaxRetryAfter,
		RetryServerErrors: true,
	}
}

// WithRetryPolicy sets the retry policy. Without it the client does not retry, so a caller that
// already has its own retry wrapper is not silently double-retrying.
func WithRetryPolicy(policy RetryPolicy) ClientOption {
	return func(c *Client) { c.retry = policy }
}

// shouldRetry reports whether an error is worth another attempt, and how long to wait first.
// replayable says whether sending the same request a second time is harmless.
func (c *Client) shouldRetry(err error, attempt int, replayable bool) (time.Duration, bool) {
	if c.retry.MaxAttempts <= 1 || attempt >= c.retry.MaxAttempts {
		return 0, false
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) == false {
		return 0, false
	}

	// A 429 was refused before Jira executed anything, so repeating it is safe whatever the request
	// was. A 5xx says nothing about whether the request ran, so it is repeated only when a second
	// execution would be harmless or the policy accepts the risk.
	retryable := apiErr.StatusCode == http.StatusTooManyRequests
	if apiErr.StatusCode >= 500 && c.retry.RetryServerErrors == true {
		retryable = replayable == true || c.retry.RetryNonIdempotent == true
	}
	if retryable == false {
		return 0, false
	}

	maxRetryAfter := c.retry.MaxRetryAfter
	if maxRetryAfter <= 0 {
		maxRetryAfter = defaultMaxRetryAfter
	}
	if apiErr.RetryAfter > maxRetryAfter {
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

	// Doubled step by step and checked before each multiplication: a bit shift by the attempt number
	// overflows after a few dozen attempts and would come out as no wait at all.
	delay := base
	for doubling := 1; doubling < attempt && delay < maxDelay; doubling++ {
		if delay > maxDelay/2 {
			delay = maxDelay
			break
		}
		delay *= 2
	}
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

// parseRetryAfter reads the Retry-After header. Jira documents it in seconds; HTTP also allows a date,
// which is accepted so an intermediary answering in that form is still honoured.
func parseRetryAfter(header http.Header) time.Duration {
	raw := header.Get("Retry-After")
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return 0
}
