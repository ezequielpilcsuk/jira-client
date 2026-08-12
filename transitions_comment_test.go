package jiraclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

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
