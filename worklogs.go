package jiraclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Worklogs record time against an issue. Three things about this API surprise callers, and all three
// are dealt with here rather than left to be discovered as a failed request:
//
//   - Time tracking has to be enabled site-wide. With it switched off every call below fails, and Jira
//     answers 403 rather than explaining itself — which arrives as ErrUnauthorized and reads like a
//     credentials problem when the credentials are fine.
//   - notifyUsers defaults to *true*, so importing a backlog of time mails every watcher of every
//     issue touched. WorklogInput.NotifyUsers is how a bulk run stays quiet.
//   - adjustEstimate accepts a different set of values on each verb, and the difference is not
//     visible from any one endpoint's reference page. It is encoded as local validation below.
//
// An issue holds at most 5,000 worklogs. Past that Jira answers 413, which arrives as
// ErrLimitExceeded — a permanent per-issue ceiling rather than a rate limit, so retrying never
// clears it.

const (
	// worklogPageSize is the page size requested when listing. It matches both this endpoint's own
	// unusual default and Jira's per-issue ceiling, so an issue's whole history of time arrives in one
	// request and the pagination loop below is normally a single pass.
	worklogPageSize = 5000

	// worklogStartedFormat is the layout Jira requires for started. It rejects RFC 3339's
	// colon-separated offset with a 400, so a time.Time cannot simply be formatted with time.RFC3339.
	worklogStartedFormat = "2006-01-02T15:04:05.000-0700"
)

// EstimateAdjust selects what a worklog write does to the issue's remaining estimate. Which values
// are usable depends on the operation — see WorklogInput.Adjust and DeleteWorklog.
type EstimateAdjust string

const (
	// EstimateAuto moves the remaining estimate by the time logged. It is Jira's default on every
	// worklog operation, so passing no adjustment at all means this.
	EstimateAuto EstimateAdjust = "auto"

	// EstimateLeave keeps the remaining estimate exactly as it is.
	EstimateLeave EstimateAdjust = "leave"

	// EstimateNew replaces the remaining estimate outright with WorklogInput.NewEstimate.
	EstimateNew EstimateAdjust = "new"

	// EstimateManual moves the remaining estimate by an amount you name rather than by the time
	// logged. Unusable on UpdateWorklog — see that method for why.
	EstimateManual EstimateAdjust = "manual"
)

// Worklog is one entry of time logged against an issue, flattened the way Issue is: Jira omits the
// nested objects it has nothing to say about, so every one of them is collapsed at decode time.
type Worklog struct {
	ID string
	// IssueID is the numeric id of the issue the time was logged against, not its key.
	IssueID string

	AuthorID   string
	AuthorName string
	// UpdateAuthorID and UpdateAuthorName name whoever last edited the entry, which is not
	// necessarily whoever logged it.
	UpdateAuthorID   string
	UpdateAuthorName string

	// Comment is the note attached to the entry, flattened to plain text exactly as Issue.Comments are.
	Comment string

	// TimeSpent is Jira's rendering of the duration, e.g. "3h 20m". TimeSpentSeconds is the same
	// duration as a number and is the one to do arithmetic on.
	TimeSpent        string
	TimeSpentSeconds int64

	// Started is when the work happened; Created and Updated are when the record of it was written.
	// They differ whenever time is logged after the fact, which is most of the time.
	Started time.Time
	Created time.Time
	Updated time.Time

	// Visibility restricts who can see the entry. Its zero value means unrestricted.
	Visibility Visibility
}

