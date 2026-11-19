package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// maxReconcileIssues is Jira's cap on the reconcileIssues parameter.
const maxReconcileIssues = 50

// maxSearchQueryBytes is the longest query string a search is sent with. Past it the search goes in a
// POST body: Jira serves the same search on both verbs, and a URL of several kilobytes is turned away
// by a gateway before Jira ever sees it.
const maxSearchQueryBytes = 6000

// SearchQuery describes a JQL search. The zero value beyond JQL is the common case.
type SearchQuery struct {
	// JQL is the query. Required.
	JQL string
	// Fields limits what is fetched. Empty uses SearchFields, which is what you almost always want —
	// a full-fidelity fetch of a few thousand issues is dramatically larger and slower.
	Fields []string
	// ReconcileIssues names numeric issue IDs (Issue.ID, not keys) that Jira must reconcile before
	// answering, giving read-after-write consistency for those issues only. Jira accepts at most 50.
	// Leave empty unless you are searching for something you just wrote.
	ReconcileIssues []string
	// Limit stops the search once this many issues are in, sparing the requests for the rest. Zero
	// returns every match. Give the JQL an ORDER BY so the issues kept are the ones wanted.
	Limit int
}

// SearchResult is a search's issues plus anything Jira wanted to say about the query itself.
type SearchResult struct {
	Issues []Issue
	// Warnings are Jira's complaints about the JQL — an unrecognised field, a value it could not
	// resolve. A warned query still returns 200 with an empty result set, so without these a JQL that
	// matched nothing *because it was wrong* is indistinguishable from one that legitimately did.
	// Worth checking whenever a search returns fewer issues than expected.
	Warnings []string
}

// Search runs a JQL query and returns every matching issue, following Jira's pagination to the end.
//
// The result set is unbounded unless SearchQuery.Limit is set — a broad JQL over a large project
// returns everything. A query too long for a URL is sent as a POST instead; the result is the same.
//
// ⚠️ Search reads an index that is only eventually consistent. After a write, it can return stale
// data or miss the issue entirely, for anything from seconds to minutes. To search for issues you
// just wrote, set SearchQuery.ReconcileIssues; to read known issues, use GetIssue or GetIssues,
// which bypass the index entirely.
func (c *Client) Search(ctx context.Context, query SearchQuery) (SearchResult, error) {
	if len(query.ReconcileIssues) > maxReconcileIssues {
		return SearchResult{}, fmt.Errorf("%w: at most %d issues can be reconciled, got %d",
			ErrInvalidArgument, maxReconcileIssues, len(query.ReconcileIssues))
	}
	if query.Limit < 0 {
		return SearchResult{}, fmt.Errorf("%w: limit cannot be negative, got %d", ErrInvalidArgument, query.Limit)
	}
	reconcileIDs := make([]int64, 0, len(query.ReconcileIssues))
	for _, id := range query.ReconcileIssues {
		numeric, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return SearchResult{}, fmt.Errorf("%w: reconcile id %q is not a numeric issue id (use Issue.ID, not the key)",
				ErrInvalidArgument, id)
		}
		reconcileIDs = append(reconcileIDs, numeric)
	}
	return c.search(ctx, query.JQL, query.Fields, reconcileIDs, query.Limit)
}

// SearchIssues is Search for the common case: a query, and only the issues back.
//
// It discards SearchResult.Warnings, so a JQL Jira objected to looks the same as one that matched
// nothing. Reach for Search when an empty result would be surprising.
func (c *Client) SearchIssues(ctx context.Context, jql string, fields []string) ([]Issue, error) {
	result, err := c.Search(ctx, SearchQuery{JQL: jql, Fields: fields})
	return result.Issues, err
}

func (c *Client) search(ctx context.Context, jql string, fields []string, reconcileIDs []int64, limit int) (SearchResult, error) {
	if strings.TrimSpace(jql) == "" {
		return SearchResult{}, fmt.Errorf("%w: jql cannot be empty", ErrInvalidArgument)
	}
	if len(fields) == 0 {
		fields = SearchFields
	}

	var result SearchResult
	seenWarning := map[string]bool{}
	seenToken := map[string]bool{}
	pageToken := ""
	for {
		pageSize := c.pageSize
		if remaining := limit - len(result.Issues); limit > 0 && remaining < pageSize {
			pageSize = remaining
		}

		body, err := c.searchPage(ctx, jql, fields, reconcileIDs, pageToken, pageSize)
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

		if limit > 0 && len(result.Issues) >= limit {
			// Jira sizes pages as it sees fit, so the cut is made here rather than trusted to it.
			result.Issues = result.Issues[:limit]
			break
		}
		if page.IsLast == true || page.NextPageToken == "" {
			break
		}
		// A token Jira has already issued leads back to a page already read, and from there round
		// again without end.
		if seenToken[page.NextPageToken] == true {
			return SearchResult{}, fmt.Errorf("%w: search returned a page token it had already issued",
				ErrPaginationStalled)
		}
		seenToken[page.NextPageToken] = true
		pageToken = page.NextPageToken
	}
	return result, nil
}

// searchPage fetches one page of a search, in the URL when the query fits there and in a POST body
// when it does not.
func (c *Client) searchPage(ctx context.Context, jql string, fields []string, reconcileIDs []int64, pageToken string, pageSize int) ([]byte, error) {
	query := url.Values{}
	query.Set("jql", jql)
	query.Set("maxResults", strconv.Itoa(pageSize))
	query.Set("fields", strings.Join(fields, ","))
	// Reconciliation is re-sent on every page: it qualifies the query, not one response.
	for _, id := range reconcileIDs {
		query.Add("reconcileIssues", strconv.FormatInt(id, 10))
	}
	if pageToken != "" {
		query.Set("nextPageToken", pageToken)
	}
	if len(query.Encode()) <= maxSearchQueryBytes {
		return c.do(ctx, "GET", buildPath("/search/jql", query), nil)
	}

	payload := map[string]any{"jql": jql, "maxResults": pageSize, "fields": fields}
	if len(reconcileIDs) > 0 {
		payload["reconcileIssues"] = reconcileIDs
	}
	if pageToken != "" {
		payload["nextPageToken"] = pageToken
	}
	return c.postReplayable(ctx, apiBase+"/search/jql", payload)
}
