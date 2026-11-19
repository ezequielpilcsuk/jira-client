package jiraclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

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

// Jira's 255 limit on a summary counts characters. Counting bytes refuses a title of accented or
// non-Latin text at a fraction of the length Jira would accept.
func TestSummaryLimit_CountsCharactersNotBytes(t *testing.T) {
	dry := NewClient("http://example.invalid", "e", "t", WithDryRun(true))
	accented := strings.Repeat("é", 200)

	if _, err := dry.CreateIssue(context.Background(),
		CreateIssueInput{ProjectKey: "ABC", IssueType: "Task", Summary: accented}); err != nil {
		t.Errorf("a 200-character summary was refused on create: %v", err)
	}
	if err := dry.UpdateSummary(context.Background(), "ABC-1", accented); err != nil {
		t.Errorf("a 200-character summary was refused on update: %v", err)
	}

	tooLong := strings.Repeat("é", SummaryMaxChars+1)
	if err := dry.UpdateSummary(context.Background(), "ABC-1", tooLong); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("a %d-character summary must be refused, got %v", SummaryMaxChars+1, err)
	}
}

func TestDocBuilder_LenCountsCharactersNotBytes(t *testing.T) {
	if got := NewDocBuilder().AddText("préço €").Len(); got != 7 {
		t.Fatalf("len = %d, want 7", got)
	}
}

// DryRunID stands in for an issue that was never created, so nothing may be asked of Jira about it.
// A transition by name starts with a read, and that read must be skipped along with the write.
func TestTransitionByName_OnTheDryRunPlaceholderAsksJiraNothing(t *testing.T) {
	requests := 0
	dry := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
	}, WithDryRun(true))

	key, err := dry.CreateIssue(context.Background(),
		CreateIssueInput{ProjectKey: "ABC", IssueType: "Task", Summary: "s"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := dry.TransitionByName(context.Background(), key, "Done"); err != nil {
		t.Errorf("transition by name: %v", err)
	}
	doc, _ := TextDoc("closing")
	if err := dry.TransitionByNameWithComment(context.Background(), key, "Done", doc); err != nil {
		t.Errorf("transition by name with comment: %v", err)
	}
	if requests != 0 {
		t.Fatalf("made %d requests about an issue that does not exist", requests)
	}
}
