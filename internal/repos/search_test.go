package repos

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rusik69/opsagent/internal/config"
)

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	// ansible-style repo
	if err := os.MkdirAll(filepath.Join(root, "ansible", "host_vars"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ansible", "host_vars", "web-01.yml"),
		[]byte("---\nnginx_port: 8080\nrole: web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ansible", "hosts.ini"),
		[]byte("[webservers]\nweb-01 ansible_host=10.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// docs repo
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "nginx.md"),
		[]byte("# nginx\nSee error.log first when troubleshooting nginx_port mismatches.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager([]config.RepoConfig{
		{Name: "ansible", Type: "ansible", Path: filepath.Join(root, "ansible")},
		{Name: "docs", Type: "docs", Path: filepath.Join(root, "docs")},
	}, "./data/repos")
	if err != nil {
		t.Fatal(err)
	}
	return m, root
}

func TestSearch(t *testing.T) {
	m, _ := newTestManager(t)
	matches := m.Search("nginx_port", 10)
	if len(matches) == 0 {
		t.Fatal("expected search matches")
	}
	ok := false
	for _, mt := range matches {
		if mt.Repo == "ansible" && strings.Contains(mt.Content, "nginx_port") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("expected nginx_port match in ansible repo, got %+v", matches)
	}
}

func TestDocsSearchOnlyDocs(t *testing.T) {
	m, _ := newTestManager(t)
	// "error.log" only appears in the docs repo.
	matches := m.DocsSearch("error.log", 10)
	if len(matches) == 0 {
		t.Fatal("expected docs match")
	}
	for _, mt := range matches {
		if mt.Repo != "docs" {
			t.Fatalf("DocsSearch returned non-docs repo %q", mt.Repo)
		}
	}
}

func TestHostConfig(t *testing.T) {
	m, _ := newTestManager(t)
	out := m.HostConfig("web-01")
	if !strings.Contains(out, "web-01") || !strings.Contains(out, "nginx_port") {
		t.Fatalf("expected host config with inventory + host_vars, got:\n%s", out)
	}
}

func TestHostConfigNoMatch(t *testing.T) {
	m, _ := newTestManager(t)
	out := m.HostConfig("does-not-exist")
	if !strings.Contains(out, "no configuration") {
		t.Fatalf("expected no-config message, got:\n%s", out)
	}
}

func TestFileSnippet(t *testing.T) {
	m, _ := newTestManager(t)
	content, err := m.FileSnippet("ansible", "host_vars/web-01.yml", 1024)
	if err != nil {
		t.Fatalf("FileSnippet: %v", err)
	}
	if !strings.Contains(content, "nginx_port") {
		t.Fatalf("unexpected snippet: %q", content)
	}
}

func TestFileSnippetRejectsTraversal(t *testing.T) {
	m, root := newTestManager(t)
	// A secret file outside the repo.
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("topsecret"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"../../secret.txt",
		"host_vars/../../secret.txt",
		"../../../etc/passwd",
		"/etc/passwd",
		"",
	}
	for _, p := range bad {
		if _, err := m.FileSnippet("ansible", p, 1024); err == nil {
			t.Errorf("FileSnippet(%q) should have been rejected", p)
		}
	}
}

func TestFileSnippetRejectsSymlinkEscape(t *testing.T) {
	m, root := newTestManager(t)
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("topsecret"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Symlink inside repo pointing outside.
	if err := os.Symlink(secret, filepath.Join(root, "ansible", "escape.yml")); err != nil {
		t.Skip("symlink not supported")
	}
	if _, err := m.FileSnippet("ansible", "escape.yml", 1024); err == nil {
		t.Error("expected symlink escape to be rejected")
	}
}

func TestFileSnippetUnknownRepo(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.FileSnippet("nope", "x", 1024); err == nil {
		t.Fatal("expected error for unknown repo")
	}
}

func TestReposListAndSyncLocal(t *testing.T) {
	m, _ := newTestManager(t)
	repos := m.Repos()
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(repos))
	}
	out, err := m.Sync(t.Context())
	if err != nil {
		t.Fatalf("sync local repos: %v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("expected local ok in sync output, got %q", out)
	}
}

func TestSyncOneUnknown(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.SyncOne(t.Context(), "nope"); err == nil {
		t.Fatal("expected error for unknown repo")
	}
}
