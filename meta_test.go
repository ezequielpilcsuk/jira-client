package jiraclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

func TestMyself_DecodesTheAuthenticatedAccount(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, `{"accountId":"5b10ac8d","accountType":"atlassian",
			"displayName":"Automation Bot","emailAddress":"bot@example.com","active":true,
			"timeZone":"America/Los_Angeles","locale":"en_US"}`)
	})

	me, err := client.Myself(context.Background())
	if err != nil {
		t.Fatalf("myself: %v", err)
	}
	if requestedPath != "/rest/api/3/myself" {
		t.Errorf("got %q, want the /myself endpoint", requestedPath)
	}
	if me.AccountID != "5b10ac8d" || me.DisplayName != "Automation Bot" {
		t.Errorf("account not decoded: %+v", me)
	}
	if me.Email != "bot@example.com" || me.Active == false {
		t.Errorf("email/active not decoded: %+v", me)
	}
}

// Profile visibility hides emailAddress even on your own account, so its absence is normal.
func TestMyself_ToleratesAHiddenEmail(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"accountId":"5b10ac8d","displayName":"Bot","active":true}`)
	})

	me, err := client.Myself(context.Background())
	if err != nil {
		t.Fatalf("a hidden email must not fail the decode: %v", err)
	}
	if me.Email != "" || me.AccountID != "5b10ac8d" {
		t.Errorf("unexpected account: %+v", me)
	}
}

// A 401 here means the token is wrong or expired, which is the reason to call it at startup.
func TestMyself_UnauthorizedMapsToTheCredentialSentinel(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errorMessages":["Client must be authenticated"]}`)
	})

	if _, err := client.Myself(context.Background()); errors.Is(err, ErrUnauthorized) == false {
		t.Errorf("want ErrUnauthorized, got %v", err)
	}
}

func TestServerInfo_ReportsTheSiteTimeZone(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, `{"baseUrl":"https://example.atlassian.net","version":"1001.0.0",
			"versionNumbers":[1001,0,0],"deploymentType":"Cloud","buildNumber":100245,
			"buildDate":"2026-08-01T00:00:00.000-0700",
			"serverTime":"2026-08-11T13:00:32.478-0700",
			"serverTimeZone":"America/Los_Angeles","serverTitle":"Example Jira"}`)
	})

	info, err := client.ServerInfo(context.Background())
	if err != nil {
		t.Fatalf("server info: %v", err)
	}
	if requestedPath != "/rest/api/3/serverInfo" {
		t.Errorf("got %q, want the /serverInfo endpoint", requestedPath)
	}
	if info.BaseURL != "https://example.atlassian.net" || info.DeploymentType != "Cloud" {
		t.Errorf("instance identity not decoded: %+v", info)
	}
	if info.ServerTitle != "Example Jira" || info.Version != "1001.0.0" {
		t.Errorf("title/version not decoded: %+v", info)
	}
	// The zone is the point of the call: timestamps come back in it, not in UTC.
	if info.ServerTimeZone != "America/Los_Angeles" {
		t.Errorf("time zone lost: %q", info.ServerTimeZone)
	}
	if info.ServerTime.IsZero() == true {
		t.Fatal("server time did not parse")
	}
	if _, offset := info.ServerTime.Zone(); offset != -7*60*60 {
		t.Errorf("offset lost: got %d seconds", offset)
	}
}

// Site metadata is read-only, so a dry client must be able to resolve it — a dry run needs the
// custom field IDs it is planning to write just as much as a live one does.
func TestSiteMetadata_ReadsOnADryClient(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fieldsResponse)
	}, WithDryRun(true))

	id, err := client.FieldIDByName(context.Background(), "Story Points")
	if err != nil {
		t.Fatalf("field lookup on a dry client: %v", err)
	}
	if id != "customfield_10004" {
		t.Errorf("got %q, want customfield_10004", id)
	}
}
