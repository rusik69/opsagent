package repos

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rusik69/opsagent/internal/config"
)

type Repo struct {
	Name   string
	Type   string // puppet | ansible | generic
	Root   string // absolute local path
	URL    string
	Branch string
}

func (r *Repo) String() string { return r.Name }

// Manager owns the set of Puppet/Ansible/generic config repos.
type Manager struct {
	mu      sync.Mutex
	repos   []*Repo
	syncing map[string]bool
	// fileLists caches the searchable files of each repo to avoid re-walking
	// the tree on every query; invalidated when the repo is synced.
	fileLists map[string]cachedFiles
}

func NewManager(repos []config.RepoConfig, cacheDir string) (*Manager, error) {
	if cacheDir == "" {
		cacheDir = "./data/repos"
	}
	m := &Manager{syncing: map[string]bool{}, fileLists: map[string]cachedFiles{}}
	for _, rc := range repos {
		repo := &Repo{Name: rc.Name, Type: rc.Type, URL: rc.URL, Branch: rc.Branch}
		switch {
		case rc.Path != "":
			abs, err := filepath.Abs(rc.Path)
			if err != nil {
				return nil, err
			}
			repo.Root = abs
		case rc.URL != "":
			name := rc.Name
			if name == "" {
				name = strings.TrimSuffix(filepath.Base(rc.URL), ".git")
			}
			repo.Root = filepath.Join(cacheDir, sanitize(name))
		default:
			return nil, fmt.Errorf("repo %q: must set url or path", rc.Name)
		}
		m.repos = append(m.repos, repo)
	}
	return m, nil
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '-' || r == '_':
			return r
		}
		return '_'
	}, s)
}

// Repos returns a snapshot of managed repos.
func (m *Manager) Repos() []*Repo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Repo, len(m.repos))
	copy(out, m.repos)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// snapshot returns a copy of the repo list without locking callers for the
// duration of slow git operations.
func (m *Manager) snapshot() []*Repo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Repo, len(m.repos))
	copy(out, m.repos)
	return out
}

func (m *Manager) find(name string) *Repo {
	for _, r := range m.repos {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// beginSync marks a repo as syncing, returning false if a sync is already in
// flight for it. This prevents overlapping git operations on the same checkout.
func (m *Manager) beginSync(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.syncing[name] {
		return false
	}
	m.syncing[name] = true
	return true
}

func (m *Manager) endSync(name string) {
	m.mu.Lock()
	delete(m.syncing, name)
	// The checkout changed; the file list cache is stale.
	delete(m.fileLists, name)
	m.mu.Unlock()
}

// Sync clones missing git repos or pulls existing ones. Local-path repos are
// validated to exist. Returns a summary string. The manager lock is not held
// while git runs, so repository reads are never blocked by a slow sync.
func (m *Manager) Sync(ctx context.Context) (string, error) {
	var lines []string
	for _, r := range m.snapshot() {
		if !m.beginSync(r.Name) {
			lines = append(lines, fmt.Sprintf("repo %s: sync already in progress", r.Name))
			continue
		}
		lines = append(lines, m.syncRepo(ctx, r))
		m.endSync(r.Name)
	}
	return strings.Join(lines, "\n"), nil
}

// SyncOne syncs a single repo by name.
func (m *Manager) SyncOne(ctx context.Context, name string) (string, error) {
	r := m.find(name)
	if r == nil {
		return "", fmt.Errorf("repo %q not found", name)
	}
	if !m.beginSync(name) {
		return "", fmt.Errorf("repo %q: sync already in progress", name)
	}
	defer m.endSync(name)
	if r.URL == "" {
		return "local path (no sync needed)", nil
	}
	if _, err := os.Stat(filepath.Join(r.Root, ".git")); err == nil {
		cmd := exec.CommandContext(ctx, "git", "-C", r.Root, "pull", "--ff-only")
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("pull: %s", strings.TrimSpace(string(out)))
		}
		return "pulled", nil
	}
	args := []string{"clone", "--depth", "1"}
	if r.Branch != "" {
		args = append(args, "--branch", r.Branch)
	}
	args = append(args, r.URL, r.Root)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("clone: %s", strings.TrimSpace(string(out)))
	}
	return "cloned", nil
}

// syncRepo performs the actual git work for a single repo without holding the
// manager lock, returning a human-readable summary line.
func (m *Manager) syncRepo(ctx context.Context, r *Repo) string {
	if r.URL == "" {
		if fi, err := os.Stat(r.Root); err != nil || !fi.IsDir() {
			return fmt.Sprintf("repo %s: local path %s not found", r.Name, r.Root)
		}
		return fmt.Sprintf("repo %s: ok (%s)", r.Name, r.Root)
	}
	if _, err := os.Stat(filepath.Join(r.Root, ".git")); err == nil {
		cmd := exec.CommandContext(ctx, "git", "-C", r.Root, "pull", "--ff-only")
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Sprintf("repo %s: pull failed: %s", r.Name, strings.TrimSpace(string(out)))
		}
		return fmt.Sprintf("repo %s: pulled", r.Name)
	}
	args := []string{"clone", "--depth", "1"}
	if r.Branch != "" {
		args = append(args, "--branch", r.Branch)
	}
	args = append(args, r.URL, r.Root)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("repo %s: clone failed: %s", r.Name, strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("repo %s: cloned", r.Name)
}

// LastSync returns the mtime of the newest cloned/pulled repo, zero if none.
func (m *Manager) LastSync() time.Time {
	var latest time.Time
	for _, r := range m.snapshot() {
		fi, err := os.Stat(r.Root)
		if err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest
}
