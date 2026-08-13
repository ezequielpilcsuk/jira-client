package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// commentPageSize is the page size for listing comments, matching the endpoint's own default.
const commentPageSize = 100

// Visibility types accepted by the comment API.
const (
	VisibilityGroup = "group"
	VisibilityRole  = "role"
)

// CommentOrder sorts a comment listing. Created is the only sortable field this endpoint has.
type CommentOrder string

// Comment sort orders. Jira rejects any other value with a 400, so the set is closed.
const (
	// CommentOrderOldestFirst is Jira's default; "+created" is an accepted synonym for it.
	CommentOrderOldestFirst CommentOrder = "created"
	CommentOrderNewestFirst CommentOrder = "-created"
)

// validCommentOrders is every value orderBy accepts, including the "+created" synonym that has no
// constant of its own.
var validCommentOrders = map[CommentOrder]bool{
	"created":  true,
	"+created": true,
	"-created": true,
}

// Visibility restricts a comment to a group or a project role. Its zero value means unrestricted —
// visible to anyone who can see the issue.
type Visibility struct {
	// Type is VisibilityGroup or VisibilityRole.
	Type string
	// Value is the group or role name.
	Value string
	// Identifier is the group ID or role ID. Prefer it over Value: Atlassian's own documentation flags
	// the group *name* as mutable, so a rename silently breaks a restriction keyed on the name while
	// one keyed on the identifier survives.
	Identifier string
}

// validate rejects a restriction Jira could not apply.
func (v Visibility) validate() error {
	if v.Type != VisibilityGroup && v.Type != VisibilityRole {
		return fmt.Errorf("%w: visibility type must be %q or %q, got %q",
			ErrInvalidArgument, VisibilityGroup, VisibilityRole, v.Type)
	}
	if v.Value == "" && v.Identifier == "" {
		return fmt.Errorf("%w: visibility needs a value or an identifier", ErrInvalidArgument)
	}
	return nil
}

// payload renders the visibility object, omitting whichever key was not supplied.
func (v Visibility) payload() map[string]string {
	fields := map[string]string{"type": v.Type}
	if v.Value != "" {
		fields["value"] = v.Value
	}
	if v.Identifier != "" {
		fields["identifier"] = v.Identifier
	}
	return fields
}

// rawComment mirrors Jira's comment payload. Author is a pointer because Jira omits it for comments
// left by a deleted account.
type rawComment struct {
	ID      string   `json:"id"`
	Body    *ADFDoc  `json:"body"`
	Author  *rawUser `json:"author"`
	Created string   `json:"created"`
}

// toComment flattens a raw comment, tolerating an absent author or body.
func (r rawComment) toComment() Comment {
	comment := Comment{ID: r.ID, Created: parseJiraTime(r.Created)}
	if r.Body != nil {
		comment.Body = r.Body.Text()
	}
	if r.Author != nil {
		comment.AuthorID = r.Author.AccountID
	}
	return comment
}

