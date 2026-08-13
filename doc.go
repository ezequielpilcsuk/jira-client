// Package jiraclient is a Go client for the Jira Cloud REST v3 API, built for automations that read
// and mutate issues in bulk.
//
// It exists so services stop each carrying their own partial Jira integration. Four properties are
// deliberately first-class, because ad-hoc integrations tend to reinvent all of them:
//
//   - Dry run is a client option, not a global. A dry client logs and skips every mutation while
//     reads still work, so a caller can produce a full plan against production without writing.
//     Creating calls return [DryRunID] in place of the identifier they would have produced.
//   - Reads are nil-safe. Jira omits absent fields entirely, and code that dereferences
//     description, reporter or assignee unconditionally panics on real tickets.
//   - Site-specific identifiers are discoverable. Custom field IDs, link type IDs, priorities and
//     account IDs all differ per site; hardcoding them is what breaks an integration on the next
//     Jira. Every one can be resolved by name.
//   - Consistency is explicit. Jira's search index is eventually consistent, which silently breaks
//     search-after-write. See the Consistency section below.
//
// # Naming
//
// Two conventions run through the API, and knowing which is which tells you what a call does:
//
//   - Get… fetches specific entities by identifier: [Client.GetIssue], [Client.GetIssues],
//     [Client.GetProject], [Client.GetIssueProperty]. These read the entity directly.
//   - A bare noun lists or queries a collection: [Client.Comments], [Client.Fields],
//     [Client.Projects], [Client.Watchers], [Client.Changelog], [Client.RemoteLinks]. These follow
//     pagination to the end and return everything.
//
// Mutations are verb-first — Add…, Set…, Update…, Delete… — and every one of them is suppressed on
// a dry-run client.
//
// # Consistency
//
// Not all reads are equal, and picking the wrong one is the classic cause of "my automation did not
// see the ticket it just created":
//
//   - [Client.GetIssue] and [Client.GetIssues] read issues directly and are strongly consistent.
//   - [Client.Search] with [SearchQuery.ReconcileIssues] is strongly consistent for the issues named
//     there, and eventually consistent for the rest. Jira accepts at most 50.
//   - [Client.Search] and [Client.SearchIssues] are eventually consistent. After a write they can
//     return stale data or omit the issue entirely, for anything from seconds to minutes.
//
// # Errors
//
// Failures that callers routinely branch on are available as sentinels for errors.Is:
// [ErrNotFound], [ErrUnauthorized], [ErrRateLimited], [ErrLimitExceeded] and [ErrInvalidArgument].
// Anything else unwraps to an [*APIError] carrying Jira's own message, which is where the actionable
// detail lives — a 400 on a malformed document names the node it rejected.
//
// [ErrRateLimited] (429) is transient and worth retrying; the client already does so per its
// [RetryPolicy]. [ErrLimitExceeded] (413) is a permanent per-issue ceiling — 5,000 comments or
// worklogs, 2,000 attachments or links on one issue — and retrying never clears it.
//
// [ErrInvalidArgument] means the call was rejected locally, before any request was sent. The client
// refuses arguments that cannot possibly succeed rather than spending a round trip discovering it.
//
// # Compatibility
//
// The module is stdlib-only and has no dependencies. The go directive names the oldest Go release
// the package is known to build with, not a preference — a consumer on a newer toolchain gets that
// toolchain's standard library and its security fixes regardless.
//
// While the major version is 0 the exported API may change between minor releases; see CHANGELOG.md.
package jiraclient
