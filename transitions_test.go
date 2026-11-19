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

func TestTransitionByName_ResolvesID(t *testing.T) {
	var transitioned string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"transitions":[
				{"id":"11","name":"To Do","to":{"id":"10002","name":"To Do"}},
				{"id":"71","name":"Won't Do","to":{"id":"10100","name":"Won't Do"}}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]map[string]string
		_ = json.Unmarshal(body, &payload)
		transitioned = payload["transition"]["id"]
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.TransitionByName(context.Background(), "ABC-1", "won't do"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if transitioned != "71" {
		t.Fatalf("resolved to %q, want 71", transitioned)
	}
}

func TestTransitionByName_UnavailableTransition(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"transitions":[{"id":"11","name":"To Do","to":{}}]}`)
	})

	err := client.TransitionByName(context.Background(), "ABC-1", "Won't Do")
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}

// A transition that explains itself should carry the comment in the same request: Jira applies both
// atomically, so a rejected transition cannot leave an orphaned comment behind.
func TestTransitionWithComment_SendsBothInOneRequest(t *testing.T) {
	var requests int
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusNoContent)
	})

	doc, err := TextDoc("After validating this issue please change status to \"To Do\"")
	if err != nil {
		t.Fatalf("build doc: %v", err)
	}
	if err := client.TransitionWithComment(context.Background(), "ABC-1", "11", doc); err != nil {
		t.Fatalf("transition: %v", err)
	}

	if requests != 1 {
		t.Fatalf("expected one request, got %d", requests)
	}
	transition, ok := payload["transition"].(map[string]any)
	if ok == false || transition["id"] != "11" {
		t.Errorf("transition id missing from payload: %v", payload)
	}
	update, ok := payload["update"].(map[string]any)
	if ok == false {
		t.Fatalf("comment not attached to the transition: %v", payload)
	}
	if _, ok := update["comment"]; ok == false {
		t.Errorf("update block carries no comment: %v", update)
	}
}

// A nil comment must produce exactly the payload Transition always sent, so adding this did not
// change the plain path.
func TestTransitionWithComment_NilCommentMatchesPlainTransition(t *testing.T) {
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.Transition(context.Background(), "ABC-1", "11"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if _, present := payload["update"]; present == true {
		t.Errorf("a plain transition must not send an update block: %v", payload)
	}
}

func TestTransitionByNameWithComment_ResolvesThenComments(t *testing.T) {
	var transitionPayload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"transitions":[{"id":"21","name":"Pre-check","to":{"id":"2","name":"Pre-check"}}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &transitionPayload)
		w.WriteHeader(http.StatusNoContent)
	})

	doc, _ := TextDoc("please validate")
	if err := client.TransitionByNameWithComment(context.Background(), "ABC-1", "pre-CHECK", doc); err != nil {
		t.Fatalf("transition by name: %v", err)
	}

	transition, _ := transitionPayload["transition"].(map[string]any)
	if transition["id"] != "21" {
		t.Errorf("resolved the wrong transition: %v", transitionPayload)
	}
	if _, ok := transitionPayload["update"]; ok == false {
		t.Errorf("comment not attached: %v", transitionPayload)
	}
}

func TestTransitionWithComment_DryRunDoesNotCall(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a dry client must not transition")
	}, WithDryRun(true))

	doc, _ := TextDoc("x")
	if err := client.TransitionWithComment(context.Background(), "ABC-1", "11", doc); err != nil {
		t.Fatalf("dry transition: %v", err)
	}
}

// A workflow that demands a resolution on the way to done rejects a bare transition. The fields go
// in the same request, and so does the comment explaining the move.
func TestApplyTransition_SendsFieldsAndCommentWithTheTransition(t *testing.T) {
	var posted map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		posted = decodeBody(t, r)
		w.WriteHeader(http.StatusNoContent)
	})
	comment, _ := TextDoc("closing")

	err := client.ApplyTransition(context.Background(), "ABC-1", TransitionInput{
		ID:      "31",
		Comment: comment,
		Fields:  map[string]any{"resolution": map[string]string{"name": "Won't Do"}},
	})

	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	encoded, _ := json.Marshal(posted)
	for _, fragment := range []string{
		`"transition":{"id":"31"}`,
		`"fields":{"resolution":{"name":"Won't Do"}}`,
		`"update":{"comment":[{"add":{"body":{`,
	} {
		if strings.Contains(string(encoded), fragment) == false {
			t.Errorf("payload lacks %s: %s", fragment, encoded)
		}
	}
}

func TestApplyTransition_ResolvesTheTransitionByName(t *testing.T) {
	var postedID any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"transitions":[{"id":"11","name":"Start"},{"id":"31","name":"Won't Do"}]}`)
			return
		}
		postedID = decodeBody(t, r)["transition"].(map[string]any)["id"]
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.ApplyTransition(context.Background(), "ABC-1", TransitionInput{Name: "won't do"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if postedID != "31" {
		t.Fatalf("posted transition id = %v, want 31", postedID)
	}
}

// Naming a transition the issue does not offer is the commonest failure, and the fix is nearly always
// visible in the list of what it does offer.
func TestApplyTransition_AnUnknownNameSaysWhatIsAvailable(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"transitions":[{"id":"11","name":"Start"},{"id":"31","name":"Won't Do"}]}`)
	})

	err := client.ApplyTransition(context.Background(), "ABC-1", TransitionInput{Name: "Done"})

	if errors.Is(err, ErrInvalidArgument) == false {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if strings.Contains(err.Error(), "Start") == false || strings.Contains(err.Error(), "Won't Do") == false {
		t.Fatalf("the error must list the available transitions: %v", err)
	}
}

func TestApplyTransition_NeedsExactlyOneOfIDAndName(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { requests++ })

	for name, input := range map[string]TransitionInput{
		"neither": {},
		"both":    {ID: "31", Name: "Done"},
	} {
		if err := client.ApplyTransition(context.Background(), "ABC-1", input); errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("%s: want ErrInvalidArgument, got %v", name, err)
		}
	}
	if requests != 0 {
		t.Fatalf("made %d requests for inputs that could not succeed", requests)
	}
}

// DryRunID stands in for an issue that was never created, so nothing may be asked of Jira about it.
// A transition by name starts with a read, and that read must be skipped along with the write.
func TestTransitionByName_OnTheDryRunPlaceholderAsksJiraNothing(t *testing.T) {
	requests := 0
	dry := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
	}, WithDryRun(true))

	key, err := dry.CreateIssue(context.Background(),
		CreateIssueInput{ProjectKey: "ABC", IssueType: "Task", Summary: "s"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := dry.TransitionByName(context.Background(), key, "Done"); err != nil {
		t.Errorf("transition by name: %v", err)
	}
	doc, _ := TextDoc("closing")
	if err := dry.TransitionByNameWithComment(context.Background(), key, "Done", doc); err != nil {
		t.Errorf("transition by name with comment: %v", err)
	}
	if requests != 0 {
		t.Fatalf("made %d requests about an issue that does not exist", requests)
	}
}
