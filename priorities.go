package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// Priority is one entry in a site's priority scheme.
type Priority struct {
	ID   string
	Name string
	// Rank is the entry's position in the order the site returned, 0 being the most urgent.
	Rank        int
	Description string
	IsDefault   bool
}

// PriorityQuery narrows the priority lookup.
//
// Jira Cloud allows more than one priority scheme per site, and schemes are assigned per project, so
// there is no single global ordering on a multi-scheme site. Pass a ProjectID to get the set that
// actually applies to a project.
type PriorityQuery struct {
	ProjectID string
	// Name filters by priority name, matched as a substring by Jira.
	Name string
}

// Priorities returns the site's priority scheme in the order the site returns it, most urgent first.
//
// Reach for this rather than comparing priority IDs. IDs are assigned in creation order, not in rank
// order, so any priority added after a scheme was customised carries a 10000-series ID regardless of
// where it actually sits. A site with the default scheme plus one added "Normal" ranks
// Blocker(1), Critical(2), Major(3), Normal(10000), Minor(4), Trivial(5) — an ID comparison there
// sorts Normal below Trivial and silently inverts any "take the most urgent" rule built on it.
//
// Jira does not document the ordering of this endpoint, so Rank reflects the returned order rather
// than a rank the API states. In practice that order is the scheme order; if you need a guarantee,
// the only endpoint carrying an explicit sequence requires Administer Jira.
func (c *Client) Priorities(ctx context.Context, query ...PriorityQuery) ([]Priority, error) {
	var (
		priorities []Priority
		startAt    int
		filter     PriorityQuery
	)
	if len(query) > 0 {
		filter = query[0]
	}

	for {
		params := url.Values{}
		params.Set("startAt", strconv.Itoa(startAt))
		params.Set("maxResults", strconv.Itoa(prioritySearchPageSize))
		if filter.ProjectID != "" {
			params.Set("projectId", filter.ProjectID)
		}
		if filter.Name != "" {
			params.Set("priorityName", filter.Name)
		}

		body, err := c.do(ctx, "GET", buildPath("/priority/search", params), nil)
		if err != nil {
			return nil, err
		}

		var page struct {
			IsLast bool `json:"isLast"`
			Values []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Description string `json:"description"`
				IsDefault   bool   `json:"isDefault"`
			} `json:"values"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode priorities: %w", err)
		}

		for _, entry := range page.Values {
			priorities = append(priorities, Priority{
				ID:          entry.ID,
				Name:        entry.Name,
				Rank:        len(priorities),
				Description: entry.Description,
				IsDefault:   entry.IsDefault,
			})
		}

		if page.IsLast == true || len(page.Values) == 0 {
			break
		}
		startAt += len(page.Values)
	}

	if len(priorities) == 0 {
		return nil, fmt.Errorf("%w: site returned an empty priority scheme", ErrInvalidArgument)
	}
	return priorities, nil
}

// PriorityRanks returns each priority name mapped to its rank, 0 being the most urgent.
//
// Keyed by name because the name is the only value that works end to end: it is what the API accepts
// when setting a priority, and what Issue.Priority reports on a read.
func (c *Client) PriorityRanks(ctx context.Context, query ...PriorityQuery) (map[string]int, error) {
	priorities, err := c.Priorities(ctx, query...)
	if err != nil {
		return nil, err
	}

	ranks := make(map[string]int, len(priorities))
	for _, priority := range priorities {
		if priority.Name == "" {
			continue
		}
		ranks[priority.Name] = priority.Rank
	}
	return ranks, nil
}
