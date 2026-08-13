package jiraclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Attachment upload is the one endpoint that departs from JSON in and JSON out.
const (
	// attachmentFormField is the multipart field name Jira requires. Any other name is rejected.
	attachmentFormField = "file"

	// xsrfHeader must be present on every multipart request or Jira blocks it as cross-site. The
	// value is not checked; its presence is the whole point.
	xsrfHeader  = "X-Atlassian-Token"
	xsrfNoCheck = "no-check"
)

// Attachment is a file on an issue.
type Attachment struct {
	ID       string
	Filename string
	MimeType string
	Size     int64
	AuthorID string
	// AuthorName is the display name, absent when the uploader's profile hides it.
	AuthorName string
	Created    time.Time
	// ContentURL and ThumbnailURL are Jira API URLs, not public links. They answer with a redirect to
	// a media host, which is why DownloadAttachment exists rather than leaving callers to fetch them.
	ContentURL   string
	ThumbnailURL string
}

// AttachmentSettings is the site's upload configuration.
type AttachmentSettings struct {
	// Enabled is false when attachments are switched off site-wide, in which case every upload fails.
	Enabled bool
	// UploadLimit is the maximum size of a single file, in bytes.
	UploadLimit int64
}

// AddAttachment uploads one file to an issue and returns it as Jira stored it.
//
// The content is read fully into memory before sending: the request is multipart and may be retried,
// and a retry cannot replay a stream that has already been consumed. Size accordingly — Jira's own
// per-file limit is available from AttachmentSettings.
//
// Uploading is subject to two ceilings that both surface as ErrLimitExceeded: the per-file size
// limit, and 2,000 attachments on one issue.
func (c *Client) AddAttachment(ctx context.Context, key, filename string, content io.Reader) ([]Attachment, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}
	if strings.TrimSpace(filename) == "" {
		return nil, fmt.Errorf("%w: attachment filename cannot be empty", ErrInvalidArgument)
	}
	if content == nil {
		return nil, fmt.Errorf("%w: attachment content cannot be nil", ErrInvalidArgument)
	}
	if c.skipMutation("AddAttachment", key, filename) == true {
		return []Attachment{{ID: DryRunID, Filename: filename}}, nil
	}

	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	part, err := writer.CreateFormFile(attachmentFormField, filename)
	if err != nil {
		return nil, fmt.Errorf("build upload for %s: %w", filename, err)
	}
	if _, err := io.Copy(part, content); err != nil {
		return nil, fmt.Errorf("read attachment %s: %w", filename, err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finalise upload for %s: %w", filename, err)
	}

	body, _, err := c.doRequest(ctx, request{
		Method:      "POST",
		Path:        apiBase + "/issue/" + url.PathEscape(key) + "/attachments",
		Body:        buffer.Bytes(),
		ContentType: writer.FormDataContentType(),
		Headers:     map[string]string{xsrfHeader: xsrfNoCheck},
	})
	if err != nil {
		return nil, err
	}

	var raw []rawAttachment
	if err := json.Unmarshal(body, &raw); err != nil {
		// The upload succeeded — Jira answered 2xx — so an unreadable body costs the metadata, not
		// the file. Erroring here would invite a retry that uploads it twice.
		c.logf("jira: attachment posted to %s but the response could not be decoded: %v", key, err)
		return nil, nil
	}

	attachments := make([]Attachment, 0, len(raw))
	for _, entry := range raw {
		attachments = append(attachments, entry.toAttachment())
	}
	return attachments, nil
}

