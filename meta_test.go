package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// fieldsResponse is a /rest/api/3/field body: a system field with no schema of its own, a custom
// field, and two custom fields that share the name "Severity" — which Jira permits.
const fieldsResponse = `[
	{"id":"summary","key":"summary","name":"Summary","custom":false,"navigable":true,
	 "orderable":true,"searchable":true,"clauseNames":["summary"],
	 "schema":{"type":"string","system":"summary"}},
	{"id":"customfield_10004","key":"customfield_10004","name":"Story Points","custom":true,
	 "navigable":true,"orderable":true,"searchable":true,
	 "clauseNames":["cf[10004]","Story Points"],
	 "schema":{"type":"number","custom":"com.atlassian.jira.plugin.system.customfieldtypes:float",
	           "customId":10004}},
	{"id":"customfield_10050","key":"customfield_10050","name":"Severity","custom":true,
	 "clauseNames":["cf[10050]","Severity"],
	 "schema":{"type":"option","custom":"com.atlassian.jira.plugin.system.customfieldtypes:select"}},
	{"id":"customfield_10099","key":"customfield_10099","name":"Severity","custom":true,
	 "clauseNames":["cf[10099]","Severity"],
	 "schema":{"type":"array","items":"string",
	           "custom":"com.atlassian.jira.plugin.system.customfieldtypes:labels"}},
	{"id":"issuekey","key":"issuekey","name":"Key","custom":false,"clauseNames":["id","issue","key"]}
]`

func TestFields_DecodesSchemaAndClauseNames(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, fieldsResponse)
	})

	fields, err := client.Fields(context.Background())
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	if requestedPath != "/rest/api/3/field" {
		t.Errorf("got %q, want the /field endpoint", requestedPath)
	}
	if len(fields) != 5 {
		t.Fatalf("got %d fields, want 5", len(fields))
	}

	points := fields[1]
	if points.ID != "customfield_10004" || points.Custom == false {
		t.Errorf("custom field not decoded: %+v", points)
	}
	if points.SchemaType != "number" || strings.Contains(points.SchemaCustom, "float") == false {
		t.Errorf("schema not decoded: %+v", points)
	}
	// JQL takes cf[10004] as well as the display name; a caller building a query needs both.
	if len(points.ClauseNames) != 2 || points.ClauseNames[0] != "cf[10004]" {
		t.Errorf("clause names lost: %v", points.ClauseNames)
	}
	if fields[3].SchemaItems != "string" {
		t.Errorf("array item type lost: %+v", fields[3])
	}
}

// A field can arrive with no schema at all, and must not take the decode down with it.
func TestFields_ToleratesAMissingSchema(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fieldsResponse)
	})

	fields, err := client.Fields(context.Background())
	if err != nil {
		t.Fatalf("fields: %v", err)
	}

	bare := fields[4]
	if bare.ID != "issuekey" {
		t.Fatalf("unexpected field: %+v", bare)
	}
	if bare.SchemaType != "" || bare.SchemaItems != "" || bare.SchemaCustom != "" {
		t.Errorf("absent schema should be zero: %+v", bare)
	}
}

// Resolving "Story Points" to customfield_10004 is the whole point: the ID is per-site.
func TestFieldIDByName_ResolvesCaseInsensitively(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fieldsResponse)
	})

	id, err := client.FieldIDByName(context.Background(), "story points")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "customfield_10004" {
		t.Errorf("got %q, want customfield_10004", id)
	}
}

// Jira does not enforce unique field names, so picking the first "Severity" would write to the wrong
// field on some sites and the right one on others — undiagnosable from the caller's side.
func TestFieldIDByName_AmbiguousNameIsAnErrorNotAGuess(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fieldsResponse)
	})

	id, err := client.FieldIDByName(context.Background(), "Severity")
	if err == nil {
		t.Fatalf("two fields share the name, want an error, got %q", id)
	}
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("want ErrInvalidArgument, got %v", err)
	}
	if id != "" {
		t.Errorf("no id should be returned on an ambiguity, got %q", id)
	}
	// The message has to name the candidates, or the caller cannot act on it.
	for _, want := range []string{"customfield_10050", "customfield_10099"} {
		if strings.Contains(err.Error(), want) == false {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

func TestFieldsByName_ReturnsEveryMatchSoCallersCanDisambiguate(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fieldsResponse)
	})
	ctx := context.Background()

	matches, err := client.FieldsByName(ctx, "SEVERITY")
	if err != nil {
		t.Fatalf("fields by name: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2: %+v", len(matches), matches)
	}
	// The schemas differ, which is how a caller tells them apart.
	if matches[0].SchemaType == matches[1].SchemaType {
		t.Errorf("expected distinguishable schemas: %+v", matches)
	}

	// A name nobody uses is an empty result, not an error.
	none, err := client.FieldsByName(ctx, "Nonexistent")
	if err != nil {
		t.Fatalf("unknown name should not error: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("got %d matches, want 0", len(none))
	}
}

