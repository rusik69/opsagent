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
	mu    sync.Mutex
	repos []*Repo
}

func NewManager(repos []config.RepoConfig, cacheDir string) (*Manager, error) {
	if cacheDir == "" {
		cacheDir = "./data/repos"
	}
	m := &Manager{}
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

func (m *Manager) find(name string) *Repo {
	for _, r := range m.repos {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// Sync clones missing git repos or pulls existing ones. Local-path repos are
// validated to exist. Returns a summary string.
func (m *Manager) Sync(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var lines []string
	for _, r := range m.repos {
		if r.URL != "" {
			if _, err := os.Stat(filepath.Join(r.Root, ".git")); err == nil {
				cmd := exec.CommandContext(ctx, "git", "-C", r.Root, "pull", "--ff-only")
				cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
				if out, err := cmd.CombinedOutput(); err != nil {
					lines = append(lines, fmt.Sprintf("repo %s: pull failed: %s", r.Name, strings.TrimSpace(string(out))))
				} else {
					lines = append(lines, fmt.Sprintf("repo %s: pulled", r.Name))
				}
			} else {
				args := []string{"clone", "--depth", "1"}
				if r.Branch != "" {
					args = append(args, "--branch", r.Branch)
				}
				args = append(args, r.URL, r.Root)
				cmd := exec.CommandContext(ctx, "git", args...)
				cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
				if out, err := cmd.CombinedOutput(); err != nil {
					lines = append(lines, fmt.Sprintf("repo %s: clone failed: %s", r.Name, strings.TrimSpace(string(out))))
				} else {
					lines = append(lines, fmt.Sprintf("repo %s: cloned", r.Name))
				}
			}
		} else {
			if fi, err := os.Stat(r.Root); err != nil || !fi.IsDir() {
				lines = append(lines, fmt.Sprintf("repo %s: local path %s not found", r.Name, r.Root))
			} else {
				lines = append(lines, fmt.Sprintf("repo %s: ok (%s)", r.Name, r.Root))
			}
		}
	}
	return strings.Join(lines, "\n"), nil
}

// SyncOne syncs a single repo by name.
func (m *Manager) SyncOne(ctx context.Context, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.find(name)
	if r == nil {
		return "", fmt.Errorf("repo %q not found", name)
	}
	if r.URL != "" {
		if _, err := os.Stat(filepath.Join(r.Root, ".git")); err == nil {
			cmd := exec.CommandContext(ctx, "git", "-C", r.Root, "pull", "--ff-only")
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
		if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
			return "", fmt.Errorf("clone: %s", strings.TrimSpace(string(out)))
		}
		return "cloned", nil
	}
	return "local path (no sync needed)", nil
}

// LastSync returns the mtime of the newest cloned/pulled repo, zero if none.
func (m *Manager) LastSync() time.Time {
	var latest time.Time
	for _, r := range m.repos {
		fi, err := os.Stat(r.Root)
		if err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest
}
