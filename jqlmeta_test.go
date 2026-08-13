package jiraclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// /search/jql no longer returns total, so this endpoint is the only way to size a result set without
// paging the whole thing.
func TestApproximateCount_PostsTheJQLAndReadsTheCount(t *testing.T) {
	var requestedPath, requestedMethod string
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath, requestedMethod = r.URL.Path, r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		_, _ = io.WriteString(w, `{"count":1234}`)
	})

	count, err := client.ApproximateCount(context.Background(), "project = ABC AND statusCategory != Done")
	if err != nil {
		t.Fatalf("approximate count: %v", err)
	}
	if requestedPath != "/rest/api/3/search/approximate-count" {
		t.Errorf("got %q, want the approximate-count endpoint", requestedPath)
	}
	if requestedMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", requestedMethod)
	}
	if payload["jql"] != "project = ABC AND statusCategory != Done" {
		t.Errorf("jql not sent in the body: %v", payload)
	}
	if count != 1234 {
		t.Errorf("count = %d, want 1234", count)
	}
}

// A zero count is a legitimate answer, not a decode failure.
func TestApproximateCount_ZeroIsNotAnError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"count":0}`)
	})

	count, err := client.ApproximateCount(context.Background(), "project = EMPTY")
	if err != nil {
		t.Fatalf("approximate count: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

func TestApproximateCount_RejectsEmptyJQLBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })

	count, err := client.ApproximateCount(context.Background(), "   ")
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("want ErrInvalidArgument, got %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 on a rejected argument", count)
	}
	if called == true {
		t.Error("no request should have been sent")
	}
}

// /search/jql dropped validateQuery, so this is the only way to check JQL without executing it.
func TestValidateJQL_BatchesQueriesInOneStrictRequest(t *testing.T) {
	var requestedPath, validation string
	var calls int
	var payload struct {
		Queries []string `json:"queries"`
	}
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		requestedPath = r.URL.Path
		validation = r.URL.Query().Get("validation")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		_, _ = io.WriteString(w, `{"queries":[
			{"query":"project = ABC","structure":{}},
			{"query":"project = NOPE","errors":["The value 'NOPE' does not exist for the field 'project'."]}
		]}`)
	})

	results, err := client.ValidateJQL(context.Background(), "project = ABC", "project = NOPE")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if calls != 1 {
		t.Errorf("a batch must cost one request, got %d", calls)
	}
	if requestedPath != "/rest/api/3/jql/parse" {
		t.Errorf("got %q, want the /jql/parse endpoint", requestedPath)
	}
	// Without strict validation Jira downgrades an unresolvable value to a warning.
	if validation != "strict" {
		t.Errorf("validation = %q, want strict", validation)
	}
	if len(payload.Queries) != 2 || payload.Queries[1] != "project = NOPE" {
		t.Errorf("queries not batched into the body: %v", payload.Queries)
	}

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if len(results[0].Errors) != 0 {
		t.Errorf("a valid query should carry no errors: %+v", results[0])
	}
	if len(results[1].Errors) != 1 || strings.Contains(results[1].Errors[0], "does not exist") == false {
		t.Errorf("error not surfaced: %+v", results[1])
	}
	if results[1].Query != "project = NOPE" {
		t.Errorf("results must stay attributable to their query: %+v", results[1])
	}
}

func TestValidateJQL_SurfacesWarningsSeparatelyFromErrors(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"queries":[{"query":"status = Done",
			"warnings":["The operator '=' is deprecated for this field."]}]}`)
	})

	results, err := client.ValidateJQL(context.Background(), "status = Done")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	// A warned query still runs, so it must not be reported as an error.
	if len(results[0].Errors) != 0 {
		t.Errorf("a warning is not an error: %+v", results[0])
	}
	if len(results[0].Warnings) != 1 {
		t.Errorf("warning lost: %+v", results[0])
	}
}

func TestValidateJQL_RejectsAnEmptyBatchBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	ctx := context.Background()

	if _, err := client.ValidateJQL(ctx); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("empty list: want ErrInvalidArgument, got %v", err)
	}
	if _, err := client.ValidateJQL(ctx, "project = ABC", "  "); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("blank query: want ErrInvalidArgument, got %v", err)
	}
	if called == true {
		t.Error("no request should have been sent")
	}
}

