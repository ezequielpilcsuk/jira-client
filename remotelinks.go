package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Remote links point an issue at something that is not a Jira issue — a dashboard, a build, a log
// line, the source file that caused the ticket. CreateLink already covers Jira↔Jira, which leaves
// unserved the case an automation hits most often: it has just produced an artefact somewhere else
// and wants the issue to carry a link to it.
//
// Everything here needs the Link issues permission on the project and issue linking enabled
// site-wide. A site with linking switched off answers 403 rather than saying so, which surfaces as
// ErrUnauthorized and reads like a credentials problem.

// RemoteLinkInput describes a link from an issue to an external resource. URL and Title are the only
// required fields — Jira rejects an object without them, so they are checked before a request is sent.
type RemoteLinkInput struct {
	// GlobalID is the caller's own identifier for the link and doubles as its idempotency key.
	// SetRemoteLink documents what reusing one does.
	GlobalID string
	// Relationship is the phrase Jira renders above the link, e.g. "causes". Jira falls back to
	// "links to" when it is empty.
	Relationship string

	URL     string
	Title   string
	Summary string

	// IconURL is a 16x16 image shown beside the title; IconTitle is its tooltip.
	IconURL   string
	IconTitle string

	// ApplicationType and ApplicationName name the system on the far end, e.g. "com.acme.tracker"
	// and "Acme". Jira groups an issue's remote links by application in the UI.
	ApplicationType string
	ApplicationName string

	// Resolved strikes the link through, marking whatever it points at as already dealt with.
	Resolved bool
}

// RemoteLink is a remote link as Jira returns it, flattened. Jira nests the interesting values two
// levels down inside optional objects it omits when unset, so the nesting is collapsed at decode time
// and an absent object reads as a zero value rather than panicking.
type RemoteLink struct {
	ID           int64
	GlobalID     string
	Relationship string

	URL     string
	Title   string
	Summary string

	Resolved bool

	ApplicationType string
	ApplicationName string
}

