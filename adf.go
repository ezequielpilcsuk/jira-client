package jiraclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Atlassian Document Format node and mark types used by this package.
const (
	adfDoc       = "doc"
	adfParagraph = "paragraph"
	adfText      = "text"
	adfHeading   = "heading"
	adfTable     = "table"
	adfTableRow  = "tableRow"
	adfTableCell = "tableCell"
	adfTableHead = "tableHeader"
	adfMention   = "mention"
	adfLink      = "link"

	tableLayoutDefault = "default"
	tableWidthDefault  = 760

	// CommentMaxChars is Jira's limit on the text content of a comment. The cap applies to the
	// rendered text, not the serialized JSON, so a large ADF document is fine as long as the words
	// inside it stay under this.
	CommentMaxChars = 32767
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

// ADFDoc is an Atlassian Document Format document — Jira's rich-text representation.
type ADFDoc struct {
	Version int       `json:"version,omitempty"`
	Type    string    `json:"type"`
	Content []ADFNode `json:"content"`
}

// Text renders a document to plain text, which is how a description written by one service is read
// back by another.
func (d *ADFDoc) Text() string {
	if d == nil {
		return ""
	}
	return nodesText(d.Content, true)
}

// ADFNode is a single node in an ADF document.
type ADFNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text,omitempty"`
	Content []ADFNode `json:"content,omitempty"`
	Attrs   *ADFAttrs `json:"attrs,omitempty"`
	Marks   []ADFMark `json:"marks,omitempty"`
}

// ADFAttrs holds node attributes. Every field is omitempty because Jira rejects unexpected keys on
// some node types.
type ADFAttrs struct {
	Href                  string `json:"href,omitempty"`
	Color                 string `json:"color,omitempty"`
	ID                    string `json:"id,omitempty"`
	Layout                string `json:"layout,omitempty"`
	Level                 int    `json:"level,omitempty"`
	IsNumberColumnEnabled bool   `json:"isNumberColumnEnabled,omitempty"`
	Width                 int    `json:"width,omitempty"`
	// URL is the address of a smart link (inlineCard, blockCard, embedCard), which is what pasting a
	// URL into Jira's editor produces.
	URL string `json:"url,omitempty"`
	// Text is the label of a mention ("@Ada Lovelace"), a status lozenge or an emoji.
	Text string `json:"text,omitempty"`
	// ShortName is an emoji's name, e.g. ":warning:".
	ShortName string `json:"shortName,omitempty"`
	// Timestamp is a date node's value: milliseconds since the Unix epoch, as a string.
	Timestamp string `json:"timestamp,omitempty"`
}

// UnmarshalJSON reads the attributes this package uses and skips whatever it cannot read. attrs is an
// open bag whose value types vary by node — width is an integer on a table and a fractional
// percentage on an image — so a shape that is not modelled here is dropped rather than allowed to
// fail the decode of the issue that contains it.
func (a *ADFAttrs) UnmarshalJSON(data []byte) error {
	// Anything but an object leaves raw empty, which reads as no attributes at all: a malformed attrs
	// is not worth failing the decode of a whole issue for.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)

	*a = ADFAttrs{
		Href:      attrString(raw["href"]),
		Color:     attrString(raw["color"]),
		ID:        attrString(raw["id"]),
		Layout:    attrString(raw["layout"]),
		Level:     int(attrNumber(raw["level"])),
		Width:     int(attrNumber(raw["width"])),
		URL:       attrString(raw["url"]),
		Text:      attrString(raw["text"]),
		ShortName: attrString(raw["shortName"]),
		Timestamp: attrString(raw["timestamp"]),
	}
	var numbered bool
	if json.Unmarshal(raw["isNumberColumnEnabled"], &numbered) == nil {
		a.IsNumberColumnEnabled = numbered
	}
	return nil
}

// attrString reads an attribute as a string, accepting a number where Jira sends one. Anything else,
// including an absent attribute, reads as "".
func attrString(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

// attrNumber reads an attribute as a number. Anything else, including an absent attribute, reads as 0.
func attrNumber(raw json.RawMessage) float64 {
	var number float64
	if json.Unmarshal(raw, &number) != nil {
		return 0
	}
	return number
}

// ADFMark is inline formatting applied to a text node (strong, em, link, code...).
type ADFMark struct {
	Type  string    `json:"type"`
	Attrs *ADFAttrs `json:"attrs,omitempty"`
}

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
	return &DocBuilder{issueBaseURL: strings.TrimRight(c.baseURL, "/")}
}

