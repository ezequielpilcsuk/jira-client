package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

const (
	// changelogPageSize is the page size requested per changelog page; the endpoint's own default is 100.
	changelogPageSize = 100

	// changelogBulkChunk and changelogMaxFields are Jira's caps on one /changelog/bulkfetch request.
	changelogBulkChunk = 1000
	changelogMaxFields = 10
)

// ChangeItem is one field's before-and-after within a single change.
//
// From and To carry Jira's internal identifiers (a status id, an account id) while FromString and
// ToString carry what a person would recognise. A side is empty when the field held nothing at that
// point: an empty From means the field was being set for the first time, an empty To that it was
// cleared.
type ChangeItem struct {
	Field      string
	FieldID    string
	FieldType  string
	From       string
	FromString string
	To         string
	ToString   string
}

// ChangelogEntry is one edit to an issue: everything one person changed in one action.
type ChangelogEntry struct {
	ID         string
	AuthorID   string
	AuthorName string
	Created    time.Time
	Items      []ChangeItem
}

// Changelog returns an issue's complete history, following pagination to the end.
//
// This endpoint is the only complete source of it: expand=changelog on an ordinary issue read is
// capped at 20 entries, and an issue that has been through a workflow a few times exceeds that
// without any sign in the response that history was dropped.
//
// Paging follows isLast rather than counting pages from total, which Jira warns can change between
// requests as the issue is edited underneath the traversal.
func (c *Client) Changelog(ctx context.Context, key string) ([]ChangelogEntry, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}

	var entries []ChangelogEntry
	startAt := 0
	for {
		params := url.Values{}
		params.Set("startAt", strconv.Itoa(startAt))
		params.Set("maxResults", strconv.Itoa(changelogPageSize))

		body, err := c.do(ctx, "GET", buildPath("/issue/"+url.PathEscape(key)+"/changelog", params), nil)
		if err != nil {
			return nil, err
		}

		var page struct {
			Values []rawChangelog `json:"values"`
			IsLast bool           `json:"isLast"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode changelog for %s: %w", key, err)
		}
		for _, raw := range page.Values {
			entries = append(entries, raw.toEntry())
		}

		// The empty-page guard matters on its own: a page that returns nothing without setting isLast
		// would otherwise re-request the same startAt forever.
		if page.IsLast == true || len(page.Values) == 0 {
			break
		}
		startAt += len(page.Values)
	}
	return entries, nil
}

// Changelogs fetches many issues' histories together, chunked at Jira's cap of 1000 issues per
// request and following the endpoint's page token to the end.
//
// The result is keyed by numeric issue id — what the endpoint reports back — and not by the key you
// passed in, so a caller starting from keys has to map them itself. Issue.ID carries the id.
//
// fieldIDs narrows the history to particular fields, e.g. "status" or "assignee". Jira accepts at
// most 10; an empty slice returns every field. Issues with no history at all are simply absent from
// the map.
func (c *Client) Changelogs(ctx context.Context, keys, fieldIDs []string) (map[string][]ChangelogEntry, error) {
	if len(fieldIDs) > changelogMaxFields {
		return nil, fmt.Errorf("%w: at most %d field ids can be requested, got %d",
			ErrInvalidArgument, changelogMaxFields, len(fieldIDs))
	}

	histories := make(map[string][]ChangelogEntry, len(keys))
	for start := 0; start < len(keys); start += changelogBulkChunk {
		end := start + changelogBulkChunk
		if end > len(keys) {
			end = len(keys)
		}

		pageToken := ""
		for {
			payload := map[string]any{
				"issueIdsOrKeys": keys[start:end],
				"maxResults":     changelogPageSize,
			}
			if len(fieldIDs) > 0 {
				payload["fieldIds"] = fieldIDs
			}
			if pageToken != "" {
				payload["nextPageToken"] = pageToken
			}

			body, err := c.do(ctx, "POST", apiBase+"/changelog/bulkfetch", payload)
			if err != nil {
				return nil, err
			}

			var page struct {
				IssueChangeLogs []struct {
					IssueID         string         `json:"issueId"`
					ChangeHistories []rawChangelog `json:"changeHistories"`
				} `json:"issueChangeLogs"`
				NextPageToken string `json:"nextPageToken"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				return nil, fmt.Errorf("decode bulk changelog response: %w", err)
			}
			for _, issue := range page.IssueChangeLogs {
				for _, raw := range issue.ChangeHistories {
					histories[issue.IssueID] = append(histories[issue.IssueID], raw.toEntry())
				}
			}

			if page.NextPageToken == "" {
				break
			}
			pageToken = page.NextPageToken
		}
	}
	return histories, nil
}

// rawChangelog mirrors Jira's changelog payload. Author is a pointer because a change made by an
// automation rule or a deleted account arrives with no author at all.
type rawChangelog struct {
	ID      string   `json:"id"`
	Author  *rawUser `json:"author"`
	Created string   `json:"created"`
	Items   []struct {
		Field   string `json:"field"`
		FieldID string `json:"fieldId"`
		// Jira really does spell this one lowercase, alone among its neighbours. Do not "fix" it.
		FieldType  string `json:"fieldtype"`
		From       string `json:"from"`
		FromString string `json:"fromString"`
		To         string `json:"to"`
		ToString   string `json:"toString"`
	} `json:"items"`
}

// toEntry flattens a raw changelog, tolerating an absent author and unparseable timestamp.
func (r rawChangelog) toEntry() ChangelogEntry {
	entry := ChangelogEntry{
		ID:      r.ID,
		Created: parseJiraTime(r.Created),
		Items:   make([]ChangeItem, 0, len(r.Items)),
	}
	if r.Author != nil {
		entry.AuthorID, entry.AuthorName = r.Author.AccountID, r.Author.DisplayName
	}
	for _, item := range r.Items {
		entry.Items = append(entry.Items, ChangeItem{
			Field:      item.Field,
			FieldID:    item.FieldID,
			FieldType:  item.FieldType,
			From:       item.From,
			FromString: item.FromString,
			To:         item.To,
			ToString:   item.ToString,
		})
	}
	return entry
}
