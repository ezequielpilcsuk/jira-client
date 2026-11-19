package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

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

// /project/search may return an empty page before its last one, so an empty page cannot end the scan
// — but a run of them with no end in sight has to.
func TestProjects_FailsWhenIsLastNeverArrives(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = io.WriteString(w, `{"values":[],"maxResults":100}`)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Projects(ctx, ProjectQuery{})

	if errors.Is(err, ErrPaginationStalled) == false {
		t.Fatalf("want ErrPaginationStalled, got %v", err)
	}
	if requests != maxEmptyProjectPages {
		t.Fatalf("requests = %d, want the %d-page allowance", requests, maxEmptyProjectPages)
	}
}

// A page with rows on it resets the allowance: only an unbroken run of empty pages counts.
func TestProjects_AnEmptyPageBetweenFullOnesIsNotAStall(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case requests == maxEmptyProjectPages+10:
			_, _ = io.WriteString(w, `{"values":[{"id":"2","key":"DEF"}],"maxResults":100,"isLast":true}`)
		case requests%2 == 0:
			_, _ = io.WriteString(w, `{"values":[],"maxResults":100}`)
		default:
			_, _ = io.WriteString(w, `{"values":[{"id":"1","key":"ABC"}],"maxResults":100}`)
		}
	})

	projects, err := client.Projects(context.Background(), ProjectQuery{})

	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	if requests != maxEmptyProjectPages+10 || len(projects) == 0 {
		t.Fatalf("requests = %d, projects = %d", requests, len(projects))
	}
}