// WorklogInput describes time to log or an entry to correct.
type WorklogInput struct {
	// TimeSpent is a Jira duration string, e.g. "3h 20m". TimeSpentSeconds is the same thing as a
	// number. Give one or the other — Jira accepts both keys in a payload but the pair is contradictory
	// whenever they disagree, so sending both is refused here.
	TimeSpent        string
	TimeSpentSeconds int64

	// Started is when the work happened. Left zero, Jira stamps the entry with the current time, which
	// is wrong for anything logged after the fact.
	Started time.Time

	// Comment is an optional note on the entry.
	Comment *ADFDoc

	// Adjust decides what happens to the issue's remaining estimate. The three verbs do not accept the
	// same set:
	//
	//	AddWorklog     auto, leave, new, manual  (manual takes ReduceBy)
	//	UpdateWorklog  auto, leave, new          (manual is not usable at all)
	//	DeleteWorklog  auto, leave, new, manual  (manual takes an increase)
	//
	// Left empty, Jira applies auto.
	Adjust EstimateAdjust

	// NewEstimate is the remaining estimate to set, required when Adjust is EstimateNew and meaningless
	// otherwise. It is a duration string like "4d 2h".
	NewEstimate string

	// ReduceBy is how much to take off the remaining estimate, required when Adjust is EstimateManual.
	// It applies to AddWorklog only: an update cannot express a manual adjustment and a delete moves
	// the estimate the other way.
	ReduceBy string

	// NotifyUsers controls whether watchers are emailed. Nil leaves Jira's default in place, which is
	// to notify — set it to false for a bulk run that should not generate one mail per entry.
	NotifyUsers *bool
}

// rawWorklog mirrors Jira's worklog payload. Every nested object is a pointer because Jira omits the
// ones that are unset rather than sending nulls: an entry logged by a since-deleted account carries
// no author, and one logged without a note carries no comment.
type rawWorklog struct {
	ID               string   `json:"id"`
	IssueID          string   `json:"issueId"`
	Author           *rawUser `json:"author"`
	UpdateAuthor     *rawUser `json:"updateAuthor"`
	Comment          *ADFDoc  `json:"comment"`
	Started          string   `json:"started"`
	TimeSpent        string   `json:"timeSpent"`
	TimeSpentSeconds int64    `json:"timeSpentSeconds"`
	Created          string   `json:"created"`
	Updated          string   `json:"updated"`
	Visibility       *struct {
		Type       string `json:"type"`
		Value      string `json:"value"`
		Identifier string `json:"identifier"`
	} `json:"visibility"`
}

// toWorklog flattens a raw worklog, tolerating every absent object.
func (r rawWorklog) toWorklog() Worklog {
	worklog := Worklog{
		ID:               r.ID,
		IssueID:          r.IssueID,
		TimeSpent:        r.TimeSpent,
		TimeSpentSeconds: r.TimeSpentSeconds,
		Started:          parseJiraTime(r.Started),
		Created:          parseJiraTime(r.Created),
		Updated:          parseJiraTime(r.Updated),
	}
	if r.Author != nil {
		worklog.AuthorID, worklog.AuthorName = r.Author.AccountID, r.Author.DisplayName
	}
	if r.UpdateAuthor != nil {
		worklog.UpdateAuthorID = r.UpdateAuthor.AccountID
		worklog.UpdateAuthorName = r.UpdateAuthor.DisplayName
	}
	if r.Comment != nil {
		worklog.Comment = r.Comment.Text()
	}
	if r.Visibility != nil {
		worklog.Visibility = Visibility{
			Type:       r.Visibility.Type,
			Value:      r.Visibility.Value,
			Identifier: r.Visibility.Identifier,
		}
	}
	return worklog
}

// Worklogs returns every worklog on an issue, following pagination to the end.
//
// Times are flattened and comments rendered to plain text, so a caller can sum TimeSpentSeconds
// without touching a nested object.
func (c *Client) Worklogs(ctx context.Context, key string) ([]Worklog, error) {
	return c.worklogs(ctx, key, time.Time{}, time.Time{})
}

// WorklogsStarted returns the worklogs an issue holds whose *started* time falls inside a window.
// Either bound may be zero to leave that side open.
//
// The window filters on when the work happened, not on when it was recorded, so an entry logged today
// for last Friday belongs to last Friday. A timesheet wants exactly this; an audit of what changed
// recently does not, and should read Worklog.Updated instead.
func (c *Client) WorklogsStarted(ctx context.Context, key string, after, before time.Time) ([]Worklog, error) {
	if after.IsZero() == false && before.IsZero() == false && after.After(before) == true {
		return nil, fmt.Errorf("%w: worklog window starts (%s) after it ends (%s)",
			ErrInvalidArgument, after, before)
	}
	return c.worklogs(ctx, key, after, before)
}

