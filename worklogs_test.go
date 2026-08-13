package jiraclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// worklogPage renders one page of the worklog list.
func worklogPage(startAt, total int, ids ...string) string {
	page := `{"startAt":` + strconv.Itoa(startAt) + `,"maxResults":5000,"total":` +
		strconv.Itoa(total) + `,"worklogs":[`
	for i, id := range ids {
		if i > 0 {
			page += ","
		}
		page += `{"id":"` + id + `","issueId":"10001","timeSpent":"3h 20m","timeSpentSeconds":12000,
			"started":"2026-08-12T09:00:00.000-0300","created":"2026-08-12T10:00:00.000-0300",
			"updated":"2026-08-12T11:00:00.000-0300",
			"author":{"accountId":"acct-` + id + `","displayName":"Ada"},
			"updateAuthor":{"accountId":"acct-editor","displayName":"Bot"},
			"visibility":{"type":"group","value":"devs","identifier":"grp-1"},
			"comment":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
				{"type":"text","text":"worked on ` + id + `"}]}]}}`
	}
	return page + `]}`
}

func TestWorklogs_PagesUntilTotal(t *testing.T) {
	var requestedStarts []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("startAt")
		requestedStarts = append(requestedStarts, start)
		if start == "0" {
			_, _ = io.WriteString(w, worklogPage(0, 3, "10", "11"))
			return
		}
		_, _ = io.WriteString(w, worklogPage(2, 3, "12"))
	})

	worklogs, err := client.Worklogs(context.Background(), "ABC-1")

	if err != nil {
		t.Fatalf("worklogs: %v", err)
	}
	if len(worklogs) != 3 {
		t.Fatalf("got %d worklogs, want 3", len(worklogs))
	}
	if worklogs[0].ID != "10" || worklogs[2].ID != "12" {
		t.Fatalf("unexpected ids: %+v", worklogs)
	}
	if len(requestedStarts) != 2 || requestedStarts[1] != "2" {
		t.Fatalf("pagination cursors: %v", requestedStarts)
	}
}

// Jira documents total as unstable between pages, so the traversal must not be derived from it.
func TestWorklogs_StopsOnAnEmptyPage(t *testing.T) {
	requests := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > 5 {
			t.Fatal("pagination did not terminate")
		}
		if requests == 1 {
			_, _ = io.WriteString(w, worklogPage(0, 99, "10"))
			return
		}
		_, _ = io.WriteString(w, worklogPage(1, 99))
	})

	worklogs, err := client.Worklogs(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("worklogs: %v", err)
	}
	if len(worklogs) != 1 || requests != 2 {
		t.Fatalf("got %d worklogs over %d requests, want 1 over 2", len(worklogs), requests)
	}
}

func TestWorklogs_FlattensEveryNestedObject(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, worklogPage(0, 1, "10"))
	})

	worklogs, err := client.Worklogs(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("worklogs: %v", err)
	}

	got := worklogs[0]
	if got.Comment != "worked on 10" {
		t.Errorf("comment not flattened: %q", got.Comment)
	}
	if got.AuthorID != "acct-10" || got.AuthorName != "Ada" {
		t.Errorf("author: %+v", got)
	}
	// Whoever edited an entry is not necessarily whoever logged it.
	if got.UpdateAuthorID != "acct-editor" || got.UpdateAuthorName != "Bot" {
		t.Errorf("update author: %+v", got)
	}
	if got.TimeSpentSeconds != 12000 || got.TimeSpent != "3h 20m" {
		t.Errorf("duration: %+v", got)
	}
	if got.Visibility.Type != VisibilityGroup || got.Visibility.Identifier != "grp-1" {
		t.Errorf("visibility: %+v", got.Visibility)
	}
	if got.Started.IsZero() == true || got.Created.IsZero() == true || got.Updated.IsZero() == true {
		t.Errorf("timestamps not parsed: %+v", got)
	}
	// Started is when the work happened, Created when it was recorded — they must not be conflated.
	if got.Started.Equal(got.Created) == true {
		t.Errorf("started and created should be distinct: %v", got.Started)
	}
}

