// Package jiraclient is a Go client for the Jira Cloud REST v3 API, covering the operations an
// automation typically performs: searching with JQL, reading issues, and mutating them (labels,
// comments, assignee, summary, status transitions, issue links).
//
// It exists so services stop each carrying their own partial Jira integration. Two properties are
// deliberately first-class because ad-hoc integrations tend to reinvent both:
//
//   - DryRun is a client option, not a global. A dry client logs and skips every mutation while
//     reads still work, so a caller can produce a full plan against production without writing.
//   - Reads are nil-safe. Jira omits absent fields entirely, and code that dereferences
//     description/reporter/assignee unconditionally panics on real tickets.
package jiraclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout    = 30 * time.Second
	defaultPageSize   = 100
	contentTypeJSON   = "application/json"
	headerContentType = "Content-Type"
	headerAccept      = "Accept"

	// apiBase is the REST v3 prefix. v2 is deprecated by Atlassian; v3 is ADF-native.
	apiBase = "/rest/api/3"
)

// Logger is the minimal logging surface the client needs. It matches the shape of most structured
// loggers closely enough to adapt in a line, and keeps this library free of a logging dependency.
type Logger interface {
	Printf(format string, args ...any)
}

// Client talks to one Jira site.
type Client struct {
	baseURL    string
	email      string
	apiToken   string
	httpClient *http.Client
	dryRun     bool
	pageSize   int
	logger     Logger
	retry      RetryPolicy
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithHTTPClient supplies a custom HTTP client (timeouts, transport, instrumentation).
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

// WithTimeout sets the request timeout on the default HTTP client.
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		if timeout > 0 {
			c.httpClient.Timeout = timeout
		}
	}
}

// WithDryRun makes every mutating call a logged no-op while leaving reads live. Use it to build and
// review a plan against production before allowing it to write.
func WithDryRun(dryRun bool) ClientOption {
	return func(c *Client) { c.dryRun = dryRun }
}

// WithPageSize overrides the JQL search page size (default 100, Jira's maximum).
func WithPageSize(size int) ClientOption {
	return func(c *Client) {
		if size > 0 {
			c.pageSize = size
		}
	}
}

// WithLogger attaches a logger. Without one the client is silent, including in dry-run mode.
func WithLogger(logger Logger) ClientOption {
	return func(c *Client) { c.logger = logger }
}

// NewClient builds a client for a Jira site, e.g.
// NewClient("https://your-site.atlassian.net", "bot@example.com", token).
//
// Authentication is Jira Cloud basic auth: the account email plus an API token.
func NewClient(baseURL, email, apiToken string, opts ...ClientOption) *Client {
	client := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		email:      email,
		apiToken:   apiToken,
		httpClient: &http.Client{Timeout: defaultTimeout},
		pageSize:   defaultPageSize,
	}
	for _, opt := range opts {
		opt(client)
	}
	return client
}

// IsDryRun reports whether mutations are suppressed.
func (c *Client) IsDryRun() bool { return c.dryRun }

// logf emits a message when a logger is configured.
func (c *Client) logf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Printf(format, args...)
	}
}

// skipMutation reports whether a write should be suppressed, logging it when so. Every mutating
// method calls this first, which is what makes dry-run total rather than best-effort.
func (c *Client) skipMutation(operation string, args ...any) bool {
	if c.dryRun == false {
		return false
	}
	c.logf("DRY RUN: %s %v", operation, args)
	return true
}

// do performs a request, retrying rate-limited and transient failures per the retry policy.
func (c *Client) do(ctx context.Context, method, path string, payload any) ([]byte, error) {
	var encoded []byte
	if payload != nil {
		var err error
		if encoded, err = json.Marshal(payload); err != nil {
			return nil, fmt.Errorf("marshal %s %s: %w", method, path, err)
		}
	}

	for attempt := 1; ; attempt++ {
		body, err := c.attempt(ctx, method, path, encoded)
		if err == nil {
			return body, nil
		}

		delay, retry := c.shouldRetry(err, attempt)
		if retry == false {
			return nil, err
		}
		c.logf("jira %s %s failed (attempt %d), retrying in %s: %v", method, path, attempt, delay, err)
		if sleepErr := sleep(ctx, delay); sleepErr != nil {
			return nil, sleepErr
		}
	}
}

// attempt performs one request. A non-2xx response becomes an *APIError carrying the status and
// Jira's own message, which is where the useful detail lives (a 400 from a bad ADF document names
// the node it rejected).
func (c *Client) attempt(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request %s %s: %w", method, path, err)
	}
	req.Header.Set(headerAccept, contentTypeJSON)
	req.Header.Set(headerContentType, contentTypeJSON)
	req.SetBasicAuth(c.email, c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := newAPIError(resp.StatusCode, method+" "+path, responseBody)
		apiErr.RetryAfter = parseRetryAfter(resp.Header)
		apiErr.RateLimitReason = resp.Header.Get("RateLimit-Reason")
		return nil, apiErr
	}
	return responseBody, nil
}

// buildPath joins the API base with a path and query values.
func buildPath(path string, query url.Values) string {
	full := apiBase + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	return full
}
