# Changelog

This project is pre-1.0. While the major version is `0`, **the exported API may change between minor
releases** — every such change is listed here with the mechanical fix. Patch releases never break it.

At 1.0 the API freezes under semantic versioning, which is why the warts below are being removed now
rather than lived with forever.

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
