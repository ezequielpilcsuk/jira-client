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
	"sync"
	"time"
)

const (
	defaultTimeout    = 30 * time.Second
	defaultPageSize   = 100
	contentTypeJSON   = "application/json"
	headerContentType = "Content-Type"
	headerAccept      = "Accept"

	// apiBase is the REST v3 prefix, chosen because v3 is ADF-native. Atlassian maintains v2 alongside
	// it with the same operations, and /rest/api/latest resolves to v2 semantics — so it is not a safe
	// alias for this.
	apiBase = "/rest/api/3"

	// prioritySearchPageSize is the page size for /priority/search, whose own default is 50.
	prioritySearchPageSize = 100
)

// DryRunID is the placeholder identifier that creating calls return on a dry-run client. Nothing was
// created, so feeding it back into a later mutation is itself a no-op. Compare against this rather
// than the literal, which is what makes a dry plan inspectable.
const DryRunID = "DRY-RUN"

// Logger is the minimal logging surface the client needs. It matches the shape of most structured
// loggers closely enough to adapt in a line, and keeps this library free of a logging dependency.
type Logger interface {
	Printf(format string, args ...any)
}

// Client talks to one Jira site.
type Client struct {
	baseURL string
	// siteURL is where people open the site, when that is not baseURL. See WithSiteURL.
	siteURL    string
	email      string
	apiToken   string
	httpClient *http.Client
	dryRun     bool
	pageSize   int
	logger     Logger
	retry      RetryPolicy
	// deprecations remembers which deprecation notices have been logged, so each is reported once.
	deprecations sync.Map
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

// WithTimeout sets the request timeout. Options apply in the order given, so pass it after
// WithHTTPClient when using both.
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		if timeout > 0 {
			// Set on a copy: the client in place may be one the caller supplied and shares elsewhere.
			withTimeout := *c.httpClient
			withTimeout.Timeout = timeout
			c.httpClient = &withTimeout
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

// WithSiteURL sets the address people open the site at, e.g. "https://your-site.atlassian.net", which
// IssueURL and "[issue:KEY]" links are built on.
//
// It is needed only when baseURL is not that address. That is the case for scoped API tokens, which
// work solely through Atlassian's gateway ("https://api.atlassian.com/ex/jira/{cloudId}"): the
// gateway serves the API and not the pages, so a link built on it opens nothing.
func WithSiteURL(siteURL string) ClientOption {
	return func(c *Client) { c.siteURL = strings.TrimRight(siteURL, "/") }
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

// request describes one HTTP call. Everything except Method and Path is optional, and the zero value
// produces the JSON request that almost every endpoint wants.
type request struct {
	Method string
	Path   string
	// Payload is JSON-marshalled. Mutually exclusive with Body.
	Payload any
	// Body is sent verbatim, for endpoints that do not take JSON — attachment upload is multipart.
	// It is buffered so a retry can replay it.
	Body []byte
	// ContentType overrides application/json. Required whenever Body is set.
	ContentType string
	// Accept overrides application/json, e.g. for a binary download.
	Accept string
	// Headers are set on the request, after the defaults above.
	Headers map[string]string
	// NoRedirect returns the 3xx response instead of following it. Jira answers attachment downloads
	// with a 303 to a media host, and Go strips Authorization across hosts, so following it in-client
	// would fail auth — the caller needs the Location instead.
	NoRedirect bool
	// Replayable marks a POST that is safe to send twice: a read Jira models as a POST, or a write that
	// sets a state rather than adding to it. Every other POST creates something each time it runs, so
	// a 5xx on one is not retried unless the policy opts in. Other methods are replayable by nature
	// and ignore this.
	Replayable bool
}

// do performs a request, retrying rate-limited and transient failures per the retry policy.
func (c *Client) do(ctx context.Context, method, path string, payload any) ([]byte, error) {
	body, _, err := c.doRequest(ctx, request{Method: method, Path: path, Payload: payload})
	return body, err
}

// doRequest is do with full control over the request, returning the response headers alongside the
// body. Reach for it only when an endpoint departs from the JSON-in/JSON-out norm.
func (c *Client) doRequest(ctx context.Context, req request) ([]byte, http.Header, error) {
	if req.Payload != nil && req.Body != nil {
		return nil, nil, fmt.Errorf("%w: a request carries either Payload or Body, not both", ErrInvalidArgument)
	}

	encoded := req.Body
	if req.Payload != nil {
		var err error
		if encoded, err = json.Marshal(req.Payload); err != nil {
			return nil, nil, fmt.Errorf("marshal %s %s: %w", req.Method, req.Path, err)
		}
	}

	replayable := req.Method != http.MethodPost || req.Replayable == true
	for attempt := 1; ; attempt++ {
		body, header, err := c.attempt(ctx, req, encoded)
		if err == nil {
			return body, header, nil
		}

		delay, retry := c.shouldRetry(err, attempt, replayable)
		if retry == false {
			return nil, nil, err
		}
		// A wait the context cannot outlive would end in a deadline error that hides why the call
		// failed, so the failure itself is returned instead.
		if deadline, ok := ctx.Deadline(); ok == true && time.Until(deadline) < delay {
			return nil, nil, err
		}
		c.logf("jira %s %s failed (attempt %d), retrying in %s: %v", req.Method, req.Path, attempt, delay, err)
		if sleepErr := sleep(ctx, delay); sleepErr != nil {
			return nil, nil, fmt.Errorf("%w while waiting to retry: %w", sleepErr, err)
		}
	}
}

// postReplayable is do for a POST that is safe to send twice. See request.Replayable.
func (c *Client) postReplayable(ctx context.Context, path string, payload any) ([]byte, error) {
	body, _, err := c.doRequest(ctx, request{Method: "POST", Path: path, Payload: payload, Replayable: true})
	return body, err
}

// attempt performs one request. A non-2xx response becomes an *APIError carrying the status and
// Jira's own message, which is where the useful detail lives (a 400 from a bad ADF document names
// the node it rejected).
func (c *Client) attempt(ctx context.Context, spec request, payload []byte) ([]byte, http.Header, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, spec.Method, c.baseURL+spec.Path, body)
	if err != nil {
		return nil, nil, fmt.Errorf("build request %s %s: %w", spec.Method, spec.Path, err)
	}

	accept := spec.Accept
	if accept == "" {
		accept = contentTypeJSON
	}
	contentType := spec.ContentType
	if contentType == "" {
		contentType = contentTypeJSON
	}
	req.Header.Set(headerAccept, accept)
	req.Header.Set(headerContentType, contentType)
	for name, value := range spec.Headers {
		req.Header.Set(name, value)
	}
	req.SetBasicAuth(c.email, c.apiToken)

	httpClient := c.httpClient
	if spec.NoRedirect == true {
		// A shallow copy: the shared client's CheckRedirect must not be mutated, and callers may have
		// supplied their own client via WithHTTPClient.
		noFollow := *c.httpClient
		noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		httpClient = &noFollow
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", spec.Method, spec.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	c.noteDeprecation(spec, resp.Header)

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response %s %s: %w", spec.Method, spec.Path, err)
	}

	// A 3xx is a success for a caller that asked not to follow it; the Location is the answer.
	redirected := spec.NoRedirect == true && resp.StatusCode >= 300 && resp.StatusCode <= 399
	if redirected == false && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		apiErr := newAPIError(resp.StatusCode, spec.Method+" "+spec.Path, responseBody)
		apiErr.RetryAfter = parseRetryAfter(resp.Header)
		apiErr.RateLimitReason = resp.Header.Get("RateLimit-Reason")
		apiErr.RateLimitReset = parseRateLimitReset(resp.Header)
		apiErr.NearLimit = resp.Header.Get("X-RateLimit-NearLimit") == "true"
		return nil, resp.Header, apiErr
	}
	return responseBody, resp.Header, nil
}

// noteDeprecation logs, once per notice, that Jira has marked the endpoint just called for removal.
//
// Atlassian sets Deprecation and Sunset headers on a deprecated endpoint and gives at least six
// months' notice before removing one, so this line in a service's own log is the earliest warning it
// gets that a call it depends on is going away. Without a logger there is nowhere to say it.
func (c *Client) noteDeprecation(spec request, header http.Header) {
	deprecation, sunset := header.Get("Deprecation"), header.Get("Sunset")
	if c.logger == nil || (deprecation == "" && sunset == "") {
		return
	}
	link := header.Get("Link")

	// Keyed on the notice rather than the path: a path carries the issue key, and a bulk run through
	// a deprecated endpoint would otherwise log once per issue.
	notice := spec.Method + "|" + deprecation + "|" + sunset + "|" + link
	if _, logged := c.deprecations.LoadOrStore(notice, true); logged == true {
		return
	}
	path, _, _ := strings.Cut(spec.Path, "?")
	c.logf("jira: %s %s is deprecated and scheduled for removal (Deprecation: %q, Sunset: %q, Link: %q)",
		spec.Method, path, deprecation, sunset, link)
}

// buildPath joins the API base with a path and query values.
func buildPath(path string, query url.Values) string {
	full := apiBase + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	return full
}
