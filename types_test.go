package jiraclient

import (
	"context"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"
)

// Jira omits unset fields entirely. Decoding must not assume any of them are present.
func TestSearch_DecodesIssueMissingEveryOptionalField(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{"summary":"bare"}}],"isLast":true}`)
	})

	issues, err := client.SearchIssues(context.Background(), "project = ABC", nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	issue := issues[0]
	if issue.Description != "" || issue.Status != "" || issue.AssigneeID != "" || issue.ReporterID != "" {
		t.Fatalf("absent fields should be zero: %+v", issue)
	}
	if issue.Created.IsZero() == false {
		t.Fatalf("absent created should be zero, got %v", issue.Created)
	}
}

func TestSearch_MapsPopulatedFields(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[{"id":"1","key":"ABC-1","fields":{
			"summary":"s","labels":["a"],"created":"2026-08-05T09:30:00.000-0700",
			"status":{"id":"3","name":"In Progress"},
			"assignee":{"accountId":"acc","displayName":"Dev"},
			"reporter":{"accountId":"rep","displayName":"Reporter"},
			"priority":{"name":"High"},"issuetype":{"name":"Bug"},"resolution":{"name":"Done"},
			"description":{"type":"doc","version":1,"content":[
				{"type":"paragraph","content":[{"type":"text","text":"hello"}]}]}
		}}],"isLast":true}`)
	})

	issues, _ := client.SearchIssues(context.Background(), "project = ABC", nil)
	issue := issues[0]

	if issue.Status != "In Progress" || issue.StatusID != "3" {
		t.Fatalf("status: %+v", issue)
	}
	if issue.AssigneeID != "acc" || issue.AssigneeName != "Dev" || issue.ReporterID != "rep" {
		t.Fatalf("people: %+v", issue)
	}
	if issue.Priority != "High" || issue.IssueType != "Bug" || issue.Resolution != "Done" {
		t.Fatalf("named fields: %+v", issue)
	}
	if issue.Description != "hello" {
		t.Fatalf("description: %q", issue.Description)
	}
	if issue.Created.Year() != 2026 {
		t.Fatalf("created: %v", issue.Created)
	}
	if issue.HasLabel("A") == false {
		t.Fatal("label matching should be case-insensitive")
	}
}

// Jira nests comments under a paginated "comment" object and renders bodies as ADF, so a consumer
// that wants their text should not have to walk the document itself.
func TestSearch_FlattensCommentBodies(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{
			"summary":"one",
			"comment":{"comments":[
				{"id":"10","created":"2026-08-12T09:00:00.000-0300","author":{"accountId":"acct-1"},
				 "body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
				   {"type":"text","text":"first note"}]}]}},
				{"id":"11","body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
				   {"type":"text","text":"second note"}]}]}}
			]}}}],"isLast":true}`)
	})

	issues, err := client.SearchIssues(context.Background(), "project = ABC", []string{"summary", "comment"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}

	comments := issues[0].Comments
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	if comments[0].Body != "first note" || comments[1].Body != "second note" {
		t.Errorf("bodies not flattened: %+v", comments)
	}
	if comments[0].AuthorID != "acct-1" {
		t.Errorf("author: got %q, want acct-1", comments[0].AuthorID)
	}
	if comments[0].Created.IsZero() {
		t.Error("created timestamp not parsed")
	}
}

// An issue fetched without the comment field must report no comments rather than panicking.
func TestSearch_MissingCommentFieldIsNotAnError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issues":[{"key":"ABC-1","fields":{"summary":"one"}}],"isLast":true}`)
	})

	issues, err := client.SearchIssues(context.Background(), "project = ABC", nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(issues[0].Comments) != 0 {
		t.Errorf("expected no comments, got %+v", issues[0].Comments)
	}
}

func TestIssue_IsAssigned(t *testing.T) {
	if (Issue{AssigneeID: "acct-1"}).IsAssigned() == false {
		t.Error("an issue with an assignee id should report as assigned")
	}
	if (Issue{}).IsAssigned() == true {
		t.Error("an issue with no assignee id should report as unassigned")
	}
}

