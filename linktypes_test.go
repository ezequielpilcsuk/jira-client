package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

// liveLinkTypesResponse is a /issueLinkType body from a site that renamed a built-in and added its
// own type — exactly the case the hardcoded Link* constants get wrong.
const liveLinkTypesResponse = `{"issueLinkTypes":[
	{"id":"10000","name":"Blocks","inward":"is blocked by","outward":"blocks",
	 "self":"https://site/rest/api/3/issueLinkType/10000"},
	{"id":"10001","name":"Cloners","inward":"is cloned by","outward":"clones"},
	{"id":"10002","name":"Related to","inward":"relates to","outward":"relates to"},
	{"id":"10003","name":"Causes","inward":"is caused by","outward":"causes"}
]}`

func TestLinkTypes_DecodesTheSiteConfiguration(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, liveLinkTypesResponse)
	})

	types, err := client.LinkTypes(context.Background())
	if err != nil {
		t.Fatalf("link types: %v", err)
	}
	if requestedPath != "/rest/api/3/issueLinkType" {
		t.Errorf("got %q, want the issueLinkType endpoint", requestedPath)
	}
	if len(types) != 4 {
		t.Fatalf("got %d types, want 4", len(types))
	}
	first := types[0]
	if first.ID != "10000" || first.Name != "Blocks" {
		t.Errorf("identity: %+v", first)
	}
	if first.Inward != "is blocked by" || first.Outward != "blocks" {
		t.Errorf("phrasing: %+v", first)
	}
}

// The reason this endpoint is read rather than assumed: the site renamed "Relates" to "Related to",
// so the LinkRelates constant no longer names anything that exists.
func TestLinkTypeIDByName_ResolvesARenamedBuiltIn(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, liveLinkTypesResponse)
	})
	ctx := context.Background()

	id, err := client.LinkTypeIDByName(ctx, "Related to")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "10002" {
		t.Errorf("got %q, want 10002", id)
	}

	if _, err := client.LinkTypeIDByName(ctx, LinkRelates); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("the constant no longer matches this site; want ErrInvalidArgument, got %v", err)
	}
}

func TestLinkTypeIDByName_MatchesCaseInsensitively(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, liveLinkTypesResponse)
	})

	for _, name := range []string{"Blocks", "blocks", "BLOCKS", "bLoCkS"} {
		id, err := client.LinkTypeIDByName(context.Background(), name)
		if err != nil || id != "10000" {
			t.Errorf("%q resolved to (%q, %v), want 10000", name, id, err)
		}
	}
}

// Callers hold whichever phrase they read off the issue, so both ends resolve too.
func TestLinkTypeIDByName_MatchesInwardAndOutwardPhrasing(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, liveLinkTypesResponse)
	})

	cases := map[string]string{
		"is blocked by": "10000",
		"is cloned by":  "10001",
		"causes":        "10003",
	}
	for phrase, want := range cases {
		id, err := client.LinkTypeIDByName(context.Background(), phrase)
		if err != nil || id != want {
			t.Errorf("%q resolved to (%q, %v), want %q", phrase, id, err, want)
		}
	}
}

