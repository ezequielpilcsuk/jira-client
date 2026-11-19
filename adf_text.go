package jiraclient

import (
	"strconv"
	"strings"
	"time"
)

// Text renders a document to plain text, which is how a description written by one service is read
// back by another.
func (d *ADFDoc) Text() string {
	if d == nil {
		return ""
	}
	return nodesText(d.Content, true)
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
