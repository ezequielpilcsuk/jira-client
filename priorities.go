package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
)

// Priority is one entry in a site's priority scheme.
type Priority struct {
	ID   string
	Name string
	// Rank is the entry's position in the scheme, 0 being the most urgent.
	Rank int
}

// Priorities returns the site's priority scheme in rank order, most urgent first.
//
// Reach for this rather than comparing priority IDs. IDs are assigned in creation order, not in rank
// order, so any priority added after a scheme was customised carries a 10000-series ID regardless of
// where it actually sits. A site with the default scheme plus one added "Normal" ranks
// Blocker(1), Critical(2), Major(3), Normal(10000), Minor(4), Trivial(5) — an ID comparison there
// sorts Normal below Trivial and silently inverts any "take the most urgent" rule built on it.
func (c *Client) Priorities(ctx context.Context) ([]Priority, error) {
	body, err := c.do(ctx, "GET", apiBase+"/priority", nil)
	if err != nil {
		return nil, err
	}

	var decoded []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode priorities: %w", err)
	}
	if len(decoded) == 0 {
		return nil, fmt.Errorf("%w: site returned an empty priority scheme", ErrInvalidArgument)
	}

	priorities := make([]Priority, 0, len(decoded))
	for rank, entry := range decoded {
		priorities = append(priorities, Priority{ID: entry.ID, Name: entry.Name, Rank: rank})
	}
	return priorities, nil
}

// PriorityRanks returns each priority name mapped to its rank, 0 being the most urgent.
//
// Keyed by name because the name is the only value that works end to end: it is what the API accepts
// when setting a priority, and what Issue.Priority reports on a read.
func (c *Client) PriorityRanks(ctx context.Context) (map[string]int, error) {
	priorities, err := c.Priorities(ctx)
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
