package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// SummaryMaxChars is Jira's hard limit on the summary field. Exceeding it rejects the whole edit, so
// UpdateSummary refuses rather than sending a request that cannot succeed.
const SummaryMaxChars = 255

// Link type names present in a default Jira installation. A site may define others; pass any name.
const (
	LinkDuplicate = "Duplicate"
	LinkRelates   = "Relates"
	LinkBlocks    = "Blocks"
	LinkClones    = "Cloners"
)

// maxReconcileIssues is Jira's cap on the reconcileIssues parameter.
const maxReconcileIssues = 50

// Search runs a JQL query and returns every matching issue, following Jira's pagination to the end.
// Pass nil fields to use SearchFields.
//
// The result set is unbounded — a broad JQL over a large project returns everything. Narrow the JQL
// rather than relying on a limit.
//
// ⚠️ Search reads an index that is only eventually consistent. After a write, a search can return
// stale data or miss the issue entirely, for anything from seconds to minutes. If you are searching
// for issues you just created or changed, use SearchReconciled with their IDs, or read them directly
// with GetIssue/GetIssues, which are strongly consistent.
func (c *Client) Search(ctx context.Context, jql string, fields []string) ([]Issue, error) {
	result, err := c.search(ctx, jql, fields, nil)
	return result.Issues, err
}

// SearchResult is a search's issues plus anything Jira wanted to say about the query itself.
type SearchResult struct {
	Issues []Issue
	// Warnings are Jira's complaints about the JQL — an unrecognised field, a value it could not
	// resolve. A warned query still returns 200 with an empty result set, so without these a JQL that
	// matched nothing *because it was wrong* is indistinguishable from one that legitimately did.
	Warnings []string
}

// SearchReconciled is Search with read-after-write consistency for specific issues.
//
// Pass the IDs of issues you just wrote. Jira reconciles those before answering, so a search run
// immediately after a create or update sees them. Consistency is guaranteed only for the IDs given —
// the rest of the result set is still eventually consistent. Jira accepts at most 50.
//
// The IDs are numeric issue IDs, not keys. Issue.ID carries them.
func (c *Client) SearchReconciled(ctx context.Context, jql string, fields []string, issueIDs []string) (SearchResult, error) {
	if len(issueIDs) > maxReconcileIssues {
		return SearchResult{}, fmt.Errorf("%w: at most %d issues can be reconciled, got %d",
			ErrInvalidArgument, maxReconcileIssues, len(issueIDs))
	}
	return c.search(ctx, jql, fields, issueIDs)
}

func (c *Client) search(ctx context.Context, jql string, fields, reconcileIDs []string) (SearchResult, error) {
	if strings.TrimSpace(jql) == "" {
		return SearchResult{}, fmt.Errorf("%w: jql cannot be empty", ErrInvalidArgument)
	}
	if len(fields) == 0 {
		fields = SearchFields
	}

	var result SearchResult
	seenWarning := map[string]bool{}
	pageToken := ""
	for {
		query := url.Values{}
		query.Set("jql", jql)
		query.Set("maxResults", strconv.Itoa(c.pageSize))
		query.Set("fields", strings.Join(fields, ","))
		// Reconciliation is re-sent on every page: it qualifies the query, not one response.
		for _, id := range reconcileIDs {
			query.Add("reconcileIssues", id)
		}
		if pageToken != "" {
			query.Set("nextPageToken", pageToken)
		}

		body, err := c.do(ctx, "GET", buildPath("/search/jql", query), nil)
		if err != nil {
			return SearchResult{}, err
		}

		var page searchResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return SearchResult{}, fmt.Errorf("decode search response: %w", err)
		}
		for _, raw := range page.Issues {
			result.Issues = append(result.Issues, raw.toIssue())
		}
		for _, warning := range page.warnings() {
			if seenWarning[warning] == false {
				seenWarning[warning] = true
				result.Warnings = append(result.Warnings, warning)
			}
		}

		if page.IsLast == true || page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return result, nil
}

// GetIssue fetches a single issue by key or ID.
func (c *Client) GetIssue(ctx context.Context, key string, fields []string) (Issue, error) {
	if key == "" {
		return Issue{}, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}
	if len(fields) == 0 {
		fields = SearchFields
	}

	query := url.Values{}
	query.Set("fields", strings.Join(fields, ","))

	body, err := c.do(ctx, "GET", buildPath("/issue/"+url.PathEscape(key), query), nil)
	if err != nil {
		return Issue{}, err
	}

	var raw rawIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		return Issue{}, fmt.Errorf("decode issue %s: %w", key, err)
	}
	return raw.toIssue(), nil
}

