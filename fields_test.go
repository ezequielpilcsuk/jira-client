package jiraclient

import (
	"context"
	"io"
	"net/http"
	"slices"
	"testing"
)

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

func TestComments_CarryTheAuthorNameAndTheUpdateTime(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"total":1,"comments":[{"id":"1",
			"author":{"accountId":"acc-1","displayName":"Ada Lovelace"},
			"created":"2026-08-11T13:00:32.478-0700","updated":"2026-08-12T09:15:00.000-0700",
			"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"hi"}]}]}}]}`)
	})

	comments, err := client.Comments(context.Background(), "ABC-1", "")

	if err != nil || len(comments) != 1 {
		t.Fatalf("comments: %v, err %v", comments, err)
	}
	comment := comments[0]
	if comment.AuthorName != "Ada Lovelace" {
		t.Errorf("author name = %q", comment.AuthorName)
	}
	if comment.Updated.IsZero() == true || comment.Updated.After(comment.Created) == false {
		t.Errorf("updated = %s, created = %s", comment.Updated, comment.Created)
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

// Scoped API tokens only work through Atlassian's gateway, which serves the API but not the pages
// people open. Links have to point at the site.
func TestWithSiteURL_IsWhereIssueLinksPoint(t *testing.T) {
	client := NewClient("https://api.atlassian.com/ex/jira/cloud-id/", "e", "t",
		WithSiteURL("https://acme.atlassian.net/"))

	if got := client.IssueURL("ABC-1"); got != "https://acme.atlassian.net/browse/ABC-1" {
		t.Errorf("issue url = %q", got)
	}
	doc, err := client.NewDocBuilder().AddText("see [issue:ABC-1]").Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	link := doc.Content[0].Content[1]
	if link.Marks[0].Attrs.Href != "https://acme.atlassian.net/browse/ABC-1" {
		t.Errorf("link = %+v", link.Marks[0].Attrs)
	}
}
