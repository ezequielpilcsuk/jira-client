package jiraclient

import (
	"context"
	"net/http"
	"testing"
)

func TestDeleteIssue(t *testing.T) {
	var method, path string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.DeleteIssue(context.Background(), "ABC-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if method != "DELETE" || path != "/rest/api/3/issue/ABC-1" {
		t.Errorf("got %s %s, want DELETE /rest/api/3/issue/ABC-1", method, path)
	}
}

func TestDeleteIssue_RejectsEmptyKey(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not reach the server")
	})
	if err := client.DeleteIssue(context.Background(), ""); err == nil {
		t.Fatal("empty key must be rejected")
	}
}

// Delete is a mutation, so a dry client must refuse it like every other write.
func TestDeleteIssue_DryRunDoesNotCall(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a dry client must not issue a delete")
	}, WithDryRun(true))

	if err := client.DeleteIssue(context.Background(), "ABC-1"); err != nil {
		t.Fatalf("dry delete: %v", err)
	}
}
