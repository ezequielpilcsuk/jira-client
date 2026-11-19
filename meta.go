package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Myself returns the account the client is authenticated as.
//
// It doubles as a credential check, and the cheapest one available: a 401 here means the API token
// is wrong or expired rather than that some permission is missing. Jira Cloud tokens now carry an
// expiry (1 year by default), so this is worth calling at startup instead of discovering a dead
// token mid-run.
//
// Email obeys profile visibility exactly as it does in SearchUsers, so it is frequently empty even
// for your own account. Do not treat that as a decode failure.
func (c *Client) Myself(ctx context.Context) (User, error) {
	body, err := c.do(ctx, "GET", apiBase+"/myself", nil)
	if err != nil {
		return User{}, err
	}

	var raw struct {
		AccountID    string `json:"accountId"`
		AccountType  string `json:"accountType"`
		DisplayName  string `json:"displayName"`
		EmailAddress string `json:"emailAddress"`
		Active       bool   `json:"active"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return User{}, fmt.Errorf("decode myself: %w", err)
	}

	return User{
		AccountID:   raw.AccountID,
		AccountType: raw.AccountType,
		DisplayName: raw.DisplayName,
		Email:       raw.EmailAddress,
		Active:      raw.Active,
	}, nil
}

// ServerInfo describes the Jira instance itself.
type ServerInfo struct {
	BaseURL        string
	Version        string
	DeploymentType string
	ServerTitle    string
	// ServerTime is the instance's current time, and ServerTimeZone its zone ID ("America/Los_Angeles").
	ServerTime     time.Time
	ServerTimeZone string
}

// ServerInfo reads the instance's identity and clock.
//
// ServerTimeZone is the reason this is here. Every timestamp Jira returns is rendered in the system
// default user time zone, not UTC, and the offset alone does not tell you the zone — so "issues
// updated today" computed in the caller's local zone quietly straddles the wrong day boundary on any
// site whose zone differs. This endpoint is the documented way to ask what that zone is.
func (c *Client) ServerInfo(ctx context.Context) (ServerInfo, error) {
	body, err := c.do(ctx, "GET", apiBase+"/serverInfo", nil)
	if err != nil {
		return ServerInfo{}, err
	}

	var raw struct {
		BaseURL        string `json:"baseUrl"`
		Version        string `json:"version"`
		DeploymentType string `json:"deploymentType"`
		ServerTitle    string `json:"serverTitle"`
		ServerTime     string `json:"serverTime"`
		ServerTimeZone string `json:"serverTimeZone"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ServerInfo{}, fmt.Errorf("decode server info: %w", err)
	}

	return ServerInfo{
		BaseURL:        raw.BaseURL,
		Version:        raw.Version,
		DeploymentType: raw.DeploymentType,
		ServerTitle:    raw.ServerTitle,
		ServerTime:     parseJiraTime(raw.ServerTime),
		ServerTimeZone: raw.ServerTimeZone,
	}, nil
}