// IssueURL is the human-facing URL for an issue key on this client's site.
func (c *Client) IssueURL(key string) string {
	return strings.TrimRight(c.baseURL, "/") + "/browse/" + key
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

// nodesText recursively renders nodes to plain text. linkTargets appends the address of a link whose
// visible text does not show it; it is off when a document is being measured against Jira's text
// limit, which does not count addresses.
func nodesText(nodes []ADFNode, linkTargets bool) string {
	var builder strings.Builder
	for i, node := range nodes {
		last := i == len(nodes)-1
		switch node.Type {
		case adfText:
			builder.WriteString(node.Text)
			if linkTargets == true {
				if target := hiddenLinkTarget(node); target != "" {
					builder.WriteString(" (" + target + ")")
				}
			}
		case "hardBreak":
			builder.WriteString("\n")
		case adfMention:
			builder.WriteString(mentionText(node.Attrs))
		case "inlineCard":
			builder.WriteString(cardURL(node.Attrs))
		case "blockCard", "embedCard":
			builder.WriteString(cardURL(node.Attrs))
			if last == false {
				builder.WriteString("\n")
			}
		case "emoji":
			if node.Attrs != nil {
				if node.Attrs.Text != "" {
					builder.WriteString(node.Attrs.Text)
				} else {
					builder.WriteString(node.Attrs.ShortName)
				}
			}
		case "status":
			if node.Attrs != nil {
				builder.WriteString(node.Attrs.Text)
			}
		case "date":
			builder.WriteString(dateText(node.Attrs))
		case adfParagraph, adfHeading:
			builder.WriteString(nodesText(node.Content, linkTargets))
			if last == false {
				builder.WriteString("\n")
			}
		case adfTable:
			builder.WriteString(tableText(node, linkTargets))
			if last == false {
				builder.WriteString("\n")
			}
		case "mediaSingle", "media":
			// Media carries no text.
		case "codeBlock":
			builder.WriteString("```\n" + nodesText(node.Content, linkTargets) + "\n```\n")
		case "blockquote":
			builder.WriteString("> " + nodesText(node.Content, linkTargets) + "\n")
		case "bulletList", "orderedList":
			for _, item := range node.Content {
				if item.Type == "listItem" {
					builder.WriteString("• " + nodesText(item.Content, linkTargets) + "\n")
				}
			}
		default:
			if len(node.Content) > 0 {
				builder.WriteString(nodesText(node.Content, linkTargets))
			} else if node.Text != "" {
				builder.WriteString(node.Text)
			}
		}
	}
	return builder.String()
}

// tableText renders a table one row per line with " | " between cells, so two neighbouring values
// stay two values. A cell's own line breaks become spaces to keep the row on its line.
func tableText(table ADFNode, linkTargets bool) string {
	rows := make([]string, 0, len(table.Content))
	for _, row := range table.Content {
		cells := make([]string, 0, len(row.Content))
		for _, cell := range row.Content {
			text := strings.TrimSpace(nodesText(cell.Content, linkTargets))
			cells = append(cells, strings.ReplaceAll(text, "\n", " "))
		}
		rows = append(rows, strings.Join(cells, " | "))
	}
	return strings.Join(rows, "\n")
}

// hiddenLinkTarget returns the address a linked text node points at when reading the text alone would
// lose it. It is empty when the text already shows the address, or names the issue the address opens.
func hiddenLinkTarget(node ADFNode) string {
	for _, mark := range node.Marks {
		if mark.Type != adfLink || mark.Attrs == nil || mark.Attrs.Href == "" {
			continue
		}
		href := mark.Attrs.Href
		if strings.Contains(node.Text, href) == true || strings.HasSuffix(href, "/browse/"+node.Text) == true {
			return ""
		}
		return href
	}
	return ""
}

// mentionText renders a mention as the name Jira stored for it, falling back to the account id for
// a mention written without one.
func mentionText(attrs *ADFAttrs) string {
	switch {
	case attrs == nil:
		return ""
	case attrs.Text != "":
		return attrs.Text
	case attrs.ID != "":
		return "@" + attrs.ID
	}
	return ""
}

// cardURL is a smart link's address. It lives in attrs.url; href is read as a fallback for a card
// built with this package's link attribute.
func cardURL(attrs *ADFAttrs) string {
	switch {
	case attrs == nil:
		return ""
	case attrs.URL != "":
		return attrs.URL
	}
	return attrs.Href
}

// dateText renders a date node as a calendar date. Its timestamp is milliseconds since the epoch;
// one that does not parse is shown as it came.
func dateText(attrs *ADFAttrs) string {
	if attrs == nil || attrs.Timestamp == "" {
		return ""
	}
	millis, err := strconv.ParseInt(attrs.Timestamp, 10, 64)
	if err != nil {
		return attrs.Timestamp
	}
	return time.UnixMilli(millis).UTC().Format("2006-01-02")
}

// errEmptyDoc guards callers that build a document without checking Build's error.
var errEmptyDoc = errors.New("empty ADF document")
