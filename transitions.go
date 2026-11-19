package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Transition moves an issue through a workflow transition, identified by its transition ID (not the
// destination status ID — they differ, and using the wrong one silently 400s).
// Use Transitions to discover the available IDs for an issue.
func (c *Client) Transition(ctx context.Context, key, transitionID string) error {
	return c.ApplyTransition(ctx, key, TransitionInput{ID: transitionID})
}

// TransitionWithComment moves an issue through a transition and posts a comment as part of the same
// request. Jira applies both atomically, so the comment cannot be orphaned by a transition that is
// rejected — which is the reason to prefer it over a transition followed by a separate AddComment
// when the comment explains the move. Pass a nil doc for no comment.
func (c *Client) TransitionWithComment(ctx context.Context, key, transitionID string, comment *ADFDoc) error {
	return c.ApplyTransition(ctx, key, TransitionInput{ID: transitionID, Comment: comment})
}

// TransitionInput describes a workflow transition to apply. Give it an ID or a Name, not both.
type TransitionInput struct {
	// ID is the transition's own ID, which is not the destination status ID.
	ID string
	// Name is the transition's display name, matched case-insensitively against what the issue
	// currently offers. It costs one extra read and survives a site whose IDs differ.
	Name string
	// Comment is posted in the same request, so it cannot be orphaned by a transition Jira rejects.
	Comment *ADFDoc
	// Fields are set as the transition is applied, keyed by field ID. A workflow that demands a
	// resolution on the way to done takes it here:
	//
	//	Fields: map[string]any{"resolution": map[string]string{"name": "Won't Do"}}
	//
	// Jira accepts only the fields on the transition's screen and rejects the request for any other.
	Fields map[string]any
}

// ApplyTransition moves an issue through a workflow transition together with whatever the transition
// needs alongside it: a comment, the fields its screen requires, or both. The other Transition…
// methods are this with fewer arguments.
func (c *Client) ApplyTransition(ctx context.Context, key string, input TransitionInput) error {
	if key == "" {
		return fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}
	if (input.ID == "") == (input.Name == "") {
		return fmt.Errorf("%w: a transition needs an id or a name, and only one of them", ErrInvalidArgument)
	}
	// DryRunID stands for an issue a dry run never created, so it has no transitions to look a name
	// up in, and asking Jira would only report that the issue does not exist.
	if key == DryRunID && c.skipMutation("Transition", key, input.ID+input.Name) == true {
		return nil
	}

	transitionID := input.ID
	if input.Name != "" {
		resolved, err := c.transitionIDByName(ctx, key, input.Name)
		if err != nil {
			return err
		}
		transitionID = resolved
	}
	if c.skipMutation("Transition", key, transitionID) == true {
		return nil
	}

	payload := map[string]any{"transition": map[string]string{"id": transitionID}}
	if len(input.Fields) > 0 {
		payload["fields"] = input.Fields
	}
	if input.Comment != nil {
		payload["update"] = map[string]any{
			"comment": []map[string]any{{"add": map[string]any{"body": input.Comment}}},
		}
	}

	_, err := c.do(ctx, "POST", apiBase+"/issue/"+url.PathEscape(key)+"/transitions", payload)
	return err
}

// transitionIDByName finds the transition an issue currently offers under a display name. When there
// is none the error lists what the issue does offer, which is usually where the answer is.
func (c *Client) transitionIDByName(ctx context.Context, key, name string) (string, error) {
	options, err := c.Transitions(ctx, key)
	if err != nil {
		return "", err
	}

	available := make([]string, 0, len(options))
	for _, option := range options {
		if strings.EqualFold(option.Name, name) == true {
			return option.ID, nil
		}
		available = append(available, option.Name)
	}
	return "", fmt.Errorf("%w: transition %q not available on %s (available: %s)",
		ErrInvalidArgument, name, key, strings.Join(available, ", "))
}

// TransitionOption is a workflow transition available on an issue.
type TransitionOption struct {
	ID         string
	Name       string
	ToStatus   string
	ToStatusID string
}

// Transitions lists the transitions currently available on an issue. Availability is workflow- and
// permission-dependent, so resolving a transition by name at runtime is safer than hardcoding an ID.
func (c *Client) Transitions(ctx context.Context, key string) ([]TransitionOption, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "GET", apiBase+"/issue/"+url.PathEscape(key)+"/transitions", nil)
	if err != nil {
		return nil, err
	}

	var decoded struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode transitions for %s: %w", key, err)
	}

	options := make([]TransitionOption, 0, len(decoded.Transitions))
	for _, transition := range decoded.Transitions {
		options = append(options, TransitionOption{
			ID:         transition.ID,
			Name:       transition.Name,
			ToStatus:   transition.To.Name,
			ToStatusID: transition.To.ID,
		})
	}
	return options, nil
}

// TransitionByName resolves a transition by its display name and applies it, so callers can say
// "Won't Do" instead of depending on a numeric ID that differs between Jira sites.
func (c *Client) TransitionByName(ctx context.Context, key, name string) error {
	return c.ApplyTransition(ctx, key, TransitionInput{Name: name})
}

// TransitionByNameWithComment resolves a transition by name and posts a comment with it.
func (c *Client) TransitionByNameWithComment(ctx context.Context, key, name string, comment *ADFDoc) error {
	return c.ApplyTransition(ctx, key, TransitionInput{Name: name, Comment: comment})
}
