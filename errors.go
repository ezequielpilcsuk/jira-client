package jiraclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors for conditions callers routinely branch on.
var (
	// ErrNotFound is returned when an issue or resource does not exist.
	ErrNotFound = errors.New("jira resource not found")

	// ErrUnauthorized is returned for 401/403 — bad credentials, or a token without the scope.
	ErrUnauthorized = errors.New("jira request not authorized")

	// ErrRateLimited is returned on 429. Jira rate-limits aggressively; back off and retry.
	ErrRateLimited = errors.New("jira rate limit exceeded")

	// ErrInvalidArgument is returned for arguments rejected before any request is sent.
	ErrInvalidArgument = errors.New("invalid argument")
)

// APIError carries a non-2xx Jira response. Jira puts the actionable detail in the body — a 400 on a
// malformed ADF document names the offending node — so Messages is preserved verbatim.
type APIError struct {
	StatusCode int
	Operation  string
	Messages   []string
	Body       string
	// RetryAfter is the server's requested minimum wait, from the Retry-After header (429 only).
	RetryAfter time.Duration
	// RateLimitReason is the RateLimit-Reason header, naming which limit was exceeded — global,
	// tenant, burst, or per-issue-on-write.
	RateLimitReason string
}

// Error implements error.
func (e *APIError) Error() string {
	if len(e.Messages) > 0 {
		return fmt.Sprintf("jira %s: status %d: %s", e.Operation, e.StatusCode, strings.Join(e.Messages, "; "))
	}
	return fmt.Sprintf("jira %s: status %d: %s", e.Operation, e.StatusCode, e.Body)
}

// Unwrap maps the status onto a sentinel so callers can use errors.Is.
func (e *APIError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	case http.StatusTooManyRequests:
		return ErrRateLimited
	}
	return nil
}

// jiraErrorBody is Jira's standard error envelope.
type jiraErrorBody struct {
	ErrorMessages []string          `json:"errorMessages"`
	Errors        map[string]string `json:"errors"`
}

// newAPIError builds an APIError, extracting Jira's messages when the body is its usual envelope.
func newAPIError(statusCode int, operation string, body []byte) *APIError {
	apiErr := &APIError{
		StatusCode: statusCode,
		Operation:  operation,
		Body:       strings.TrimSpace(string(body)),
	}

	var parsed jiraErrorBody
	if json.Unmarshal(body, &parsed) == nil {
		apiErr.Messages = append(apiErr.Messages, parsed.ErrorMessages...)
		for field, message := range parsed.Errors {
			apiErr.Messages = append(apiErr.Messages, field+": "+message)
		}
	}
	return apiErr
}
