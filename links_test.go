package jiraclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func postedLink(t *testing.T, call func(*Client) error) map[string]any {
	t.Helper()
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusCreated)
	})
	if err := call(client); err != nil {
		t.Fatalf("link: %v", err)
	}
	return payload
}

// Sites rename link types, so an ID must be usable as the stable reference.
func TestCreateLink_ByTypeID(t *testing.T) {
	payload := postedLink(t, func(c *Client) error {
		return c.CreateLink(context.Background(), LinkInput{
			TypeID: "10003", InwardKey: "ABC-1", OutwardKey: "ABC-2",
		})
	})
	linkType, _ := payload["type"].(map[string]any)
	if linkType["id"] != "10003" {
		t.Errorf("expected the type id, got %v", payload["type"])
	}
	if _, named := linkType["name"]; named == true {
		t.Errorf("an id link must not also send a name: %v", linkType)
	}
}

func TestCreateLink_CarriesAComment(t *testing.T) {
	doc, _ := TextDoc("This issue might be related to ABC-2")
	payload := postedLink(t, func(c *Client) error {
		return c.CreateLink(context.Background(), LinkInput{
			TypeID: "10003", InwardKey: "ABC-1", OutwardKey: "ABC-2", Comment: doc,
		})
	})
	if _, ok := payload["comment"]; ok == false {
		t.Errorf("comment not sent with the link: %v", payload)
	}
}

// LinkIssues keeps its old behaviour exactly: a name, and no comment.
func TestLinkIssues_UnchangedShape(t *testing.T) {
	payload := postedLink(t, func(c *Client) error {
		return c.LinkIssues(context.Background(), "Relates", "ABC-1", "ABC-2")
	})
	linkType, _ := payload["type"].(map[string]any)
	if linkType["name"] != "Relates" {
		t.Errorf("expected a named type, got %v", payload["type"])
	}
	if _, ok := payload["comment"]; ok == true {
		t.Errorf("LinkIssues must not send a comment: %v", payload)
	}
}

func TestCreateLink_Rejects(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not reach the server")
	})
	ctx := context.Background()
	for name, input := range map[string]LinkInput{
		"no type":   {InwardKey: "A-1", OutwardKey: "A-2"},
		"no inward": {TypeID: "1", OutwardKey: "A-2"},
		"self link": {TypeID: "1", InwardKey: "A-1", OutwardKey: "A-1"},
	} {
		if err := client.CreateLink(ctx, input); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}
