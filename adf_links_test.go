package jiraclient

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func TestDocBuilder_ExpandsIssueTokenIntoALink(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})

	doc, err := client.NewDocBuilder().AddParagraphs("tracked by [issue:CBS-117985] now").Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	nodes := doc.Content[0].Content
	if len(nodes) != 3 {
		t.Fatalf("got %d inline nodes, want 3", len(nodes))
	}
	if nodes[1].Text != "CBS-117985" {
		t.Errorf("link text: got %q, want the issue key", nodes[1].Text)
	}
	if len(nodes[1].Marks) != 1 || nodes[1].Marks[0].Type != adfLink {
		t.Fatalf("issue token must carry a link mark, got %+v", nodes[1].Marks)
	}
	want := client.IssueURL("CBS-117985")
	if nodes[1].Marks[0].Attrs.Href != want {
		t.Errorf("href: got %q, want %q", nodes[1].Marks[0].Attrs.Href, want)
	}
}

// A mention and an issue link on one line must expand left to right.
func TestDocBuilder_MixedTokensStayInOrder(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})

	doc, err := client.NewDocBuilder().AddParagraphs("[@acct-1] see [issue:ABC-10] thanks").Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	nodes := doc.Content[0].Content
	if len(nodes) != 4 {
		t.Fatalf("got %d inline nodes, want 4", len(nodes))
	}
	if nodes[0].Type != adfMention || nodes[0].Attrs.ID != "acct-1" {
		t.Errorf("first node must be the mention, got %+v", nodes[0])
	}
	if nodes[2].Text != "ABC-10" || len(nodes[2].Marks) != 1 {
		t.Errorf("third node must be the linked issue key, got %+v", nodes[2])
	}
}

// A builder made without a client has no site to link into, so the key renders as plain text rather
// than as a broken relative link.
func TestDocBuilder_IssueTokenWithoutABaseURLIsPlainText(t *testing.T) {
	doc, err := NewDocBuilder().AddParagraphs("see [issue:ABC-10]").Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	nodes := doc.Content[0].Content
	last := nodes[len(nodes)-1]
	if last.Text != "ABC-10" {
		t.Errorf("got %q, want the bare key", last.Text)
	}
	if len(last.Marks) != 0 {
		t.Errorf("no base URL means no link mark, got %+v", last.Marks)
	}
}

// Comments quote issue summaries back at people, and CBS summaries carry a bracketed brand list.
// Nothing in that shape may be mistaken for a token.
func TestDocBuilder_BracketedProseIsNotAToken(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})
	line := "Title updated to: NS [SureFire, Shield Sights] volitiontx.com"

	doc, err := client.NewDocBuilder().AddParagraphs(line).Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	nodes := doc.Content[0].Content
	if len(nodes) != 1 || nodes[0].Text != line {
		t.Fatalf("prose must pass through untouched, got %+v", nodes)
	}
}

// The whole document has to survive a round trip to Jira, blank separator lines included.
func TestDocBuilder_BlankLinesStayValidParagraphs(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})

	doc, err := client.NewDocBuilder().AddParagraphs("first\n\n[issue:ABC-1]").Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(doc.Content) != 3 {
		t.Fatalf("got %d paragraphs, want 3", len(doc.Content))
	}
	if doc.Content[1].Content != nil {
		t.Errorf("a blank line must be an empty paragraph, got %+v", doc.Content[1].Content)
	}
}

// End to end: the posted payload carries the mention and the link, not the raw tokens.
func TestAddComment_PostsExpandedTokens(t *testing.T) {
	var posted string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		posted = string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"1"}`)
	})

	doc, err := client.NewDocBuilder().AddParagraphs("[@acct-1] duplicate of [issue:ABC-10]").Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := client.AddComment(context.Background(), "ABC-11", doc); err != nil {
		t.Fatalf("add comment: %v", err)
	}

	for _, fragment := range []string{`"type":"mention"`, `"id":"acct-1"`, `"type":"link"`, `/browse/ABC-10`} {
		if contains(posted, fragment) == false {
			t.Errorf("payload missing %s\ngot: %s", fragment, posted)
		}
	}
	if contains(posted, "[issue:") || contains(posted, "[@") {
		t.Errorf("raw tokens leaked into the payload: %s", posted)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