func TestFieldIDByName_UnknownNameIsRejected(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fieldsResponse)
	})

	_, err := client.FieldIDByName(context.Background(), "Story Point")
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("want ErrInvalidArgument, got %v", err)
	}
}

func TestFieldsByName_RejectsAnEmptyNameBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })

	if _, err := client.FieldsByName(context.Background(), "  "); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("want ErrInvalidArgument, got %v", err)
	}
	if called == true {
		t.Error("no request should have been sent")
	}
}

func TestMyself_DecodesTheAuthenticatedAccount(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, `{"accountId":"5b10ac8d","accountType":"atlassian",
			"displayName":"Automation Bot","emailAddress":"bot@example.com","active":true,
			"timeZone":"America/Los_Angeles","locale":"en_US"}`)
	})

	me, err := client.Myself(context.Background())
	if err != nil {
		t.Fatalf("myself: %v", err)
	}
	if requestedPath != "/rest/api/3/myself" {
		t.Errorf("got %q, want the /myself endpoint", requestedPath)
	}
	if me.AccountID != "5b10ac8d" || me.DisplayName != "Automation Bot" {
		t.Errorf("account not decoded: %+v", me)
	}
	if me.Email != "bot@example.com" || me.Active == false {
		t.Errorf("email/active not decoded: %+v", me)
	}
}

// Profile visibility hides emailAddress even on your own account, so its absence is normal.
func TestMyself_ToleratesAHiddenEmail(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"accountId":"5b10ac8d","displayName":"Bot","active":true}`)
	})

	me, err := client.Myself(context.Background())
	if err != nil {
		t.Fatalf("a hidden email must not fail the decode: %v", err)
	}
	if me.Email != "" || me.AccountID != "5b10ac8d" {
		t.Errorf("unexpected account: %+v", me)
	}
}

// A 401 here means the token is wrong or expired, which is the reason to call it at startup.
func TestMyself_UnauthorizedMapsToTheCredentialSentinel(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errorMessages":["Client must be authenticated"]}`)
	})

	if _, err := client.Myself(context.Background()); errors.Is(err, ErrUnauthorized) == false {
		t.Errorf("want ErrUnauthorized, got %v", err)
	}
}

func TestServerInfo_ReportsTheSiteTimeZone(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, `{"baseUrl":"https://example.atlassian.net","version":"1001.0.0",
			"versionNumbers":[1001,0,0],"deploymentType":"Cloud","buildNumber":100245,
			"buildDate":"2026-08-01T00:00:00.000-0700",
			"serverTime":"2026-08-11T13:00:32.478-0700",
			"serverTimeZone":"America/Los_Angeles","serverTitle":"Example Jira"}`)
	})

	info, err := client.ServerInfo(context.Background())
	if err != nil {
		t.Fatalf("server info: %v", err)
	}
	if requestedPath != "/rest/api/3/serverInfo" {
		t.Errorf("got %q, want the /serverInfo endpoint", requestedPath)
	}
	if info.BaseURL != "https://example.atlassian.net" || info.DeploymentType != "Cloud" {
		t.Errorf("instance identity not decoded: %+v", info)
	}
	if info.ServerTitle != "Example Jira" || info.Version != "1001.0.0" {
		t.Errorf("title/version not decoded: %+v", info)
	}
	// The zone is the point of the call: timestamps come back in it, not in UTC.
	if info.ServerTimeZone != "America/Los_Angeles" {
		t.Errorf("time zone lost: %q", info.ServerTimeZone)
	}
	if info.ServerTime.IsZero() == true {
		t.Fatal("server time did not parse")
	}
	if _, offset := info.ServerTime.Zone(); offset != -7*60*60 {
		t.Errorf("offset lost: got %d seconds", offset)
	}
}

// The plain /rest/api/3/project endpoint is deprecated, so the client must be on /project/search.
func TestProjects_UsesTheNonDeprecatedEndpoint(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, `{"isLast":true,"values":[]}`)
	})

	if _, err := client.Projects(context.Background(), ProjectQuery{}); err != nil {
		t.Fatalf("projects: %v", err)
	}
	if requestedPath != "/rest/api/3/project/search" {
		t.Errorf("got %q, want the /project/search endpoint", requestedPath)
	}
}

