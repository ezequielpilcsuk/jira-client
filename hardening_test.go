package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

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
