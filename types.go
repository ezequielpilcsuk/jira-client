package jiraclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Issue is a nil-safe projection of a Jira issue. Every optional field is flattened to a zero value
// at decode time, so callers never dereference a missing description, assignee or reporter — the
// single most common panic in hand-rolled Jira integrations.
type Issue struct {
	ID          string
	Key         string
	Summary     string
	Description string // plain text, extracted from the ADF document
	Status      string
	StatusID    string
	// StatusCategory is the stable lifecycle bucket behind the status: "new", "indeterminate" or
	// "done". Prefer it over Status for "is this finished" — status *names* are per-workflow and
	// site-editable, so a check against "Done" breaks on any project that renamed it.
	StatusCategory string
	Priority       string
	IssueType      string
	Labels         []string
	AssigneeID     string
	AssigneeName   string
	ReporterID     string
	ReporterName   string
	Resolution     string
	// Comments holds each comment's body as plain text, oldest first. Populated only when "comment"
	// is among the requested fields; Jira omits it otherwise.
	Comments []Comment
	// Links are the issue's links to other issues, each naming both ends. Populated only when
	// "issuelinks" is among the requested fields.
	Links   []IssueLink
	Created time.Time
	Updated time.Time
	// RawFields holds every field Jira returned that has no member above — custom fields first among
	// them — as raw JSON keyed by field ID. A field is here only if it was asked for, so pass its ID
	// in the fields argument of the read ("customfield_10004"; Client.FieldIDByName finds it). Read
	// one with DecodeField or FieldText. Nil when there are none.
	RawFields map[string]json.RawMessage
}

// Comment is one comment on an issue, with its body flattened to plain text.
type Comment struct {
	ID       string
	Body     string
	AuthorID string
	// AuthorName is the display name, empty for a comment left by a since-deleted account.
	AuthorName string
	Created    time.Time
	// Updated is when the body was last edited, equal to Created for a comment never touched since.
	Updated time.Time
}

// Status category keys. These are fixed by Jira and are the same on every site, unlike status names.
const (
	StatusCategoryNew        = "new"
	StatusCategoryInProgress = "indeterminate"
	StatusCategoryDone       = "done"
)

// IsAssigned reports whether the issue has a human or service assignee.
func (i Issue) IsAssigned() bool { return i.AssigneeID != "" }

// IsDone reports whether the issue has reached a done-category status, whatever that status is
// called on this site. Empty when the status field was not requested.
func (i Issue) IsDone() bool { return i.StatusCategory == StatusCategoryDone }

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

// DecodeField unmarshals one of RawFields into dest. It reports false, and no error, when the field
// is absent or null: Jira omits a field that was not requested and sends null for one that is unset,
// and neither is a failure.
func (i Issue) DecodeField(fieldID string, dest any) (bool, error) {
	raw, present := i.RawFields[fieldID]
	if present == false || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) == true {
		return false, nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, fmt.Errorf("decode field %s on %s: %w", fieldID, i.Key, err)
	}
	return true, nil
}

// FieldText renders one of RawFields as text without the caller knowing which of Jira's shapes the
// field uses: a string or number as itself, a select option as its value (a cascading one as
// "Parent - Child"), a user as their display name, a version, component or sprint as its name, an
// issue as its key, rich text as plain text, and a list as its items separated by ", ".
//
// It is empty for a field that is absent, unset, or of a shape none of those cover. Use DecodeField
// when the exact value matters.
func (i Issue) FieldText(fieldID string) string {
	raw, present := i.RawFields[fieldID]
	if present == false {
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return fieldValueText(value)
}

// fieldValueLabels are the keys Jira puts a value's human-readable form under, most specific first.
var fieldValueLabels = []string{"value", "displayName", "name", "key"}

// fieldValueText renders a decoded field value. See Issue.FieldText for the shapes it knows.
func fieldValueText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, element := range typed {
			if text := fieldValueText(element); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		if typed["type"] == adfDoc {
			return adfValueText(typed)
		}
		for _, label := range fieldValueLabels {
			text, isText := typed[label].(string)
			if isText == false || text == "" {
				continue
			}
			if child := fieldValueText(typed["child"]); child != "" {
				text += " - " + child
			}
			return text
		}
	}
	return ""
}

