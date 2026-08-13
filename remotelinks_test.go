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

// liveRemoteLinksResponse is a /remotelink body with one fully-populated link and one carrying only
// the object Jira requires — the shape a link created by hand in the UI has.
const liveRemoteLinksResponse = `[
	{"id":10000,"globalId":"system=https://ci.example.com&id=42","relationship":"causes",
	 "application":{"type":"com.acme.tracker","name":"Acme CI"},
	 "object":{"url":"https://ci.example.com/build/42","title":"Build 42","summary":"nightly",
	           "icon":{"url16x16":"https://ci.example.com/favicon.ico","title":"CI"},
	           "status":{"resolved":true,"icon":{"url16x16":"https://x/i.png","title":"done","link":"https://x"}}}},
	{"id":10001,"object":{"url":"https://example.com/doc","title":"Spec"}}
]`

// postedRemoteLink captures the body SetRemoteLink sends.
func postedRemoteLink(t *testing.T, input RemoteLinkInput) map[string]any {
	t.Helper()
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		_, _ = io.WriteString(w, `{"id":10010,"self":"https://site/rest/api/3/issue/ABC-1/remotelink/10010"}`)
	})
	if _, err := client.SetRemoteLink(context.Background(), "ABC-1", input); err != nil {
		t.Fatalf("set remote link: %v", err)
	}
	return payload
}

// The endpoint answers with a bare array, not the paginated envelope the rest of the API uses.
func TestRemoteLinks_DecodesABareArray(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, liveRemoteLinksResponse)
	})

	links, err := client.RemoteLinks(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("remote links: %v", err)
	}
	if requestedPath != "/rest/api/3/issue/ABC-1/remotelink" {
		t.Errorf("got %q, want the per-issue remotelink endpoint", requestedPath)
	}
	if len(links) != 2 {
		t.Fatalf("got %d links, want 2", len(links))
	}

	first := links[0]
	if first.ID != 10000 || first.URL != "https://ci.example.com/build/42" || first.Title != "Build 42" {
		t.Errorf("object not flattened: %+v", first)
	}
	if first.Summary != "nightly" || first.Relationship != "causes" || first.Resolved == false {
		t.Errorf("nested values not flattened: %+v", first)
	}
	if first.ApplicationType != "com.acme.tracker" || first.ApplicationName != "Acme CI" {
		t.Errorf("application not flattened: %+v", first)
	}
	if first.GlobalID != "system=https://ci.example.com&id=42" {
		t.Errorf("global id: %q", first.GlobalID)
	}
}