// rawRemoteLink mirrors Jira's payload. The nested objects are pointers because Jira omits the ones
// that were never set instead of sending nulls for them.
type rawRemoteLink struct {
	ID           int64  `json:"id"`
	GlobalID     string `json:"globalId"`
	Relationship string `json:"relationship"`
	Application  *struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"application"`
	Object *struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Status  *struct {
			Resolved bool `json:"resolved"`
		} `json:"status"`
	} `json:"object"`
}

// toRemoteLink flattens a raw link, tolerating every absent object.
func (r rawRemoteLink) toRemoteLink() RemoteLink {
	link := RemoteLink{ID: r.ID, GlobalID: r.GlobalID, Relationship: r.Relationship}
	if r.Object != nil {
		link.URL, link.Title, link.Summary = r.Object.URL, r.Object.Title, r.Object.Summary
		if r.Object.Status != nil {
			link.Resolved = r.Object.Status.Resolved
		}
	}
	if r.Application != nil {
		link.ApplicationType, link.ApplicationName = r.Application.Type, r.Application.Name
	}
	return link
}

// RemoteLinks lists an issue's remote links.
//
// The endpoint answers with a bare array rather than a paginated envelope, so this is the issue's
// complete set in one request and there is no page to follow.
func (c *Client) RemoteLinks(ctx context.Context, key string) ([]RemoteLink, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "GET", apiBase+"/issue/"+url.PathEscape(key)+"/remotelink", nil)
	if err != nil {
		return nil, err
	}

	var raw []rawRemoteLink
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode remote links for %s: %w", key, err)
	}

	links := make([]RemoteLink, 0, len(raw))
	for _, entry := range raw {
		links = append(links, entry.toRemoteLink())
	}
	return links, nil
}

// SetRemoteLink creates or replaces a remote link and returns Jira's ID for it. In dry-run mode it
// returns 0 and links nothing.
//
// It is Set rather than Add because Jira treats the POST as an upsert keyed on GlobalID: posting a
// GlobalID that already exists on the issue updates that link instead of adding a second one. A
// retry-heavy automation therefore gets idempotency without a read-first — choose a stable GlobalID
// (the build URL, the pipeline run ID) and post it as often as the retries demand.
//
// ⚠️ That update is a replace, not a merge. Atlassian documents that "any fields without values in the
// request are set to null", so a later call carrying only URL and Title clears the summary, icon,
// relationship and resolved flag an earlier one set. Send the whole intended state every time.
//
// An issue holds at most 2,000 remote links; beyond that Jira answers 413, which arrives as
// ErrLimitExceeded.
func (c *Client) SetRemoteLink(ctx context.Context, key string, input RemoteLinkInput) (int64, error) {
	if key == "" {
		return 0, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}
	if strings.TrimSpace(input.URL) == "" || strings.TrimSpace(input.Title) == "" {
		return 0, fmt.Errorf("%w: a remote link needs both a url and a title", ErrInvalidArgument)
	}
	if c.skipMutation("SetRemoteLink", key, input.GlobalID, input.URL) == true {
		return 0, nil
	}

	object := map[string]any{
		"url":   input.URL,
		"title": input.Title,
		// Always sent: because the write replaces, omitting the status would clear the resolved flag
		// on an existing link rather than leave it alone, so the input has to state it outright.
		"status": map[string]any{"resolved": input.Resolved},
	}
	if input.Summary != "" {
		object["summary"] = input.Summary
	}
	if input.IconURL != "" || input.IconTitle != "" {
		object["icon"] = map[string]string{"url16x16": input.IconURL, "title": input.IconTitle}
	}

	payload := map[string]any{"object": object}
	if input.GlobalID != "" {
		payload["globalId"] = input.GlobalID
	}
	if input.Relationship != "" {
		payload["relationship"] = input.Relationship
	}
	if input.ApplicationType != "" || input.ApplicationName != "" {
		payload["application"] = map[string]string{
			"type": input.ApplicationType,
			"name": input.ApplicationName,
		}
	}

	body, err := c.do(ctx, "POST", apiBase+"/issue/"+url.PathEscape(key)+"/remotelink", payload)
	if err != nil {
		return 0, err
	}

	var written struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &written); err != nil {
		return 0, fmt.Errorf("decode remote link response for %s: %w", key, err)
	}
	return written.ID, nil
}

// DeleteRemoteLink removes one remote link by the ID Jira assigned it, which RemoteLinks reports.
func (c *Client) DeleteRemoteLink(ctx context.Context, key, linkID string) error {
	if key == "" || linkID == "" {
		return fmt.Errorf("%w: issue key and link id are required", ErrInvalidArgument)
	}
	if c.skipMutation("DeleteRemoteLink", key, linkID) == true {
		return nil
	}

	_, err := c.do(ctx, "DELETE",
		apiBase+"/issue/"+url.PathEscape(key)+"/remotelink/"+url.PathEscape(linkID), nil)
	return err
}

// DeleteRemoteLinkByGlobalID removes the link carrying a global ID. It is the counterpart to
// SetRemoteLink: an automation that owns its global IDs can retract exactly what it created without
// first listing the issue to find Jira's ID for it.
//
// The global ID travels in the query string, and the ones systems generate routinely contain reserved
// characters — "system=https://ci.example.com&id=42" is a conventional shape. It is percent-escaped
// here; sent raw it would be truncated at the first "&" and delete the wrong link, or none.
func (c *Client) DeleteRemoteLinkByGlobalID(ctx context.Context, key, globalID string) error {
	if key == "" || globalID == "" {
		return fmt.Errorf("%w: issue key and global id are required", ErrInvalidArgument)
	}
	if c.skipMutation("DeleteRemoteLinkByGlobalID", key, globalID) == true {
		return nil
	}

	query := url.Values{}
	query.Set("globalId", globalID)

	_, err := c.do(ctx, "DELETE", buildPath("/issue/"+url.PathEscape(key)+"/remotelink", query), nil)
	return err
}
