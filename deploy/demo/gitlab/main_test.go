package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rusik69/opsagent/internal/gitlab"
)

func TestFakeGitLabEndToEnd(t *testing.T) {
	st := newState()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handle(st, w, r)
	}))
	defer srv.Close()

	c := gitlab.New(srv.URL, "demo-token")
	c.DefaultProjectID = "acme/infra"
	c.TargetBranch = "main"
	c.SourceBranchPrefix = "opsagent-fix"
	ctx := context.Background()

	// The full MR-creation flow opsagent performs.
	mr, err := c.OpenMRWithChanges(ctx, "", "", "main",
		"fix: correct nginx worker_processes", "fixes broken.conf", []gitlab.FileChange{
			{Path: "roles/nginx/templates/nginx.conf.j2", Content: "worker_processes 4;\n"},
		})
	if err != nil {
		t.Fatalf("OpenMRWithChanges: %v", err)
	}
	if mr.IID <= 0 || mr.URL == "" {
		t.Fatalf("unexpected MR: %+v", mr)
	}

	iid, ok := gitlab.ParseMRID(mr.URL)
	if !ok {
		t.Fatalf("could not parse MRID from %q", mr.URL)
	}
	if state, err := c.GetMRState(ctx, "acme/infra", iid); err != nil || state != "opened" {
		t.Fatalf("expected opened, got %q (%v)", state, err)
	}

	// Merge via the demo helper, then verify the poller sees it merged.
	resp, err := http.Post(srv.URL+fmt.Sprintf("/_mock/merge/%d", iid), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("merge helper: %d", resp.StatusCode)
	}
	if state, err := c.GetMRState(ctx, "acme/infra", iid); err != nil || state != "merged" {
		t.Fatalf("expected merged, got %q (%v)", state, err)
	}

	// Re-using the same source branch (branch already exists) must not fail.
	mr2, err := c.OpenMRWithChanges(ctx, "", "opsagent-fix-again", "main", "another fix", "desc",
		[]gitlab.FileChange{{Path: "a/b.txt", Content: "x"}})
	if err != nil {
		t.Fatalf("second MR on existing branch: %v", err)
	}
	if mr2.IID <= mr.IID {
		t.Fatalf("expected a new MR iid, got %d (prev %d)", mr2.IID, mr.IID)
	}
}
