package repos

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Match is a single search hit across the config repos.
type Match struct {
	Repo    string `json:"repo"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Content string `json:"content"`
}

const maxFileSize = 1 << 20 // 1 MiB

func isSkippablePath(p string) bool {
	base := filepath.Base(p)
	switch {
	case base == ".git", base == ".hg", base == ".svn", base == "node_modules":
		return true
	case strings.HasPrefix(p, ".git/"):
		return true
	case strings.HasSuffix(base, ".pyc"), strings.HasSuffix(base, ".o"), strings.HasSuffix(base, ".so"):
		return true
	}
	return false
}

// Search walks all repos and returns file:line matches for query.
func (m *Manager) Search(query string, maxResults int) []Match {
	if maxResults <= 0 {
		maxResults = 50
	}
	q := strings.ToLower(query)
	out := []Match{}
	for _, r := range m.Repos() {
		_ = filepath.WalkDir(r.Root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if isSkippablePath(d.Name()) && path != r.Root {
					return filepath.SkipDir
				}
				return nil
			}
			if isSkippablePath(path) || len(out) >= maxResults {
				return nil
			}
			if strings.HasSuffix(d.Name(), ".git") || strings.HasSuffix(d.Name(), ".png") ||
				strings.HasSuffix(d.Name(), ".jpg") || strings.HasSuffix(d.Name(), ".gz") {
				return nil
			}
			if fi, err := d.Info(); err != nil || fi.Size() > maxFileSize {
				return nil
			}
			if m.matchFile(path, q, r.Name, maxResults, &out) {
				return filepath.SkipDir
			}
			return nil
		})
		if len(out) >= maxResults {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// matchFile returns true when it reached the result cap while scanning the file.
func (m *Manager) matchFile(path, q, repoName string, max int, out *[]Match) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	rel := relPath(repoName, path)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if strings.Contains(strings.ToLower(text), q) {
			content := strings.TrimSpace(text)
			if len(content) > 300 {
				content = content[:300]
			}
			*out = append(*out, Match{Repo: repoName, Path: rel, Line: line, Content: content})
			if len(*out) >= max {
				return true
			}
		}
	}
	return false
}

func relPath(repo, path string) string {
	if strings.HasPrefix(path, repo+string(filepath.Separator)) {
		return strings.TrimPrefix(path, repo+string(filepath.Separator))
	}
	return filepath.Base(path)
}

// DocsSearch searches only repos of type "docs" (documentation). It returns
// file:line matches suitable for feeding the agent.
func (m *Manager) DocsSearch(query string, maxResults int) []Match {
	if maxResults <= 0 {
		maxResults = 50
	}
	q := strings.ToLower(query)
	out := []Match{}
	for _, r := range m.Repos() {
		if r.Type != "docs" {
			continue
		}
		_ = filepath.WalkDir(r.Root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if isSkippablePath(d.Name()) && path != r.Root {
					return filepath.SkipDir
				}
				return nil
			}
			if isSkippablePath(path) || len(out) >= maxResults {
				return nil
			}
			if fi, err := d.Info(); err != nil || fi.Size() > maxFileSize {
				return nil
			}
			if m.matchFile(path, q, r.Name, maxResults, &out) {
				return filepath.SkipDir
			}
			return nil
		})
		if len(out) >= maxResults {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// HostConfig looks up configuration relevant to a host across the repos and
// returns a human-readable summary. It covers:
//   - Ansible: inventory entries, host_vars/<host>.yml, and references to the
//     host in playbooks/roles.
//   - Puppet: hieradata host files, nodes/<host>.pp, and manifest references.
func (m *Manager) HostConfig(host string) string {
	var sb strings.Builder
	host = strings.TrimSpace(host)
	if host == "" {
		return "no host given"
	}
	for _, r := range m.Repos() {
		hits := m.SearchIn(r.Name, host, 25)
		hits = append(hits, m.hostNamedFiles(r, host)...)
		if len(hits) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "== repo %s (%s) ==\n", r.Name, r.Type)
		seen := map[string]bool{}
		for _, h := range hits {
			key := h.Path
			if !seen[key] {
				fmt.Fprintf(&sb, "[%s]\n", h.Path)
				seen[key] = true
			}
			fmt.Fprintf(&sb, "  %d: %s\n", h.Line, h.Content)
		}
		sb.WriteString("\n")
	}
	if sb.Len() == 0 {
		return fmt.Sprintf("no configuration found for host %q in the configured repos", host)
	}
	return strings.TrimSuffix(sb.String(), "\n\n")
}

// hostNamedFiles returns files whose base name (without extension) equals the
// host, e.g. ansible host_vars/web-01.yml or puppet nodes/web-01.pp.
func (m *Manager) hostNamedFiles(r *Repo, host string) []Match {
	var out []Match
	_ = filepath.WalkDir(r.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if isSkippablePath(d.Name()) && path != r.Root {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		base := strings.TrimSuffix(name, filepath.Ext(name))
		if !strings.EqualFold(base, host) {
			return nil
		}
		if fi, err := d.Info(); err != nil || fi.Size() > maxFileSize {
			return nil
		}
		content, err := readFirstLines(path, 50)
		if err != nil {
			return nil
		}
		for i, line := range content {
			out = append(out, Match{Repo: r.Name, Path: relPath(r.Name, path), Line: i + 1, Content: line})
			if len(out) >= 25 {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return out
}

func readFirstLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := []string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() && len(out) < n {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

// SearchIn searches a single repo by name.
func (m *Manager) SearchIn(name, query string, maxResults int) []Match {
	r := m.find(name)
	if r == nil {
		return nil
	}
	if maxResults <= 0 {
		maxResults = 50
	}
	q := strings.ToLower(query)
	var out []Match
	_ = filepath.WalkDir(r.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if isSkippablePath(d.Name()) && path != r.Root {
				return filepath.SkipDir
			}
			return nil
		}
		if isSkippablePath(path) || len(out) >= maxResults {
			return nil
		}
		if fi, err := d.Info(); err != nil || fi.Size() > maxFileSize {
			return nil
		}
		if m.matchFile(path, q, r.Name, maxResults, &out) {
			return filepath.SkipDir
		}
		return nil
	})
	return out
}

// FileSnippet returns the full contents of a repo file (capped), useful for
// the agent to read a specific config file found by search. The path must
// resolve to a file inside the repo root; traversal attempts are rejected.
func (m *Manager) FileSnippet(repoName, path string, maxBytes int) (string, error) {
	r := m.find(repoName)
	if r == nil {
		return "", fmt.Errorf("repo %q not found", repoName)
	}
	full, err := safeJoin(r.Root, filepath.FromSlash(path))
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(full); err != nil || fi.IsDir() {
		return "", fmt.Errorf("file %s not found in repo %s", path, repoName)
	}
	if maxBytes <= 0 {
		maxBytes = 64 * 1024
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if len(data) > maxBytes {
		data = data[:maxBytes]
	}
	return string(bytes.TrimSpace(data)), nil
}

// safeJoin joins root and rel and rejects any path that escapes root,
// including symlinks that point outside of it. Both sides are symlink-resolved
// before comparison so container/temp prefixes (e.g. /var -> /private/var on
// macOS) do not cause false rejections.
func safeJoin(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	clean := filepath.Clean(filepath.Join(root, rel))
	if !strings.HasPrefix(clean, root+string(filepath.Separator)) && clean != root {
		return "", fmt.Errorf("path escapes repo root")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = filepath.Clean(root)
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		if !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) && resolved != resolvedRoot {
			return "", fmt.Errorf("path escapes repo root via symlink")
		}
	}
	return clean, nil
}
