package jiraclient

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// liveSchemeResponse is a /rest/api/3/priority/search body from a site whose scheme was customised
// after creation: "Normal" was added later, so it carries a 10000-series ID while ranking third.
const liveSchemeResponse = `{"isLast":true,"values":[
	{"id":"1","name":"Blocker"},
	{"id":"2","name":"Critical"},
	{"id":"3","name":"Major"},
	{"id":"10000","name":"Normal"},
	{"id":"4","name":"Minor"},
	{"id":"5","name":"Trivial"}
]}`

// The plain /rest/api/3/priority endpoint is deprecated, so the client must be on /priority/search.
func TestPriorities_UsesTheNonDeprecatedEndpoint(t *testing.T) {
	var requestedPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, liveSchemeResponse)
	})

	if _, err := client.Priorities(context.Background(), PriorityQuery{}); err != nil {
		t.Fatalf("priorities: %v", err)
	}
	if requestedPath != "/rest/api/3/priority/search" {
		t.Errorf("got %q, want the /priority/search endpoint", requestedPath)
	}
}

// Priorities are per-scheme on Jira Cloud, so a project scopes the lookup to the set that applies.
func TestPriorities_ScopesByProject(t *testing.T) {
	var requestedQuery string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestedQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, liveSchemeResponse)
	})

	if _, err := client.Priorities(context.Background(), PriorityQuery{ProjectID: "10001"}); err != nil {
		t.Fatalf("priorities: %v", err)
	}
	if strings.Contains(requestedQuery, "projectId=10001") == false {
		t.Errorf("project not passed through: %s", requestedQuery)
	}
}

func TestPriorities_FollowsPagination(t *testing.T) {
	var calls int
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("startAt") == "0" {
			_, _ = io.WriteString(w, `{"isLast":false,"values":[{"id":"1","name":"Blocker"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"isLast":true,"values":[{"id":"2","name":"Critical"}]}`)
	})

	priorities, err := client.Priorities(context.Background(), PriorityQuery{})
	if err != nil {
		t.Fatalf("priorities: %v", err)
	}
	if calls != 2 {
		t.Errorf("got %d requests, want 2", calls)
	}
	if len(priorities) != 2 || priorities[1].Name != "Critical" || priorities[1].Rank != 1 {
		t.Errorf("pages not accumulated in order: %+v", priorities)
	}
}

func TestPriorities_RanksInResponseOrderNotByID(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, liveSchemeResponse)
	})

	priorities, err := client.Priorities(context.Background(), PriorityQuery{})
	if err != nil {
		t.Fatalf("priorities: %v", err)
	}

	want := []string{"Blocker", "Critical", "Major", "Normal", "Minor", "Trivial"}
	if len(priorities) != len(want) {
		t.Fatalf("got %d priorities, want %d", len(priorities), len(want))
	}
	for i, name := range want {
		if priorities[i].Name != name {
			t.Errorf("rank %d: got %q, want %q", i, priorities[i].Name, name)
		}
		if priorities[i].Rank != i {
			t.Errorf("%s: got rank %d, want %d", name, priorities[i].Rank, i)
		}
	}
}

// The whole reason this endpoint is called rather than inferred: Normal outranks Minor and Trivial
// despite an ID that sorts after both.
func TestPriorityRanks_NormalOutranksMinorDespiteHigherID(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, liveSchemeResponse)
	})

	ranks, err := client.PriorityRanks(context.Background(), PriorityQuery{})
	if err != nil {
		t.Fatalf("priority ranks: %v", err)
	}

	if ranks["Normal"] >= ranks["Minor"] {
		t.Errorf("Normal (rank %d) must outrank Minor (rank %d)", ranks["Normal"], ranks["Minor"])
	}
	if ranks["Normal"] >= ranks["Trivial"] {
		t.Errorf("Normal (rank %d) must outrank Trivial (rank %d)", ranks["Normal"], ranks["Trivial"])
	}
	if ranks["Blocker"] != 0 {
		t.Errorf("Blocker must be the most urgent, got rank %d", ranks["Blocker"])
	}
	if len(ranks) != 6 {
		t.Errorf("got %d ranks, want 6", len(ranks))
	}
}

func TestPriorities_EmptySchemeIsAnError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"isLast":true,"values":[]}`)
	})

	if _, err := client.Priorities(context.Background(), PriorityQuery{}); err == nil {
		t.Fatal("an empty scheme must be an error, not an empty ranking")
	}
}

// Reads must keep working on a dry client — a dry run has to be able to rank.
func TestPriorityRanks_WorksOnADryClient(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, liveSchemeResponse)
	}, WithDryRun(true))

	ranks, err := client.PriorityRanks(context.Background(), PriorityQuery{})
	if err != nil {
		t.Fatalf("priority ranks on dry client: %v", err)
	}
	if ranks["Major"] != 2 {
		t.Errorf("got rank %d for Major, want 2", ranks["Major"])
	}
}