// Jira omits application and status when they were never set; reading must not depend on them.
func TestRemoteLinks_ToleratesEveryAbsentNestedObject(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"id":10001},{"id":10002,"object":{"url":"https://x","title":"t"}}]`)
	})

	links, err := client.RemoteLinks(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("a link without an object must not fail the read: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("got %d links, want 2", len(links))
	}
	if links[0].URL != "" || links[0].Resolved == true || links[0].ApplicationName != "" {
		t.Errorf("absent objects should be zero values: %+v", links[0])
	}
	if links[1].URL != "https://x" || links[1].Resolved == true {
		t.Errorf("second link: %+v", links[1])
	}
}

func TestSetRemoteLink_SendsTheWholeObjectAndReturnsTheID(t *testing.T) {
	var requestedPath, requestedMethod string
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath, requestedMethod = r.URL.Path, r.Method
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		_, _ = io.WriteString(w, `{"id":10010,"self":"https://site/x"}`)
	})

	id, err := client.SetRemoteLink(context.Background(), "ABC-1", RemoteLinkInput{
		GlobalID: "run-42", Relationship: "causes",
		URL: "https://ci.example.com/build/42", Title: "Build 42", Summary: "nightly",
		IconURL: "https://ci.example.com/favicon.ico", IconTitle: "CI",
		ApplicationType: "com.acme.tracker", ApplicationName: "Acme CI",
		Resolved: true,
	})
	if err != nil {
		t.Fatalf("set remote link: %v", err)
	}
	if id != 10010 {
		t.Errorf("got id %d, want 10010", id)
	}
	if requestedMethod != http.MethodPost || requestedPath != "/rest/api/3/issue/ABC-1/remotelink" {
		t.Errorf("got %s %s, want POST the per-issue remotelink endpoint", requestedMethod, requestedPath)
	}

	object, _ := payload["object"].(map[string]any)
	if object["url"] != "https://ci.example.com/build/42" || object["title"] != "Build 42" {
		t.Fatalf("object: %v", payload["object"])
	}
	if object["summary"] != "nightly" {
		t.Errorf("summary not sent: %v", object)
	}
	icon, _ := object["icon"].(map[string]any)
	if icon["url16x16"] != "https://ci.example.com/favicon.ico" || icon["title"] != "CI" {
		t.Errorf("icon: %v", object["icon"])
	}
	status, _ := object["status"].(map[string]any)
	if status["resolved"] != true {
		t.Errorf("resolved: %v", object["status"])
	}
	if payload["globalId"] != "run-42" || payload["relationship"] != "causes" {
		t.Errorf("top-level fields: %v", payload)
	}
	application, _ := payload["application"].(map[string]any)
	if application["type"] != "com.acme.tracker" || application["name"] != "Acme CI" {
		t.Errorf("application: %v", payload["application"])
	}
}

// The write replaces rather than merges, so the resolved flag has to be stated on every call —
// omitting it would clear it on an existing link instead of leaving it alone.
func TestSetRemoteLink_AlwaysStatesResolvedAndOmitsUnsetObjects(t *testing.T) {
	payload := postedRemoteLink(t, RemoteLinkInput{URL: "https://x", Title: "t"})

	object, _ := payload["object"].(map[string]any)
	status, present := object["status"].(map[string]any)
	if present == false {
		t.Fatalf("status must always be sent: %v", object)
	}
	if status["resolved"] != false {
		t.Errorf("resolved should be an explicit false: %v", status)
	}
	for _, key := range []string{"summary", "icon"} {
		if _, sent := object[key]; sent == true {
			t.Errorf("unset %q should be omitted: %v", key, object)
		}
	}
	for _, key := range []string{"globalId", "relationship", "application"} {
		if _, sent := payload[key]; sent == true {
			t.Errorf("unset %q should be omitted: %v", key, payload)
		}
	}
}

// The upsert is the whole point: a repeated global ID must go straight back out as a POST, with no
// read-first and no switch to a PUT by id.
func TestSetRemoteLink_RepeatsThePostForAKnownGlobalID(t *testing.T) {
	var methods []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		_, _ = io.WriteString(w, `{"id":10010,"self":"https://site/x"}`)
	})

	ctx := context.Background()
	input := RemoteLinkInput{GlobalID: "run-42", URL: "https://x", Title: "Build 42"}
	for attempt := 0; attempt < 2; attempt++ {
		id, err := client.SetRemoteLink(ctx, "ABC-1", input)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if id != 10010 {
			t.Errorf("attempt %d returned id %d, want the same 10010", attempt, id)
		}
	}

	if len(methods) != 2 {
		t.Fatalf("got %v, want exactly one request per call", methods)
	}
	for _, method := range methods {
		if method != http.MethodPost {
			t.Errorf("got %s, want every upsert to be a POST: %v", method, methods)
		}
	}
}

// Jira rejects an object without a url or a title, so it never gets sent.
func TestSetRemoteLink_RejectsBeforeSending(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not reach the server")
	})
	ctx := context.Background()

	cases := map[string]RemoteLinkInput{
		"no url":       {Title: "t"},
		"no title":     {URL: "https://x"},
		"blank url":    {URL: "   ", Title: "t"},
		"blank title":  {URL: "https://x", Title: "  "},
		"neither":      {},
		"only summary": {Summary: "s"},
	}
	for name, input := range cases {
		id, err := client.SetRemoteLink(ctx, "ABC-1", input)
		if errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("%s: want ErrInvalidArgument, got %v", name, err)
		}
		if id != 0 {
			t.Errorf("%s: a rejected link must not report an id, got %d", name, id)
		}
	}

	if _, err := client.SetRemoteLink(ctx, "", RemoteLinkInput{URL: "https://x", Title: "t"}); errors.Is(err, ErrInvalidArgument) == false {
		t.Errorf("empty key: want ErrInvalidArgument, got %v", err)
	}
	for name, err := range map[string]error{
		"delete without a key":       client.DeleteRemoteLink(ctx, "", "10000"),
		"delete without an id":       client.DeleteRemoteLink(ctx, "ABC-1", ""),
		"delete without a global id": client.DeleteRemoteLinkByGlobalID(ctx, "ABC-1", ""),
		"list without a key":         mustErr(func() error { _, err := client.RemoteLinks(ctx, ""); return err }),
	} {
		if errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("%s: want ErrInvalidArgument, got %v", name, err)
		}
	}
}

// 2,000 remote links per issue is a permanent ceiling, not a rate limit.
func TestSetRemoteLink_PerIssueCeilingIsALimitNotARateLimit(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"errorMessages":["Issue has too many remote links"]}`)
	})

	_, err := client.SetRemoteLink(context.Background(), "ABC-1",
		RemoteLinkInput{URL: "https://x", Title: "t"})
	if errors.Is(err, ErrLimitExceeded) == false {
		t.Errorf("want ErrLimitExceeded, got %v", err)
	}
	if errors.Is(err, ErrRateLimited) == true {
		t.Error("a per-issue ceiling must not be confused with a rate limit")
	}
}

