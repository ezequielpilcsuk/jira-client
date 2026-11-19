package jiraclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
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

// Len is checked against Jira's limit on a comment's text, which does not count link addresses.
func TestDocBuilder_LenDoesNotCountLinkTargets(t *testing.T) {
	builder := NewDocBuilder()
	if err := builder.AddLinkedTable([]string{"Link"}, [][]Cell{{{Text: "open", Href: "https://example.com/widget"}}}); err != nil {
		t.Fatalf("table: %v", err)
	}

	if got, want := builder.Len(), len("Link\nopen"); got != want {
		t.Fatalf("len = %d, want %d", got, want)
	}
}

// Only something shaped like an account id is a mention. Text quoted from a crawled page can hold
// "[@]" or "[@ $19.99]", and a mention node built from either is invalid ADF that Jira rejects along
// with the whole comment.
func TestDocBuilder_OnlyAnAccountIDShapedTokenIsAMention(t *testing.T) {
	line := "price [@] was [@ $19.99] per [@unit price] ok"
	doc, err := NewDocBuilder().AddText(line).Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	nodes := doc.Content[0].Content
	if len(nodes) != 1 || nodes[0].Type != adfText || nodes[0].Text != line {
		t.Fatalf("the line must stay one text node, got %+v", nodes)
	}
}

func TestDocBuilder_BothAccountIDFormatsAreMentions(t *testing.T) {
	doc, err := NewDocBuilder().
		AddText("[@5b10ac8d82e05b22cc7d4ef5] and [@557058:f58131cb-b67d-43c7-b30d-6b58d40bd077]").Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	nodes := doc.Content[0].Content
	if len(nodes) != 3 || nodes[0].Type != adfMention || nodes[2].Type != adfMention {
		t.Fatalf("want mention, text, mention, got %+v", nodes)
	}
	if nodes[2].Attrs.ID != "557058:f58131cb-b67d-43c7-b30d-6b58d40bd077" {
		t.Fatalf("id = %q", nodes[2].Attrs.ID)
	}
}

// Text the caller did not write — a crawled title, a database value — must reach Jira exactly as it
// is, whatever tokens it happens to contain.
func TestDocBuilder_AddLiteralExpandsNothing(t *testing.T) {
	text := "[@5b10ac8d82e05b22cc7d4ef5] see [issue:ABC-1]\n\nsecond"
	doc, err := NewDocBuilder().AddLiteral(text).Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if len(doc.Content) != 3 {
		t.Fatalf("want one paragraph per line, got %d", len(doc.Content))
	}
	first := doc.Content[0].Content
	if len(first) != 1 || first[0].Type != adfText || first[0].Text != "[@5b10ac8d82e05b22cc7d4ef5] see [issue:ABC-1]" {
		t.Fatalf("tokens were expanded: %+v", first)
	}
	if len(doc.Content[1].Content) != 0 {
		t.Fatalf("a blank line must be an empty paragraph, got %+v", doc.Content[1])
	}
}

func TestLiteralDoc(t *testing.T) {
	doc, err := LiteralDoc("see [@acct-1]")
	if err != nil {
		t.Fatalf("literal doc: %v", err)
	}
	if got := doc.Content[0].Content[0]; got.Type != adfText || got.Text != "see [@acct-1]" {
		t.Fatalf("got %+v", got)
	}

	if _, err := LiteralDoc("  \n "); err == nil || strings.Contains(err.Error(), "invalid argument") == false {
		t.Fatalf("blank text must be refused, got %v", err)
	}
}

func TestDocBuilder_LenCountsCharactersNotBytes(t *testing.T) {
	if got := NewDocBuilder().AddText("préço €").Len(); got != 7 {
		t.Fatalf("len = %d, want 7", got)
	}
}
