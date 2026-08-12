package jiraclient

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
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

	tableLayoutDefault = "default"
	tableWidthDefault  = 760

	// CommentMaxChars is Jira's limit on the text content of a comment. The cap applies to the
	// rendered text, not the serialized JSON, so a large ADF document is fine as long as the words
	// inside it stay under this.
	CommentMaxChars = 32767
)

// mentionPattern recognises "[@accountId]" in text passed to the builder, converting it into a real
// Jira mention node so the user is actually notified. Plain "@name" text notifies nobody.
var mentionPattern = regexp.MustCompile(`\[@(.*?)\]`)

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
	return nodesText(d.Content)
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
}

// NewDocBuilder returns an empty builder.
func NewDocBuilder() *DocBuilder {
	return &DocBuilder{}
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
	b.nodes = append(b.nodes, ADFNode{Type: adfParagraph, Content: inlineNodes(text)})
	return b
}

// AddParagraphs appends one paragraph per line, preserving blank lines as empty paragraphs.
func (b *DocBuilder) AddParagraphs(text string) *DocBuilder {
	for _, line := range strings.Split(text, "\n") {
		paragraph := ADFNode{Type: adfParagraph}
		if line != "" {
			paragraph.Content = inlineNodes(line)
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

// Len reports the rendered text length, for checking against CommentMaxChars before posting.
func (b *DocBuilder) Len() int { return len(nodesText(b.nodes)) }

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
			text.Marks = append(text.Marks, ADFMark{Type: "link", Attrs: &ADFAttrs{Href: cell.Href}})
		}
		paragraph.Content = []ADFNode{text}
	}
	return ADFNode{Type: nodeType, Attrs: &ADFAttrs{}, Content: []ADFNode{paragraph}}
}

// inlineNodes splits a line into text and mention nodes.
func inlineNodes(line string) []ADFNode {
	var nodes []ADFNode
	last := 0
	for _, match := range mentionPattern.FindAllStringSubmatchIndex(line, -1) {
		if before := line[last:match[0]]; before != "" {
			nodes = append(nodes, ADFNode{Type: adfText, Text: before})
		}
		nodes = append(nodes, ADFNode{
			Type:  adfMention,
			Attrs: &ADFAttrs{ID: line[match[2]:match[3]]},
		})
		last = match[1]
	}
	if remaining := line[last:]; remaining != "" {
		nodes = append(nodes, ADFNode{Type: adfText, Text: remaining})
	}
	return nodes
}

// nodesText recursively renders nodes to plain text.
func nodesText(nodes []ADFNode) string {
	var builder strings.Builder
	for i, node := range nodes {
		switch node.Type {
		case adfText:
			builder.WriteString(node.Text)
		case "hardBreak":
			builder.WriteString("\n")
		case adfMention:
			if node.Attrs != nil && node.Attrs.ID != "" {
				builder.WriteString("@" + node.Attrs.ID)
			}
		case "inlineCard":
			if node.Attrs != nil && node.Attrs.Href != "" {
				builder.WriteString(node.Attrs.Href)
			}
		case adfParagraph, adfHeading:
			builder.WriteString(nodesText(node.Content))
			if i < len(nodes)-1 {
				builder.WriteString("\n")
			}
		case "mediaSingle", "media":
			// Media carries no text.
		case "codeBlock":
			builder.WriteString("```\n" + nodesText(node.Content) + "\n```\n")
		case "blockquote":
			builder.WriteString("> " + nodesText(node.Content) + "\n")
		case "bulletList", "orderedList":
			for _, item := range node.Content {
				if item.Type == "listItem" {
					builder.WriteString("• " + nodesText(item.Content) + "\n")
				}
			}
		default:
			if len(node.Content) > 0 {
				builder.WriteString(nodesText(node.Content))
			} else if node.Text != "" {
				builder.WriteString(node.Text)
			}
		}
	}
	return builder.String()
}

// errEmptyDoc guards callers that build a document without checking Build's error.
var errEmptyDoc = errors.New("empty ADF document")