// Jira omits the author of an entry logged by a deleted account, and every object it has nothing to
// say about.
func TestWorklogs_DecodesAWorklogMissingEveryOptionalObject(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"total":1,"worklogs":[{"id":"10","timeSpentSeconds":60}]}`)
	})

	worklogs, err := client.Worklogs(context.Background(), "ABC-1")
	if err != nil {
		t.Fatalf("worklogs: %v", err)
	}

	got := worklogs[0]
	if got.Comment != "" || got.AuthorID != "" || got.UpdateAuthorID != "" {
		t.Errorf("absent fields should be zero: %+v", got)
	}
	if got.Visibility != (Visibility{}) {
		t.Errorf("an absent visibility should read as unrestricted: %+v", got.Visibility)
	}
	if got.Started.IsZero() == false {
		t.Errorf("absent started should be zero, got %v", got.Started)
	}
}

// The window bounds are epoch milliseconds. Seconds are a valid number too, so Jira would accept them
// and answer about 1970 rather than complaining.
func TestWorklogsStarted_SendsMillisecondBounds(t *testing.T) {
	var after, before string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		after = r.URL.Query().Get("startedAfter")
		before = r.URL.Query().Get("startedBefore")
		_, _ = io.WriteString(w, worklogPage(0, 0))
	})

	from := time.Unix(1755000000, 0)
	to := time.Unix(1755086400, 0)
	if _, err := client.WorklogsStarted(context.Background(), "ABC-1", from, to); err != nil {
		t.Fatalf("worklogs: %v", err)
	}

	if after != "1755000000000" {
		t.Errorf("startedAfter = %q, want epoch milliseconds", after)
	}
	if before != "1755086400000" {
		t.Errorf("startedBefore = %q, want epoch milliseconds", before)
	}
}

// An open-ended window is legitimate; only an inverted one is not.
func TestWorklogsStarted_OmitsAZeroBoundAndRejectsAnInvertedWindow(t *testing.T) {
	var query url.Values
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		_, _ = io.WriteString(w, worklogPage(0, 0))
	})

	ctx := context.Background()
	if _, err := client.WorklogsStarted(ctx, "ABC-1", time.Unix(1755000000, 0), time.Time{}); err != nil {
		t.Fatalf("worklogs: %v", err)
	}
	if _, present := query["startedBefore"]; present == true {
		t.Errorf("a zero bound must be omitted: %v", query)
	}

	_, err := client.WorklogsStarted(ctx, "ABC-1", time.Unix(1755086400, 0), time.Unix(1755000000, 0))
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}

func TestGetWorklog_ReadsOneEntry(t *testing.T) {
	var method, path string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_, _ = io.WriteString(w, `{"id":"10042","timeSpent":"1h","timeSpentSeconds":3600}`)
	})

	worklog, err := client.GetWorklog(context.Background(), "ABC-1", "10042")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if method != http.MethodGet || path != "/rest/api/3/issue/ABC-1/worklog/10042" {
		t.Fatalf("request: %s %s", method, path)
	}
	if worklog.ID != "10042" || worklog.TimeSpentSeconds != 3600 {
		t.Fatalf("worklog not decoded: %+v", worklog)
	}
}

func TestAddWorklog_PostsAndReturnsTheCreatedEntry(t *testing.T) {
	var method, path string
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"10042","issueId":"10001","timeSpent":"3h 20m",
			"timeSpentSeconds":12000,"author":{"accountId":"acct-1","displayName":"Ada"},
			"comment":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[
				{"type":"text","text":"triage"}]}]}}`)
	})

	doc, _ := TextDoc("triage")
	worklog, err := client.AddWorklog(context.Background(), "ABC-1", WorklogInput{
		TimeSpent: "3h 20m",
		Comment:   doc,
	})

	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if method != http.MethodPost || path != "/rest/api/3/issue/ABC-1/worklog" {
		t.Fatalf("request: %s %s", method, path)
	}
	if payload["timeSpent"] != "3h 20m" {
		t.Errorf("timeSpent not sent: %v", payload)
	}
	if _, present := payload["comment"]; present == false {
		t.Errorf("comment not sent: %v", payload)
	}
	if worklog.ID != "10042" || worklog.Comment != "triage" || worklog.AuthorID != "acct-1" {
		t.Fatalf("created worklog not flattened: %+v", worklog)
	}
}

