package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// User is a Jira account as the user search returns it.
type User struct {
	AccountID   string
	AccountType string
	DisplayName string
	Email       string
	Active      bool
}

// userSearchLimit caps the user search. Callers resolving a single person want the top match; a
// larger page only costs bandwidth.
const userSearchLimit = 10

// SearchUsers finds accounts matching a query, which Jira matches as a *prefix* against display name
// and — for accounts whose profile permits it — email address.
//
// Three things make an empty result ambiguous, and none of them is distinguishable from "no such
// person": Jira returns only **active** users, so a deactivated account never appears; profile
// visibility can hide an email so the address will not match even when it is correct; and a caller
// without the *Browse users and groups* permission gets an empty list rather than a 403.
func (c *Client) SearchUsers(ctx context.Context, query string) ([]User, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: query cannot be empty", ErrInvalidArgument)
	}

	params := url.Values{}
	params.Set("query", query)
	params.Set("maxResults", strconv.Itoa(userSearchLimit))

	body, err := c.do(ctx, "GET", buildPath("/user/search", params), nil)
	if err != nil {
		return nil, err
	}

	var raw []struct {
		AccountID    string `json:"accountId"`
		AccountType  string `json:"accountType"`
		DisplayName  string `json:"displayName"`
		EmailAddress string `json:"emailAddress"`
		Active       bool   `json:"active"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode user search response: %w", err)
	}

	users := make([]User, 0, len(raw))
	for _, entry := range raw {
		users = append(users, User{
			AccountID:   entry.AccountID,
			AccountType: entry.AccountType,
			DisplayName: entry.DisplayName,
			Email:       entry.EmailAddress,
			Active:      entry.Active,
		})
	}
	return users, nil
}

// AccountIDByEmail resolves an email address to a single account id, returning "" when nothing
// matches. A caller that cannot resolve a reporter generally wants to carry on without one rather
// than fail, so "not found" is not an error here.
//
// Jira matches the query as a *prefix*, so "ada@example.com" also matches "ada@example.com.au". An
// exact, case-insensitive match on a visible email always wins. Only when no candidate exposes an
// email at all — the common case under restrictive profile visibility — does this fall back to the
// first result, which is a guess and can be wrong if the address is a prefix of another.
//
// See SearchUsers for why an empty result does not prove the person has no account.
func (c *Client) AccountIDByEmail(ctx context.Context, email string) (string, error) {
	if strings.TrimSpace(email) == "" {
		return "", nil
	}

	users, err := c.SearchUsers(ctx, email)
	if err != nil {
		return "", err
	}
	if len(users) == 0 {
		return "", nil
	}

	anyEmailVisible := false
	for _, user := range users {
		if user.Email == "" {
			continue
		}
		anyEmailVisible = true
		if strings.EqualFold(user.Email, email) == true {
			return user.AccountID, nil
		}
	}

	// Every candidate matched on something other than a visible email — a display name, or an email
	// Jira declined to show. There is nothing to verify against, so the top match is the best guess.
	if anyEmailVisible == true {
		return "", nil
	}
	return users[0].AccountID, nil
}
