package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Field is one field definition on the site, system or custom.
type Field struct {
	ID   string
	Key  string
	Name string
	// Custom distinguishes a site-defined field from a built-in one.
	Custom     bool
	Navigable  bool
	Orderable  bool
	Searchable bool
	// ClauseNames are the names JQL accepts for this field. They are *not* always Name: a custom field
	// is addressable as cf[10004] as well, and a rename can leave an old clause name in place, so a
	// query built from Name alone can fail on a field this list would have matched.
	ClauseNames []string
	// SchemaType is the value shape ("number", "string", "array", "option", ...), SchemaItems the
	// element type when SchemaType is "array", and SchemaCustom the custom field type key
	// ("com.atlassian.jira.plugin.system.customfieldtypes:float"). All three are empty for a field
	// whose schema Jira omitted.
	SchemaType   string
	SchemaItems  string
	SchemaCustom string
}

// Fields lists every field on the site.
//
// This closes the loop on custom fields. UpdateCustomField and CreateIssueInput.CustomFields both
// take an ID like "customfield_10004", but nothing else in the library can tell a caller what that
// ID is — and the ID is per-site, so a constant copied out of one Jira's URL bar is the single most
// site-brittle thing in an integration. Resolve it by name at startup instead.
//
// The response is a bare array with no pagination: one request returns the lot.
func (c *Client) Fields(ctx context.Context) ([]Field, error) {
	body, err := c.do(ctx, "GET", apiBase+"/field", nil)
	if err != nil {
		return nil, err
	}

	var raw []struct {
		ID          string   `json:"id"`
		Key         string   `json:"key"`
		Name        string   `json:"name"`
		Custom      bool     `json:"custom"`
		Navigable   bool     `json:"navigable"`
		Orderable   bool     `json:"orderable"`
		Searchable  bool     `json:"searchable"`
		ClauseNames []string `json:"clauseNames"`
		// Schema is a pointer because Jira omits it entirely for some system fields.
		Schema *struct {
			Type   string `json:"type"`
			Items  string `json:"items"`
			Custom string `json:"custom"`
		} `json:"schema"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode fields: %w", err)
	}

	fields := make([]Field, 0, len(raw))
	for _, entry := range raw {
		field := Field{
			ID:          entry.ID,
			Key:         entry.Key,
			Name:        entry.Name,
			Custom:      entry.Custom,
			Navigable:   entry.Navigable,
			Orderable:   entry.Orderable,
			Searchable:  entry.Searchable,
			ClauseNames: entry.ClauseNames,
		}
		if entry.Schema != nil {
			field.SchemaType, field.SchemaItems, field.SchemaCustom =
				entry.Schema.Type, entry.Schema.Items, entry.Schema.Custom
		}
		fields = append(fields, field)
	}
	return fields, nil
}

// FieldsByName returns every field whose name matches, compared case-insensitively.
//
// Jira does not enforce unique field names, so this returns a slice rather than one field. Use it
// when FieldIDByName reports an ambiguity and you need to pick between the candidates yourself —
// SchemaType and Custom are usually enough to tell two same-named fields apart. An empty result is
// not an error: it means the site has no such field.
func (c *Client) FieldsByName(ctx context.Context, name string) ([]Field, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: field name cannot be empty", ErrInvalidArgument)
	}

	fields, err := c.Fields(ctx)
	if err != nil {
		return nil, err
	}

	matches := make([]Field, 0, 1)
	for _, field := range fields {
		if strings.EqualFold(field.Name, name) == true {
			matches = append(matches, field)
		}
	}
	return matches, nil
}

// FieldIDByName resolves a field's display name to the ID the write APIs take, e.g. "Story Points"
// to "customfield_10004".
//
// Two same-named fields are an error rather than a coin toss. Jira lets an admin create a second
// custom field called "Severity" without complaint, and the two get different IDs — so silently
// taking the first match would write to the wrong field on some sites and the right one on others,
// which is close to undiagnosable from the caller's side. Use FieldsByName to disambiguate.
func (c *Client) FieldIDByName(ctx context.Context, name string) (string, error) {
	matches, err := c.FieldsByName(ctx, name)
	if err != nil {
		return "", err
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: no field named %q on this site", ErrInvalidArgument, name)
	case 1:
		return matches[0].ID, nil
	}

	ids := make([]string, 0, len(matches))
	for _, field := range matches {
		ids = append(ids, field.ID)
	}
	return "", fmt.Errorf("%w: field name %q is ambiguous, %d fields share it (%s)",
		ErrInvalidArgument, name, len(matches), strings.Join(ids, ", "))
}
