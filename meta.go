package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// projectSearchPageSize is the page size for /project/search. Jira's own default is 50 and it
	// silently clamps anything above 100 rather than erroring, so 100 is the largest useful value.
	projectSearchPageSize = 100

	// maxProjectFilterValues is Jira's cap on the repeatable keys and id filters of /project/search.
	maxProjectFilterValues = 50
)

// projectActions are the values /project/search accepts for its action filter, which narrows results
// to projects the caller may act on in that way.
var projectActions = []string{"view", "browse", "edit", "create"}

// Field is one field definition on the site, system or custom.
type Field struct {
	ID   string
	Key  string
	Name string
	// Custom distinguishes a site-defined field from a built-in one.
	Custom     bool
	Navigable  bool
	Orderable  bool
	Searchable bool
	// ClauseNames are the names JQL accepts for this field. They are *not* always Name: a custom field
	// is addressable as cf[10004] as well, and a rename can leave an old clause name in place, so a
	// query built from Name alone can fail on a field this list would have matched.
	ClauseNames []string
	// SchemaType is the value shape ("number", "string", "array", "option", ...), SchemaItems the
	// element type when SchemaType is "array", and SchemaCustom the custom field type key
	// ("com.atlassian.jira.plugin.system.customfieldtypes:float"). All three are empty for a field
	// whose schema Jira omitted.
	SchemaType   string
	SchemaItems  string
	SchemaCustom string
}