// bulkFetchChunk is Jira's cap on issues per /issue/bulkfetch request.
const bulkFetchChunk = 100

// GetIssues fetches issues by key or ID in batches, which is far cheaper than one request each.
//
// It reads issues directly rather than searching for them, so unlike Search it is strongly
// consistent: an issue created a moment ago is returned. Keys are matched case-insensitively, and a
// key that has been moved resolves to the issue's current identity.
//
// Keys that do not exist, or that the caller cannot see, are simply absent from the map — Jira
// reports them per-key rather than failing the batch, and the two cases are not distinguishable.
func (c *Client) GetIssues(ctx context.Context, keys []string, fields []string) (map[string]Issue, error) {
	if len(fields) == 0 {
		fields = SearchFields
	}

	issues := make(map[string]Issue, len(keys))
	for start := 0; start < len(keys); start += bulkFetchChunk {
		end := start + bulkFetchChunk
		if end > len(keys) {
			end = len(keys)
		}

		payload := map[string]any{
			"issueIdsOrKeys": keys[start:end],
			"fields":         fields,
		}
		body, err := c.do(ctx, "POST", apiBase+"/issue/bulkfetch", payload)
		if err != nil {
			return nil, err
		}

		var decoded struct {
			Issues []rawIssue `json:"issues"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			return nil, fmt.Errorf("decode bulk fetch response: %w", err)
		}
		for _, raw := range decoded.Issues {
			issue := raw.toIssue()
			issues[issue.Key] = issue
		}
	}
	return issues, nil
}

// CreateIssueInput describes a new issue.
type CreateIssueInput struct {
	ProjectKey  string
	IssueType   string
	Summary     string
	Description *ADFDoc
	ReporterID  string
	AssigneeID  string
	Priority    string
	Labels      []string
	// CustomFields is merged into the fields object as-is, e.g. {"customfield_10004": 3}.
	CustomFields map[string]any
}

// CreateIssue opens an issue and returns its key. In dry-run mode it returns "DRY-RUN" and creates
// nothing.
func (c *Client) CreateIssue(ctx context.Context, input CreateIssueInput) (string, error) {
	if input.ProjectKey == "" || input.IssueType == "" || strings.TrimSpace(input.Summary) == "" {
		return "", fmt.Errorf("%w: project, issue type and summary are required", ErrInvalidArgument)
	}
	if len(input.Summary) > SummaryMaxChars {
		return "", fmt.Errorf("%w: summary is %d chars, over the %d limit",
			ErrInvalidArgument, len(input.Summary), SummaryMaxChars)
	}
	if c.skipMutation("CreateIssue", input.ProjectKey, input.Summary) == true {
		return "DRY-RUN", nil
	}

	fields := map[string]any{
		"project":   map[string]string{"key": input.ProjectKey},
		"issuetype": map[string]string{"name": input.IssueType},
		"summary":   input.Summary,
	}
	if input.Description != nil {
		fields["description"] = input.Description
	}
	if input.ReporterID != "" {
		fields["reporter"] = map[string]string{"id": input.ReporterID}
	}
	if input.AssigneeID != "" {
		fields["assignee"] = map[string]string{"id": input.AssigneeID}
	}
	if input.Priority != "" {
		fields["priority"] = map[string]string{"name": input.Priority}
	}
	if len(input.Labels) > 0 {
		fields["labels"] = input.Labels
	}
	for name, value := range input.CustomFields {
		fields[name] = value
	}

	body, err := c.do(ctx, "POST", apiBase+"/issue", map[string]any{"fields": fields})
	if err != nil {
		return "", err
	}

	var created struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return "", fmt.Errorf("decode create response: %w", err)
	}
	return created.Key, nil
}

// UpdateSummary replaces an issue's summary.
// DeleteIssue permanently removes an issue. Jira does not undo this, so it exists mainly for tests
// that file real tickets and have to clean up after themselves.
func (c *Client) DeleteIssue(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}
	if c.skipMutation("DeleteIssue", key) == true {
		return nil
	}
	_, err := c.do(ctx, "DELETE", apiBase+"/issue/"+url.PathEscape(key), nil)
	return err
}

func (c *Client) UpdateSummary(ctx context.Context, key, summary string) error {
	if key == "" || strings.TrimSpace(summary) == "" {
		return fmt.Errorf("%w: key and summary are required", ErrInvalidArgument)
	}
	if len(summary) > SummaryMaxChars {
		return fmt.Errorf("%w: summary is %d chars, over the %d limit",
			ErrInvalidArgument, len(summary), SummaryMaxChars)
	}
	if c.skipMutation("UpdateSummary", key, summary) == true {
		return nil
	}
	return c.updateFields(ctx, key, map[string]any{"summary": summary})
}

// UpdateCustomField sets a single custom field, e.g. UpdateCustomField(ctx, key,
// "customfield_10004", 3).
func (c *Client) UpdateCustomField(ctx context.Context, key, fieldID string, value any) error {
	if key == "" || fieldID == "" {
		return fmt.Errorf("%w: key and field id are required", ErrInvalidArgument)
	}
	if c.skipMutation("UpdateCustomField", key, fieldID, value) == true {
		return nil
	}
	return c.updateFields(ctx, key, map[string]any{fieldID: value})
}

// updateFields PUTs a fields patch.
func (c *Client) updateFields(ctx context.Context, key string, fields map[string]any) error {
	_, err := c.do(ctx, "PUT", apiBase+"/issue/"+url.PathEscape(key),
		map[string]any{"fields": fields})
	return err
}

// SetAssignee assigns an issue. An empty accountID unassigns it.
func (c *Client) SetAssignee(ctx context.Context, key, accountID string) error {
	if key == "" {
		return fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}
	if c.skipMutation("SetAssignee", key, accountID) == true {
		return nil
	}

	// A nil accountId is how Jira expresses "unassigned"; an empty string is rejected.
	var accountIDValue *string
	if accountID != "" {
		accountIDValue = &accountID
	}
	_, err := c.do(ctx, "PUT", apiBase+"/issue/"+url.PathEscape(key)+"/assignee",
		map[string]any{"accountId": accountIDValue})
	return err
}

// AddLabel adds a label to an issue.
func (c *Client) AddLabel(ctx context.Context, key, label string) error {
	return c.changeLabel(ctx, key, label, "add")
}

// RemoveLabel removes a label from an issue.
func (c *Client) RemoveLabel(ctx context.Context, key, label string) error {
	return c.changeLabel(ctx, key, label, "remove")
}

func (c *Client) changeLabel(ctx context.Context, key, label, operation string) error {
	if key == "" || label == "" {
		return fmt.Errorf("%w: key and label are required", ErrInvalidArgument)
	}
	// Jira labels cannot contain spaces; it rejects the edit rather than normalising.
	if strings.ContainsAny(label, " \t\n") == true {
		return fmt.Errorf("%w: label %q contains whitespace", ErrInvalidArgument, label)
	}
	if c.skipMutation("ChangeLabel", key, operation, label) == true {
		return nil
	}

	_, err := c.do(ctx, "PUT", apiBase+"/issue/"+url.PathEscape(key), map[string]any{
		"update": map[string]any{
			"labels": []map[string]string{{operation: label}},
		},
	})
	return err
}

// AddComment posts a comment built from an ADF document.
func (c *Client) AddComment(ctx context.Context, key string, doc *ADFDoc) error {
	if key == "" {
		return fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}
	if doc == nil || len(doc.Content) == 0 {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, errEmptyDoc)
	}
	if c.skipMutation("AddComment", key) == true {
		return nil
	}

	_, err := c.do(ctx, "POST", apiBase+"/issue/"+url.PathEscape(key)+"/comment",
		map[string]any{"body": doc})
	return err
}

// AddTextComment posts a plain-text comment. "[@accountId]" becomes a real mention.
func (c *Client) AddTextComment(ctx context.Context, key, text string) error {
	doc, err := TextDoc(text)
	if err != nil {
		return err
	}
	return c.AddComment(ctx, key, doc)
}

// Transition moves an issue through a workflow transition, identified by its transition ID (not the
// destination status ID — they differ, and using the wrong one silently 400s).
// Use Transitions to discover the available IDs for an issue.
func (c *Client) Transition(ctx context.Context, key, transitionID string) error {
	return c.TransitionWithComment(ctx, key, transitionID, nil)
}

// TransitionWithComment moves an issue through a transition and posts a comment as part of the same
// request. Jira applies both atomically, so the comment cannot be orphaned by a transition that is
// rejected — which is the reason to prefer it over a transition followed by a separate AddComment
// when the comment explains the move. Pass a nil doc for no comment.
func (c *Client) TransitionWithComment(ctx context.Context, key, transitionID string, comment *ADFDoc) error {
	if key == "" || transitionID == "" {
		return fmt.Errorf("%w: key and transition id are required", ErrInvalidArgument)
	}
	if c.skipMutation("Transition", key, transitionID) == true {
		return nil
	}

	payload := map[string]any{"transition": map[string]string{"id": transitionID}}
	if comment != nil {
		payload["update"] = map[string]any{
			"comment": []map[string]any{{"add": map[string]any{"body": comment}}},
		}
	}

	_, err := c.do(ctx, "POST", apiBase+"/issue/"+url.PathEscape(key)+"/transitions", payload)
	return err
}

// TransitionOption is a workflow transition available on an issue.
type TransitionOption struct {
	ID         string
	Name       string
	ToStatus   string
	ToStatusID string
}

// Transitions lists the transitions currently available on an issue. Availability is workflow- and
// permission-dependent, so resolving a transition by name at runtime is safer than hardcoding an ID.
func (c *Client) Transitions(ctx context.Context, key string) ([]TransitionOption, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "GET", apiBase+"/issue/"+url.PathEscape(key)+"/transitions", nil)
	if err != nil {
		return nil, err
	}

	var decoded struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode transitions for %s: %w", key, err)
	}

	options := make([]TransitionOption, 0, len(decoded.Transitions))
	for _, transition := range decoded.Transitions {
		options = append(options, TransitionOption{
			ID:         transition.ID,
			Name:       transition.Name,
			ToStatus:   transition.To.Name,
			ToStatusID: transition.To.ID,
		})
	}
	return options, nil
}

// TransitionByName resolves a transition by its display name and applies it, so callers can say
// "Won't Do" instead of depending on a numeric ID that differs between Jira sites.
func (c *Client) TransitionByName(ctx context.Context, key, name string) error {
	options, err := c.Transitions(ctx, key)
	if err != nil {
		return err
	}
	for _, option := range options {
		if strings.EqualFold(option.Name, name) == true {
			return c.Transition(ctx, key, option.ID)
		}
	}
	return fmt.Errorf("%w: transition %q not available on %s", ErrInvalidArgument, name, key)
}

// TransitionByNameWithComment resolves a transition by name and posts a comment with it.
func (c *Client) TransitionByNameWithComment(ctx context.Context, key, name string, comment *ADFDoc) error {
	options, err := c.Transitions(ctx, key)
	if err != nil {
		return err
	}
	for _, option := range options {
		if strings.EqualFold(option.Name, name) == true {
			return c.TransitionWithComment(ctx, key, option.ID, comment)
		}
	}
	return fmt.Errorf("%w: transition %q not available on %s", ErrInvalidArgument, name, key)
}

// LinkIssues links two issues. The link reads "<outwardKey> <type's outward description> <inwardKey>"
// — for LinkDuplicate pass the duplicate as outwardKey and the surviving issue as inwardKey, so the
// duplicate reads "duplicates ORIGIN" and the origin reads "is duplicated by DUPLICATE".
func (c *Client) LinkIssues(ctx context.Context, linkType, inwardKey, outwardKey string) error {
	return c.CreateLink(ctx, LinkInput{TypeName: linkType, InwardKey: inwardKey, OutwardKey: outwardKey})
}

// LinkInput describes an issue link. Give it either a TypeName or a TypeID — sites rename link types,
// so an ID is the stable reference and a name is the readable one.
type LinkInput struct {
	TypeName string
	TypeID   string
	// InwardKey and OutwardKey read as "<outward> <type outward> <inward>".
	InwardKey  string
	OutwardKey string
	// Comment, when set, is posted on the link in the same request.
	Comment *ADFDoc
}

// CreateLink links two issues, optionally by type ID and optionally with a comment.
func (c *Client) CreateLink(ctx context.Context, input LinkInput) error {
	if input.TypeName == "" && input.TypeID == "" {
		return fmt.Errorf("%w: a link type name or id is required", ErrInvalidArgument)
	}
	if input.InwardKey == "" || input.OutwardKey == "" {
		return fmt.Errorf("%w: both issue keys are required", ErrInvalidArgument)
	}
	if input.InwardKey == input.OutwardKey {
		return fmt.Errorf("%w: cannot link %s to itself", ErrInvalidArgument, input.InwardKey)
	}
	if c.skipMutation("CreateLink", input.TypeName+input.TypeID, input.InwardKey, input.OutwardKey) == true {
		return nil
	}

	linkType := map[string]string{}
	if input.TypeID != "" {
		linkType["id"] = input.TypeID
	} else {
		linkType["name"] = input.TypeName
	}

	payload := map[string]any{
		"type":         linkType,
		"inwardIssue":  map[string]string{"key": input.InwardKey},
		"outwardIssue": map[string]string{"key": input.OutwardKey},
	}
	if input.Comment != nil {
		payload["comment"] = map[string]any{"body": input.Comment}
	}

	_, err := c.do(ctx, "POST", apiBase+"/issueLink", payload)
	return err
}