// Comments returns every comment on an issue, following pagination to the end. Bodies are flattened
// to plain text, exactly as Issue.Comments are.
//
// Pass an order to sort; without one Jira returns oldest first. Only created, +created and -created
// are accepted, so an unknown order is refused locally rather than spent on a certain 400.
//
// Two limits are worth knowing about. An issue holds at most 5,000 comments, and a write past that
// ceiling returns 413 — surfaced here as ErrLimitExceeded, which unlike a rate limit never clears by
// retrying. And jsdPublic, the flag deciding whether a Jira Service Management comment is visible to
// the customer or internal, is read-only on this API: it can only be set through the Service Desk
// API, so this endpoint cannot make a comment customer-facing.
func (c *Client) Comments(ctx context.Context, key string, order ...CommentOrder) ([]Comment, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}

	var sort CommentOrder
	if len(order) > 0 {
		sort = order[0]
	}
	if sort != "" && validCommentOrders[sort] == false {
		return nil, fmt.Errorf("%w: comment order %q is not one of created, +created, -created",
			ErrInvalidArgument, sort)
	}

	var comments []Comment
	startAt := 0
	for {
		params := url.Values{}
		params.Set("startAt", strconv.Itoa(startAt))
		params.Set("maxResults", strconv.Itoa(commentPageSize))
		if sort != "" {
			params.Set("orderBy", string(sort))
		}

		body, err := c.do(ctx, "GET", buildPath("/issue/"+url.PathEscape(key)+"/comment", params), nil)
		if err != nil {
			return nil, err
		}

		var page struct {
			Comments []rawComment `json:"comments"`
			Total    int          `json:"total"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode comments for %s: %w", key, err)
		}

		for _, raw := range page.Comments {
			comments = append(comments, raw.toComment())
		}

		// Advance by what the page actually held rather than the requested size: Jira may cap
		// maxResults below what was asked, and an empty page is the only safe stop for a bad total.
		startAt += len(page.Comments)
		if len(page.Comments) == 0 || startAt >= page.Total {
			break
		}
	}
	return comments, nil
}

// AddCommentReturning posts a comment and returns it as Jira stored it, unlike AddComment which
// discards the response. Take this one when the comment has to be edited or deleted later — its ID
// is the only handle on it, and there is no way to recover it afterwards short of listing every
// comment on the issue and guessing.
//
// In dry-run mode nothing is posted and the returned Comment carries the ID "DRY-RUN", mirroring
// CreateIssue. A caller feeding that ID back into UpdateComment or DeleteComment is likewise a no-op.
func (c *Client) AddCommentReturning(ctx context.Context, key string, doc *ADFDoc) (Comment, error) {
	return c.addComment(ctx, key, doc, nil)
}

// AddRestrictedComment posts a comment only members of a group or project role can read. Everyone
// else sees the issue without it.
//
// Restricting to a group by name is the fragile option — see Visibility.Identifier. Note also that
// this restricts platform visibility, not Service Desk visibility: on a JSM project it does not
// control whether the customer sees the comment. That is jsdPublic, which this API cannot set.
func (c *Client) AddRestrictedComment(ctx context.Context, key string, doc *ADFDoc, vis Visibility) (Comment, error) {
	if err := vis.validate(); err != nil {
		return Comment{}, err
	}
	return c.addComment(ctx, key, doc, &vis)
}

// addComment posts a comment, optionally restricted.
func (c *Client) addComment(ctx context.Context, key string, doc *ADFDoc, vis *Visibility) (Comment, error) {
	if key == "" {
		return Comment{}, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}
	if doc == nil || len(doc.Content) == 0 {
		return Comment{}, fmt.Errorf("%w: %v", ErrInvalidArgument, errEmptyDoc)
	}
	if c.skipMutation("AddComment", key) == true {
		return Comment{ID: "DRY-RUN"}, nil
	}

	payload := map[string]any{"body": doc}
	if vis != nil {
		payload["visibility"] = vis.payload()
	}

	body, err := c.do(ctx, "POST", apiBase+"/issue/"+url.PathEscape(key)+"/comment", payload)
	if err != nil {
		return Comment{}, err
	}

	var created rawComment
	if err := json.Unmarshal(body, &created); err != nil {
		return Comment{}, fmt.Errorf("decode created comment on %s: %w", key, err)
	}
	return created.toComment(), nil
}

// UpdateComment replaces a comment's body. The comment ID comes from AddCommentReturning or
// Comments; it is not the issue key.
//
// Jira rewrites the body in place and records the caller as its update author — there is no revision
// history, so the previous text is gone. Editing someone else's comment needs the Edit all comments
// permission; Edit own comments only covers your own.
func (c *Client) UpdateComment(ctx context.Context, key, commentID string, doc *ADFDoc) error {
	if key == "" || commentID == "" {
		return fmt.Errorf("%w: key and comment id are required", ErrInvalidArgument)
	}
	if doc == nil || len(doc.Content) == 0 {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, errEmptyDoc)
	}
	if c.skipMutation("UpdateComment", key, commentID) == true {
		return nil
	}

	_, err := c.do(ctx, "PUT",
		apiBase+"/issue/"+url.PathEscape(key)+"/comment/"+url.PathEscape(commentID),
		map[string]any{"body": doc})
	return err
}

// DeleteComment removes a comment permanently — Jira keeps no copy and offers no undo.
func (c *Client) DeleteComment(ctx context.Context, key, commentID string) error {
	if key == "" || commentID == "" {
		return fmt.Errorf("%w: key and comment id are required", ErrInvalidArgument)
	}
	if c.skipMutation("DeleteComment", key, commentID) == true {
		return nil
	}

	_, err := c.do(ctx, "DELETE",
		apiBase+"/issue/"+url.PathEscape(key)+"/comment/"+url.PathEscape(commentID), nil)
	return err
}
