# jira-client

[![Go Reference](https://pkg.go.dev/badge/github.com/ezequielpilcsuk/jira-client.svg)](https://pkg.go.dev/github.com/ezequielpilcsuk/jira-client)
[![Licence: MIT](https://img.shields.io/badge/licence-MIT-blue.svg)](LICENSE)

A Go client for the **Jira Cloud REST API v3**, built for automations that read and mutate issues in
bulk. Standard library only — no dependencies.

**Contents:** [Why](#why) · [Installation](#installation) · [Usage](#usage) ·
[What it covers](#what-it-covers) · [Staying ahead of Jira API changes](#staying-ahead-of-jira-api-changes) ·
[Authentication](#authentication) · [Stability and versioning](#stability-and-versioning)

## Why

Hand-rolled Jira integrations tend to rediscover the same handful of problems. This library solves
them once:

- **Dry run is a client option, not a global flag.** A dry client logs and skips every mutation while
  reads keep working, so an automation can produce a complete plan against production without
  writing a single change.
- **Reads are nil-safe.** Jira omits unset fields rather than sending nulls, so code that
  dereferences `description`, `assignee` or `reporter` unconditionally panics on real issues. Every
  field is flattened to a zero value at decode time.
- **Retries cannot file a ticket twice.** Jira limits on several axes, including a per-issue write
  limit that a workflow linking, commenting, labelling and transitioning the *same* issue trips at
  modest volume. With a retry policy the client backs off and retries — rate limits always, server
  errors only for requests that are safe to send twice. See [Retries and rate limits](#retries-and-rate-limits).
- **ADF has sharp edges.** A text node with empty text is invalid and rejects the entire request with
  a 400, which is trivially easy to produce from a table cell that happens to be blank. The document
  builder emits a valid empty paragraph instead.
- **Descriptions read back as usable text.** Smart links keep their URL, table cells stay separate
  values, and mentions carry the person's name — so a description written by one service can be
  parsed by another.
- **A transition can carry its comment and its fields.** `ApplyTransition` sends them in one request,
  so a rejected transition cannot leave an orphaned comment explaining a move that never happened.
- **Transitions can be resolved by name.** Transition IDs differ between sites and are not status
  IDs; `TransitionByName` looks up what is actually available on the issue.
- **Comments expand inline tokens.** `[@accountId]` becomes a mention and `[issue:KEY]` a link to
  that issue, so an automation composes a notification as plain text and gets valid ADF out.
- **Priority rank is read, not guessed.** Priority IDs are assigned in creation order, not rank
  order, so a scheme customised after setup can rank `Normal`(10000) above `Minor`(4). `Priorities`
  and `PriorityRanks` return the site's real ordering.
- **Search-after-write is not silently wrong.** Jira's search index is eventually consistent, so an
  issue created a moment ago can be missing from a JQL result. `GetIssues` reads issues directly and
  `SearchQuery.ReconcileIssues` reconciles the ids you name, rather than leaving it to chance.
- **Site-specific IDs are discoverable.** Custom field IDs, link type IDs, priorities and account ids
  differ per site, and hardcoding them is what makes an integration break on the next Jira. Every one
  can be resolved by name.

## Installation

```bash
go get github.com/ezequielpilcsuk/jira-client
```

The package name is `jiraclient`. It builds on Go 1.22 and later.

## Usage

```go
import jiraclient "github.com/ezequielpilcsuk/jira-client"

client := jiraclient.NewClient(
    "https://your-site.atlassian.net",
    "bot@example.com",
    apiToken,
    jiraclient.WithRetryPolicy(jiraclient.DefaultRetryPolicy()),
    jiraclient.WithLogger(log.New(os.Stderr, "jira ", log.LstdFlags)),
    jiraclient.WithDryRun(cfg.DryRun),
)

issues, err := client.SearchIssues(ctx, `project = ABC AND statusCategory != Done`, nil)
```

### Reading

```go
// Every optional field is already flattened — no nil checks needed.
for _, issue := range issues {
    fmt.Println(issue.Key, issue.Status, issue.AssigneeName, issue.Created)
}

one, err := client.GetIssue(ctx, "ABC-123", nil)

// Batched, chunked at 100 keys per request.
byKey, err := client.GetIssues(ctx, []string{"ABC-1", "ABC-2"}, nil)
```

`Search` follows pagination to the end, so a broad JQL returns everything. Set a `Limit` when only
the first few are wanted:

```go
newest, err := client.Search(ctx, jiraclient.SearchQuery{
    JQL:   `project = ABC ORDER BY created DESC`,
    Limit: 5,
})
```

#### Custom fields and links

A field with no member on `Issue` — a custom field above all — is returned in `Issue.RawFields`
whenever it is requested by ID:

```go
storyPoints, err := client.FieldIDByName(ctx, "Story Points")   // "customfield_10004" on this site

issue, err := client.GetIssue(ctx, "ABC-123", []string{"summary", "issuelinks", storyPoints})

fmt.Println(issue.FieldText(storyPoints))            // "3" — whatever shape Jira used

var points float64
found, err := issue.DecodeField(storyPoints, &points) // the exact value; found is false when unset

for _, link := range issue.Links {                    // needs "issuelinks" among the fields
    fmt.Println(link.ID, link.Type.Name, link.InwardKey, link.OutwardKey)
}
```

`FieldText` renders the shapes Jira uses for field values — text, numbers, select options, users,
versions, rich text, lists of any of them — so a caller does not need to know which one a field is.

#### People

```go
// Resolve a person to an account id. "" and no error means nothing matched.
accountID, err := client.AccountIDByEmail(ctx, "ada@example.com")

users, err := client.SearchUsers(ctx, "Ada")
```

Jira's visibility rules apply to user search: an account whose email is private will not match on
email even when the address is correct, so an empty result is not proof that the person has no
account.

### Mutating

```go
err := client.SetAssignee(ctx, "ABC-123", accountID)   // "" unassigns
err = client.AddLabel(ctx, "ABC-123", "triaged")
err = client.UpdateSummary(ctx, "ABC-123", "New title")
comment, err := client.AddTextComment(ctx, "ABC-123", "Handled automatically.")

// Reads "ABC-124 duplicates ABC-123" / "ABC-123 is duplicated by ABC-124".
err = client.LinkIssues(ctx, jiraclient.LinkDuplicate, "ABC-123", "ABC-124")

err = client.TransitionByName(ctx, "ABC-124", "Won't Do")
```

Several edits to one issue belong in one request. Jira limits writes per issue, and one request is
applied whole or not at all:

```go
quiet := false
err := client.UpdateIssue(ctx, "ABC-123", jiraclient.IssueUpdate{
    Summary:      "New title",
    Fields:       map[string]any{storyPoints: 3},
    AddLabels:    []string{"triaged"},
    RemoveLabels: []string{"needs-triage"},
    NotifyUsers:  &quiet,   // no email to watchers; needs an admin permission
})
```

A workflow that demands a resolution on the way to done takes it with the transition:

```go
err := client.ApplyTransition(ctx, "ABC-124", jiraclient.TransitionInput{
    Name:    "Won't Do",
    Comment: doc,
    Fields:  map[string]any{"resolution": map[string]string{"name": "Won't Do"}},
})
```

### Rich comments

```go
builder := jiraclient.NewDocBuilder().
    AddHeading(3, "Validation report").
    AddText("Owner [@5b10ac8d82e05b22cc7d4ef5] please review.")

_ = builder.AddLinkedTable(
    []string{"Product", "Price", "URL"},
    [][]jiraclient.Cell{{
        {Text: "Widget"}, {Text: "19.99"}, {Text: "open", Href: "https://example.com/widget"},
    }},
)

if builder.Len() > jiraclient.CommentMaxChars {
    // Trim before posting; the limit applies to rendered text, not the JSON payload.
}

doc, err := builder.Build()
comment, err := client.AddComment(ctx, "ABC-123", doc)   // the ID is the only handle on it
```

`[@accountId]` becomes a real mention node, which actually notifies the user — plain `@name` text
does not.

Text the automation did not write — a crawled title, a database value, a quoted error — goes through
`AddLiteral` (or `LiteralDoc`), which expands nothing. A stray `[@…]` in it stays text:

```go
builder.AddText("Cause reported by the crawler:").AddLiteral(crawlerMessage)
```

### Dry run

```go
client := jiraclient.NewClient(baseURL, email, token, jiraclient.WithDryRun(true))

// Reads still hit Jira.
issues, _ := client.SearchIssues(ctx, jql, nil)

// Every mutation is logged and skipped; creating calls return jiraclient.DryRunID.
_ = client.TransitionByName(ctx, "ABC-124", "Won't Do")
```

Attach a logger to see what would have happened:

```go
jiraclient.WithLogger(log.New(os.Stderr, "", log.LstdFlags))
```

### Discovery

Site-specific IDs are the most brittle thing to hardcode in a Jira integration, so they can all be
resolved by name:

```go
// customfield_10004 is not the same number on the next site.
fieldID, err := client.FieldIDByName(ctx, "Story Points")   // ambiguous name -> ErrInvalidArgument
linkTypeID, err := client.LinkTypeIDByName(ctx, "Blocks")
ranks, err := client.PriorityRanks(ctx, jiraclient.PriorityQuery{})
accountID, err := client.AccountIDByEmail(ctx, "ada@example.com")

me, err := client.Myself(ctx)                                // also a credential check
projects, err := client.Projects(ctx, jiraclient.ProjectQuery{})
types, err := client.ProjectStatuses(ctx, "ABC")             // issue types + their statuses
```

Prefer `Issue.StatusCategory` (`new` / `indeterminate` / `done`) over `Issue.Status` for "is this
finished" — status *names* are per-workflow and site-editable, so a check against `"Done"` breaks on
any project that renamed it. `Issue.IsDone()` wraps it.

### Building and checking a query

A value pasted between quotes breaks the query as soon as it contains a quote of its own, and when it
came from outside — a ticket title, a product name — it can rewrite the query. Quote it:

```go
jql := "project = ABC AND summary ~ " + jiraclient.JQLQuote(title) +
    " AND labels IN " + jiraclient.JQLList("triaged", "bot")
```

`/search/jql` dropped the `validateQuery` parameter, so validating JQL and counting matches are
separate calls. Both suit a dry run, which is meant to produce a reviewable plan without writing:

```go
results, err := client.ValidateJQL(ctx, jql)     // errors and warnings, without executing
count, err := client.ApproximateCount(ctx, jql)  // estimated; /search/jql no longer returns a total
allowed, err := client.MyPermissions(ctx,
    jiraclient.PermissionScope{IssueKey: "ABC-123"}, "EDIT_ISSUES")
```

⚠️ Scope permission checks to an **issue** where you can. Atlassian documents project-scoped answers
as optimistic: a user can be reported as holding a permission in a project context without holding it
for any particular issue.

### Idempotency

An automation that must not process a ticket twice usually ends up encoding state in labels or
comment text. Issue properties are the intended mechanism — invisible to users, and they survive
edits:

```go
err := client.SetIssueProperty(ctx, "ABC-123", "my-bot:processed", map[string]any{"run": 42})

var state struct{ Run int }
err = client.GetIssueProperty(ctx, "ABC-123", "my-bot:processed", &state)
```

`SetRemoteLink` is idempotent the same way: posting with a `globalId` that already exists updates
that link rather than adding a second one — though it is a *replace*, so fields you omit are nulled.

### Attachments and worklogs

```go
uploaded, err := client.AddAttachment(ctx, "ABC-123", "crash.log", file)
content, err := client.DownloadAttachment(ctx, uploaded[0].ID)

_, err = client.AddWorklog(ctx, "ABC-123", jiraclient.WorklogInput{
    TimeSpentSeconds: 3600,
    Adjust:           jiraclient.EstimateAuto,
})
```

Worklog estimate adjustment is asymmetric per verb and enforced locally: `manual` is unusable on
update, and a companion amount supplied without its adjustment is silently dropped by Jira — which
then applies `auto` and quietly corrupts the remaining estimate.

### Consistency: reads are not all equal

Jira serves searches from an index that is only **eventually consistent**. After a write, a search
can return stale data or miss the issue outright, for anything from seconds to minutes. Three
different guarantees are available, and picking the wrong one is the classic source of
"my automation didn't see the ticket it just created":

| Call | Consistency |
|---|---|
| `GetIssue`, `GetIssues` | **Strong** — reads issues directly |
| `Search` with `SearchQuery.ReconcileIssues` | Strong **for the ids named** (max 50), eventual for the rest |
| `Search`, `SearchIssues` | Eventual |

`Search` also returns `SearchResult.Warnings` — worth checking, because a JQL that Jira warned about
still returns HTTP 200 with an empty result set, making a broken query indistinguishable from one
that legitimately matched nothing.

### Retries and rate limits

The client does not retry unless given a policy. `DefaultRetryPolicy` follows Atlassian's published
guidance — exponential backoff with jitter, never shorter than the server's `Retry-After`:

| Response | Retried? |
|---|---|
| 429 | Always. Jira refused the request before executing it, so sending it again is safe. |
| 5xx on a read, a `PUT`, a `DELETE` or an upsert | Yes. A second execution leaves the same state. |
| 5xx on a create — issue, comment, worklog, link, attachment, transition | **No.** A gateway can time out after Jira stored the write, and a replay would file it twice. Set `RetryPolicy.RetryNonIdempotent` to accept that risk. |
| `Retry-After` longer than `RetryPolicy.MaxRetryAfter` (one minute by default) | No. The error comes back at once with `APIError.RetryAfter`, so the work can be rescheduled. |

On a 429, `APIError` carries `RateLimitReason` (which ceiling was hit — global, tenant, burst, or
per-issue-on-write), `RateLimitReset` (when the quota refills) and `NearLimit`.

`ErrLimitExceeded` (HTTP 413) is a different thing entirely: a permanent per-issue ceiling — 5,000
comments or worklogs, 2,000 attachments or links on one issue. Retrying never clears it.

### Errors

```go
switch {
case errors.Is(err, jiraclient.ErrNotFound):
case errors.Is(err, jiraclient.ErrUnauthorized):
case errors.Is(err, jiraclient.ErrRateLimited):       // 429, transient — back off
case errors.Is(err, jiraclient.ErrLimitExceeded):     // 413, permanent — retrying will not help
case errors.Is(err, jiraclient.ErrPaginationStalled): // Jira stopped advancing a listing
case errors.Is(err, jiraclient.ErrInvalidArgument):   // rejected before any request was sent
}

var apiErr *jiraclient.APIError
if errors.As(err, &apiErr) {
    // apiErr.Messages carries Jira's own explanation — a 400 on a bad ADF document
    // names the node it rejected.
    log.Printf("status %d: %v (%s)", apiErr.StatusCode, apiErr.Messages, apiErr.RateLimitReason)
}
```

## What it covers

| Area | Methods |
|---|---|
| Search | `Search`, `SearchIssues`, `ApproximateCount`, `ValidateJQL`, `JQLQuote`, `JQLList` |
| Issues | `GetIssue`, `GetIssues`, `CreateIssue`, `UpdateIssue`, `UpdateSummary`, `UpdateCustomField`, `SetAssignee`, `AddLabel`, `RemoveLabel`, `DeleteIssue` |
| Workflow | `Transitions`, `ApplyTransition`, `Transition`, `TransitionByName`, and their `…WithComment` forms |
| Comments | `Comments`, `AddComment`, `AddTextComment`, `AddRestrictedComment`, `UpdateComment`, `DeleteComment` |
| Rich text (ADF) | `NewDocBuilder`, `TextDoc`, `LiteralDoc`, `ADFDoc.Text` |
| Issue links | `LinkIssues`, `CreateLink`, `GetIssueLink`, `DeleteIssueLink`, `LinkTypes`, `LinkTypeIDByName`, `Issue.Links` |
| Remote links | `RemoteLinks`, `SetRemoteLink`, `DeleteRemoteLink`, `DeleteRemoteLinkByGlobalID` |
| History | `Changelog`, `Changelogs` |
| Attachments | `AddAttachment`, `GetAttachment`, `DownloadAttachment`, `DeleteAttachment`, `AttachmentSettings` |
| Worklogs | `Worklogs`, `WorklogsStarted`, `GetWorklog`, `AddWorklog`, `UpdateWorklog`, `DeleteWorklog` |
| Issue properties | `IssuePropertyKeys`, `GetIssueProperty`, `SetIssueProperty`, `DeleteIssueProperty` |
| Watchers | `Watchers`, `AddWatcher`, `RemoveWatcher` |
| Fields | `Fields`, `FieldsByName`, `FieldIDByName`, `Issue.RawFields`, `Issue.FieldText`, `Issue.DecodeField` |
| Projects and site | `Projects`, `GetProject`, `ProjectStatuses`, `Priorities`, `PriorityRanks`, `ServerInfo` |
| People and permissions | `Myself`, `SearchUsers`, `AccountIDByEmail`, `MyPermissions` |

Not covered: the Jira Software (boards, sprints) and Jira Service Management APIs, bulk create and
bulk edit, create/edit metadata, filters, webhooks, and administration.

## Staying ahead of Jira API changes

Atlassian marks an endpoint it is going to remove with `Deprecation` and `Sunset` response headers,
at least six months ahead. A client with a logger reports them, once per notice:

```
jira: GET /rest/api/3/… is deprecated and scheduled for removal (Deprecation: "@1767225600", Sunset: "Wed, 01 Jul 2026 00:00:00 GMT", Link: "<…>; rel=\"deprecation\"")
```

Alert on `is deprecated and scheduled for removal` in a service's logs and the removal is known
months before it happens rather than on the day. Without a logger the client stays silent, so pass
`WithLogger` in production. The changes themselves are announced in the
[Jira Cloud platform changelog](https://developer.atlassian.com/cloud/jira/platform/changelog/).

## Authentication

Jira Cloud basic auth: the account email plus an API token.

- API tokens **expire** — between 1 and 365 days after creation, a year by default — so a 401 on a
  call that used to work usually means the token needs reissuing rather than that permissions
  changed. `Myself` at startup is the cheapest check.
- **Scoped** API tokens, the only kind a service account can have, do not work against the site URL.
  Atlassian serves them through its gateway, so the base URL is the gateway's and the site URL is
  given separately for links:

```go
client := jiraclient.NewClient(
    "https://api.atlassian.com/ex/jira/"+cloudID,
    "bot@example.com",
    scopedToken,
    jiraclient.WithSiteURL("https://your-site.atlassian.net"),   // where IssueURL and [issue:KEY] point
)
```

## Stability and versioning

The module is pre-1.0. While the major version is `0` the exported API may change between minor
releases; every such change, and every change in behaviour, is listed in
[CHANGELOG.md](CHANGELOG.md) with the fix for it. Patch releases never break the API. Pin a version
in `go.mod` and read the changelog before raising it.

The `go` directive names the oldest Go release the package builds with (1.22), not a preference: a
consumer on a newer toolchain gets that toolchain's standard library and its security fixes
regardless.

## Notes

- REST **v3** throughout, because it is ADF-native. v2 is *not* deprecated — Atlassian maintains both
  and documents them as offering the same operations. `/rest/api/latest` resolves to v2 semantics, so
  it is not a safe alias for v3.
- Arguments that cannot possibly succeed — an over-long summary, a label containing whitespace, an
  empty document, a self-link, more than 50 reconcile ids — are rejected locally as
  `ErrInvalidArgument` rather than sent.
- The client is safe for concurrent use.
- Run `go test -race ./...` and `golangci-lint run` before tagging a release.

## License

MIT — see [LICENSE](LICENSE). Free to use, modify and redistribute, including commercially.