func TestDeleteRemoteLink_TargetsTheLinkID(t *testing.T) {
	var requestedPath, requestedMethod string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath, requestedMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.DeleteRemoteLink(context.Background(), "ABC-1", "10000"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if requestedMethod != http.MethodDelete {
		t.Errorf("method: %s", requestedMethod)
	}
	if requestedPath != "/rest/api/3/issue/ABC-1/remotelink/10000" {
		t.Errorf("path: %s", requestedPath)
	}
}

// Generated global ids routinely carry "&" and "=". Unescaped, the query would truncate at the first
// separator and delete a different link, or none.
func TestDeleteRemoteLinkByGlobalID_EscapesReservedCharacters(t *testing.T) {
	const globalID = "system=https://ci.example.com/builds&id=42"

	var rawQuery, decodedGlobalID, requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		rawQuery, requestedPath = r.URL.RawQuery, r.URL.Path
		decodedGlobalID = r.URL.Query().Get("globalId")
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.DeleteRemoteLinkByGlobalID(context.Background(), "ABC-1", globalID); err != nil {
		t.Fatalf("delete by global id: %v", err)
	}
	if requestedPath != "/rest/api/3/issue/ABC-1/remotelink" {
		t.Errorf("path: %s", requestedPath)
	}
	if decodedGlobalID != globalID {
		t.Errorf("global id did not survive the round trip: got %q, want %q", decodedGlobalID, globalID)
	}
	if strings.Contains(rawQuery, "&id=42") == true {
		t.Errorf("the separator was sent unescaped, splitting the value: %s", rawQuery)
	}
}

// A dry client must be able to list remote links while writing none of them.
func TestRemoteLinks_DryRunSuppressesWritesButNotReads(t *testing.T) {
	var writes []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes = append(writes, r.Method+" "+r.URL.Path)
		}
		_, _ = io.WriteString(w, liveRemoteLinksResponse)
	}, WithDryRun(true))

	ctx := context.Background()
	links, err := client.RemoteLinks(ctx, "ABC-1")
	if err != nil || len(links) != 2 {
		t.Fatalf("read must still work on a dry client: %d links, %v", len(links), err)
	}

	id, err := client.SetRemoteLink(ctx, "ABC-1", RemoteLinkInput{URL: "https://x", Title: "t"})
	if err != nil {
		t.Fatalf("SetRemoteLink should be a no-op in dry run, got %v", err)
	}
	if id != 0 {
		t.Errorf("a dry run created nothing, so it has no id to report, got %d", id)
	}
	for name, err := range map[string]error{
		"DeleteRemoteLink":           client.DeleteRemoteLink(ctx, "ABC-1", "10000"),
		"DeleteRemoteLinkByGlobalID": client.DeleteRemoteLinkByGlobalID(ctx, "ABC-1", "run-42"),
	} {
		if err != nil {
			t.Errorf("%s should be a no-op in dry run, got %v", name, err)
		}
	}
	if len(writes) != 0 {
		t.Fatalf("dry run performed writes: %v", writes)
	}
}
