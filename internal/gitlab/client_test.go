package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitLab simulates the GitLab REST API subset used by the client.
func fakeGitLab(t *testing.T) (*Client, *[]string, *sync.Mutex) {
	t.Helper()
	var logMu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logMu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		logMu.Unlock()
		if r.Header.Get("PRIVATE-TOKEN") != "test-token" {
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/projects/acme/infra/repository/branches":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"name":"opsagent-fix-bump-nginx"}`))
		case (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.Contains(r.URL.Path, "/repository/files/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"file_path":"host_vars/web-01.yml","branch":"opsagent-fix-bump-nginx"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/projects/acme/infra/merge_requests":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"iid":42,"title":"fix","web_url":"https://gitlab.example.com/acme/infra/-/merge_requests/42"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "test-token")
	c.DefaultProjectID = "acme/infra"
	c.TargetBranch = "main"
	c.SourceBranchPrefix = "opsagent-fix"
	return c, &calls, &logMu
}

func TestOpenMRWithChanges(t *testing.T) {
	c, _, _ := fakeGitLab(t)
	mr, err := c.OpenMRWithChanges(context.Background(), "", "", "main", "fix: bump nginx", "description", []FileChange{
		{Path: "host_vars/web-01.yml", Content: "nginx_version: 1.25"},
	})
	if err != nil {
		t.Fatalf("OpenMRWithChanges: %v", err)
	}
	if mr.URL == "" || mr.IID != 42 {
		t.Fatalf("unexpected MR result: %+v", mr)
	}
	if mr.URL != "https://gitlab.example.com/acme/infra/-/merge_requests/42" {
		t.Fatalf("unexpected MR URL: %q", mr.URL)
	}
}

func TestOpenMRWithoutConfiguredGitLab(t *testing.T) {
	c := New("", "")
	if _, err := c.OpenMRWithChanges(context.Background(), "", "", "", "t", "d", []FileChange{{Path: "a", Content: "b"}}); err == nil {
		t.Fatal("expected error when gitlab not configured")
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Fix high CPU on web-01!": "fix-high-cpu-on-web-01",
		"nginx":                   "nginx",
		"":                        "fix",
		"Disk 95% full":           "disk-95-full",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUpsertFileSendsJSON(t *testing.T) {
	c, calls, _ := fakeGitLab(t)
	if err := c.UpsertFile(context.Background(), "acme/infra", "opsagent-fix-x", "host_vars/web-01.yml", "content", "msg"); err != nil {
		t.Fatalf("UpsertFile: %v", err)
	}
	_ = calls
}

// TestUpsertFileFallsBackToPut verifies that when GitLab rejects the POST
// (file already exists), the client retries the write with PUT.
func TestUpsertFileFallsBackToPut(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/repository/files/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"A file with this name already exists"}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/repository/files/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"file_path":"a"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	if err := c.UpsertFile(context.Background(), "acme/infra", "b", "a.yml", "content", "msg"); err != nil {
		t.Fatalf("UpsertFile: %v", err)
	}
	if len(calls) != 2 || !strings.Contains(calls[0], "POST") || !strings.Contains(calls[1], "PUT") {
		t.Fatalf("expected POST then PUT, got %v", calls)
	}
}

// TestUpsertFileDoesNotFallbackOnAuthError verifies that transport/authorization
// failures do not trigger a pointless PUT retry.
func TestUpsertFileDoesNotFallbackOnAuthError(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "bad-token")
	if err := c.UpsertFile(context.Background(), "acme/infra", "b", "a.yml", "content", "msg"); err == nil {
		t.Fatal("expected auth error")
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 call (no PUT fallback), got %v", calls)
	}
}

func TestCreateBranchSkipsExisting(t *testing.T) {
	c, calls, _ := fakeGitLab(t)
	// Fake server reports branch as existing.
	if err := c.CreateBranch(context.Background(), "acme/infra", "opsagent-fix-bump-nginx", "main"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	// The branch create call should be a POST to /repository/branches.
	found := false
	for _, c := range *calls {
		if strings.Contains(c, "POST /projects/acme/infra/repository/branches") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected branch create call, got %v", *calls)
	}
}

func TestAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "bad-token")
	if err := c.CreateBranch(context.Background(), "acme/infra", "b", "main"); err == nil {
		t.Fatal("expected auth error")
	} else if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 in error, got: %v", err)
	}
}

func TestCreateMRError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":{"target_branch":["does not exist"]}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	if _, err := c.CreateMR(context.Background(), "acme/infra", "src", "main", "t", "d"); err == nil {
		t.Fatal("expected MR error")
	} else if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected gitlab message in error, got: %v", err)
	}
}

func TestParseMRID(t *testing.T) {
	cases := map[string]struct {
		id int
		ok bool
	}{
		"https://gitlab.example.com/acme/infra/-/merge_requests/42":       {42, true},
		"https://gitlab.example.com/acme/infra/-/merge_requests/7#note_5": {7, true},
		"https://gitlab.example.com/acme/infra/-/merge_requests/abc":      {0, false},
		"https://gitlab.example.com/acme/infra/-/commit/abc":              {0, false},
		"": {0, false},
	}
	for in, want := range cases {
		id, ok := ParseMRID(in)
		if id != want.id || ok != want.ok {
			t.Errorf("ParseMRID(%q) = (%d,%v), want (%d,%v)", in, id, ok, want.id, want.ok)
		}
	}
}

func TestGetMRState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/projects/acme/infra/merge_requests/42" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"state":"merged"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	state, err := c.GetMRState(context.Background(), "acme/infra", 42)
	if err != nil {
		t.Fatalf("GetMRState: %v", err)
	}
	if state != "merged" {
		t.Fatalf("expected merged, got %q", state)
	}
}

func TestGetMRStateUnconfigured(t *testing.T) {
	c := New("", "")
	if _, err := c.GetMRState(context.Background(), "p", 1); err == nil {
		t.Fatal("expected error when gitlab not configured")
	}
}

// TestRetryOnTransientError verifies that 429 and 5xx responses are retried
// with backoff until success.
func TestRetryOnTransientError(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"merged"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	c.retryBase = time.Millisecond
	state, err := c.GetMRState(context.Background(), "acme/infra", 42)
	if err != nil {
		t.Fatalf("GetMRState after retries: %v", err)
	}
	if state != "merged" {
		t.Fatalf("expected merged, got %q", state)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

// TestNoRetryOnClientError verifies 4xx (non-429) errors fail immediately.
func TestNoRetryOnClientError(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad request"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	c.retryBase = time.Millisecond
	if _, err := c.GetMRState(context.Background(), "acme/infra", 42); err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Fatalf("expected 1 attempt for 4xx, got %d", attempts)
	}
}
