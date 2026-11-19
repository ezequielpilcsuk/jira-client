# Changelog

This project is pre-1.0. While the major version is `0`, **the exported API may change between minor
releases** — every such change is listed here with the mechanical fix. Patch releases never break it.

At 1.0 the API freezes under semantic versioning, which is why the warts below are being removed now
rather than lived with forever.

## v0.11.0

No exported signature changed: code written against v0.10.0 compiles as it is. Several behaviours
did change, each of them a fix, and they come first because they are what an upgrade can notice.

### Behaviour changes

| What | Before | Now |
|---|---|---|
| A 5xx on a create — issue, comment, worklog, link, attachment, transition | Retried under `RetryServerErrors`. A gateway timing out after Jira stored the write meant the retry filed it twice. | Returned to the caller. 429s are still retried, since Jira refuses those before executing anything. Set `RetryPolicy.RetryNonIdempotent` for the old behaviour. |
| A `Retry-After` longer than a minute | Slept through, however long. | Returned at once as `ErrRateLimited` with `APIError.RetryAfter` set. The ceiling is `RetryPolicy.MaxRetryAfter`. |
| A retry wait longer than the context has left | Slept to the deadline, then returned `context.DeadlineExceeded`, hiding the cause. | The API error is returned immediately. |
| Cancelling during a retry wait | Returned the context error alone. | The error matches both `context.Canceled` and the failure being retried. |
| `ADFDoc.Text()`, and so `Issue.Description` and comment bodies | Dropped smart-link URLs, ran table cells together (`12345` and `678` read as `12345678`), rendered a mention as its account id, and dropped link targets, emoji, statuses and dates. | Smart links render as their URL; a table is one row per line with ` \| ` between cells; a mention renders as the name Jira stored; a link whose text does not show its address is followed by it in brackets; emoji, statuses and dates render. |
| `[@…]` in builder text | Anything between the brackets became a mention, so `[@]` produced invalid ADF and a 400 for the whole comment. | Only an account-id-shaped token (letters, digits, `:`, `-`, `_`) is a mention. |
| `DocBuilder.Len()` and the summary limit | Counted bytes, refusing a 200-character accented summary as "400 chars". | Count characters, as Jira does. `Len()` also leaves link addresses out. |
| `SearchQuery.ReconcileIssues` | Sent as given. | A value that is not a numeric issue id is refused as `ErrInvalidArgument`; Jira reconciles by id only. |
| `TransitionByName` on `DryRunID` | Asked Jira for the placeholder's transitions and failed with a 404. | A no-op, like every other mutation given the placeholder. |
| An unknown transition name | `transition "X" not available on KEY`. | The same, followed by the transitions that are available. |
| `APIError.Messages` for field errors | In random order, so identical failures produced different error text. | Sorted by field name. |
| `WithTimeout` after `WithHTTPClient` | Set the timeout on the caller's own `http.Client`. | Sets it on a copy. |

### Fixed

- An issue whose description held an image with a fractional width failed to decode, and took the
  whole search page with it. `ADFAttrs` now skips attribute values it cannot read.
- `Search` requested the same page for ever when Jira repeated a page token, and `Projects` did the
  same when `isLast` never arrived. Both now stop with `ErrPaginationStalled`.
- Backoff fell to no wait at all from the 34th attempt of a policy with a large `MaxAttempts`.
- A `Retry-After` given as an HTTP date was ignored.

### Added

- **Reading custom fields** — `Issue.RawFields` holds every requested field that has no member on
  `Issue`; `Issue.FieldText` renders one as text whatever shape Jira used, and `Issue.DecodeField`
  unmarshals the exact value.
- **`Issue.Links`** — an issue's links, each naming both ends, when `issuelinks` is requested. It is
  where the ID `DeleteIssueLink` takes comes from.
- **`UpdateIssue`** — several edits to one issue (summary, fields, label changes) in a single request,
  with `NotifyUsers` to keep a bulk run quiet.
- **`ApplyTransition`** — a transition by ID or name with a comment and the fields its screen
  requires, such as a resolution.
- **`SearchQuery.Limit`**, and searches whose query is too long for a URL are sent as a POST.
- **`JQLQuote`** and **`JQLList`** for building JQL from values that are not constants.
- **`DocBuilder.AddLiteral`** and **`LiteralDoc`** for text that must reach Jira exactly as written.
- **Deprecation warnings** — a client with a logger reports, once per notice, any endpoint Jira
  answers with `Deprecation` or `Sunset` headers.
- **`WithSiteURL`** for clients whose base URL is Atlassian's API gateway, as scoped API tokens
  require.
- `Comment.AuthorName` and `Comment.Updated`; `RetryPolicy.MaxRetryAfter` and
  `RetryPolicy.RetryNonIdempotent`; `ErrPaginationStalled`; `ADFAttrs.URL`, `Text`, `ShortName` and
  `Timestamp`.

## v0.10.0

Breaking changes, an API-quality pass ahead of a v1 freeze, plus attachments and worklogs.

### Breaking