// GetAttachment reads one attachment's metadata. It does not fetch the file — see DownloadAttachment.
func (c *Client) GetAttachment(ctx context.Context, attachmentID string) (Attachment, error) {
	if attachmentID == "" {
		return Attachment{}, fmt.Errorf("%w: attachment id cannot be empty", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "GET", apiBase+"/attachment/"+url.PathEscape(attachmentID), nil)
	if err != nil {
		return Attachment{}, err
	}

	var raw rawAttachment
	if err := json.Unmarshal(body, &raw); err != nil {
		return Attachment{}, fmt.Errorf("decode attachment %s: %w", attachmentID, err)
	}
	return raw.toAttachment(), nil
}

// DownloadAttachment fetches an attachment's bytes.
//
// Jira answers the content endpoint with a 303 to a media host rather than the file itself, and Go's
// HTTP client strips the Authorization header when a redirect crosses hosts — so following it inside
// this client would arrive unauthenticated. The redirect is read instead and the media URL fetched
// as a plain request, which is what the signed URL expects.
func (c *Client) DownloadAttachment(ctx context.Context, attachmentID string) ([]byte, error) {
	if attachmentID == "" {
		return nil, fmt.Errorf("%w: attachment id cannot be empty", ErrInvalidArgument)
	}

	body, header, err := c.doRequest(ctx, request{
		Method:     "GET",
		Path:       apiBase + "/attachment/content/" + url.PathEscape(attachmentID),
		Accept:     "*/*",
		NoRedirect: true,
	})
	if err != nil {
		return nil, err
	}

	location := header.Get("Location")
	if location == "" {
		// Some deployments serve the bytes directly rather than redirecting.
		return body, nil
	}
	return c.fetchRedirected(ctx, location)
}

// fetchRedirected follows an attachment redirect without the Jira credentials, which the signed media
// URL neither needs nor accepts.
func (c *Client) fetchRedirected(ctx context.Context, location string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", location, nil)
	if err != nil {
		return nil, fmt.Errorf("build media request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch attachment content: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read attachment content: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, newAPIError(resp.StatusCode, "GET attachment content", content)
	}
	return content, nil
}

// DeleteAttachment removes an attachment permanently.
func (c *Client) DeleteAttachment(ctx context.Context, attachmentID string) error {
	if attachmentID == "" {
		return fmt.Errorf("%w: attachment id cannot be empty", ErrInvalidArgument)
	}
	if c.skipMutation("DeleteAttachment", attachmentID) == true {
		return nil
	}

	_, err := c.do(ctx, "DELETE", apiBase+"/attachment/"+url.PathEscape(attachmentID), nil)
	return err
}

// AttachmentSettings reports whether attachments are enabled site-wide and how large one file may be.
// Worth reading once before a bulk upload: an over-size file is rejected per-request, not up front.
func (c *Client) AttachmentSettings(ctx context.Context) (AttachmentSettings, error) {
	body, err := c.do(ctx, "GET", apiBase+"/attachment/meta", nil)
	if err != nil {
		return AttachmentSettings{}, err
	}

	var raw struct {
		Enabled     bool  `json:"enabled"`
		UploadLimit int64 `json:"uploadLimit"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return AttachmentSettings{}, fmt.Errorf("decode attachment settings: %w", err)
	}
	return AttachmentSettings{Enabled: raw.Enabled, UploadLimit: raw.UploadLimit}, nil
}

// rawAttachment mirrors Jira's attachment payload.
type rawAttachment struct {
	ID        string   `json:"id"`
	Filename  string   `json:"filename"`
	MimeType  string   `json:"mimeType"`
	Size      int64    `json:"size"`
	Created   string   `json:"created"`
	Content   string   `json:"content"`
	Thumbnail string   `json:"thumbnail"`
	Author    *rawUser `json:"author"`
}

func (r rawAttachment) toAttachment() Attachment {
	attachment := Attachment{
		ID:           r.ID,
		Filename:     r.Filename,
		MimeType:     r.MimeType,
		Size:         r.Size,
		Created:      parseJiraTime(r.Created),
		ContentURL:   r.Content,
		ThumbnailURL: r.Thumbnail,
	}
	if r.Author != nil {
		attachment.AuthorID, attachment.AuthorName = r.Author.AccountID, r.Author.DisplayName
	}
	return attachment
}
