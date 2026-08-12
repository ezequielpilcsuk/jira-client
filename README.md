# jira-client

A Go client for the Jira Cloud REST v3 API, built for automations that read and mutate issues in
bulk.

## Why

Hand-rolled Jira integrations tend to rediscover the same handful of problems. This library solves
them once:

- **Dry run is a client option, not a global flag.** A dry client logs and skips every mutation while
  reads keep working, so an automation can produce a complete plan against production without
  writing a single change.
- **Reads are nil-safe.** Jira omits unset fields rather than sending nulls, so code that
  dereferences `description`, `assignee` or `reporter` unconditionally panics on real issues. Every
  field is flattened to a zero value at decode time.
- **Rate limiting is handled.** Jira limits on several axes, including a per-issue write limit — any
  workflow that links, comments, labels and transitions the *same* issue can trip it at modest
  volume. The client retries with exponential backoff and jitter, honouring `Retry-After`.
- **ADF has sharp edges.** A text node with empty text is invalid and rejects the entire request with
  a 400, which is trivially easy to produce from a table cell that happens to be blank. The document
  builder emits a valid empty paragraph instead.
- **Transitions can be resolved by name.** Transition IDs differ between sites and are not status
  IDs; `TransitionByName` looks up what is actually available on the issue.
- **Comments expand inline tokens.** `[@accountId]` becomes a mention and `[issue:KEY]` a link to
  that issue, so an automation composes a notification as plain text and gets valid ADF out.
- **Priority rank is read, not guessed.** Priority IDs are assigned in creation order, not rank
  order, so a scheme customised after setup can rank `Normal`(10000) above `Minor`(4). `Priorities`
  and `PriorityRanks` return the site's real ordering.

## Installation

```bash
go get github.com/ezequielpilcsuk/jira-client
```

## Usage

```go
import jiraclient "github.com/ezequielpilcsuk/jira-client"

client := jiraclient.NewClient(
    "https://your-site.atlassian.net",
    "bot@example.com",
    apiToken,
    jiraclient.WithRetryPolicy(jiraclient.DefaultRetryPolicy()),
    jiraclient.WithDryRun(cfg.DryRun),
)

issues, err := client.Search(ctx, `project = ABC AND statusCategory != Done`, nil)
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

`Search` follows pagination to the end, so a broad JQL returns everything — narrow the query rather
than relying on a limit.

### Mutating

```go
err := client.SetAssignee(ctx, "ABC-123", accountID)   // "" unassigns
err = client.AddLabel(ctx, "ABC-123", "triaged")
err = client.UpdateSummary(ctx, "ABC-123", "New title")
err = client.AddTextComment(ctx, "ABC-123", "Handled automatically.")

// Reads "ABC-124 duplicates ABC-123" / "ABC-123 is duplicated by ABC-124".
err = client.LinkIssues(ctx, jiraclient.LinkDuplicate, "ABC-123", "ABC-124")

err = client.TransitionByName(ctx, "ABC-124", "Won't Do")
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
err = client.AddComment(ctx, "ABC-123", doc)
```

`[@accountId]` becomes a real mention node, which actually notifies the user — plain `@name` text
does not.

### Dry run

```go
client := jiraclient.NewClient(baseURL, email, token, jiraclient.WithDryRun(true))

// Reads still hit Jira.
issues, _ := client.Search(ctx, jql, nil)

// Every mutation is logged and skipped; CreateIssue returns "DRY-RUN".
_ = client.TransitionByName(ctx, "ABC-124", "Won't Do")
```

Attach a logger to see what would have happened:

```go
jiraclient.WithLogger(log.New(os.Stderr, "", log.LstdFlags))
```

### Errors

```go
switch {
case errors.Is(err, jiraclient.ErrNotFound):
case errors.Is(err, jiraclient.ErrUnauthorized):
case errors.Is(err, jiraclient.ErrRateLimited):
case errors.Is(err, jiraclient.ErrInvalidArgument):  // rejected before any request was sent
}

var apiErr *jiraclient.APIError
if errors.As(err, &apiErr) {
    // apiErr.Messages carries Jira's own explanation — a 400 on a bad ADF document
    // names the node it rejected.
    log.Printf("status %d: %v (%s)", apiErr.StatusCode, apiErr.Messages, apiErr.RateLimitReason)
}
```

## Notes

- Authentication is Jira Cloud basic auth: account email plus an API token.
- REST **v3** throughout; v2 is deprecated and is not ADF-native.
- Arguments that cannot possibly succeed — an over-long summary, a label containing whitespace, an
  empty document, a self-link — are rejected locally as `ErrInvalidArgument` rather than sent.
- The client is stateless and safe for concurrent use.
