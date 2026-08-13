package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// jqlValidationStrict asks /jql/parse to report anything Jira would reject, rather than downgrading
// unknown fields and values to warnings.
const jqlValidationStrict = "strict"

// ApproximateCount returns how many issues a JQL query matches, without fetching them.
//
// This exists because /search/jql no longer returns total. Without it the only way to count a result
// set is to page through the whole thing, which for a broad query means thousands of issues fetched
// and discarded — so a cheap "how big is this before I run it" check was simply unavailable.
//
// Two caveats worth honouring. The number is an estimate drawn from the search index, which is
// eventually consistent: immediately after a write it can be stale, and it can differ slightly from
// the row count a full page-through returns. And the JQL must be bounded — Jira rejects a query that
// is only an order by clause, since it would have to count the entire site.
func (c *Client) ApproximateCount(ctx context.Context, jql string) (int, error) {
	if strings.TrimSpace(jql) == "" {
		return 0, fmt.Errorf("%w: jql cannot be empty", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "POST", apiBase+"/search/approximate-count",
		map[string]any{"jql": jql})
	if err != nil {
		return 0, err
	}

	var decoded struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return 0, fmt.Errorf("decode approximate count: %w", err)
	}
	return decoded.Count, nil
}

// JQLResult is Jira's verdict on one query. A query is acceptable when Errors is empty; Warnings
// flag things Jira tolerated but that usually mean the query does not match what its author thought.
type JQLResult struct {
	Query    string
	Errors   []string
	Warnings []string
}

// ValidateJQL checks queries without running them, returning one result per query in the order
// given.
//
// /search/jql dropped the validateQuery parameter, so executing a query is otherwise the only way to
// find out whether it parses — and a query with an unresolvable field or value comes back 200 with
// an empty result set, indistinguishable from one that legitimately matched nothing. That makes this
// the natural partner to WithDryRun: a dry run can validate every query in its plan up front and
// report the broken ones, instead of producing a plan full of silently-empty steps.
//
// The endpoint is batch-capable and anonymous — it parses text and touches no issues — so validating
// a whole plan costs one request.
func (c *Client) ValidateJQL(ctx context.Context, jql ...string) ([]JQLResult, error) {
	if len(jql) == 0 {
		return nil, fmt.Errorf("%w: at least one query is required", ErrInvalidArgument)
	}
	for index, query := range jql {
		if strings.TrimSpace(query) == "" {
			return nil, fmt.Errorf("%w: query %d is empty", ErrInvalidArgument, index)
		}
	}

	params := url.Values{}
	params.Set("validation", jqlValidationStrict)

	body, err := c.do(ctx, "POST", buildPath("/jql/parse", params),
		map[string]any{"queries": jql})
	if err != nil {
		return nil, err
	}

	var decoded struct {
		Queries []struct {
			Query    string   `json:"query"`
			Errors   []string `json:"errors"`
			Warnings []string `json:"warnings"`
		} `json:"queries"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode jql validation: %w", err)
	}

	results := make([]JQLResult, 0, len(decoded.Queries))
	for _, entry := range decoded.Queries {
		results = append(results, JQLResult{
			Query:    entry.Query,
			Errors:   entry.Errors,
			Warnings: entry.Warnings,
		})
	}
	return results, nil
}

// PermissionScope selects what MyPermissions asks about. Set at most one field: leaving both empty
// asks about the site globally, and Jira rejects a request carrying more than one context.
//
// Prefer IssueKey. See MyPermissions for why a project answer is not a promise.
type PermissionScope struct {
	ProjectKey string
	IssueKey   string
}

// MyPermissions reports whether the authenticated account holds each named permission, keyed by
// permission key ("EDIT_ISSUES", "ADD_COMMENTS", "TRANSITION_ISSUES", "CREATE_ISSUES").
//
// ⚠️ A project-scoped answer is optimistic, not authoritative. Atlassian documents that this can
// report a permission as held in a project context while the account does not actually hold it for
// any given issue in that project — issue-level security schemes and permission schemes keyed on
// issue properties are both evaluated per issue and cannot be answered project-wide. Only an
// IssueKey-scoped check tells you what will really happen. Treat a project-scoped true as "probably,
// worth attempting" and never as a reason to skip error handling on the write itself.
//
// The permissions parameter is required — Jira 400s a request without it rather than returning
// everything — so an empty keys list is rejected here.
func (c *Client) MyPermissions(ctx context.Context, scope PermissionScope, keys ...string) (map[string]bool, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: at least one permission key is required", ErrInvalidArgument)
	}
	for index, key := range keys {
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("%w: permission key %d is empty", ErrInvalidArgument, index)
		}
	}
	if scope.ProjectKey != "" && scope.IssueKey != "" {
		return nil, fmt.Errorf("%w: a permission scope takes one context, got both a project and an issue",
			ErrInvalidArgument)
	}

	params := url.Values{}
	params.Set("permissions", strings.Join(keys, ","))
	if scope.ProjectKey != "" {
		params.Set("projectKey", scope.ProjectKey)
	}
	if scope.IssueKey != "" {
		params.Set("issueKey", scope.IssueKey)
	}

	body, err := c.do(ctx, "GET", buildPath("/mypermissions", params), nil)
	if err != nil {
		return nil, err
	}

	// Jira keys the response by permission rather than returning an array, so the decode target is a
	// map and the order the caller asked in is not preserved.
	var decoded struct {
		Permissions map[string]struct {
			HavePermission bool `json:"havePermission"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode permissions: %w", err)
	}

	held := make(map[string]bool, len(decoded.Permissions))
	for key, permission := range decoded.Permissions {
		held[key] = permission.HavePermission
	}
	return held, nil
}