// Fields lists every field on the site.
//
// This closes the loop on custom fields. UpdateCustomField and CreateIssueInput.CustomFields both
// take an ID like "customfield_10004", but nothing else in the library can tell a caller what that
// ID is — and the ID is per-site, so a constant copied out of one Jira's URL bar is the single most
// site-brittle thing in an integration. Resolve it by name at startup instead.
//
// The response is a bare array with no pagination: one request returns the lot.
func (c *Client) Fields(ctx context.Context) ([]Field, error) {
	body, err := c.do(ctx, "GET", apiBase+"/field", nil)
	if err != nil {
		return nil, err
	}

	var raw []struct {
		ID          string   `json:"id"`
		Key         string   `json:"key"`
		Name        string   `json:"name"`
		Custom      bool     `json:"custom"`
		Navigable   bool     `json:"navigable"`
		Orderable   bool     `json:"orderable"`
		Searchable  bool     `json:"searchable"`
		ClauseNames []string `json:"clauseNames"`
		// Schema is a pointer because Jira omits it entirely for some system fields.
		Schema *struct {
			Type   string `json:"type"`
			Items  string `json:"items"`
			Custom string `json:"custom"`
		} `json:"schema"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode fields: %w", err)
	}

	fields := make([]Field, 0, len(raw))
	for _, entry := range raw {
		field := Field{
			ID:          entry.ID,
			Key:         entry.Key,
			Name:        entry.Name,
			Custom:      entry.Custom,
			Navigable:   entry.Navigable,
			Orderable:   entry.Orderable,
			Searchable:  entry.Searchable,
			ClauseNames: entry.ClauseNames,
		}
		if entry.Schema != nil {
			field.SchemaType, field.SchemaItems, field.SchemaCustom =
				entry.Schema.Type, entry.Schema.Items, entry.Schema.Custom
		}
		fields = append(fields, field)
	}
	return fields, nil
}

// FieldsByName returns every field whose name matches, compared case-insensitively.
//
// Jira does not enforce unique field names, so this returns a slice rather than one field. Use it
// when FieldIDByName reports an ambiguity and you need to pick between the candidates yourself —
// SchemaType and Custom are usually enough to tell two same-named fields apart. An empty result is
// not an error: it means the site has no such field.
func (c *Client) FieldsByName(ctx context.Context, name string) ([]Field, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: field name cannot be empty", ErrInvalidArgument)
	}

	fields, err := c.Fields(ctx)
	if err != nil {
		return nil, err
	}

	matches := make([]Field, 0, 1)
	for _, field := range fields {
		if strings.EqualFold(field.Name, name) == true {
			matches = append(matches, field)
		}
	}
	return matches, nil
}

// FieldIDByName resolves a field's display name to the ID the write APIs take, e.g. "Story Points"
// to "customfield_10004".
//
// Two same-named fields are an error rather than a coin toss. Jira lets an admin create a second
// custom field called "Severity" without complaint, and the two get different IDs — so silently
// taking the first match would write to the wrong field on some sites and the right one on others,
// which is close to undiagnosable from the caller's side. Use FieldsByName to disambiguate.
func (c *Client) FieldIDByName(ctx context.Context, name string) (string, error) {
	matches, err := c.FieldsByName(ctx, name)
	if err != nil {
		return "", err
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: no field named %q on this site", ErrInvalidArgument, name)
	case 1:
		return matches[0].ID, nil
	}

	ids := make([]string, 0, len(matches))
	for _, field := range matches {
		ids = append(ids, field.ID)
	}
	return "", fmt.Errorf("%w: field name %q is ambiguous, %d fields share it (%s)",
		ErrInvalidArgument, name, len(matches), strings.Join(ids, ", "))
}

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

// Project is a nil-safe projection of a Jira project.
type Project struct {
	ID   string
	Key  string
	Name string
	// TypeKey is "software", "service_desk" or "business".
	TypeKey string
	// Style is "classic" (company-managed) or "next-gen" (team-managed).
	Style string
	// Simplified reports a team-managed project. It matters before writing: team-managed projects
	// carry their own field configuration and workflow, so an issue type, status or custom field that
	// exists site-wide may simply not be available in one.
	Simplified  bool
	IsPrivate   bool
	Archived    bool
	LeadID      string
	LeadName    string
	Description string
	// CategoryName is empty when the project is in no category.
	CategoryName string
}

// ProjectQuery narrows the project search. A zero ProjectQuery lists every visible project.
type ProjectQuery struct {
	// Query matches against project key and name as a substring.
	Query string
	// Keys and IDs restrict the result to specific projects. Jira accepts at most 50 of each.
	Keys []string
	IDs  []string
	// TypeKeys restricts by project type: "software", "service_desk", "business".
	TypeKeys []string
	// Action restricts to projects the caller may act on: "view", "browse", "edit" or "create".
	Action string
	// OrderBy sorts the result, e.g. "key" or "-name". Jira's default is "key".
	OrderBy string
}

// Projects lists projects, following pagination to the end.
//
// This is /project/search, not the deprecated /project — the latter returns every project in one
// unpaginated response and Atlassian has flagged it for removal.
func (c *Client) Projects(ctx context.Context, filter ProjectQuery) ([]Project, error) {
	if len(filter.Keys) > maxProjectFilterValues {
		return nil, fmt.Errorf("%w: at most %d project keys can be filtered on, got %d",
			ErrInvalidArgument, maxProjectFilterValues, len(filter.Keys))
	}
	if len(filter.IDs) > maxProjectFilterValues {
		return nil, fmt.Errorf("%w: at most %d project ids can be filtered on, got %d",
			ErrInvalidArgument, maxProjectFilterValues, len(filter.IDs))
	}
	if filter.Action != "" && slices.Contains(projectActions, filter.Action) == false {
		return nil, fmt.Errorf("%w: action %q is not one of %s",
			ErrInvalidArgument, filter.Action, strings.Join(projectActions, ", "))
	}

	var (
		projects []Project
		startAt  int
	)
	for {
		params := url.Values{}
		params.Set("startAt", strconv.Itoa(startAt))
		params.Set("maxResults", strconv.Itoa(projectSearchPageSize))
		if filter.Query != "" {
			params.Set("query", filter.Query)
		}
		for _, key := range filter.Keys {
			params.Add("keys", key)
		}
		for _, id := range filter.IDs {
			params.Add("id", id)
		}
		if len(filter.TypeKeys) > 0 {
			params.Set("typeKey", strings.Join(filter.TypeKeys, ","))
		}
		if filter.Action != "" {
			params.Set("action", filter.Action)
		}
		if filter.OrderBy != "" {
			params.Set("orderBy", filter.OrderBy)
		}

		body, err := c.do(ctx, "GET", buildPath("/project/search", params), nil)
		if err != nil {
			return nil, err
		}

		// total is deliberately not decoded: Jira documents that it can change between pages, so a
		// page count derived from it is wrong the moment anyone else creates a project mid-scan.
		var page struct {
			MaxResults int          `json:"maxResults"`
			IsLast     bool         `json:"isLast"`
			Values     []rawProject `json:"values"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode projects: %w", err)
		}
		for _, raw := range page.Values {
			projects = append(projects, raw.toProject())
		}

		if page.IsLast == true {
			break
		}
		// Advance by the page size, not by the number of rows returned. Jira documents that a
		// requested page can legitimately come back empty while later pages still hold results, and
		// stepping by len(values) would stall there forever. The server's own maxResults wins because
		// it is the size actually applied after clamping.
		step := page.MaxResults
		if step <= 0 {
			step = projectSearchPageSize
		}
		startAt += step
	}
	return projects, nil
}