// adfValueText renders a rich-text field value, which arrives as an ADF document.
func adfValueText(value map[string]any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	var doc ADFDoc
	if err := json.Unmarshal(encoded, &doc); err != nil {
		return ""
	}
	return doc.Text()
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
	// Warnings is the /search/jql shape; WarningMessages is what the retired /search returned. Both
	// are decoded so the complaint surfaces either way.
	Warnings []struct {
		Message string `json:"message"`
	} `json:"warnings"`
	WarningMessages []string `json:"warningMessages"`
}

// warnings flattens whichever warning shape the site returned.
func (s searchResponse) warnings() []string {
	messages := make([]string, 0, len(s.Warnings)+len(s.WarningMessages))
	for _, warning := range s.Warnings {
		if warning.Message != "" {
			messages = append(messages, warning.Message)
		}
	}
	return append(messages, s.WarningMessages...)
}

// rawIssue mirrors Jira's issue payload. Every nested object is a pointer because Jira omits fields
// that are unset rather than sending nulls for them.
type rawIssue struct {
	ID     string    `json:"id"`
	Key    string    `json:"key"`
	Fields rawFields `json:"fields"`
}

type rawFields struct {
	Summary     string         `json:"summary"`
	Description *ADFDoc        `json:"description"`
	Labels      []string       `json:"labels"`
	Created     string         `json:"created"`
	Updated     string         `json:"updated"`
	Status      *rawNamed      `json:"status"`
	Priority    *rawNamed      `json:"priority"`
	IssueType   *rawNamed      `json:"issuetype"`
	Resolution  *rawNamed      `json:"resolution"`
	Assignee    *rawUser       `json:"assignee"`
	Reporter    *rawUser       `json:"reporter"`
	Comment     *rawComments   `json:"comment"`
	IssueLinks  []rawIssueLink `json:"issuelinks"`
	// Other is every field with no member above, kept as Jira sent it. It becomes Issue.RawFields.
	Other map[string]json.RawMessage `json:"-"`
}

// modelledFields are the field IDs rawFields decodes into members, read off its own JSON tags so the
// set cannot drift from the struct.
var modelledFields = func() map[string]bool {
	fields := map[string]bool{}
	shape := reflect.TypeOf(rawFields{})
	for index := 0; index < shape.NumField(); index++ {
		name, _, _ := strings.Cut(shape.Field(index).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			fields[name] = true
		}
	}
	return fields
}()

// UnmarshalJSON decodes the modelled fields into their members and keeps the rest in Other.
func (f *rawFields) UnmarshalJSON(data []byte) error {
	// A method-less twin, so decoding into it does not call this method again.
	type modelled rawFields
	var typed modelled
	if err := json.Unmarshal(data, &typed); err != nil {
		return err
	}

	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	for fieldID := range all {
		if modelledFields[fieldID] == true {
			delete(all, fieldID)
		}
	}

	*f = rawFields(typed)
	if len(all) > 0 {
		f.Other = all
	}
	return nil
}

// rawComments is the paginated comment container Jira nests under the "comment" field.
type rawComments struct {
	Comments []rawComment `json:"comments"`
}

type rawNamed struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// StatusCategory is populated on the status field only, and arrives with every normal read.
	StatusCategory *struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"statusCategory"`
}

type rawUser struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Active      bool   `json:"active"`
}

// jiraTimeLayouts are the timestamp formats accepted, most common first.
//
// Jira documents only "ISO 8601, in the system default user time zone" and in practice returns
// "2026-08-11T13:00:32.478-0700". Nothing guarantees the fractional-second precision or the offset
// style, and a mismatch here is silent — the field decodes to the zero time with no error — so the
// stricter layout is tried first and the standard ones catch anything else.
var jiraTimeLayouts = []string{
	"2006-01-02T15:04:05.000-0700",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05-0700",
	"2006-01-02",
}

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
		if r.Fields.Status.StatusCategory != nil {
			issue.StatusCategory = r.Fields.Status.StatusCategory.Key
		}
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
			issue.Comments = append(issue.Comments, raw.toComment())
		}
	}
	if len(r.Fields.IssueLinks) > 0 {
		issue.Links = make([]IssueLink, 0, len(r.Fields.IssueLinks))
		for _, raw := range r.Fields.IssueLinks {
			issue.Links = append(issue.Links, raw.toIssueLink(r.Key))
		}
	}
	issue.RawFields = r.Fields.Other
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
	for _, layout := range jiraTimeLayouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