// Validating a plan is a read, so it has to keep working when writes are suppressed.
func TestValidateJQL_WorksOnADryClient(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"queries":[{"query":"project = ABC"}]}`)
	}, WithDryRun(true))

	results, err := client.ValidateJQL(context.Background(), "project = ABC")
	if err != nil {
		t.Fatalf("validate on a dry client: %v", err)
	}
	if len(results) != 1 || len(results[0].Errors) != 0 {
		t.Errorf("unexpected results: %+v", results)
	}
}

func TestMyPermissions_SendsTheRequiredKeysAndOneScope(t *testing.T) {
	var requestedPath, permissions, issueKey, projectKey string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		permissions = r.URL.Query().Get("permissions")
		issueKey = r.URL.Query().Get("issueKey")
		projectKey = r.URL.Query().Get("projectKey")
		_, _ = io.WriteString(w, `{"permissions":{
			"EDIT_ISSUES":{"id":"12","key":"EDIT_ISSUES","name":"Edit Issues","type":"PROJECT",
			 "description":"Ability to edit issues.","havePermission":true},
			"DELETE_ISSUES":{"id":"13","key":"DELETE_ISSUES","name":"Delete Issues","type":"PROJECT",
			 "havePermission":false}}}`)
	})

	held, err := client.MyPermissions(context.Background(),
		PermissionScope{IssueKey: "ABC-1"}, "EDIT_ISSUES", "DELETE_ISSUES")
	if err != nil {
		t.Fatalf("my permissions: %v", err)
	}
	if requestedPath != "/rest/api/3/mypermissions" {
		t.Errorf("got %q, want the /mypermissions endpoint", requestedPath)
	}
	// The permissions parameter is comma-separated and mandatory; omitting it is a 400.
	if permissions != "EDIT_ISSUES,DELETE_ISSUES" {
		t.Errorf("permissions = %q, want the comma-joined keys", permissions)
	}
	if issueKey != "ABC-1" || projectKey != "" {
		t.Errorf("scope: issueKey=%q projectKey=%q, want only the issue", issueKey, projectKey)
	}

	// The response is a map keyed by permission, not an array.
	if len(held) != 2 {
		t.Fatalf("got %d permissions, want 2: %v", len(held), held)
	}
	if held["EDIT_ISSUES"] == false {
		t.Error("EDIT_ISSUES should be held")
	}
	if held["DELETE_ISSUES"] == true {
		t.Error("DELETE_ISSUES should not be held")
	}
}

func TestMyPermissions_ScopesByProject(t *testing.T) {
	var projectKey string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		projectKey = r.URL.Query().Get("projectKey")
		_, _ = io.WriteString(w, `{"permissions":{"CREATE_ISSUES":{"havePermission":true}}}`)
	})

	held, err := client.MyPermissions(context.Background(),
		PermissionScope{ProjectKey: "ABC"}, "CREATE_ISSUES")
	if err != nil {
		t.Fatalf("my permissions: %v", err)
	}
	if projectKey != "ABC" {
		t.Errorf("projectKey = %q, want ABC", projectKey)
	}
	if held["CREATE_ISSUES"] == false {
		t.Error("CREATE_ISSUES should be held")
	}
}

// Jira 400s a request with no permissions parameter and one carrying two contexts, so both are
// refused locally.
func TestMyPermissions_RejectsImpossibleArgumentsBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	ctx := context.Background()

	_, noKeys := client.MyPermissions(ctx, PermissionScope{})
	_, blankKey := client.MyPermissions(ctx, PermissionScope{}, " ")
	_, twoScopes := client.MyPermissions(ctx,
		PermissionScope{ProjectKey: "ABC", IssueKey: "ABC-1"}, "EDIT_ISSUES")

	cases := map[string]error{
		"no keys":    noKeys,
		"blank key":  blankKey,
		"two scopes": twoScopes,
	}
	for name, err := range cases {
		if errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("%s: want ErrInvalidArgument, got %v", name, err)
		}
	}
	if called == true {
		t.Fatal("no request should have been sent")
	}
}

// A project-scoped answer is optimistic — Atlassian documents that it can report a permission the
// account does not hold on any particular issue — so the issue-scoped check is the trustworthy one
// and the two are allowed to disagree.
func TestMyPermissions_ProjectAndIssueScopesCanDisagree(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("issueKey") != "" {
			_, _ = io.WriteString(w, `{"permissions":{"EDIT_ISSUES":{"havePermission":false}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"permissions":{"EDIT_ISSUES":{"havePermission":true}}}`)
	})
	ctx := context.Background()

	byProject, err := client.MyPermissions(ctx, PermissionScope{ProjectKey: "ABC"}, "EDIT_ISSUES")
	if err != nil {
		t.Fatalf("project scope: %v", err)
	}
	byIssue, err := client.MyPermissions(ctx, PermissionScope{IssueKey: "ABC-1"}, "EDIT_ISSUES")
	if err != nil {
		t.Fatalf("issue scope: %v", err)
	}

	if byProject["EDIT_ISSUES"] == false {
		t.Error("the project scope answered false, losing the optimistic reading")
	}
	if byIssue["EDIT_ISSUES"] == true {
		t.Error("the issue scope is the authoritative one and must be reported as-is")
	}
}