// GetProject fetches one project by key or numeric ID.
func (c *Client) GetProject(ctx context.Context, idOrKey string) (Project, error) {
	if strings.TrimSpace(idOrKey) == "" {
		return Project{}, fmt.Errorf("%w: project id or key cannot be empty", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "GET", apiBase+"/project/"+url.PathEscape(idOrKey), nil)
	if err != nil {
		return Project{}, err
	}

	var raw rawProject
	if err := json.Unmarshal(body, &raw); err != nil {
		return Project{}, fmt.Errorf("decode project %s: %w", idOrKey, err)
	}
	return raw.toProject(), nil
}

// rawProject mirrors Jira's project payload. Lead and category are pointers because Jira omits them
// rather than sending nulls — a project with no category has no projectCategory key at all.
type rawProject struct {
	ID             string `json:"id"`
	Key            string `json:"key"`
	Name           string `json:"name"`
	ProjectTypeKey string `json:"projectTypeKey"`
	Style          string `json:"style"`
	Simplified     bool   `json:"simplified"`
	IsPrivate      bool   `json:"isPrivate"`
	Archived       bool   `json:"archived"`
	Description    string `json:"description"`
	Lead           *struct {
		AccountID   string `json:"accountId"`
		DisplayName string `json:"displayName"`
	} `json:"lead"`
	ProjectCategory *struct {
		Name string `json:"name"`
	} `json:"projectCategory"`
}

// toProject flattens a raw project, tolerating every absent field.
func (r rawProject) toProject() Project {
	project := Project{
		ID:          r.ID,
		Key:         r.Key,
		Name:        r.Name,
		TypeKey:     r.ProjectTypeKey,
		Style:       r.Style,
		Simplified:  r.Simplified,
		IsPrivate:   r.IsPrivate,
		Archived:    r.Archived,
		Description: r.Description,
	}
	if r.Lead != nil {
		project.LeadID, project.LeadName = r.Lead.AccountID, r.Lead.DisplayName
	}
	if r.ProjectCategory != nil {
		project.CategoryName = r.ProjectCategory.Name
	}
	return project
}

// Status is one workflow status, carrying the category that makes it comparable across workflows.
type Status struct {
	ID   string
	Name string
	// CategoryKey is the stable "new", "indeterminate" or "done" value, and CategoryName its display
	// label ("To Do", "In Progress", "Done").
	CategoryKey  string
	CategoryName string
}

// IssueTypeStatuses is the set of statuses one issue type can reach in a project.
type IssueTypeStatuses struct {
	IssueTypeID   string
	IssueTypeName string
	// Subtask marks the sub-task issue type, whose workflow is usually not the parent's.
	Subtask  bool
	Statuses []Status
}

// ProjectStatuses lists the statuses available per issue type in a project, by key or numeric ID.
//
// Read CategoryKey, not Name, when deciding whether something is finished. Status names are defined
// per workflow and are freely editable — one site's "Done" is another's "Shipped", "Closed" or
// "Resolved", and two workflows in the *same* project can disagree. The category key is one of only
// three values ("new", "indeterminate", "done") and is the same everywhere, which is what makes
// statusCategory workable in JQL where a status name is not.
//
// The response is a bare array with no pagination.
func (c *Client) ProjectStatuses(ctx context.Context, projectIDOrKey string) ([]IssueTypeStatuses, error) {
	if strings.TrimSpace(projectIDOrKey) == "" {
		return nil, fmt.Errorf("%w: project id or key cannot be empty", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "GET",
		apiBase+"/project/"+url.PathEscape(projectIDOrKey)+"/statuses", nil)
	if err != nil {
		return nil, err
	}

	var raw []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Subtask  bool   `json:"subtask"`
		Statuses []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			StatusCategory *struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"statusCategory"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode statuses for %s: %w", projectIDOrKey, err)
	}

	byIssueType := make([]IssueTypeStatuses, 0, len(raw))
	for _, entry := range raw {
		issueType := IssueTypeStatuses{
			IssueTypeID:   entry.ID,
			IssueTypeName: entry.Name,
			Subtask:       entry.Subtask,
			Statuses:      make([]Status, 0, len(entry.Statuses)),
		}
		for _, rawStatus := range entry.Statuses {
			status := Status{ID: rawStatus.ID, Name: rawStatus.Name}
			if rawStatus.StatusCategory != nil {
				status.CategoryKey, status.CategoryName =
					rawStatus.StatusCategory.Key, rawStatus.StatusCategory.Name
			}
			issueType.Statuses = append(issueType.Statuses, status)
		}
		byIssueType = append(byIssueType, issueType)
	}
	return byIssueType, nil
}
