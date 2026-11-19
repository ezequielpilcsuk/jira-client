package jiraclient

import (
	"encoding/json"
	"strings"
	"testing"
)

// docText decodes an ADF document the way an issue read does and renders it.
func docText(t *testing.T, raw string) string {
	t.Helper()
	var doc ADFDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode adf: %v", err)
	}
	return doc.Text()
}

// Pasting a URL into Jira's editor stores a smart link, whose address lives in attrs.url. It is the
// commonest way a URL reaches a description, so losing it loses the URL.
func TestText_KeepsSmartLinkURLs(t *testing.T) {
	text := docText(t, `{"type":"doc","version":1,"content":[
		{"type":"paragraph","content":[
			{"type":"text","text":"Bad price on "},
			{"type":"inlineCard","attrs":{"url":"https://example.com/p/123"}}]},
		{"type":"blockCard","attrs":{"url":"https://example.com/block"}},
		{"type":"embedCard","attrs":{"url":"https://example.com/embed","layout":"wide"}},
		{"type":"paragraph","content":[{"type":"text","text":"after"}]}]}`)

	want := "Bad price on https://example.com/p/123\nhttps://example.com/block\nhttps://example.com/embed\nafter"
	if text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

// Two neighbouring cells holding 12345 and 678 must not read back as 12345678.
func TestText_SeparatesTableCellsAndRows(t *testing.T) {
	builder := NewDocBuilder().AddText("Products")
	if err := builder.AddTable([]string{"Website ID", "Product ID"}, [][]string{{"12345", "678"}, {"9", ""}}); err != nil {
		t.Fatalf("table: %v", err)
	}
	doc, err := builder.AddText("done").Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	want := "Products\nWebsite ID | Product ID\n12345 | 678\n9 | \ndone"
	if text := doc.Text(); text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

// A cell holding several paragraphs still occupies one line, or the row structure is lost.
func TestText_KeepsAMultiParagraphCellOnOneLine(t *testing.T) {
	text := docText(t, `{"type":"doc","version":1,"content":[{"type":"table","content":[
		{"type":"tableRow","content":[
			{"type":"tableCell","content":[
				{"type":"paragraph","content":[{"type":"text","text":"first"}]},
				{"type":"paragraph","content":[{"type":"text","text":"second"}]}]},
			{"type":"tableCell","content":[{"type":"paragraph","content":[{"type":"text","text":"b"}]}]}]}]}]}`)

	if text != "first second | b" {
		t.Fatalf("text = %q", text)
	}
}

// Jira stores the name a mention displayed alongside the account id. The name is what a reader of
// the text can use; the id is the fallback for a mention written without one.
func TestText_RendersAMentionByItsDisplayName(t *testing.T) {
	text := docText(t, `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
		{"type":"mention","attrs":{"id":"5b10ac8d82e05b22cc7d4ef5","text":"@Ada Lovelace"}},
		{"type":"text","text":" and "},
		{"type":"mention","attrs":{"id":"acct-2"}}]}]}`)

	if text != "@Ada Lovelace and @acct-2" {
		t.Fatalf("text = %q", text)
	}
}

// A link whose visible text is not its address would otherwise read back with the address gone.
func TestText_KeepsALinkTargetTheTextDoesNotShow(t *testing.T) {
	text := docText(t, `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
		{"type":"text","text":"click here","marks":[{"type":"link","attrs":{"href":"https://example.com/hidden"}}]}]}]}`)

	if text != "click here (https://example.com/hidden)" {
		t.Fatalf("text = %q", text)
	}
}

// Repeating an address the text already shows, or the browse URL of the issue key it names, adds
// length and nothing else.
func TestText_DoesNotRepeatALinkTargetTheTextAlreadyShows(t *testing.T) {
	text := docText(t, `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
		{"type":"text","text":"https://example.com/shown","marks":[{"type":"link","attrs":{"href":"https://example.com/shown"}}]},
		{"type":"text","text":" "},
		{"type":"text","text":"ABC-10","marks":[{"type":"link","attrs":{"href":"https://site.atlassian.net/browse/ABC-10"}}]}]}]}`)

	if text != "https://example.com/shown ABC-10" {
		t.Fatalf("text = %q", text)
	}
}

func TestText_RendersEmojiStatusAndDate(t *testing.T) {
	text := docText(t, `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
		{"type":"emoji","attrs":{"shortName":":warning:","text":"⚠️"}},
		{"type":"emoji","attrs":{"shortName":":custom:"}},
		{"type":"text","text":" "},
		{"type":"status","attrs":{"text":"BLOCKED"}},
		{"type":"text","text":" due "},
		{"type":"date","attrs":{"timestamp":"1700000000000"}}]}]}`)

	if text != "⚠️:custom: BLOCKED due 2023-11-14" {
		t.Fatalf("text = %q", text)
	}
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

// attrs is an open bag whose value types vary by node: width is an integer on a table and a
// fractional percentage on an image. A shape this package does not model must not fail the read of
// the issue that contains it.
func TestText_SurvivesAttributeShapesItDoesNotModel(t *testing.T) {
	text := docText(t, `{"type":"doc","version":1,"content":[
		{"type":"mediaSingle","attrs":{"layout":"wide","width":66.67},"content":[
			{"type":"media","attrs":{"id":"abc","type":"file","collection":"c","width":1024.5,"height":768}}]},
		{"type":"extension","attrs":{"text":{"nested":true},"level":"three","id":42}},
		{"type":"rule","attrs":["not","an","object"]},
		{"type":"paragraph","content":[
			{"type":"date","attrs":{"timestamp":1700000000000}},
			{"type":"text","text":" kept"}]}]}`)

	if text != "2023-11-14 kept" {
		t.Fatalf("text = %q", text)
	}
}