// Jira clamps maxResults above 100 silently, so asking for more than that is never useful.
func TestProjects_RequestsAtMostTheClampedPageSize(t *testing.T) {
	var requestedMax string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedMax = r.URL.Query().Get("maxResults")
		_, _ = io.WriteString(w, `{"isLast":true,"values":[]}`)
	})

	if _, err := client.Projects(context.Background(), ProjectQuery{}); err != nil {
		t.Fatalf("projects: %v", err)
	}
	if requestedMax != "100" {
		t.Errorf("maxResults = %q, want 100", requestedMax)
	}
}

// Jira documents that total can change between pages and that a page can legitimately come back
// empty, so paging must be driven by isLast alone. A total-derived page count stops after page one
// here; a values-length step stalls forever on the empty page.
func TestProjects_PagesOnIsLastNotOnTotalOrRowCount(t *testing.T) {
	var requestedStarts []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		startAt := r.URL.Query().Get("startAt")
		requestedStarts = append(requestedStarts, startAt)
		switch startAt {
		case "0":
			_, _ = io.WriteString(w, `{"startAt":0,"maxResults":100,"total":1,"isLast":false,
				"values":[{"id":"1","key":"ABC","name":"Alpha"}]}`)
		case "100":
			// Empty but not last: Jira permits this, and it must not end the scan.
			_, _ = io.WriteString(w, `{"startAt":100,"maxResults":100,"total":9,"isLast":false,"values":[]}`)
		default:
			_, _ = io.WriteString(w, `{"startAt":200,"maxResults":100,"total":2,"isLast":true,
				"values":[{"id":"2","key":"DEF","name":"Delta"}]}`)
		}
	})

	projects, err := client.Projects(context.Background(), ProjectQuery{})
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	if len(requestedStarts) != 3 {
		t.Fatalf("got %d requests, want 3: %v", len(requestedStarts), requestedStarts)
	}
	if requestedStarts[1] != "100" || requestedStarts[2] != "200" {
		t.Errorf("startAt must advance by the page size: %v", requestedStarts)
	}
	if len(projects) != 2 || projects[0].Key != "ABC" || projects[1].Key != "DEF" {
		t.Errorf("pages not accumulated in order: %+v", projects)
	}
}

func TestProjects_PassesEveryFilterThrough(t *testing.T) {
	var query url.Values
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		_, _ = io.WriteString(w, `{"isLast":true,"values":[]}`)
	})

	_, err := client.Projects(context.Background(), ProjectQuery{
		Query:    "plat",
		Keys:     []string{"ABC", "DEF"},
		IDs:      []string{"10001"},
		TypeKeys: []string{"software", "service_desk"},
		Action:   "browse",
		OrderBy:  "-name",
	})
	if err != nil {
		t.Fatalf("projects: %v", err)
	}

	if query.Get("query") != "plat" || query.Get("orderBy") != "-name" || query.Get("action") != "browse" {
		t.Errorf("scalar filters lost: %v", query)
	}
	// keys and id are repeatable parameters, not comma-joined ones.
	if keys := query["keys"]; len(keys) != 2 || keys[0] != "ABC" || keys[1] != "DEF" {
		t.Errorf("keys not repeated: %v", keys)
	}
	if ids := query["id"]; len(ids) != 1 || ids[0] != "10001" {
		t.Errorf("ids not repeated: %v", ids)
	}
	if query.Get("typeKey") != "software,service_desk" {
		t.Errorf("typeKey should be comma-joined, got %q", query.Get("typeKey"))
	}
}

// Jira caps keys and id at 50 each, and rejects an action outside its four values.
func TestProjects_RejectsImpossibleFiltersBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	ctx := context.Background()

	oversized := make([]string, maxProjectFilterValues+1)
	for i := range oversized {
		oversized[i] = "ABC"
	}

	cases := map[string]ProjectQuery{
		"too many keys":  {Keys: oversized},
		"too many ids":   {IDs: oversized},
		"unknown action": {Action: "delete"},
	}
	for name, filter := range cases {
		if _, err := client.Projects(ctx, filter); errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("%s: want ErrInvalidArgument, got %v", name, err)
		}
	}
	if called == true {
		t.Fatal("no request should have been sent")
	}
}

func TestGetProject_FlattensLeadAndCategory(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, `{"id":"10001","key":"ABC","name":"Alpha",
			"projectTypeKey":"software","style":"next-gen","simplified":true,"isPrivate":false,
			"archived":false,"description":"The alpha project",
			"lead":{"accountId":"5b10ac8d","displayName":"Ada"},
			"projectCategory":{"name":"Platform"}}`)
	})

	project, err := client.GetProject(context.Background(), "ABC")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if requestedPath != "/rest/api/3/project/ABC" {
		t.Errorf("got %q, want /project/ABC", requestedPath)
	}
	if project.LeadID != "5b10ac8d" || project.LeadName != "Ada" {
		t.Errorf("lead not flattened: %+v", project)
	}
	if project.CategoryName != "Platform" || project.Description != "The alpha project" {
		t.Errorf("category/description not decoded: %+v", project)
	}
	// simplified marks a team-managed project, which has its own field and workflow rules.
	if project.Simplified == false || project.Style != "next-gen" {
		t.Errorf("team-managed markers lost: %+v", project)
	}
}

