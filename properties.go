package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"unicode/utf8"
)

// Jira's documented ceilings on an entity property. Both are checked locally because a request that
// breaches either is rejected outright, so sending it only buys a round trip and a 400.
const (
	PropertyKeyMaxChars   = 255
	PropertyValueMaxChars = 32768
)

// IssuePropertyKeys lists the property keys currently set on an issue.
//
// Entity properties are arbitrary JSON stored on an issue under a key of your choosing. They are
// invisible in the Jira UI and survive edits, transitions and reassignment, which makes one the
// natural idempotency key for an automation that must not process the same ticket twice: write a
// marker when the work completes, and read it back before starting. The alternatives — encoding that
// state in a label, or grepping it out of comment text — are visible to users, editable by them, and
// routinely lost to a well-meaning cleanup.
func (c *Client) IssuePropertyKeys(ctx context.Context, key string) ([]string, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "GET", apiBase+"/issue/"+url.PathEscape(key)+"/properties", nil)
	if err != nil {
		return nil, err
	}

	var decoded struct {
		Keys []struct {
			Key string `json:"key"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode property keys for %s: %w", key, err)
	}

	keys := make([]string, 0, len(decoded.Keys))
	for _, entry := range decoded.Keys {
		keys = append(keys, entry.Key)
	}
	return keys, nil
}

// IssueProperty reads one property and unmarshals its value into dest, which must be a non-nil
// pointer.
//
// A property that was never set is a 404, so errors.Is(err, ErrNotFound) is the "not processed yet"
// branch an idempotency check wants — it is not an error worth surfacing.
func (c *Client) IssueProperty(ctx context.Context, key, propertyKey string, dest any) error {
	if err := validatePropertyArgs(key, propertyKey); err != nil {
		return err
	}
	// json.Unmarshal would report a non-pointer dest only after the request has already been spent.
	if target := reflect.ValueOf(dest); target.Kind() != reflect.Pointer || target.IsNil() == true {
		return fmt.Errorf("%w: dest must be a non-nil pointer", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "GET", propertyPath(key, propertyKey), nil)
	if err != nil {
		return err
	}

	var decoded struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return fmt.Errorf("decode property %s on %s: %w", propertyKey, key, err)
	}
	// An envelope without a value leaves dest at its zero value rather than failing the decode.
	if len(decoded.Value) == 0 {
		return nil
	}
	if err := json.Unmarshal(decoded.Value, dest); err != nil {
		return fmt.Errorf("decode property %s on %s into %T: %w", propertyKey, key, dest, err)
	}
	return nil
}

// SetIssueProperty stores a value under a property key, creating or replacing it.
//
// Jira takes the bare JSON value as the request body, not an envelope wrapping it, so a value of
// map[string]any{"runID": "abc"} is stored exactly as it reads here.
func (c *Client) SetIssueProperty(ctx context.Context, key, propertyKey string, value any) error {
	if err := validatePropertyArgs(key, propertyKey); err != nil {
		return err
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: property value is not JSON-serialisable: %v", ErrInvalidArgument, err)
	}
	if size := utf8.RuneCount(encoded); size > PropertyValueMaxChars {
		return fmt.Errorf("%w: property value is %d chars, over the %d limit",
			ErrInvalidArgument, size, PropertyValueMaxChars)
	}
	if c.skipMutation("SetIssueProperty", key, propertyKey) == true {
		return nil
	}

	// The pre-encoded bytes go out verbatim: json.RawMessage marshals to itself, so the value cannot
	// drift between the size that was checked and the one that is sent.
	_, err = c.do(ctx, "PUT", propertyPath(key, propertyKey), json.RawMessage(encoded))
	return err
}

// DeleteIssueProperty removes a property. Deleting one that does not exist is a 404, which unwraps to
// ErrNotFound.
func (c *Client) DeleteIssueProperty(ctx context.Context, key, propertyKey string) error {
	if err := validatePropertyArgs(key, propertyKey); err != nil {
		return err
	}
	if c.skipMutation("DeleteIssueProperty", key, propertyKey) == true {
		return nil
	}

	_, err := c.do(ctx, "DELETE", propertyPath(key, propertyKey), nil)
	return err
}

// validatePropertyArgs rejects arguments Jira is certain to refuse.
func validatePropertyArgs(key, propertyKey string) error {
	if key == "" || propertyKey == "" {
		return fmt.Errorf("%w: issue key and property key are required", ErrInvalidArgument)
	}
	// Jira states these limits in characters, so a multi-byte key must not be judged on its byte length.
	if size := utf8.RuneCountInString(propertyKey); size > PropertyKeyMaxChars {
		return fmt.Errorf("%w: property key is %d chars, over the %d limit",
			ErrInvalidArgument, size, PropertyKeyMaxChars)
	}
	return nil
}

// propertyPath escapes both segments — a property key is caller-chosen and may hold slashes or spaces.
func propertyPath(key, propertyKey string) string {
	return apiBase + "/issue/" + url.PathEscape(key) + "/properties/" + url.PathEscape(propertyKey)
}
