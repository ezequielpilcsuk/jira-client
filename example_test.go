package jiraclient_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	jiraclient "github.com/ezequielpilcsuk/jira-client"
)

func ExampleNewClient() {
	client := jiraclient.NewClient(
		"https://your-site.atlassian.net",
		"bot@example.com",
		os.Getenv("JIRA_API_TOKEN"),
		jiraclient.WithRetryPolicy(jiraclient.DefaultRetryPolicy()),
		jiraclient.WithLogger(log.New(os.Stderr, "jira ", log.LstdFlags)),
	)

	// Myself doubles as a credential check: a 401 here means the token is wrong or has expired.
	me, err := client.Myself(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(me.DisplayName)
}

// A dry client is the intended way to review a plan against production before letting it write.
func ExampleWithDryRun() {
	client := jiraclient.NewClient("https://your-site.atlassian.net", "bot@example.com", "token",
		jiraclient.WithDryRun(true),
		jiraclient.WithLogger(log.New(os.Stderr, "", 0)),
	)
	ctx := context.Background()

	// Reads still hit Jira, so the plan is built from real data.
	issues, err := client.SearchIssues(ctx, `project = ABC AND status = "To Do"`, nil)
	if err != nil {
		log.Fatal(err)
	}

	// Mutations are logged and skipped. The returned key is DryRunID, not a real issue.
	for _, issue := range issues {
		_ = client.AddLabel(ctx, issue.Key, "triaged")
	}
}

// Search reads an eventually consistent index, so an issue written moments ago can be missing.
// Naming it in ReconcileIssues makes the search wait for it.
func ExampleClient_Search_afterAWrite() {
	client := jiraclient.NewClient("https://your-site.atlassian.net", "bot@example.com", "token")
	ctx := context.Background()

	key, err := client.CreateIssue(ctx, jiraclient.CreateIssueInput{
		ProjectKey: "ABC",
		IssueType:  "Task",
		Summary:    "Crawler returned no results",
	})
	if err != nil {
		log.Fatal(err)
	}

	created, err := client.GetIssue(ctx, key, nil)
	if err != nil {
		log.Fatal(err)
	}

	result, err := client.Search(ctx, jiraclient.SearchQuery{
		JQL:             `project = ABC AND labels = "crawler"`,
		ReconcileIssues: []string{created.ID},
	})
	if err != nil {
		log.Fatal(err)
	}

	// Warnings matter: a JQL Jira objected to still returns 200 with no issues.
	for _, warning := range result.Warnings {
		log.Printf("jql warning: %s", warning)
	}
	fmt.Println(len(result.Issues))
}

// Custom field IDs differ between sites, so resolve them rather than hardcoding customfield_10004.
func ExampleClient_FieldIDByName() {
	client := jiraclient.NewClient("https://your-site.atlassian.net", "bot@example.com", "token")
	ctx := context.Background()

	fieldID, err := client.FieldIDByName(ctx, "Story Points")
	if errors.Is(err, jiraclient.ErrInvalidArgument) == true {
		// Jira does not enforce unique field names; an ambiguous one is an error, not a coin toss.
		log.Fatalf("ambiguous field name: %v", err)
	}
	if err != nil {
		log.Fatal(err)
	}

	if err := client.UpdateCustomField(ctx, "ABC-1", fieldID, 3); err != nil {
		log.Fatal(err)
	}
}

// Issue properties are the intended way to make an automation idempotent: invisible to users, and
// they survive edits, unlike state smuggled into labels or comment text.
func ExampleClient_SetIssueProperty() {
	client := jiraclient.NewClient("https://your-site.atlassian.net", "bot@example.com", "token")
	ctx := context.Background()

	var state struct {
		Run int `json:"run"`
	}
	err := client.GetIssueProperty(ctx, "ABC-1", "my-bot:processed", &state)
	if errors.Is(err, jiraclient.ErrNotFound) == false && err != nil {
		log.Fatal(err)
	}
	if state.Run > 0 {
		return // already handled
	}

	if err := client.SetIssueProperty(ctx, "ABC-1", "my-bot:processed", map[string]any{"run": 1}); err != nil {
		log.Fatal(err)
	}
}

// A comment is composed as plain text; the tokens expand into real ADF nodes.
func ExampleClient_AddComment() {
	client := jiraclient.NewClient("https://your-site.atlassian.net", "bot@example.com", "token")
	ctx := context.Background()

	doc, err := client.NewDocBuilder().
		AddParagraphs("[@5b10ac8d82e05b22cc7d4ef5] this duplicates [issue:ABC-10].").
		Build()
	if err != nil {
		log.Fatal(err)
	}

	// The ID is the only handle on a comment, so keep it if you may need to edit or delete it.
	comment, err := client.AddComment(ctx, "ABC-11", doc)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(comment.ID)
}

// Status names are per-workflow and site-editable; the category is not.
func ExampleIssue_IsDone() {
	client := jiraclient.NewClient("https://your-site.atlassian.net", "bot@example.com", "token")

	issue, err := client.GetIssue(context.Background(), "ABC-1", nil)
	if err != nil {
		log.Fatal(err)
	}

	// Correct even on a project that renamed "Done" to "Shipped".
	if issue.IsDone() == true {
		fmt.Println("finished")
	}
}

// A custom field is requested by ID and read back from RawFields.
func ExampleIssue_FieldText() {
	client := jiraclient.NewClient("https://your-site.atlassian.net", "bot@example.com", "token")
	ctx := context.Background()

	// The ID differs per site, so it is resolved by name rather than hardcoded.
	storyPoints, err := client.FieldIDByName(ctx, "Story Points")
	if err != nil {
		log.Fatal(err)
	}

	issue, err := client.GetIssue(ctx, "ABC-123", []string{"summary", storyPoints})
	if err != nil {
		log.Fatal(err)
	}

	// As text, whatever shape Jira used for the value.
	fmt.Println(issue.FieldText(storyPoints))

	// Or the exact value. found is false when the field is unset.
	var points float64
	if found, err := issue.DecodeField(storyPoints, &points); err == nil && found == true {
		fmt.Println(points)
	}
}

// Several edits to one issue go in one request: Jira limits writes per issue, and one request is
// applied whole or not at all.
func ExampleClient_UpdateIssue() {
	client := jiraclient.NewClient("https://your-site.atlassian.net", "bot@example.com", "token")

	quiet := false
	err := client.UpdateIssue(context.Background(), "ABC-123", jiraclient.IssueUpdate{
		Summary:      "Crawler returns no results",
		Fields:       map[string]any{"customfield_10004": 3},
		AddLabels:    []string{"triaged"},
		RemoveLabels: []string{"needs-triage"},
		NotifyUsers:  &quiet,
	})
	if err != nil {
		log.Fatal(err)
	}
}

// A workflow that demands a resolution on the way to done takes it with the transition.
func ExampleClient_ApplyTransition() {
	client := jiraclient.NewClient("https://your-site.atlassian.net", "bot@example.com", "token")

	reason, err := jiraclient.TextDoc("Closing as a duplicate of [issue:ABC-10].")
	if err != nil {
		log.Fatal(err)
	}
	err = client.ApplyTransition(context.Background(), "ABC-123", jiraclient.TransitionInput{
		Name:    "Won't Do",
		Comment: reason,
		Fields:  map[string]any{"resolution": map[string]string{"name": "Duplicate"}},
	})
	if err != nil {
		log.Fatal(err)
	}
}

// A value that is not a constant in the source is quoted, so a quote inside it cannot end the string
// early and rewrite the query.
func ExampleJQLQuote() {
	title := `Widget 12" "Pro"`

	fmt.Println("project = ABC AND summary ~ " + jiraclient.JQLQuote(title))
	fmt.Println("labels IN " + jiraclient.JQLList("triaged", "bot"))
	// Output:
	// project = ABC AND summary ~ "Widget 12\" \"Pro\""
	// labels IN ("triaged", "bot")
}