func (c *Client) worklogs(ctx context.Context, key string, after, before time.Time) ([]Worklog, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}

	var worklogs []Worklog
	startAt := 0
	for {
		params := url.Values{}
		params.Set("startAt", strconv.Itoa(startAt))
		params.Set("maxResults", strconv.Itoa(worklogPageSize))
		// Milliseconds, not the seconds every other epoch timestamp uses. Seconds are still a valid
		// number, so Jira accepts them and answers about a window somewhere in 1970 instead of failing.
		if after.IsZero() == false {
			params.Set("startedAfter", strconv.FormatInt(after.UnixMilli(), 10))
		}
		if before.IsZero() == false {
			params.Set("startedBefore", strconv.FormatInt(before.UnixMilli(), 10))
		}

		body, err := c.do(ctx, "GET", buildPath("/issue/"+url.PathEscape(key)+"/worklog", params), nil)
		if err != nil {
			return nil, err
		}

		var page struct {
			Worklogs []rawWorklog `json:"worklogs"`
			Total    int          `json:"total"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode worklogs for %s: %w", key, err)
		}

		for _, raw := range page.Worklogs {
			worklogs = append(worklogs, raw.toWorklog())
		}

		// Advance by what the page actually held rather than the requested size: Jira may cap
		// maxResults below what was asked, and an empty page is the only safe stop for a bad total.
		startAt += len(page.Worklogs)
		if len(page.Worklogs) == 0 || startAt >= page.Total {
			break
		}
	}
	return worklogs, nil
}

// GetWorklog reads one worklog by the id AddWorklog returned. It is the only way back to an entry
// short of listing the issue's whole history and matching on it.
func (c *Client) GetWorklog(ctx context.Context, key, worklogID string) (Worklog, error) {
	if key == "" || worklogID == "" {
		return Worklog{}, fmt.Errorf("%w: issue key and worklog id are required", ErrInvalidArgument)
	}

	body, err := c.do(ctx, "GET",
		apiBase+"/issue/"+url.PathEscape(key)+"/worklog/"+url.PathEscape(worklogID), nil)
	if err != nil {
		return Worklog{}, err
	}

	var raw rawWorklog
	if err := json.Unmarshal(body, &raw); err != nil {
		return Worklog{}, fmt.Errorf("decode worklog %s on %s: %w", worklogID, key, err)
	}
	return raw.toWorklog(), nil
}

// AddWorklog logs time against an issue and returns the entry Jira stored, whose ID is the only
// handle on it afterwards.
//
// The remaining estimate moves unless WorklogInput.Adjust says otherwise, and every watcher is
// emailed unless WorklogInput.NotifyUsers says otherwise.
//
// In dry-run mode nothing is logged and the returned Worklog carries DryRunID, mirroring
// CreateIssue. Feeding that id back into UpdateWorklog or DeleteWorklog is likewise a no-op.
func (c *Client) AddWorklog(ctx context.Context, key string, input WorklogInput) (Worklog, error) {
	if key == "" {
		return Worklog{}, fmt.Errorf("%w: issue key cannot be empty", ErrInvalidArgument)
	}
	payload, err := input.payload(true)
	if err != nil {
		return Worklog{}, err
	}

	query := url.Values{}
	input.applyNotify(query)
	if err := estimateParams(query, "AddWorklog", "reduceBy",
		input.Adjust, input.NewEstimate, input.ReduceBy); err != nil {
		return Worklog{}, err
	}
	if c.skipMutation("AddWorklog", key, input.TimeSpent, input.TimeSpentSeconds) == true {
		return Worklog{ID: DryRunID}, nil
	}

	body, err := c.do(ctx, "POST",
		buildPath("/issue/"+url.PathEscape(key)+"/worklog", query), payload)
	if err != nil {
		return Worklog{}, err
	}

	var created rawWorklog
	if err := json.Unmarshal(body, &created); err != nil {
		return Worklog{}, fmt.Errorf("decode created worklog on %s: %w", key, err)
	}
	return created.toWorklog(), nil
}

// UpdateWorklog corrects an entry. Only the fields set on the input are sent, so an update carrying
// just a comment leaves the duration and start time alone.
//
// ⚠️ EstimateManual is not usable here and is refused before sending. Jira's schema advertises it
// among adjustEstimate's values, but the update defines neither reduceBy nor increaseBy and its own
// prose describes only the other three — there is no parameter to carry the amount, so a request
// asking for it cannot say by how much.
func (c *Client) UpdateWorklog(ctx context.Context, key, worklogID string, input WorklogInput) error {
	if key == "" || worklogID == "" {
		return fmt.Errorf("%w: issue key and worklog id are required", ErrInvalidArgument)
	}
	payload, err := input.payload(false)
	if err != nil {
		return err
	}

	query := url.Values{}
	input.applyNotify(query)
	// The empty manual parameter is what makes EstimateManual unusable on this verb.
	if err := estimateParams(query, "UpdateWorklog", "",
		input.Adjust, input.NewEstimate, input.ReduceBy); err != nil {
		return err
	}
	if c.skipMutation("UpdateWorklog", key, worklogID) == true {
		return nil
	}

	_, err = c.do(ctx, "PUT",
		buildPath("/issue/"+url.PathEscape(key)+"/worklog/"+url.PathEscape(worklogID), query), payload)
	return err
}

// WorklogDelete describes how removing a worklog should affect the remaining estimate. The zero
// value applies Jira's default, which is EstimateAuto.
type WorklogDelete struct {
	Adjust EstimateAdjust
	// EstimateAmount carries whichever companion value the adjustment needs: the replacement estimate
	// for EstimateNew, and the amount to give back for EstimateManual. Note the direction — a delete
	// *increases* the remaining estimate where AddWorklog reduces it. Leave empty otherwise.
	EstimateAmount string
	// NotifyUsers mails watchers. nil applies Jira's default, which is to notify — so a bulk cleanup
	// sends one mail per entry removed unless this is set false.
	NotifyUsers *bool
}

// DeleteWorklog removes an entry permanently — Jira keeps no copy of it.
func (c *Client) DeleteWorklog(ctx context.Context, key, worklogID string, opts WorklogDelete) error {
	if key == "" || worklogID == "" {
		return fmt.Errorf("%w: issue key and worklog id are required", ErrInvalidArgument)
	}
	adjust, estimateAmount := opts.Adjust, opts.EstimateAmount

	// The one amount is routed to whichever parameter this adjustment spends it on, so that supplying
	// it with an adjustment that spends neither is caught rather than silently dropped.
	newEstimate, increaseBy := "", estimateAmount
	if adjust == EstimateNew {
		newEstimate, increaseBy = estimateAmount, ""
	}

	query := url.Values{}
	if err := estimateParams(query, "DeleteWorklog", "increaseBy", adjust, newEstimate, increaseBy); err != nil {
		return err
	}
	if opts.NotifyUsers != nil {
		query.Set("notifyUsers", strconv.FormatBool(*opts.NotifyUsers))
	}
	if c.skipMutation("DeleteWorklog", key, worklogID) == true {
		return nil
	}

	_, err := c.do(ctx, "DELETE",
		buildPath("/issue/"+url.PathEscape(key)+"/worklog/"+url.PathEscape(worklogID), query), nil)
	return err
}

// payload renders the worklog body. requireDuration marks the operations Jira will not accept without
// one; an update may legitimately carry only a comment or a corrected start time.
func (i WorklogInput) payload(requireDuration bool) (map[string]any, error) {
	if i.TimeSpent != "" && i.TimeSpentSeconds != 0 {
		return nil, fmt.Errorf("%w: give a time spent or a duration in seconds, not both (%q and %ds)",
			ErrInvalidArgument, i.TimeSpent, i.TimeSpentSeconds)
	}
	if i.TimeSpentSeconds < 0 {
		return nil, fmt.Errorf("%w: time spent cannot be negative, got %ds",
			ErrInvalidArgument, i.TimeSpentSeconds)
	}

	fields := map[string]any{}
	switch {
	case i.TimeSpent != "":
		fields["timeSpent"] = i.TimeSpent
	case i.TimeSpentSeconds > 0:
		fields["timeSpentSeconds"] = i.TimeSpentSeconds
	case requireDuration == true:
		return nil, fmt.Errorf("%w: a worklog needs a time spent or a duration in seconds",
			ErrInvalidArgument)
	}

	if i.Comment != nil {
		if len(i.Comment.Content) == 0 {
			return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, errEmptyDoc)
		}
		fields["comment"] = i.Comment
	}
	if i.Started.IsZero() == false {
		fields["started"] = i.Started.Format(worklogStartedFormat)
	}
	// An update that sets no field would spend a request to change nothing, which is always a bug in
	// the caller rather than an intention.
	if len(fields) == 0 {
		return nil, fmt.Errorf("%w: the worklog update changes nothing", ErrInvalidArgument)
	}
	return fields, nil
}

// applyNotify sets notifyUsers only when the caller stated a preference, since Jira's own default is
// to notify and there is nothing to send when that is what is wanted.
func (i WorklogInput) applyNotify(query url.Values) {
	if i.NotifyUsers != nil {
		query.Set("notifyUsers", strconv.FormatBool(*i.NotifyUsers))
	}
}

// estimateParams renders adjustEstimate and whichever companion parameter it requires, refusing a
// combination the verb cannot express.
//
// The verbs disagree about this parameter and no single endpoint reference shows the disagreement, so
// each caller states its own rules: manualParam is what this verb calls the amount accompanying a
// manual adjustment — reduceBy when logging time, increaseBy when deleting it — and is empty for the
// update, which has no such parameter and therefore cannot adjust manually at all.
func estimateParams(query url.Values, operation, manualParam string, adjust EstimateAdjust, newEstimate, manualAmount string) error {
	// A companion amount without the adjustment that spends it is the quiet failure worth catching:
	// Jira ignores the stray parameter, applies its default adjustment instead, and answers 200.
	if newEstimate != "" && adjust != EstimateNew {
		return fmt.Errorf("%w: %s: a new estimate is only applied when the adjustment is %q, got %q",
			ErrInvalidArgument, operation, EstimateNew, adjust)
	}
	if manualAmount != "" && adjust != EstimateManual {
		return fmt.Errorf("%w: %s: a manual amount is only applied when the adjustment is %q, got %q",
			ErrInvalidArgument, operation, EstimateManual, adjust)
	}

	switch adjust {
	case "":
		// Nothing to send: Jira applies EstimateAuto when the parameter is absent.
		return nil
	case EstimateAuto, EstimateLeave:
	case EstimateNew:
		if newEstimate == "" {
			return fmt.Errorf("%w: %s: the %q adjustment needs a new estimate",
				ErrInvalidArgument, operation, adjust)
		}
		query.Set("newEstimate", newEstimate)
	case EstimateManual:
		if manualParam == "" {
			return fmt.Errorf("%w: %s cannot make a %q estimate adjustment: the API defines no parameter to carry the amount",
				ErrInvalidArgument, operation, adjust)
		}
		if manualAmount == "" {
			return fmt.Errorf("%w: %s: the %q adjustment needs a %s amount",
				ErrInvalidArgument, operation, adjust, manualParam)
		}
		query.Set(manualParam, manualAmount)
	default:
		return fmt.Errorf("%w: %s: estimate adjustment %q is not one of %q, %q, %q, %q",
			ErrInvalidArgument, operation, adjust, EstimateAuto, EstimateLeave, EstimateNew, EstimateManual)
	}

	query.Set("adjustEstimate", string(adjust))
	return nil
}