const issueWithExtraFields = `{"issues":[{"id":"10001","key":"ABC-1","fields":{
	"summary":"one",
	"customfield_10004":3.0,
	"customfield_10010":{"self":"https://x/option/1","value":"High","id":"10001"},
	"customfield_10020":null,
	"duedate":"2026-12-01",
	"issuelinks":[
		{"id":"900","type":{"id":"10000","name":"Blocks","inward":"is blocked by","outward":"blocks"},
		 "outwardIssue":{"id":"2","key":"ABC-2","fields":{"summary":"two"}}},
		{"id":"901","type":{"id":"10002","name":"Duplicate","inward":"is duplicated by","outward":"duplicates"},
		 "inwardIssue":{"id":"3","key":"ABC-3"}}]
}}],"isLast":true}`

// searchOne runs a search against a canned response and returns its only issue.
func searchOne(t *testing.T, response string) Issue {
	t.Helper()
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, response)
	})
	issues, err := client.SearchIssues(context.Background(), "project = ABC", []string{"summary"})
	if err != nil || len(issues) != 1 {
		t.Fatalf("search: %d issues, err %v", len(issues), err)
	}
	return issues[0]
}

// A custom field can be written through this client, so it has to be readable through it too.
func TestIssue_CarriesTheFieldsItHasNoMemberFor(t *testing.T) {
	issue := searchOne(t, issueWithExtraFields)

	keys := make([]string, 0, len(issue.RawFields))
	for key := range issue.RawFields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	want := []string{"customfield_10004", "customfield_10010", "customfield_10020", "duedate"}
	if slices.Equal(keys, want) == false {
		t.Fatalf("raw fields = %v, want %v", keys, want)
	}
	if issue.Summary != "one" {
		t.Fatalf("the modelled fields must still decode, summary = %q", issue.Summary)
	}
}

// The common search asks only for fields that have members, and should carry no map for it.
func TestIssue_RawFieldsIsNilWhenEveryFieldHasAMember(t *testing.T) {
	issue := searchOne(t, `{"issues":[{"key":"ABC-1","fields":{"summary":"one","labels":["a"]}}],"isLast":true}`)

	if issue.RawFields != nil {
		t.Fatalf("raw fields = %v, want nil", issue.RawFields)
	}
}

func TestIssue_DecodeField(t *testing.T) {
	issue := searchOne(t, issueWithExtraFields)

	var points float64
	if found, err := issue.DecodeField("customfield_10004", &points); err != nil || found == false || points != 3 {
		t.Errorf("number field: found %v, value %v, err %v", found, points, err)
	}

	var option struct {
		Value string `json:"value"`
	}
	if found, err := issue.DecodeField("customfield_10010", &option); err != nil || found == false || option.Value != "High" {
		t.Errorf("option field: found %v, value %q, err %v", found, option.Value, err)
	}

	// Jira sends null for a field that is unset and omits one that was not requested. Neither is a
	// failure, and neither has a value.
	for _, fieldID := range []string{"customfield_10020", "customfield_99999"} {
		var anything any
		if found, err := issue.DecodeField(fieldID, &anything); found == true || err != nil {
			t.Errorf("%s: found %v, err %v, want not found and no error", fieldID, found, err)
		}
	}

	var wrongShape []string
	if found, err := issue.DecodeField("customfield_10004", &wrongShape); found == true || err == nil {
		t.Errorf("a value that does not fit dest must be an error, got found %v, err %v", found, err)
	}
}

