package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

const (
	// projectSearchPageSize is the page size for /project/search. Jira's own default is 50 and it
	// silently clamps anything above 100 rather than erroring, so 100 is the largest useful value.
	projectSearchPageSize = 100

	// maxProjectFilterValues is Jira's cap on the repeatable keys and id filters of /project/search.
	maxProjectFilterValues = 50

	// maxEmptyProjectPages is how many empty pages in a row /project/search may return before the scan
	// is abandoned. One empty page is legitimate; this many without isLast means the end is not coming.
	maxEmptyProjectPages = 100
)

// projectActions are the values /project/search accepts for its action filter, which narrows results
// to projects the caller may act on in that way.
var projectActions = []string{"view", "browse", "edit", "create"}

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
		projects   []Project
		startAt    int
		emptyPages int
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
		// One empty page proves nothing, but an unbroken run of them with isLast never set means the
		// end is not going to be announced.
		if len(page.Values) > 0 {
			emptyPages = 0
		} else if emptyPages++; emptyPages >= maxEmptyProjectPages {
			return nil, fmt.Errorf("%w: /project/search returned %d empty pages in a row without marking the last",
				ErrPaginationStalled, emptyPages)
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
