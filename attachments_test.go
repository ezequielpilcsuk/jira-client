package jiraclient

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAddAttachment_SendsMultipartWithTheRequiredHeader(t *testing.T) {
	var (
		contentType string
		xsrf        string
		fieldName   string
		filename    string
		fileBody    string
	)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		xsrf = r.Header.Get("X-Atlassian-Token")

		mediaType, params, err := mime.ParseMediaType(contentType)
		if err == nil && strings.HasPrefix(mediaType, "multipart/") == true {
			reader := multipart.NewReader(r.Body, params["boundary"])
			if part, err := reader.NextPart(); err == nil {
				fieldName, filename = part.FormName(), part.FileName()
				raw, _ := io.ReadAll(part)
				fileBody = string(raw)
			}
		}
		_, _ = io.WriteString(w, `[{"id":"10001","filename":"log.txt","mimeType":"text/plain",
			"size":7,"created":"2026-08-11T13:00:32.478-0700",
			"author":{"accountId":"acct-1","displayName":"Ada"},
			"content":"https://example.atlassian.net/rest/api/3/attachment/content/10001"}]`)
	})

	attachments, err := client.AddAttachment(context.Background(), "ABC-1", "log.txt",
		strings.NewReader("crashed"))
	if err != nil {
		t.Fatalf("add attachment: %v", err)
	}

	// Jira blocks a multipart request without this header, and the value is never checked.
	if xsrf != "no-check" {
		t.Errorf("X-Atlassian-Token = %q, want no-check", xsrf)
	}
	if strings.HasPrefix(contentType, "multipart/form-data") == false {
		t.Errorf("Content-Type = %q, want multipart/form-data", contentType)
	}
	// The field name is fixed by Jira; anything else is rejected.
	if fieldName != "file" {
		t.Errorf("form field = %q, want file", fieldName)
	}
	if filename != "log.txt" || fileBody != "crashed" {
		t.Errorf("upload carried (%q, %q)", filename, fileBody)
	}

	if len(attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(attachments))
	}
	got := attachments[0]
	if got.ID != "10001" || got.Filename != "log.txt" || got.Size != 7 {
		t.Errorf("attachment decoded wrong: %+v", got)
	}
	if got.AuthorID != "acct-1" || got.AuthorName != "Ada" {
		t.Errorf("author not flattened: %+v", got)
	}
	if got.Created.IsZero() == true {
		t.Error("created timestamp not parsed")
	}
}

func TestAddAttachment_RejectsWhatCannotSucceed(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should have been made")
	})

	ctx := context.Background()
	cases := map[string]error{
		"no key":      firstErr(client.AddAttachment(ctx, "", "log.txt", strings.NewReader("x"))),
		"no filename": firstErr(client.AddAttachment(ctx, "ABC-1", "  ", strings.NewReader("x"))),
		"no content":  firstErr(client.AddAttachment(ctx, "ABC-1", "log.txt", nil)),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			if errors.Is(err, ErrInvalidArgument) == false {
				t.Errorf("want ErrInvalidArgument, got %v", err)
			}
		})
	}
}

func TestAddAttachment_DryRunUploadsNothing(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a dry client uploaded %s %s", r.Method, r.URL.Path)
	}, WithDryRun(true))

	attachments, err := client.AddAttachment(context.Background(), "ABC-1", "log.txt",
		strings.NewReader("crashed"))
	if err != nil {
		t.Fatalf("dry upload: %v", err)
	}
	if len(attachments) != 1 || attachments[0].ID != DryRunID {
		t.Errorf("expected a DryRunID placeholder, got %+v", attachments)
	}
}

// A 2xx whose body will not decode still means the file landed. Erroring would invite a retry that
// uploads it a second time.
func TestAddAttachment_UndecodableBodyIsNotAFailedUpload(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	if _, err := client.AddAttachment(context.Background(), "ABC-1", "log.txt",
		strings.NewReader("x")); err != nil {
		t.Errorf("an unreadable response must not read as a failed upload: %v", err)
	}
}

// Jira answers the content endpoint with a 303 to a media host. Go strips Authorization across
// hosts, so the redirect must be read rather than followed with credentials attached.
func TestDownloadAttachment_FollowsTheRedirectWithoutCredentials(t *testing.T) {
	var mediaAuth string
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "file-bytes")
	}))
	t.Cleanup(media.Close)

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", media.URL+"/signed")
		w.WriteHeader(http.StatusSeeOther)
	})

	content, err := client.DownloadAttachment(context.Background(), "10001")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(content) != "file-bytes" {
		t.Errorf("got %q, want file-bytes", content)
	}
	// The signed media URL neither needs nor accepts the Jira credentials.
	if mediaAuth != "" {
		t.Errorf("credentials leaked to the media host: %q", mediaAuth)
	}
}

// Not every deployment redirects; some serve the bytes directly.
func TestDownloadAttachment_HandlesADirectBody(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "direct-bytes")
	})

	content, err := client.DownloadAttachment(context.Background(), "10001")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(content) != "direct-bytes" {
		t.Errorf("got %q, want direct-bytes", content)
	}
}

func TestAttachmentSettings(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/attachment/meta" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"enabled":true,"uploadLimit":10485760}`)
	})

	settings, err := client.AttachmentSettings(context.Background())
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if settings.Enabled == false || settings.UploadLimit != 10485760 {
		t.Errorf("settings decoded wrong: %+v", settings)
	}
}

func TestDeleteAttachment(t *testing.T) {
	t.Run("deletes by id", func(t *testing.T) {
		var method, path string
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			method, path = r.Method, r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		})

		if err := client.DeleteAttachment(context.Background(), "10001"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if method != "DELETE" || path != "/rest/api/3/attachment/10001" {
			t.Errorf("got %s %s", method, path)
		}
	})

	t.Run("dry run deletes nothing", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("a dry client issued %s %s", r.Method, r.URL.Path)
		}, WithDryRun(true))

		if err := client.DeleteAttachment(context.Background(), "10001"); err != nil {
			t.Fatalf("dry delete: %v", err)
		}
	})
}

// The per-issue attachment ceiling is permanent, unlike a rate limit.
func TestAddAttachment_EntityCeilingIsALimitNotARateLimit(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"errorMessages":["Issue has too many attachments"]}`)
	})

	_, err := client.AddAttachment(context.Background(), "ABC-1", "log.txt", strings.NewReader("x"))
	if errors.Is(err, ErrLimitExceeded) == false {
		t.Errorf("want ErrLimitExceeded, got %v", err)
	}
}

// firstErr discards a call's value, keeping the rejection tables one column wide.
func firstErr[T any](_ T, err error) error { return err }
