package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// WatcherList is who is watching an issue.
type WatcherList struct {
	// Count is Jira's own watchCount, which is authoritative even when Watchers is empty.
	Count int
	// IsWatching reports whether the authenticated account is one of them.
	IsWatching bool
	// Watchers is the roster. Jira omits it unless the caller holds the View voters and watchers
	// permission, so an empty slice alongside a non-zero Count means "not allowed to see who", not
	// "nobody" — compare against Count rather than taking len(Watchers) as the number of watchers.
	Watchers []User
}

// Watchers lists the watchers on an issue.
//
// Watching is the durable way to make sure a person keeps seeing an issue: a watcher is notified of
// every subsequent change, whereas the /notify endpoint sends one email and is forgotten. Adding
// someone as a watcher is therefore usually the right move over notifying them.
func (c *Client) Watchers(ctx context.Context, key string) (WatcherList, error) {
	if key == "" {
		return WatcherList{}, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "GET", apiBase+"/issue/"+url.PathEscape(key)+"/watchers", nil)
	if err != nil {
		return WatcherList{}, err
	}

	var decoded struct {
		IsWatching bool      `json:"isWatching"`
		WatchCount int       `json:"watchCount"`
		Watchers   []rawUser `json:"watchers"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return WatcherList{}, fmt.Errorf("decode watchers for %s: %w", key, err)
	}

	list := WatcherList{
		Count:      decoded.WatchCount,
		IsWatching: decoded.IsWatching,
		Watchers:   make([]User, 0, len(decoded.Watchers)),
	}
	for _, watcher := range decoded.Watchers {
		list.Watchers = append(list.Watchers, User{
			AccountID:   watcher.AccountID,
			DisplayName: watcher.DisplayName,
			Active:      watcher.Active,
		})
	}
	return list, nil
}

// AddWatcher makes an account watch an issue. An empty accountID adds the authenticated account.
//
// Watching anyone other than yourself requires the Manage watcher list permission, and the whole
// feature depends on the site-wide "Allow users to watch issues" option — with it off, Jira rejects
// the call rather than silently doing nothing.
func (c *Client) AddWatcher(ctx context.Context, key, accountID string) error {
	if key == "" {
		return fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}
	if c.skipMutation("AddWatcher", key, accountID) == true {
		return nil
	}

	// The body is a bare JSON string — `"5b10ac8d..."`, not an object — which is what marshalling a Go
	// string produces. A nil payload sends no body at all, which is how Jira reads "add me".
	var payload any
	if accountID != "" {
		payload = accountID
	}

	_, err := c.do(ctx, "POST", apiBase+"/issue/"+url.PathEscape(key)+"/watchers", payload)
	return err
}

// RemoveWatcher stops an account watching an issue.
//
// The account is a query parameter here, not a body — the reverse of AddWatcher, and asymmetric for
// no reason other than Jira's history. It is also required: unlike AddWatcher there is no
// "remove me" shorthand.
func (c *Client) RemoveWatcher(ctx context.Context, key, accountID string) error {
	if key == "" || accountID == "" {
		return fmt.Errorf("%w: key and account id are required", ErrInvalidArgument)
	}
	if c.skipMutation("RemoveWatcher", key, accountID) == true {
		return nil
	}

	params := url.Values{}
	params.Set("accountId", accountID)

	_, err := c.do(ctx, "DELETE", buildPath("/issue/"+url.PathEscape(key)+"/watchers", params), nil)
	return err
}
