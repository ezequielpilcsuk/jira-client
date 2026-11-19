package jiraclient

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// mentionPattern recognises "[@accountId]" in text passed to the builder, converting it into a real
// Jira mention node so the user is actually notified. Plain "@name" text notifies nobody.
//
// An account id is opaque, but it is only ever letters, digits, colons, hyphens and underscores. The
// pattern asks for exactly that, so an empty pair of brackets, a price or a phrase that happens to
// follow "[@" stays ordinary text instead of becoming a mention node Jira would reject.
var mentionPattern = regexp.MustCompile(`\[@([A-Za-z0-9:_-]+)\]`)

// issuePattern recognises "[issue:KEY]" in text passed to the builder and renders it as a link to
// that issue. It is deliberately narrow — an uppercase project key followed by a number — so ordinary
// bracketed prose cannot match it.
var issuePattern = regexp.MustCompile(`\[issue:([A-Z][A-Z0-9_]*-\d+)\]`)

// inlinePattern finds either token in one left-to-right pass, so a line carrying both is expanded in
// the order the tokens appear. Group 1 is the mention account ID, group 2 the issue key; the group
// that did not participate reports index -1.
var inlinePattern = regexp.MustCompile(mentionPattern.String() + `|` + issuePattern.String())

// Cell is a table cell whose Text renders as a link when Href is set, keeping long URLs out of a
// column of their own.
type Cell struct {
	Text string
	Href string
}

// DocBuilder assembles an ADF document from headings, paragraphs and tables.
//
// Use it rather than hand-writing ADF: a text node with empty text is invalid ADF and makes Jira
// reject the entire request with a 400, which is easy to produce accidentally from a table cell that
// happens to have no value. The builder emits an empty paragraph instead.
type DocBuilder struct {
	nodes []ADFNode
	// issueBaseURL is the site root used to turn an "[issue:KEY]" token into a link. Empty renders the
	// key as plain text instead, so a builder made without a client still produces a valid document.
	issueBaseURL string
}

// NewDocBuilder returns an empty builder.
func NewDocBuilder() *DocBuilder {
	return &DocBuilder{}
}

// NewDocBuilder returns a builder that renders "[issue:KEY]" tokens as links into this client's site.
func (c *Client) NewDocBuilder() *DocBuilder {
	return &DocBuilder{issueBaseURL: c.browseBase()}
}

// IssueURL is the human-facing URL for an issue key on this client's site.
func (c *Client) IssueURL(key string) string {
	return c.browseBase() + "/browse/" + key
}

// browseBase is the site root that issue pages live under.
func (c *Client) browseBase() string {
	if c.siteURL != "" {
		return c.siteURL
	}
	return strings.TrimRight(c.baseURL, "/")
}

// AddHeading appends a heading. Levels outside 1-6 are clamped to 3.
func (b *DocBuilder) AddHeading(level int, text string) *DocBuilder {
	if level < 1 || level > 6 {
		level = 3
	}
	if strings.TrimSpace(text) == "" {
		return b
	}
	b.nodes = append(b.nodes, ADFNode{
		Type:    adfHeading,
		Attrs:   &ADFAttrs{Level: level},
		Content: []ADFNode{{Type: adfText, Text: text}},
	})
	return b
}

// AddText appends a paragraph. "[@accountId]" becomes a real mention node.
func (b *DocBuilder) AddText(text string) *DocBuilder {
	if text == "" {
		return b
	}
	b.nodes = append(b.nodes, ADFNode{Type: adfParagraph, Content: b.inlineNodes(text)})
	return b
}

// AddParagraphs appends one paragraph per line, preserving blank lines as empty paragraphs.
// "[@accountId]" and "[issue:KEY]" are expanded; see AddLiteral for text that must not be.
func (b *DocBuilder) AddParagraphs(text string) *DocBuilder {
	return b.addLines(text, true)
}

// AddLiteral appends one paragraph per line exactly as written, expanding no tokens. Use it for text
// the caller did not author — a crawled title, a database value, a quoted error — where a stray
// "[@…]" or "[issue:…]" must stay text rather than become a mention or a link.
func (b *DocBuilder) AddLiteral(text string) *DocBuilder {
	return b.addLines(text, false)
}

// addLines appends one paragraph per line, with or without token expansion. A blank line becomes an
// empty paragraph, since a text node with no text is invalid.
func (b *DocBuilder) addLines(text string, expandTokens bool) *DocBuilder {
	for _, line := range strings.Split(text, "\n") {
		paragraph := ADFNode{Type: adfParagraph}
		switch {
		case line == "":
		case expandTokens == true:
			paragraph.Content = b.inlineNodes(line)
		default:
			paragraph.Content = []ADFNode{{Type: adfText, Text: line}}
		}
		b.nodes = append(b.nodes, paragraph)
	}
	return b
}

// AddTable appends a table. It returns an error when a row's width does not match the headers, since
// Jira renders a ragged table unpredictably rather than rejecting it.
func (b *DocBuilder) AddTable(headers []string, rows [][]string) error {
	linked := make([][]Cell, len(rows))
	for i, row := range rows {
		linked[i] = make([]Cell, len(row))
		for j, value := range row {
			linked[i][j] = Cell{Text: value}
		}
	}
	return b.AddLinkedTable(headers, linked)
}