| Before | After | Why |
|---|---|---|
| `AddComment(ctx, key, doc) error` | `AddComment(ctx, key, doc) (Comment, error)` | The ID is the only handle on a comment; discarding it meant a comment could never be edited or deleted. `AddCommentReturning` existed only to work around this and is gone. |
| `AddTextComment(...) error` | `AddTextComment(...) (Comment, error)` | Same reason. |
| `Search(ctx, jql, fields) ([]Issue, error)` | `Search(ctx, SearchQuery) (SearchResult, error)` | Warnings were reachable only via `SearchReconciled`, which conflated "I want warnings" with "I want reconciliation". Use `SearchIssues(ctx, jql, fields)` for the old shape. |
| `SearchReconciled(ctx, jql, fields, ids)` | `Search(ctx, SearchQuery{JQL: …, ReconcileIssues: ids})` | Folded into `SearchQuery`. |
| `Changelogs(...) (map[string][]ChangelogEntry, error)` | `Changelogs(...) ([]IssueChangelog, error)` | The map was keyed by numeric issue ID while `GetIssues` keyed by issue key. The bulk endpoint returns only IDs — even when you ask by key — so a map implied a lookup the API cannot support. |
| `IssueProperty(...)` | `GetIssueProperty(...)` | Its own `SetIssueProperty`/`DeleteIssueProperty` siblings are verb-first. |
| `Priorities(ctx, ...PriorityQuery)` | `Priorities(ctx, PriorityQuery)` | Variadic-as-optional let a caller pass three filters and silently ignored two. Pass `PriorityQuery{}` for no filter. |
| `PriorityRanks(ctx, ...PriorityQuery)` | `PriorityRanks(ctx, PriorityQuery)` | Same. |
| `Projects(ctx, ...ProjectQuery)` | `Projects(ctx, ProjectQuery)` | Same. |
| `Comments(ctx, key, ...CommentOrder)` | `Comments(ctx, key, CommentOrder)` | Same. Pass `""` for Jira's default order. |
| `DeleteWorklog(ctx, key, id, adjust, amount)` | `DeleteWorklog(ctx, key, id, WorklogDelete)` | The old signature had no slot for `notifyUsers`, so a bulk cleanup always mailed every watcher. |

The whole migration for a consumer that ignores the new return values is one line per call site:

```go
// before
return client.AddComment(ctx, key, doc)
// after
_, err := client.AddComment(ctx, key, doc)
return err
```

### Added

- **Attachments** — `AddAttachment`, `GetAttachment`, `DownloadAttachment`, `DeleteAttachment`,
  `AttachmentSettings`. Upload is multipart with the `X-Atlassian-Token` header Jira requires;
  download reads Jira's 303 rather than following it, because Go strips `Authorization` across hosts
  and the redirect points at a signed media URL that neither needs nor accepts credentials.
- **Worklogs** — `Worklogs`, `WorklogsStarted`, `GetWorklog`, `AddWorklog`, `UpdateWorklog`,
  `DeleteWorklog`, with the estimate-adjustment rules enforced locally. They are asymmetric per verb:
  `manual` is unusable on update, and a companion amount supplied without its adjustment is silently
  dropped by Jira, which then applies `auto` and quietly corrupts the estimate.
- `DryRunID`, the placeholder identifier creating calls return on a dry client. Compare against this
  rather than the `"DRY-RUN"` literal.
- `doc.go` with the package overview, the naming convention, and the consistency guarantees.
- Runnable examples, a `.golangci.yml`, and this file.

### Changed

- `AddComment` and `AddAttachment` no longer fail when a 2xx response body cannot be decoded. The
  write succeeded; reporting an error there invites a retry that posts the comment or uploads the
  file a second time. The failure is logged and the identifier is lost instead.
- `Changelogs` accumulates an issue's history across pages rather than emitting the issue twice.
- The `go` directive is `1.22`, the oldest release this builds on. It was `1.25.2` — pinning a patch
  excluded consumers on `1.25.0`/`1.25.1` for no benefit. A library's `go` directive is a floor for
  its consumers, not a security control: the consumer's own toolchain supplies the standard library
  and its fixes. `1.22` rather than lower because per-iteration loop variables landed there.
- Transport gained raw bodies, custom headers and opt-out redirects. `do` is unchanged for callers.

## v0.9.0

- Issue properties, changelogs, comment lifecycle, remote links, link types, field discovery,
  watchers, project and site metadata, `ApproximateCount`, `ValidateJQL`, `MyPermissions`.
- `Issue.StatusCategory` and `Issue.IsDone()`.
- `APIError.RateLimitReset` and `APIError.NearLimit`.
- Fixed: `Priorities` moved off the deprecated `GET /priority` to `/priority/search`; `GetIssues`
  moved off JQL to `/issue/bulkfetch`, which is strongly consistent; search warnings are surfaced;
  `AccountIDByEmail` verifies an exact match rather than taking Jira's first prefix hit;
  `parseJiraTime` accepts the formats Jira actually sends; `ErrLimitExceeded` added for 413.

## v0.8.0

- `SearchUsers` and `AccountIDByEmail`.

## v0.7.0

- `CreateLink` with a comment carried inside the link payload.

## v0.6.0

- `TransitionWithComment` and `TransitionByNameWithComment`.

## v0.5.1

- MIT licence.

## v0.5.0

- `DeleteIssue`.

## v0.4.0

- `Issue.Comments`, flattened to plain text.

## v0.3.0

- `[issue:KEY]` tokens render as links; `Client.NewDocBuilder`, `Client.IssueURL`.

## v0.2.0

- `Priorities` and `PriorityRanks`, reading rank from Jira rather than inferring it from IDs.

## v0.1.0

- Initial extraction: auth, JQL search with pagination, issue reads, create, transitions, assignee,
  labels, comments, issue links, ADF authoring and extraction, retry with backoff, dry run.
