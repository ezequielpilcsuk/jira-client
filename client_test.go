package jiraclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestClient points a client at an httptest server.
func newTestClient(t *testing.T, handler http.HandlerFunc, opts ...ClientOption) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient(server.URL, "bot@example.com", "token", opts...)
}

// decodeBody reads a request's JSON body into a generic map.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

// A dry client must read normally and write nothing.
func TestDryRun_SuppressesEveryMutationButNotReads(t *testing.T) {
	var writes []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes = append(writes, r.Method+" "+r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"issues":[],"isLast":true,"transitions":[]}`)
	}, WithDryRun(true))

	ctx := context.Background()
	if _, err := client.SearchIssues(ctx, "project = ABC", nil); err != nil {
		t.Fatalf("read must still work: %v", err)
	}

	mutations := map[string]error{
		"SetAssignee":   client.SetAssignee(ctx, "ABC-1", "acc"),
		"AddLabel":      client.AddLabel(ctx, "ABC-1", "x"),
		"RemoveLabel":   client.RemoveLabel(ctx, "ABC-1", "x"),
		"UpdateSummary": client.UpdateSummary(ctx, "ABC-1", "new"),
		"AddComment":    errOf(func() (Comment, error) { return client.AddTextComment(ctx, "ABC-1", "hi") }),
		"Transition":    client.Transition(ctx, "ABC-1", "71"),
		"LinkIssues":    client.LinkIssues(ctx, LinkDuplicate, "ABC-1", "ABC-2"),
		"CustomField":   client.UpdateCustomField(ctx, "ABC-1", "customfield_1", 3),
	}
	for name, err := range mutations {
		if err != nil {
			t.Fatalf("%s should be a no-op in dry run, got %v", name, err)
		}
	}

	key, err := client.CreateIssue(ctx, CreateIssueInput{ProjectKey: "ABC", IssueType: "Bug", Summary: "s"})
	if err != nil || key != "DRY-RUN" {
		t.Fatalf("CreateIssue dry run: key=%q err=%v", key, err)
	}
	if len(writes) != 0 {
		t.Fatalf("dry run performed writes: %v", writes)
	}
}

// Arguments Jira would certainly reject are refused locally, without spending a request.
func TestLocalValidation_RejectsBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	ctx := context.Background()

	cases := map[string]error{
		"empty jql":       mustErr(func() error { _, err := client.SearchIssues(ctx, "  ", nil); return err }),
		"long summary":    client.UpdateSummary(ctx, "ABC-1", strings.Repeat("x", SummaryMaxChars+1)),
		"empty summary":   client.UpdateSummary(ctx, "ABC-1", "  "),
		"label w/ space":  client.AddLabel(ctx, "ABC-1", "two words"),
		"self link":       client.LinkIssues(ctx, LinkDuplicate, "ABC-1", "ABC-1"),
		"empty link type": client.LinkIssues(ctx, "", "ABC-1", "ABC-2"),
		"empty comment":   errOf(func() (Comment, error) { return client.AddTextComment(ctx, "ABC-1", "   ") }),
	}
	for name, err := range cases {
		if errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("%s: want ErrInvalidArgument, got %v", name, err)
		}
	}
	if called == true {
		t.Fatal("no request should have been sent")
	}
}

func mustErr(fn func() error) error { return fn() }

// errOf discards a mutator's returned value, keeping the error tables below one column wide.
func errOf(call func() (Comment, error)) error {
	_, err := call()
	return err
}

func TestClientOptions(t *testing.T) {
	t.Run("WithHTTPClient replaces the client and ignores nil", func(t *testing.T) {
		custom := &http.Client{Timeout: 3 * time.Second}
		client := NewClient("https://example.atlassian.net", "e", "t", WithHTTPClient(custom))
		if client.httpClient != custom {
			t.Error("custom http client was not applied")
		}

		client = NewClient("https://example.atlassian.net", "e", "t", WithHTTPClient(nil))
		if client.httpClient == nil {
			t.Error("a nil http client must not clear the default")
		}
	})

	t.Run("WithTimeout ignores non-positive values", func(t *testing.T) {
		client := NewClient("https://example.atlassian.net", "e", "t", WithTimeout(7*time.Second))
		if client.httpClient.Timeout != 7*time.Second {
			t.Errorf("timeout = %v", client.httpClient.Timeout)
		}

		client = NewClient("https://example.atlassian.net", "e", "t", WithTimeout(0))
		if client.httpClient.Timeout <= 0 {
			t.Error("a zero timeout must not clear the default")
		}
	})

	t.Run("WithPageSize ignores non-positive values", func(t *testing.T) {
		client := NewClient("https://example.atlassian.net", "e", "t", WithPageSize(25))
		if client.pageSize != 25 {
			t.Errorf("pageSize = %d", client.pageSize)
		}

		client = NewClient("https://example.atlassian.net", "e", "t", WithPageSize(-1))
		if client.pageSize <= 0 {
			t.Errorf("a negative page size must not be applied: %d", client.pageSize)
		}
	})

	t.Run("WithLogger and IsDryRun", func(t *testing.T) {
		var lines []string
		logger := loggerFunc(func(format string, args ...any) { lines = append(lines, format) })

		client := NewClient("https://example.atlassian.net", "e", "t",
			WithLogger(logger), WithDryRun(true))

		if client.IsDryRun() == false {
			t.Error("IsDryRun should report the configured value")
		}
		if client.logger == nil {
			t.Error("logger was not attached")
		}

		// A suppressed mutation is the one thing dry mode must always say out loud.
		client.logf("suppressed %s", "create")
		if len(lines) != 1 {
			t.Errorf("logger was not used: %v", lines)
		}
	})

	t.Run("a client without a logger is silent, not panicking", func(t *testing.T) {
		client := NewClient("https://example.atlassian.net", "e", "t", WithDryRun(true))
		client.logf("no logger attached")
	})
}

// loggerFunc adapts a function to the Logger interface.
type loggerFunc func(format string, args ...any)

func (f loggerFunc) Printf(format string, args ...any) { f(format, args...) }

// logLines collects what a client logs. It is safe for the concurrent requests a test may make.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) matching(fragment string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var matches []string
	for _, line := range l.lines {
		if strings.Contains(line, fragment) == true {
			matches = append(matches, line)
		}
	}
	return matches
}

// Atlassian marks an endpoint it is going to remove with these headers months ahead. A service that
// logs them hears about the removal from its own logs rather than from an outage.
func TestClient_WarnsOnceWhenAnEndpointIsDeprecated(t *testing.T) {
	logs := &logLines{}
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Deprecation", "@1767225600")
		w.Header().Set("Sunset", "Wed, 01 Jul 2026 00:00:00 GMT")
		w.Header().Set("Link", `<https://developer.atlassian.com/changelog/#CHANGE-1>; rel="deprecation"`)
		_, _ = io.WriteString(w, `{"key":"ABC-1","fields":{"summary":"one"}}`)
	}, WithLogger(logs))

	for _, key := range []string{"ABC-1", "ABC-2", "ABC-3"} {
		if _, err := client.GetIssue(context.Background(), key, nil); err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
	}

	warnings := logs.matching("deprecated")
	if len(warnings) != 1 {
		t.Fatalf("want one warning for three calls to the same endpoint, got %d: %v", len(warnings), warnings)
	}
	for _, fragment := range []string{"GET", "Wed, 01 Jul 2026 00:00:00 GMT", "CHANGE-1"} {
		if strings.Contains(warnings[0], fragment) == false {
			t.Errorf("warning lacks %q: %s", fragment, warnings[0])
		}
	}
}

