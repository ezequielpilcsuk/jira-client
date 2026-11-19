package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
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