// A name is the more specific statement of intent, so it must not lose to an earlier type whose
// phrasing happens to read the same.
func TestLinkTypeIDByName_PrefersANameOverAnotherTypesPhrasing(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"issueLinkTypes":[
			{"id":"10000","name":"Dependency","inward":"depends on","outward":"blocks"},
			{"id":"10001","name":"Blocks","inward":"is blocked by","outward":"blocks"}]}`)
	})

	id, err := client.LinkTypeIDByName(context.Background(), "Blocks")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "10001" {
		t.Errorf("got %q, want the type actually named Blocks (10001)", id)
	}
}

func TestLinkTypeIDByName_RejectsAnUnknownOrEmptyName(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, liveLinkTypesResponse)
	})

	if _, err := client.LinkTypeIDByName(context.Background(), "Supersedes"); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("unknown name: want ErrInvalidArgument, got %v", err)
	}

	var called bool
	strict := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	if _, err := strict.LinkTypeIDByName(context.Background(), "  "); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("empty name: want ErrInvalidArgument, got %v", err)
	}
	if called == true {
		t.Error("an empty name should not have cost a request")
	}
}

// With issue linking switched off site-wide Jira 404s this endpoint rather than saying so, and the
// 404 has to reach the caller intact for that to be diagnosable.
func TestLinkTypes_DisabledLinkingSurfacesAsNotFound(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errorMessages":["Issue linking is disabled."]}`)
	})

	_, err := client.LinkTypes(context.Background())
	if errors.Is(err, ErrNotFound) == false {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestGetIssueLink_FlattensBothEnds(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, `{"id":"10100",
			"type":{"id":"10000","name":"Blocks","inward":"is blocked by","outward":"blocks"},
			"inwardIssue":{"id":"1","key":"ABC-1","fields":{"summary":"one"}},
			"outwardIssue":{"id":"2","key":"ABC-2","fields":{"summary":"two"}}}`)
	})

	link, err := client.GetIssueLink(context.Background(), "10100")
	if err != nil {
		t.Fatalf("get issue link: %v", err)
	}
	if requestedPath != "/rest/api/3/issueLink/10100" {
		t.Errorf("path: %s", requestedPath)
	}
	if link.ID != "10100" || link.InwardKey != "ABC-1" || link.OutwardKey != "ABC-2" {
		t.Errorf("ends not flattened: %+v", link)
	}
	if link.Type.Name != "Blocks" || link.Type.Outward != "blocks" {
		t.Errorf("type not flattened: %+v", link.Type)
	}
}

// Jira returns the link and omits the issue the caller cannot see, so an absent end must decode to an
// empty key rather than panic.
func TestGetIssueLink_ToleratesAnInvisibleEnd(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"10100","outwardIssue":{"key":"ABC-2"}}`)
	})

	link, err := client.GetIssueLink(context.Background(), "10100")
	if err != nil {
		t.Fatalf("an invisible end must not fail the read: %v", err)
	}
	if link.InwardKey != "" || link.Type.ID != "" {
		t.Errorf("absent objects should be zero values: %+v", link)
	}
	if link.OutwardKey != "ABC-2" {
		t.Errorf("visible end lost: %+v", link)
	}
}

func TestDeleteIssueLink_SendsADeleteForTheLinkID(t *testing.T) {
	var requestedPath, requestedMethod string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath, requestedMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.DeleteIssueLink(context.Background(), "10100"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if requestedMethod != http.MethodDelete || requestedPath != "/rest/api/3/issueLink/10100" {
		t.Errorf("got %s %s, want DELETE /rest/api/3/issueLink/10100", requestedMethod, requestedPath)
	}

	var called bool
	strict := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	if err := strict.DeleteIssueLink(context.Background(), ""); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("empty id: want ErrInvalidArgument, got %v", err)
	}
	if _, err := strict.GetIssueLink(context.Background(), ""); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("empty id: want ErrInvalidArgument, got %v", err)
	}
	if called == true {
		t.Error("an empty id should not have cost a request")
	}
}

// Discovery must work on a dry client, and deleting a link must not.
func TestLinkTypes_DryRunReadsButDoesNotDelete(t *testing.T) {
	var writes []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes = append(writes, r.Method+" "+r.URL.Path)
		}
		_, _ = io.WriteString(w, liveLinkTypesResponse)
	}, WithDryRun(true))

	ctx := context.Background()
	id, err := client.LinkTypeIDByName(ctx, "blocks")
	if err != nil || id != "10000" {
		t.Fatalf("discovery must work on a dry client: got (%q, %v)", id, err)
	}
	if err := client.DeleteIssueLink(ctx, "10100"); err != nil {
		t.Fatalf("DeleteIssueLink should be a no-op in dry run, got %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("dry run performed writes: %v", writes)
	}
}