// FieldText answers "what does this field say" without the caller knowing which of Jira's shapes the
// field uses.
func TestIssue_FieldText(t *testing.T) {
	issue := searchOne(t, `{"issues":[{"key":"ABC-1","fields":{
		"number":3.0,
		"fraction":2.5,
		"text":"plain",
		"flag":true,
		"unset":null,
		"option":{"value":"High","id":"1"},
		"cascade":{"value":"Hardware","child":{"value":"Laptop"}},
		"user":{"accountId":"acc-1","displayName":"Ada Lovelace"},
		"version":{"id":"7","name":"v1.2"},
		"parent":{"id":"9","key":"ABC-9","fields":{"summary":"epic"}},
		"multi":[{"value":"Red"},{"value":"Blue"}],
		"tags":["a","b"],
		"rich":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"rich text"}]}]},
		"opaque":{"something":"else"}
	}}],"isLast":true}`)

	for fieldID, want := range map[string]string{
		"number":   "3",
		"fraction": "2.5",
		"text":     "plain",
		"flag":     "true",
		"unset":    "",
		"absent":   "",
		"option":   "High",
		"cascade":  "Hardware - Laptop",
		"user":     "Ada Lovelace",
		"version":  "v1.2",
		"parent":   "ABC-9",
		"multi":    "Red, Blue",
		"tags":     "a, b",
		"rich":     "rich text",
		"opaque":   "",
	} {
		if got := issue.FieldText(fieldID); got != want {
			t.Errorf("%s = %q, want %q", fieldID, got, want)
		}
	}
}

// An issue's own link list names only the far end of each link. The near end is the issue itself,
// and filling it in gives the same two-ended shape GetIssueLink returns.
func TestIssue_LinksNameBothEnds(t *testing.T) {
	issue := searchOne(t, issueWithExtraFields)

	if len(issue.Links) != 2 {
		t.Fatalf("links = %+v, want 2", issue.Links)
	}
	blocks, duplicate := issue.Links[0], issue.Links[1]
	if blocks.ID != "900" || blocks.Type.Name != "Blocks" || blocks.Type.Outward != "blocks" ||
		blocks.OutwardKey != "ABC-2" || blocks.InwardKey != "ABC-1" {
		t.Errorf("first link = %+v", blocks)
	}
	if duplicate.ID != "901" || duplicate.InwardKey != "ABC-3" || duplicate.OutwardKey != "ABC-1" {
		t.Errorf("second link = %+v", duplicate)
	}
}

// Comments embedded in an issue read are the same thing as comments listed on their own.
func TestIssue_EmbeddedCommentsCarryTheSameDetail(t *testing.T) {
	issue := searchOne(t, `{"issues":[{"key":"ABC-1","fields":{"comment":{"comments":[{"id":"1",
		"author":{"accountId":"acc-1","displayName":"Ada Lovelace"},
		"created":"2026-08-11T13:00:32.478-0700","updated":"2026-08-12T09:15:00.000-0700"}]}}}],"isLast":true}`)

	if len(issue.Comments) != 1 || issue.Comments[0].AuthorName != "Ada Lovelace" || issue.Comments[0].Updated.IsZero() == true {
		t.Fatalf("comments = %+v", issue.Comments)
	}
}

// A silently-unparsed timestamp is worse than a wrong one: it decodes to the zero time with no error.
func TestParseJiraTime_AcceptsTheFormatsJiraActuallySends(t *testing.T) {
	cases := map[string]string{
		"jira default":        "2026-08-11T13:00:32.478-0700",
		"rfc3339 with offset": "2026-08-11T13:00:32-07:00",
		"rfc3339 zulu":        "2026-08-11T13:00:32Z",
		"nanoseconds":         "2026-08-11T13:00:32.123456789Z",
		"no fraction":         "2026-08-11T13:00:32-0700",
		"date only":           "2026-08-11",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if parseJiraTime(raw).IsZero() == true {
				t.Errorf("%q did not parse", raw)
			}
		})
	}

	for _, raw := range []string{"", "not a time"} {
		if parseJiraTime(raw).IsZero() == false {
			t.Errorf("%q should not have parsed", raw)
		}
	}

	// The offset must survive, not be flattened to UTC.
	parsed := parseJiraTime("2026-08-11T13:00:32.478-0700")
	if _, offset := parsed.Zone(); offset != -7*60*60 {
		t.Errorf("offset lost: got %d seconds", offset)
	}
	if parsed.Year() != 2026 || parsed.Month() != time.August || parsed.Day() != 11 {
		t.Errorf("date decoded wrong: %s", parsed)
	}
}
