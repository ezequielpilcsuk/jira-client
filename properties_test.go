package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIssuePropertyKeys_ListsKeysSetOnTheIssue(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, `{"keys":[
			{"self":"https://x/rest/api/3/issue/ABC-1/properties/triage","key":"triage"},
			{"self":"https://x/rest/api/3/issue/ABC-1/properties/sync","key":"sync"}]}`)
	})

	keys, err := client.IssuePropertyKeys(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("property keys: %v", err)
	}
	if requestedPath != "/rest/api/3/issue/ABC-1/properties" {
		t.Errorf("path: %s", requestedPath)
	}
	if len(keys) != 2 || keys[0] != "triage" || keys[1] != "sync" {
		t.Errorf("keys: %v", keys)
	}
}

// An issue with no properties returns an empty list, not an error.
func TestIssuePropertyKeys_EmptyListIsNotAnError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"keys":[]}`)
	})

	keys, err := client.IssuePropertyKeys(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("property keys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("want no keys, got %v", keys)
	}
}

func TestIssueProperty_UnmarshalsTheValueIntoDest(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"key":"triage","value":{"runID":"run-7","attempts":2}}`)
	})

	var marker struct {
		RunID    string `json:"runID"`
		Attempts int    `json:"attempts"`
	}
	if err := client.GetIssueProperty(context.Background(), "ABC-1", "triage", &marker); err != nil {
		t.Fatalf("read property: %v", err)
	}
	if marker.RunID != "run-7" || marker.Attempts != 2 {
		t.Errorf("value not unmarshalled: %+v", marker)
	}
}

// The idempotency check a caller actually writes: never set means not yet processed.
func TestIssueProperty_UnsetPropertyIsErrNotFound(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errorMessages":["The issue property was not found."]}`)
	})

	var marker map[string]any
	err := client.GetIssueProperty(context.Background(), "ABC-1", "triage", &marker)
	if errors.Is(err, ErrNotFound) == false {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// An envelope carrying no value must leave dest alone rather than fail the decode.
func TestIssueProperty_MissingValueLeavesDestAtItsZeroValue(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"key":"triage"}`)
	})

	value := "untouched"
	if err := client.GetIssueProperty(context.Background(), "ABC-1", "triage", &value); err != nil {
		t.Fatalf("read property: %v", err)
	}
	if value != "untouched" {
		t.Errorf("dest was overwritten: %q", value)
	}
}

// Jira stores the bare value; wrapping it in an envelope would nest the payload one level too deep.
func TestSetIssueProperty_SendsTheBareValueNotAnEnvelope(t *testing.T) {
	var method, path, body string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.WriteHeader(http.StatusCreated)
	})

	err := client.SetIssueProperty(context.Background(), "ABC-1", "triage",
		map[string]any{"runID": "run-7"})
	if err != nil {
		t.Fatalf("set property: %v", err)
	}
	if method != http.MethodPut {
		t.Errorf("method: %s", method)
	}
	if path != "/rest/api/3/issue/ABC-1/properties/triage" {
		t.Errorf("path: %s", path)
	}
	if body != `{"runID":"run-7"}` {
		t.Errorf("body should be the bare value, got %s", body)
	}
}

func TestSetIssueProperty_AcceptsBothSuccessStatuses(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})
		if err := client.SetIssueProperty(context.Background(), "ABC-1", "triage", "done"); err != nil {
			t.Errorf("status %d should succeed, got %v", status, err)
		}
	}
}

func TestDeleteIssueProperty_SendsDelete(t *testing.T) {
	var method, path string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.DeleteIssueProperty(context.Background(), "ABC-1", "triage"); err != nil {
		t.Fatalf("delete property: %v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("method: %s", method)
	}
	if path != "/rest/api/3/issue/ABC-1/properties/triage" {
		t.Errorf("path: %s", path)
	}
}

// A property key is caller-chosen, so it can hold characters that would otherwise reshape the path.
func TestIssueProperty_EscapesBothPathSegments(t *testing.T) {
	var escapedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		escapedPath = r.URL.EscapedPath()
		_, _ = io.WriteString(w, `{"key":"a/b","value":1}`)
	})

	var value int
	if err := client.GetIssueProperty(context.Background(), "ABC 1", "a/b c", &value); err != nil {
		t.Fatalf("read property: %v", err)
	}
	if strings.Contains(escapedPath, "%2Fb") == false || strings.Contains(escapedPath, "ABC%201") == false {
		t.Errorf("path segments not escaped: %s", escapedPath)
	}
}

// Arguments Jira would certainly reject cost no request.
func TestIssueProperties_RejectImpossibleArgumentsBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	ctx := context.Background()
	dest := map[string]any{}

	cases := map[string]error{
		"empty issue key":     client.SetIssueProperty(ctx, "", "triage", 1),
		"empty property key":  client.SetIssueProperty(ctx, "ABC-1", "", 1),
		"empty key on read":   client.GetIssueProperty(ctx, "", "triage", &dest),
		"empty key on delete": client.DeleteIssueProperty(ctx, "ABC-1", ""),
		"long property key": client.SetIssueProperty(ctx, "ABC-1",
			strings.Repeat("k", PropertyKeyMaxChars+1), 1),
		"oversized value": client.SetIssueProperty(ctx, "ABC-1", "triage",
			strings.Repeat("v", PropertyValueMaxChars)),
		"unserialisable value": client.SetIssueProperty(ctx, "ABC-1", "triage", make(chan int)),
		"nil dest":             client.GetIssueProperty(ctx, "ABC-1", "triage", nil),
		"non-pointer dest":     client.GetIssueProperty(ctx, "ABC-1", "triage", dest),
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

// A key at exactly the limit is legal — the check must not be off by one.
func TestSetIssueProperty_AllowsAKeyAtExactlyTheLimit(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	key := strings.Repeat("k", PropertyKeyMaxChars)
	if err := client.SetIssueProperty(context.Background(), "ABC-1", key, 1); err != nil {
		t.Errorf("a key of exactly %d chars should be accepted, got %v", PropertyKeyMaxChars, err)
	}
}

// Jira states its limits in characters, so a multi-byte value must not be judged on its byte length.
func TestSetIssueProperty_MeasuresTheValueInCharactersNotBytes(t *testing.T) {
	var sent bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sent = true
		w.WriteHeader(http.StatusOK)
	})

	// Three bytes per rune, so this is over the limit in bytes but well under it in characters.
	value := strings.Repeat("é", PropertyValueMaxChars/2)
	if err := client.SetIssueProperty(context.Background(), "ABC-1", "triage", value); err != nil {
		t.Fatalf("set property: %v", err)
	}
	if sent == false {
		t.Error("a value under the character limit should have been sent")
	}
}

// Property writes are mutations, so a dry client must skip them while reads keep working.
func TestIssueProperties_DryRunSuppressesWritesButNotReads(t *testing.T) {
	var writes []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes = append(writes, r.Method+" "+r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"keys":[],"value":1}`)
	}, WithDryRun(true))

	ctx := context.Background()
	if _, err := client.IssuePropertyKeys(ctx, "ABC-1"); err != nil {
		t.Fatalf("read must still work: %v", err)
	}
	if err := client.SetIssueProperty(ctx, "ABC-1", "triage", 1); err != nil {
		t.Fatalf("SetIssueProperty should be a no-op in dry run, got %v", err)
	}
	if err := client.DeleteIssueProperty(ctx, "ABC-1", "triage"); err != nil {
		t.Fatalf("DeleteIssueProperty should be a no-op in dry run, got %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("dry run performed writes: %v", writes)
	}
}
