package jiraclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDocBuilder_AddHeading(t *testing.T) {
	t.Run("levels outside 1-6 are clamped to 3", func(t *testing.T) {
		for _, level := range []int{0, -1, 7, 99} {
			doc, err := NewDocBuilder().AddHeading(level, "Title").Build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if doc.Content[0].Attrs.Level != 3 {
				t.Errorf("level %d became %d, want 3", level, doc.Content[0].Attrs.Level)
			}
		}
	})

	t.Run("valid levels are kept", func(t *testing.T) {
		for level := 1; level <= 6; level++ {
			doc, err := NewDocBuilder().AddHeading(level, "Title").Build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if doc.Content[0].Attrs.Level != level {
				t.Errorf("level %d became %d", level, doc.Content[0].Attrs.Level)
			}
		}
	})

	// A blank heading would render as an empty text node, which Jira rejects outright.
	t.Run("blank text adds nothing", func(t *testing.T) {
		builder := NewDocBuilder().AddHeading(1, "   ")
		if len(builder.Nodes()) != 0 {
			t.Errorf("blank heading was added: %v", builder.Nodes())
		}
	})
}

func TestDocBuilder_AddText(t *testing.T) {
	t.Run("appends a paragraph", func(t *testing.T) {
		doc, err := NewDocBuilder().AddText("hello").Build()
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if doc.Content[0].Type != "paragraph" || doc.Content[0].Content[0].Text != "hello" {
			t.Errorf("unexpected node: %+v", doc.Content[0])
		}
	})

	t.Run("empty text adds nothing", func(t *testing.T) {
		if nodes := NewDocBuilder().AddText("").Nodes(); len(nodes) != 0 {
			t.Errorf("empty text was added: %v", nodes)
		}
	})

	// Unlike AddParagraphs, AddText does not split — the whole string is one paragraph.
	t.Run("newlines do not split", func(t *testing.T) {
		nodes := NewDocBuilder().AddText("one\ntwo").Nodes()
		if len(nodes) != 1 {
			t.Errorf("got %d paragraphs, want 1", len(nodes))
		}
	})
}

func TestDocBuilder_AddTable(t *testing.T) {
	t.Run("builds a header row plus one row per entry", func(t *testing.T) {
		builder := NewDocBuilder()
		if err := builder.AddTable([]string{"Key", "Status"}, [][]string{
			{"ABC-1", "Done"}, {"ABC-2", "To Do"},
		}); err != nil {
			t.Fatalf("add table: %v", err)
		}

		doc, err := builder.Build()
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		table := doc.Content[0]
		if table.Type != "table" {
			t.Fatalf("got %q, want a table", table.Type)
		}
		if len(table.Content) != 3 {
			t.Fatalf("got %d rows, want 3 (header + 2)", len(table.Content))
		}
		if table.Content[0].Content[0].Type != "tableHeader" {
			t.Errorf("first row should be headers: %+v", table.Content[0].Content[0])
		}
	})

	// Jira renders a ragged table unpredictably rather than rejecting it, so the builder refuses.
	t.Run("a ragged row is refused", func(t *testing.T) {
		err := NewDocBuilder().AddTable([]string{"A", "B"}, [][]string{{"only one"}})
		if errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("want ErrInvalidArgument, got %v", err)
		}
		if err != nil && strings.Contains(err.Error(), "row 0") == false {
			t.Errorf("error should name the offending row: %v", err)
		}
	})

	t.Run("no headers is refused", func(t *testing.T) {
		if err := NewDocBuilder().AddTable(nil, nil); errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("want ErrInvalidArgument, got %v", err)
		}
	})
}

func TestDocBuilder_AddLinkedTable(t *testing.T) {
	builder := NewDocBuilder()
	if err := builder.AddLinkedTable([]string{"Issue"}, [][]Cell{
		{{Text: "ABC-1", Href: "https://example.atlassian.net/browse/ABC-1"}},
		{{Text: ""}},
	}); err != nil {
		t.Fatalf("add linked table: %v", err)
	}

	doc, err := builder.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), `"type":"link"`) == false {
		t.Errorf("cell link mark missing:\n%s", encoded)
	}

	// An empty text node is invalid ADF and 400s the whole request, so an empty cell must render as
	// an empty paragraph with no text node at all.
	if strings.Contains(string(encoded), `"text":""`) == true {
		t.Errorf("an empty cell emitted an empty text node:\n%s", encoded)
	}
}

func TestDocBuilder_NodesAndLen(t *testing.T) {
	builder := NewDocBuilder().AddText("hello").AddText("world")

	if len(builder.Nodes()) != 2 {
		t.Errorf("got %d nodes, want 2", len(builder.Nodes()))
	}
	if builder.Len() == 0 {
		t.Error("Len should report the rendered text length")
	}

	// Len is what a caller checks against CommentMaxChars, so it must grow with content.
	shorter := NewDocBuilder().AddText("hello").Len()
	if builder.Len() <= shorter {
		t.Errorf("Len did not grow with content: %d vs %d", builder.Len(), shorter)
	}
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

func TestDefaultRetryPolicy(t *testing.T) {
	policy := DefaultRetryPolicy()

	if policy.MaxAttempts <= 1 {
		t.Errorf("MaxAttempts = %d, which disables retrying", policy.MaxAttempts)
	}
	if policy.BaseDelay <= 0 || policy.MaxDelay < policy.BaseDelay {
		t.Errorf("delays are inconsistent: base %v, max %v", policy.BaseDelay, policy.MaxDelay)
	}
	if policy.RetryServerErrors == false {
		t.Error("the default should retry the 5xx responses Jira returns transiently")
	}
}

func TestAPIError_Error(t *testing.T) {
	t.Run("prefers Jira's messages", func(t *testing.T) {
		err := &APIError{
			StatusCode: 400, Operation: "POST /issue",
			Messages: []string{"summary: is required", "bad node"},
			Body:     `{"errorMessages":["bad node"]}`,
		}
		got := err.Error()
		for _, want := range []string{"400", "POST /issue", "summary: is required", "bad node"} {
			if strings.Contains(got, want) == false {
				t.Errorf("error missing %q: %s", want, got)
			}
		}
	})

	// Jira puts the actionable detail in the body, so an unparseable body must still surface.
	t.Run("falls back to the raw body", func(t *testing.T) {
		err := &APIError{StatusCode: 500, Operation: "GET /issue", Body: "upstream exploded"}
		if strings.Contains(err.Error(), "upstream exploded") == false {
			t.Errorf("raw body not surfaced: %s", err.Error())
		}
	})
}

func TestIssue_IsAssigned(t *testing.T) {
	if (Issue{AssigneeID: "acct-1"}).IsAssigned() == false {
		t.Error("an issue with an assignee id should report as assigned")
	}
	if (Issue{}).IsAssigned() == true {
		t.Error("an issue with no assignee id should report as unassigned")
	}
}