// Jira rejects RFC 3339's colon-separated offset, so a time.Time cannot be formatted the obvious way.
func TestAddWorklog_SendsStartedInJirasOwnFormat(t *testing.T) {
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"1"}`)
	})

	started := time.Date(2026, time.August, 12, 9, 30, 0, 0, time.FixedZone("BRT", -3*60*60))
	if _, err := client.AddWorklog(context.Background(), "ABC-1", WorklogInput{
		TimeSpentSeconds: 3600,
		Started:          started,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	if payload["started"] != "2026-08-12T09:30:00.000-0300" {
		t.Errorf("started = %v, want Jira's timestamp format", payload["started"])
	}
	// The numeric duration must travel as a number, not as the string form of one.
	if payload["timeSpentSeconds"] != float64(3600) {
		t.Errorf("timeSpentSeconds = %#v, want a number", payload["timeSpentSeconds"])
	}
	// A start time Jira would stamp itself is left out entirely rather than sent as a zero time.
	if _, present := payload["comment"]; present == true {
		t.Errorf("an unset comment must be omitted: %v", payload)
	}
}

func TestAddWorklog_SendsTheEstimateAdjustment(t *testing.T) {
	cases := map[string]struct {
		input WorklogInput
		want  map[string]string
	}{
		"unset leaves jira its auto default": {
			input: WorklogInput{TimeSpent: "1h"},
			want:  map[string]string{"adjustEstimate": "", "reduceBy": "", "newEstimate": ""},
		},
		"leave sends only the adjustment": {
			input: WorklogInput{TimeSpent: "1h", Adjust: EstimateLeave},
			want:  map[string]string{"adjustEstimate": "leave", "reduceBy": "", "newEstimate": ""},
		},
		"new carries the replacement estimate": {
			input: WorklogInput{TimeSpent: "1h", Adjust: EstimateNew, NewEstimate: "4d"},
			want:  map[string]string{"adjustEstimate": "new", "newEstimate": "4d", "reduceBy": ""},
		},
		"manual carries reduceBy, not increaseBy": {
			input: WorklogInput{TimeSpent: "1h", Adjust: EstimateManual, ReduceBy: "2h"},
			want:  map[string]string{"adjustEstimate": "manual", "reduceBy": "2h", "increaseBy": ""},
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			var query url.Values
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.Query()
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"id":"1"}`)
			})

			if _, err := client.AddWorklog(context.Background(), "ABC-1", testCase.input); err != nil {
				t.Fatalf("add: %v", err)
			}
			for param, want := range testCase.want {
				if got := query.Get(param); got != want {
					t.Errorf("%s = %q, want %q (query %v)", param, got, want, query)
				}
			}
		})
	}
}

