package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SummaryMaxChars is Jira's hard limit on the summary field. Exceeding it rejects the whole edit, so
// UpdateSummary refuses rather than sending a request that cannot succeed.
const SummaryMaxChars = 255

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
		body, err := c.postReplayable(ctx, apiBase+"/issue/bulkfetch", payload)
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

// CreateIssue opens an issue and returns its key. In dry-run mode it returns DryRunID and creates
// nothing.
func (c *Client) CreateIssue(ctx context.Context, input CreateIssueInput) (string, error) {
	if input.ProjectKey == "" || input.IssueType == "" || strings.TrimSpace(input.Summary) == "" {
		return "", fmt.Errorf("%w: project, issue type and summary are required", ErrInvalidArgument)
	}
	if err := validateSummaryLength(input.Summary); err != nil {
		return "", err
	}
	if c.skipMutation("CreateIssue", input.ProjectKey, input.Summary) == true {
		return DryRunID, nil
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

// UpdateSummary replaces an issue's summary. Jira rejects anything over SummaryMaxChars outright,
// taking the whole edit with it, so an over-long summary is refused here instead.
func (c *Client) UpdateSummary(ctx context.Context, key, summary string) error {
	if key == "" || strings.TrimSpace(summary) == "" {
		return fmt.Errorf("%w: key and summary are required", ErrInvalidArgument)
	}
	if err := validateSummaryLength(summary); err != nil {
		return err
	}
	if c.skipMutation("UpdateSummary", key, summary) == true {
		return nil
	}
	return c.updateFields(ctx, key, map[string]any{"summary": summary})
}

// validateSummaryLength refuses a summary over Jira's limit, which counts characters rather than
// bytes.
func validateSummaryLength(summary string) error {
	if size := utf8.RuneCountInString(summary); size > SummaryMaxChars {
		return fmt.Errorf("%w: summary is %d chars, over the %d limit",
			ErrInvalidArgument, size, SummaryMaxChars)
	}
	return nil
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
	if err := validateLabel(label); err != nil {
		return err
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

// validateLabel refuses a label Jira would reject. Labels cannot be empty or contain whitespace, and
// Jira fails the whole edit rather than normalising one.
func validateLabel(label string) error {
	if label == "" {
		return fmt.Errorf("%w: label cannot be empty", ErrInvalidArgument)
	}
	if strings.ContainsAny(label, " \t\n") == true {
		return fmt.Errorf("%w: label %q contains whitespace", ErrInvalidArgument, label)
	}
	return nil
}

// IssueUpdate describes several edits to one issue, sent as a single request.
type IssueUpdate struct {
	// Summary replaces the summary. Empty leaves it alone.
	Summary string
	// Fields are set as given, keyed by field ID: a custom field ("customfield_10004": 3), or a system
	// field in Jira's own shape ("priority": map[string]string{"name": "High"}).
	Fields map[string]any
	// AddLabels and RemoveLabels change the label set without replacing it, so labels someone else
	// added survive the edit.
	AddLabels    []string
	RemoveLabels []string
	// NotifyUsers controls whether watchers are emailed about the edit. Nil leaves Jira's default in
	// place, which is to notify. Turning it off needs the Administer Jira or Administer projects
	// permission.
	NotifyUsers *bool
}

// UpdateIssue applies several edits to one issue in a single request.
//
// Jira limits writes per issue, so an automation that sets a field, renames the ticket and adjusts
// its labels one call at a time spends three of that allowance where this spends one — and either all
// of it is applied or none of it is.
func (c *Client) UpdateIssue(ctx context.Context, key string, update IssueUpdate) error {
	if key == "" {
		return fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}

	fields := make(map[string]any, len(update.Fields)+1)
	for name, value := range update.Fields {
		fields[name] = value
	}
	if update.Summary != "" {
		if _, twice := fields["summary"]; twice == true {
			return fmt.Errorf("%w: the summary is given both as Summary and in Fields", ErrInvalidArgument)
		}
		if strings.TrimSpace(update.Summary) == "" {
			return fmt.Errorf("%w: summary cannot be blank", ErrInvalidArgument)
		}
		if err := validateSummaryLength(update.Summary); err != nil {
			return err
		}
		fields["summary"] = update.Summary
	}

	labelChanges := make([]map[string]string, 0, len(update.AddLabels)+len(update.RemoveLabels))
	for _, label := range update.AddLabels {
		if err := validateLabel(label); err != nil {
			return err
		}
		labelChanges = append(labelChanges, map[string]string{"add": label})
	}
	for _, label := range update.RemoveLabels {
		if err := validateLabel(label); err != nil {
			return err
		}
		labelChanges = append(labelChanges, map[string]string{"remove": label})
	}
	// Jira rejects an edit that both sets a field and applies changes to it.
	if _, replaced := fields["labels"]; replaced == true && len(labelChanges) > 0 {
		return fmt.Errorf("%w: labels are both replaced through Fields and changed through AddLabels or RemoveLabels",
			ErrInvalidArgument)
	}
	if len(fields) == 0 && len(labelChanges) == 0 {
		return fmt.Errorf("%w: the update changes nothing", ErrInvalidArgument)
	}
	if c.skipMutation("UpdateIssue", key) == true {
		return nil
	}

	payload := map[string]any{}
	if len(fields) > 0 {
		payload["fields"] = fields
	}
	if len(labelChanges) > 0 {
		payload["update"] = map[string]any{"labels": labelChanges}
	}
	query := url.Values{}
	if update.NotifyUsers != nil {
		query.Set("notifyUsers", strconv.FormatBool(*update.NotifyUsers))
	}

	_, err := c.do(ctx, "PUT", buildPath("/issue/"+url.PathEscape(key), query), payload)
	return err
}
