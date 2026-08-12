package jiraclient

import (
	"strings"
	"time"
)

// Issue is a nil-safe projection of a Jira issue. Every optional field is flattened to a zero value
// at decode time, so callers never dereference a missing description, assignee or reporter — the
// single most common panic in hand-rolled Jira integrations.
type Issue struct {
	ID           string
	Key          string
	Summary      string
	Description  string // plain text, extracted from the ADF document
	Status       string
	StatusID     string
	Priority     string
	IssueType    string
	Labels       []string
	AssigneeID   string
	AssigneeName string
	ReporterID   string
	ReporterName string
	Resolution   string
	// Comments holds each comment's body as plain text, oldest first. Populated only when "comment"
	// is among the requested fields; Jira omits it otherwise.
	Comments []Comment
	Created  time.Time
	Updated  time.Time
}

// Comment is one comment on an issue, with its body flattened to plain text.
type Comment struct {
	ID       string
	Body     string
	AuthorID string
	Created  time.Time
}

// IsAssigned reports whether the issue has a human or service assignee.
func (i Issue) IsAssigned() bool { return i.AssigneeID != "" }

// HasLabel reports whether the issue carries a label, compared case-insensitively because Jira
// preserves the case a label was created with but treats labels as case-sensitive on write.
func (i Issue) HasLabel(label string) bool {
	for _, existing := range i.Labels {
		if strings.EqualFold(existing, label) == true {
			return true
		}
	}
	return false
}

// SearchFields is the default field set requested by Search. Requesting only what is needed matters:
// a full-fidelity fetch of a few thousand issues is dramatically larger and slower.
var SearchFields = []string{
	"summary", "description", "labels", "status", "assignee", "reporter",
	"created", "updated", "priority", "issuetype", "resolution",
}

// searchResponse is the raw decode target for a JQL search page.
type searchResponse struct {
	Issues        []rawIssue `json:"issues"`
	IsLast        bool       `json:"isLast"`
	NextPageToken string     `json:"nextPageToken,omitempty"`
}

// rawIssue mirrors Jira's issue payload. Every nested object is a pointer because Jira omits fields
// that are unset rather than sending nulls for them.
type rawIssue struct {
	ID     string    `json:"id"`
	Key    string    `json:"key"`
	Fields rawFields `json:"fields"`
}

type rawFields struct {
	Summary     string       `json:"summary"`
	Description *ADFDoc      `json:"description"`
	Labels      []string     `json:"labels"`
	Created     string       `json:"created"`
	Updated     string       `json:"updated"`
	Status      *rawNamed    `json:"status"`
	Priority    *rawNamed    `json:"priority"`
	IssueType   *rawNamed    `json:"issuetype"`
	Resolution  *rawNamed    `json:"resolution"`
	Assignee    *rawUser     `json:"assignee"`
	Reporter    *rawUser     `json:"reporter"`
	Comment     *rawComments `json:"comment"`
}

// rawComments is the paginated comment container Jira nests under the "comment" field.
type rawComments struct {
	Comments []struct {
		ID      string   `json:"id"`
		Body    *ADFDoc  `json:"body"`
		Author  *rawUser `json:"author"`
		Created string   `json:"created"`
	} `json:"comments"`
}

type rawNamed struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type rawUser struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Active      bool   `json:"active"`
}

// jiraTimeLayout is the timestamp format Jira returns, e.g. "2026-08-11T13:00:32.478-0700".
const jiraTimeLayout = "2006-01-02T15:04:05.000-0700"

// toIssue flattens a raw issue, tolerating every absent field.
func (r rawIssue) toIssue() Issue {
	issue := Issue{
		ID:      r.ID,
		Key:     r.Key,
		Summary: r.Fields.Summary,
		Labels:  r.Fields.Labels,
	}
	if r.Fields.Description != nil {
		issue.Description = r.Fields.Description.Text()
	}
	if r.Fields.Status != nil {
		issue.Status, issue.StatusID = r.Fields.Status.Name, r.Fields.Status.ID
	}
	if r.Fields.Priority != nil {
		issue.Priority = r.Fields.Priority.Name
	}
	if r.Fields.IssueType != nil {
		issue.IssueType = r.Fields.IssueType.Name
	}
	if r.Fields.Resolution != nil {
		issue.Resolution = r.Fields.Resolution.Name
	}
	if r.Fields.Assignee != nil {
		issue.AssigneeID, issue.AssigneeName = r.Fields.Assignee.AccountID, r.Fields.Assignee.DisplayName
	}
	if r.Fields.Reporter != nil {
		issue.ReporterID, issue.ReporterName = r.Fields.Reporter.AccountID, r.Fields.Reporter.DisplayName
	}
	if r.Fields.Comment != nil {
		issue.Comments = make([]Comment, 0, len(r.Fields.Comment.Comments))
		for _, raw := range r.Fields.Comment.Comments {
			comment := Comment{ID: raw.ID, Created: parseJiraTime(raw.Created)}
			if raw.Body != nil {
				comment.Body = raw.Body.Text()
			}
			if raw.Author != nil {
				comment.AuthorID = raw.Author.AccountID
			}
			issue.Comments = append(issue.Comments, comment)
		}
	}
	issue.Created = parseJiraTime(r.Fields.Created)
	issue.Updated = parseJiraTime(r.Fields.Updated)
	return issue
}

// parseJiraTime returns the zero time for an absent or unparseable timestamp rather than failing the
// whole decode. Callers must treat a zero Created as "unknown", not as "the beginning of time".
func parseJiraTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(jiraTimeLayout, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