// A project with no lead and no category omits both keys entirely rather than sending nulls.
func TestGetProject_ToleratesAnAbsentLeadAndCategory(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"10002","key":"DEF","name":"Delta"}`)
	})

	project, err := client.GetProject(context.Background(), "DEF")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.LeadID != "" || project.LeadName != "" || project.CategoryName != "" {
		t.Errorf("absent nested objects should be zero: %+v", project)
	}
	if project.Key != "DEF" {
		t.Errorf("key: %+v", project)
	}
}

func TestGetProject_RejectsAnEmptyKeyBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })

	if _, err := client.GetProject(context.Background(), " "); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("want ErrInvalidArgument, got %v", err)
	}
	if called == true {
		t.Error("no request should have been sent")
	}
}

// Status names are per-workflow and site-editable — "Shipped" here is a done status despite the
// name, and only statusCategory.key says so.
func TestProjectStatuses_ExposesTheStableCategoryKey(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, `[
			{"id":"10001","name":"Bug","subtask":false,"statuses":[
				{"id":"1","name":"Open","statusCategory":{"id":2,"key":"new","name":"To Do",
				 "colorName":"blue-gray"}},
				{"id":"3","name":"In Review","statusCategory":{"id":4,"key":"indeterminate",
				 "name":"In Progress","colorName":"yellow"}},
				{"id":"10100","name":"Shipped","statusCategory":{"id":3,"key":"done","name":"Done",
				 "colorName":"green"}}]},
			{"id":"10003","name":"Sub-task","subtask":true,"statuses":[
				{"id":"1","name":"Open","statusCategory":{"key":"new","name":"To Do"}}]}
		]`)
	})

	byIssueType, err := client.ProjectStatuses(context.Background(), "ABC")
	if err != nil {
		t.Fatalf("project statuses: %v", err)
	}
	if requestedPath != "/rest/api/3/project/ABC/statuses" {
		t.Errorf("got %q, want /project/ABC/statuses", requestedPath)
	}
	if len(byIssueType) != 2 {
		t.Fatalf("got %d issue types, want 2", len(byIssueType))
	}

	bug := byIssueType[0]
	if bug.IssueTypeID != "10001" || bug.IssueTypeName != "Bug" || bug.Subtask == true {
		t.Errorf("issue type not decoded: %+v", bug)
	}
	if len(bug.Statuses) != 3 {
		t.Fatalf("got %d statuses, want 3", len(bug.Statuses))
	}

	shipped := bug.Statuses[2]
	if shipped.Name != "Shipped" || shipped.CategoryKey != "done" || shipped.CategoryName != "Done" {
		t.Errorf("a done status under a custom name must still report key=done: %+v", shipped)
	}
	if bug.Statuses[0].CategoryKey != "new" || bug.Statuses[1].CategoryKey != "indeterminate" {
		t.Errorf("category keys lost: %+v", bug.Statuses)
	}
	// The sub-task workflow is reported separately because it is usually not the parent's.
	if byIssueType[1].Subtask == false {
		t.Errorf("subtask flag lost: %+v", byIssueType[1])
	}
}

// Jira nests statusCategory, and a decode that assumed it is present would panic without it.
func TestProjectStatuses_ToleratesAnAbsentStatusCategory(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"10001","name":"Task","statuses":[{"id":"1","name":"Open"}]}]`)
	})

	byIssueType, err := client.ProjectStatuses(context.Background(), "ABC")
	if err != nil {
		t.Fatalf("project statuses: %v", err)
	}
	status := byIssueType[0].Statuses[0]
	if status.CategoryKey != "" || status.CategoryName != "" {
		t.Errorf("absent category should be zero: %+v", status)
	}
}

func TestProjectStatuses_RejectsAnEmptyProjectBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })

	_, err := client.ProjectStatuses(context.Background(), "")
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("want ErrInvalidArgument, got %v", err)
	}
	if called == true {
		t.Error("no request should have been sent")
	}
}

// Site metadata is read-only, so a dry client must be able to resolve it — a dry run needs the
// custom field IDs it is planning to write just as much as a live one does.
func TestSiteMetadata_ReadsOnADryClient(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fieldsResponse)
	}, WithDryRun(true))

	id, err := client.FieldIDByName(context.Background(), "Story Points")
	if err != nil {
		t.Fatalf("field lookup on a dry client: %v", err)
	}
	if id != "customfield_10004" {
		t.Errorf("got %q, want customfield_10004", id)
	}
}