// Watchers are mailed unless the caller says otherwise, which is what makes an unattended import
// noisy. The parameter is only sent when a preference was actually stated.
func TestWorklogInput_NotifyUsersIsOnlySentWhenStated(t *testing.T) {
	quiet := false
	loud := true
	cases := map[string]struct {
		notify *bool
		want   string
	}{
		"unset": {notify: nil, want: ""},
		"false": {notify: &quiet, want: "false"},
		"true":  {notify: &loud, want: "true"},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			var query url.Values
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.Query()
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"id":"1"}`)
			})

			if _, err := client.AddWorklog(context.Background(), "ABC-1", WorklogInput{
				TimeSpent:   "1h",
				NotifyUsers: testCase.notify,
			}); err != nil {
				t.Fatalf("add: %v", err)
			}
			if got := query.Get("notifyUsers"); got != testCase.want {
				t.Errorf("notifyUsers = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestUpdateWorklog_PutsToTheWorklogPath(t *testing.T) {
	var method, path string
	var payload map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
	})

	doc, _ := TextDoc("corrected")
	err := client.UpdateWorklog(context.Background(), "ABC-1", "10042", WorklogInput{Comment: doc})

	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if method != http.MethodPut || path != "/rest/api/3/issue/ABC-1/worklog/10042" {
		t.Fatalf("request: %s %s", method, path)
	}
	// The update is partial: a comment-only correction must not carry a duration that would overwrite
	// the logged time.
	if _, present := payload["timeSpent"]; present == true {
		t.Errorf("an unset duration must be omitted: %v", payload)
	}
	if _, present := payload["comment"]; present == false {
		t.Errorf("comment not sent: %v", payload)
	}
}

// The update schema advertises a manual adjustment but defines no parameter to carry the amount, so
// the combination is refused here rather than sent to be interpreted however Jira feels.
func TestUpdateWorklog_RefusesAManualAdjustment(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should have been sent")
	})
	ctx := context.Background()

	err := client.UpdateWorklog(ctx, "ABC-1", "10042", WorklogInput{
		TimeSpent: "1h",
		Adjust:    EstimateManual,
		ReduceBy:  "2h",
	})
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}

	// Without an amount it is just as unusable, so it must not slip through as a bare parameter.
	err = client.UpdateWorklog(ctx, "ABC-1", "10042", WorklogInput{TimeSpent: "1h", Adjust: EstimateManual})
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}

// The other three adjustments are usable on an update.
func TestUpdateWorklog_SendsTheAdjustmentsItCanExpress(t *testing.T) {
	for adjust, wantNew := range map[EstimateAdjust]string{
		EstimateAuto:  "",
		EstimateLeave: "",
		EstimateNew:   "4d",
	} {
		t.Run(string(adjust), func(t *testing.T) {
			var query url.Values
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.Query()
			})

			input := WorklogInput{TimeSpent: "1h", Adjust: adjust, NewEstimate: wantNew}
			if err := client.UpdateWorklog(context.Background(), "ABC-1", "10042", input); err != nil {
				t.Fatalf("update: %v", err)
			}
			if query.Get("adjustEstimate") != string(adjust) {
				t.Errorf("adjustEstimate = %q, want %q", query.Get("adjustEstimate"), adjust)
			}
			if query.Get("newEstimate") != wantNew {
				t.Errorf("newEstimate = %q, want %q", query.Get("newEstimate"), wantNew)
			}
		})
	}
}

// A delete gives time back to the estimate, so its manual amount is an increase — the opposite of the
// reduction an add applies.
func TestDeleteWorklog_RoutesTheAmountToTheRightParameter(t *testing.T) {
	cases := map[string]struct {
		adjust EstimateAdjust
		amount string
		want   map[string]string
	}{
		"manual increases the estimate": {
			adjust: EstimateManual, amount: "2h",
			want: map[string]string{"adjustEstimate": "manual", "increaseBy": "2h", "newEstimate": ""},
		},
		"new replaces it": {
			adjust: EstimateNew, amount: "4d",
			want: map[string]string{"adjustEstimate": "new", "newEstimate": "4d", "increaseBy": ""},
		},
		"leave needs no amount": {
			adjust: EstimateLeave, amount: "",
			want: map[string]string{"adjustEstimate": "leave", "increaseBy": "", "newEstimate": ""},
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			var method, path string
			var query url.Values
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				method, path, query = r.Method, r.URL.Path, r.URL.Query()
				w.WriteHeader(http.StatusNoContent)
			})

			err := client.DeleteWorklog(context.Background(), "ABC-1", "10042",
				WorklogDelete{Adjust: testCase.adjust, EstimateAmount: testCase.amount})
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			if method != http.MethodDelete || path != "/rest/api/3/issue/ABC-1/worklog/10042" {
				t.Fatalf("request: %s %s", method, path)
			}
			for param, want := range testCase.want {
				if got := query.Get(param); got != want {
					t.Errorf("%s = %q, want %q (query %v)", param, got, want, query)
				}
			}
		})
	}
}

func TestWorklogLifecycle_RejectsBeforeSending(t *testing.T) {
	var called bool
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	ctx := context.Background()
	hour := WorklogInput{TimeSpent: "1h"}

	cases := map[string]error{
		"list without key": mustErr(func() error { _, err := client.Worklogs(ctx, ""); return err }),
		"read without key": mustErr(func() error { _, err := client.GetWorklog(ctx, "", "10"); return err }),
		"read without id":  mustErr(func() error { _, err := client.GetWorklog(ctx, "ABC-1", ""); return err }),
		"add without key":  mustErr(func() error { _, err := client.AddWorklog(ctx, "", hour); return err }),
		"add without a duration": mustErr(func() error {
			_, err := client.AddWorklog(ctx, "ABC-1", WorklogInput{})
			return err
		}),
		"add with both durations": mustErr(func() error {
			_, err := client.AddWorklog(ctx, "ABC-1", WorklogInput{TimeSpent: "1h", TimeSpentSeconds: 3600})
			return err
		}),
		"add a negative duration": mustErr(func() error {
			_, err := client.AddWorklog(ctx, "ABC-1", WorklogInput{TimeSpentSeconds: -60})
			return err
		}),
		"add an empty comment": mustErr(func() error {
			_, err := client.AddWorklog(ctx, "ABC-1", WorklogInput{TimeSpent: "1h", Comment: &ADFDoc{}})
			return err
		}),
		"new without an estimate": mustErr(func() error {
			_, err := client.AddWorklog(ctx, "ABC-1", WorklogInput{TimeSpent: "1h", Adjust: EstimateNew})
			return err
		}),
		"manual without an amount": mustErr(func() error {
			_, err := client.AddWorklog(ctx, "ABC-1", WorklogInput{TimeSpent: "1h", Adjust: EstimateManual})
			return err
		}),
		// A companion amount without its adjustment is silently ignored by Jira, which then applies the
		// default adjustment instead of the one the caller thought they asked for.
		"an estimate the adjustment will not spend": mustErr(func() error {
			_, err := client.AddWorklog(ctx, "ABC-1", WorklogInput{TimeSpent: "1h", NewEstimate: "4d"})
			return err
		}),
		"a reduction the adjustment will not spend": mustErr(func() error {
			_, err := client.AddWorklog(ctx, "ABC-1", WorklogInput{
				TimeSpent: "1h", Adjust: EstimateLeave, ReduceBy: "2h",
			})
			return err
		}),
		"an unknown adjustment": mustErr(func() error {
			_, err := client.AddWorklog(ctx, "ABC-1", WorklogInput{TimeSpent: "1h", Adjust: "reduce"})
			return err
		}),
		"update without an id":    client.UpdateWorklog(ctx, "ABC-1", "", hour),
		"update without a key":    client.UpdateWorklog(ctx, "", "10042", hour),
		"update changing nothing": client.UpdateWorklog(ctx, "ABC-1", "10042", WorklogInput{}),
		"delete without an id":    client.DeleteWorklog(ctx, "ABC-1", "", WorklogDelete{Adjust: EstimateAuto}),
		"delete without a key":    client.DeleteWorklog(ctx, "", "10042", WorklogDelete{Adjust: EstimateAuto}),
		"delete manual without an amount": client.DeleteWorklog(ctx, "ABC-1", "10042",
			WorklogDelete{Adjust: EstimateManual}),
		"delete with an amount nothing spends": client.DeleteWorklog(ctx, "ABC-1", "10042",
			WorklogDelete{Adjust: EstimateAuto, EstimateAmount: "2h"}),
	}
	for name, err := range cases {
		if errors.Is(err, ErrInvalidArgument) == false {
			t.Errorf("%s: want ErrInvalidArgument, got %v", name, err)
		}
	}
	if called == true {
		t.Fatal("no request should have been sent")
	}
}

// The write is suppressed, so there is no 201 body to decode. It has to degrade like CreateIssue
// rather than fail on an empty response.
func TestAddWorklog_DryRunWritesNothing(t *testing.T) {
	var writes []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writes = append(writes, r.Method+" "+r.URL.Path)
	}, WithDryRun(true))

	ctx := context.Background()
	hour := WorklogInput{TimeSpent: "1h"}

	worklog, err := client.AddWorklog(ctx, "ABC-1", hour)
	if err != nil {
		t.Fatalf("dry run must not fail: %v", err)
	}
	if worklog.ID != "DRY-RUN" {
		t.Fatalf("id = %q, want DRY-RUN", worklog.ID)
	}

	// The placeholder id must be safe to feed back into the rest of the lifecycle.
	if err := client.UpdateWorklog(ctx, "ABC-1", worklog.ID, hour); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := client.DeleteWorklog(ctx, "ABC-1", worklog.ID, WorklogDelete{Adjust: EstimateManual, EstimateAmount: "1h"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("dry run performed writes: %v", writes)
	}
}

// A dry run is meant to produce a reviewable plan, so an argument that could never succeed must still
// be reported rather than swallowed by the suppressed write.
func TestAddWorklog_DryRunStillValidates(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should have been sent")
	}, WithDryRun(true))

	_, err := client.AddWorklog(context.Background(), "ABC-1", WorklogInput{
		TimeSpent: "1h", TimeSpentSeconds: 3600,
	})
	if errors.Is(err, ErrInvalidArgument) == false {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}

// The 5,000-worklog ceiling is permanent, not a rate limit — a caller has to stop, not back off.
func TestAddWorklog_EntityCeilingIsALimitNotARateLimit(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"errorMessages":["Issue has too many worklogs"]}`)
	})

	_, err := client.AddWorklog(context.Background(), "ABC-1", WorklogInput{TimeSpent: "1h"})

	if errors.Is(err, ErrLimitExceeded) == false {
		t.Errorf("want ErrLimitExceeded, got %v", err)
	}
	if errors.Is(err, ErrRateLimited) == true {
		t.Error("an entity limit must not be confused with a rate limit")
	}
}

// Time tracking being disabled site-wide fails every call with a 403, which is indistinguishable from
// a credentials problem unless the caller knows to look for it.
func TestWorklogs_DisabledTimeTrackingSurfacesAsUnauthorized(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`)
	})

	_, err := client.Worklogs(context.Background(), "ABC-1")
	if errors.Is(err, ErrUnauthorized) == false {
		t.Errorf("want ErrUnauthorized, got %v", err)
	}
}
