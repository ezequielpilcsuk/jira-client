package jiraclient

import (
	"encoding/json"
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

// ADFDoc is an Atlassian Document Format document — Jira's rich-text representation.
type ADFDoc struct {
	Version int       `json:"version,omitempty"`
	Type    string    `json:"type"`
	Content []ADFNode `json:"content"`
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