// AddLinkedTable appends a table whose cells may carry hyperlinks.
func (b *DocBuilder) AddLinkedTable(headers []string, rows [][]Cell) error {
	if len(headers) == 0 {
		return fmt.Errorf("%w: table headers cannot be empty", ErrInvalidArgument)
	}
	for i, row := range rows {
		if len(row) != len(headers) {
			return fmt.Errorf("%w: row %d has %d columns, expected %d",
				ErrInvalidArgument, i, len(row), len(headers))
		}
	}

	headerCells := make([]ADFNode, len(headers))
	for i, header := range headers {
		headerCells[i] = cellNode(adfTableHead, Cell{Text: header}, true)
	}
	tableRows := []ADFNode{{Type: adfTableRow, Content: headerCells}}

	for _, row := range rows {
		cells := make([]ADFNode, len(row))
		for i, cell := range row {
			cells[i] = cellNode(adfTableCell, cell, false)
		}
		tableRows = append(tableRows, ADFNode{Type: adfTableRow, Content: cells})
	}

	b.nodes = append(b.nodes, ADFNode{
		Type: adfTable,
		Attrs: &ADFAttrs{
			IsNumberColumnEnabled: false,
			Layout:                tableLayoutDefault,
			Width:                 tableWidthDefault,
		},
		Content: tableRows,
	})
	return nil
}

// Nodes exposes the assembled nodes.
func (b *DocBuilder) Nodes() []ADFNode { return b.nodes }

// Len reports the rendered text length in characters, for checking against CommentMaxChars before
// posting. Link addresses are left out, as Jira's limit counts the text a reader sees and not where
// it points.
func (b *DocBuilder) Len() int { return utf8.RuneCountInString(nodesText(b.nodes, false)) }

// Build returns the finished document, or an error when nothing was added — Jira rejects an empty
// document, and an accidentally-empty comment is almost always a bug in the caller.
func (b *DocBuilder) Build() (*ADFDoc, error) {
	if len(b.nodes) == 0 {
		return nil, fmt.Errorf("%w: document has no content", ErrInvalidArgument)
	}
	return &ADFDoc{Version: 1, Type: adfDoc, Content: b.nodes}, nil
}

// TextDoc builds a plain-text ADF document, the common case for a simple comment.
func TextDoc(text string) (*ADFDoc, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: comment text cannot be empty", ErrInvalidArgument)
	}
	return NewDocBuilder().AddParagraphs(text).Build()
}

// LiteralDoc is TextDoc without token expansion: the text is posted exactly as written. See
// DocBuilder.AddLiteral for when that matters.
func LiteralDoc(text string) (*ADFDoc, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: comment text cannot be empty", ErrInvalidArgument)
	}
	return NewDocBuilder().AddLiteral(text).Build()
}

// cellNode builds a table cell. An empty text node is invalid ADF and 400s the whole request, so an
// empty value produces an empty paragraph instead.
func cellNode(nodeType string, cell Cell, bold bool) ADFNode {
	paragraph := ADFNode{Type: adfParagraph}
	if cell.Text != "" {
		text := ADFNode{Type: adfText, Text: cell.Text}
		if bold == true {
			text.Marks = append(text.Marks, ADFMark{Type: "strong"})
		}
		if cell.Href != "" {
			text.Marks = append(text.Marks, ADFMark{Type: adfLink, Attrs: &ADFAttrs{Href: cell.Href}})
		}
		paragraph.Content = []ADFNode{text}
	}
	return ADFNode{Type: nodeType, Attrs: &ADFAttrs{}, Content: []ADFNode{paragraph}}
}

// inlineNodes splits a line into text and mention nodes.
func (b *DocBuilder) inlineNodes(line string) []ADFNode {
	var nodes []ADFNode
	last := 0
	for _, match := range inlinePattern.FindAllStringSubmatchIndex(line, -1) {
		if before := line[last:match[0]]; before != "" {
			nodes = append(nodes, ADFNode{Type: adfText, Text: before})
		}
		switch {
		case match[2] >= 0: // [@accountId]
			nodes = append(nodes, ADFNode{
				Type:  adfMention,
				Attrs: &ADFAttrs{ID: line[match[2]:match[3]]},
			})
		case match[4] >= 0: // [issue:KEY]
			key := line[match[4]:match[5]]
			node := ADFNode{Type: adfText, Text: key}
			// A link mark rather than an inlineCard: a card has to resolve through Jira's smart-link
			// service to render, a marked text node always does.
			if b.issueBaseURL != "" {
				node.Marks = []ADFMark{{Type: adfLink, Attrs: &ADFAttrs{Href: b.issueBaseURL + "/browse/" + key}}}
			}
			nodes = append(nodes, node)
		}
		last = match[1]
	}
	if remaining := line[last:]; remaining != "" {
		nodes = append(nodes, ADFNode{Type: adfText, Text: remaining})
	}
	return nodes
}

// errEmptyDoc guards callers that build a document without checking Build's error.
var errEmptyDoc = errors.New("empty ADF document")