// The warning matters most on the call that has already started failing.
func TestClient_WarnsAboutDeprecationOnAnErrorResponseToo(t *testing.T) {
	logs := &logLines{}
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Sunset", "Wed, 01 Jul 2026 00:00:00 GMT")
		w.WriteHeader(http.StatusGone)
	}, WithLogger(logs))

	_, _ = client.GetIssue(context.Background(), "ABC-1", nil)

	if len(logs.matching("deprecated")) != 1 {
		t.Fatalf("want a deprecation warning, got %v", logs.lines)
	}
}

func TestClient_SaysNothingWhenNothingIsDeprecated(t *testing.T) {
	logs := &logLines{}
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"key":"ABC-1","fields":{"summary":"one"}}`)
	}, WithLogger(logs))

	if _, err := client.GetIssue(context.Background(), "ABC-1", nil); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(logs.lines) != 0 {
		t.Fatalf("want no log lines, got %v", logs.lines)
	}
}

// A caller may share one http.Client across several things; configuring this one must not reach it.
func TestWithTimeout_LeavesACallerSuppliedHTTPClientAlone(t *testing.T) {
	shared := &http.Client{}

	client := NewClient("http://example.invalid", "e", "t", WithHTTPClient(shared), WithTimeout(5*time.Second))

	if shared.Timeout != 0 {
		t.Fatalf("the caller's client was given a %s timeout", shared.Timeout)
	}
	if client.httpClient.Timeout != 5*time.Second {
		t.Fatalf("the client's own timeout = %s, want 5s", client.httpClient.Timeout)
	}
}

// Scoped API tokens only work through Atlassian's gateway, which serves the API but not the pages
// people open. Links have to point at the site.
func TestWithSiteURL_IsWhereIssueLinksPoint(t *testing.T) {
	client := NewClient("https://api.atlassian.com/ex/jira/cloud-id/", "e", "t",
		WithSiteURL("https://acme.atlassian.net/"))

	if got := client.IssueURL("ABC-1"); got != "https://acme.atlassian.net/browse/ABC-1" {
		t.Errorf("issue url = %q", got)
	}
	doc, err := client.NewDocBuilder().AddText("see [issue:ABC-1]").Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	link := doc.Content[0].Content[1]
	if link.Marks[0].Attrs.Href != "https://acme.atlassian.net/browse/ABC-1" {
		t.Errorf("link = %+v", link.Marks[0].Attrs)
	}
}
