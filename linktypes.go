package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// A Jira site defines its own issue link types and can rename the built-in ones, so the LinkDuplicate
// / LinkRelates / LinkBlocks / LinkClones constants are a guess about a site rather than a fact about
// it — a CreateLink using one fails on any site that renamed "Relates" to "Related to". LinkTypes
// reads what is actually configured, the way Transitions does for a workflow, and LinkTypeIDByName
// turns a readable name into the ID that survives the next rename.
//
// This also closes an asymmetry: the client could create issue links but never look one up or remove
// one.
//
// All of it needs issue linking enabled site-wide. With the feature switched off Jira answers 404 on
// /issueLinkType, which reads like a wrong path but really means there is nothing to configure.

// LinkType is one issue link type as the site defines it. Inward and Outward are the phrases rendered
// on each end — for "Blocks" they are "is blocked by" and "blocks".
type LinkType struct {
	ID      string
	Name    string
	Inward  string
	Outward string
}

// LinkTypes lists the issue link types defined on the site.
func (c *Client) LinkTypes(ctx context.Context) ([]LinkType, error) {
	body, err := c.do(ctx, "GET", apiBase+"/issueLinkType", nil)
	if err != nil {
		return nil, err
	}

	var decoded struct {
		IssueLinkTypes []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Inward  string `json:"inward"`
			Outward string `json:"outward"`
		} `json:"issueLinkTypes"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode issue link types: %w", err)
	}

	types := make([]LinkType, 0, len(decoded.IssueLinkTypes))
	for _, entry := range decoded.IssueLinkTypes {
		types = append(types, LinkType{
			ID:      entry.ID,
			Name:    entry.Name,
			Inward:  entry.Inward,
			Outward: entry.Outward,
		})
	}
	return types, nil
}

// LinkTypeIDByName resolves a link type to its ID, so config can hold a readable name while
// CreateLink is given the reference that outlives a rename.
//
// Matching is case-insensitive and runs in two passes: every type's Name first, then every type's
// Inward and Outward phrasing, so "Blocks", "blocks" and "is blocked by" all resolve. Names win
// outright because one type's phrasing can collide with another's name, and a name is the more
// specific statement of intent.
func (c *Client) LinkTypeIDByName(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w: link type name cannot be empty", ErrInvalidArgument)
	}

	types, err := c.LinkTypes(ctx)
	if err != nil {
		return "", err
	}
	for _, linkType := range types {
		if strings.EqualFold(linkType.Name, name) == true {
			return linkType.ID, nil
		}
	}
	for _, linkType := range types {
		if strings.EqualFold(linkType.Inward, name) == true ||
			strings.EqualFold(linkType.Outward, name) == true {
			return linkType.ID, nil
		}
	}
	return "", fmt.Errorf("%w: link type %q is not defined on this site", ErrInvalidArgument, name)
}

// IssueLink is one link between two issues, flattened to the key on each end.
type IssueLink struct {
	ID   string
	Type LinkType
	// InwardKey and OutwardKey read as "<outward> <type outward> <inward>", matching LinkInput.
	InwardKey  string
	OutwardKey string
}

// GetIssueLink reads a single issue link by its own ID.
//
// A key comes back empty when the caller cannot see the issue on that end: Jira omits that issue and
// still returns the link rather than refusing the whole read, so an empty key means "not visible to
// you", not "not linked".
func (c *Client) GetIssueLink(ctx context.Context, linkID string) (IssueLink, error) {
	if linkID == "" {
		return IssueLink{}, fmt.Errorf("%w: link id cannot be empty", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "GET", apiBase+"/issueLink/"+url.PathEscape(linkID), nil)
	if err != nil {
		return IssueLink{}, err
	}

	var raw struct {
		ID   string `json:"id"`
		Type *struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Inward  string `json:"inward"`
			Outward string `json:"outward"`
		} `json:"type"`
		InwardIssue *struct {
			Key string `json:"key"`
		} `json:"inwardIssue"`
		OutwardIssue *struct {
			Key string `json:"key"`
		} `json:"outwardIssue"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return IssueLink{}, fmt.Errorf("decode issue link %s: %w", linkID, err)
	}

	link := IssueLink{ID: raw.ID}
	if raw.Type != nil {
		link.Type = LinkType{
			ID:      raw.Type.ID,
			Name:    raw.Type.Name,
			Inward:  raw.Type.Inward,
			Outward: raw.Type.Outward,
		}
	}
	if raw.InwardIssue != nil {
		link.InwardKey = raw.InwardIssue.Key
	}
	if raw.OutwardIssue != nil {
		link.OutwardKey = raw.OutwardIssue.Key
	}
	return link, nil
}

// DeleteIssueLink removes an issue link. It takes the link's own ID, not either issue's — GetIssueLink
// and an issue's issuelinks field are where that ID comes from. Both issues survive; only the
// relationship between them goes.
func (c *Client) DeleteIssueLink(ctx context.Context, linkID string) error {
	if linkID == "" {
		return fmt.Errorf("%w: link id cannot be empty", ErrInvalidArgument)
	}
	if c.skipMutation("DeleteIssueLink", linkID) == true {
		return nil
	}

	_, err := c.do(ctx, "DELETE", apiBase+"/issueLink/"+url.PathEscape(linkID), nil)
	return err
}
