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
	"time"
)

// Match is a single search hit across the config repos.
type Match struct {
	Repo    string `json:"repo"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Content string `json:"content"`
	// Context holds the matched line plus a few surrounding lines.
	Context string `json:"context,omitempty"`
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

// cachedFiles is a repo file-list cache entry.
type cachedFiles struct {
	files   []string
	builtAt time.Time
}

// fileCacheTTL bounds how long a repo file list is reused between syncs.
const fileCacheTTL = 10 * time.Minute

// searchableFiles returns the searchable files of a repo, caching the walk so
// repeated queries do not re-traverse the tree.
func (m *Manager) searchableFiles(r *Repo) []string {
	m.mu.Lock()
	cf, ok := m.fileLists[r.Name]
	m.mu.Unlock()
	if ok && time.Since(cf.builtAt) < fileCacheTTL {
		return cf.files
	}
	files := m.walkFiles(r)
	m.mu.Lock()
	m.fileLists[r.Name] = cachedFiles{files: files, builtAt: time.Now()}
	m.mu.Unlock()
	return files
}

// walkFiles lists files under r.Root that are eligible for search.
func (m *Manager) walkFiles(r *Repo) []string {
	files := []string{}
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
		if isSkippablePath(path) {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".git") || strings.HasSuffix(name, ".png") ||
			strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".gz") {
			return nil
		}
		if fi, err := d.Info(); err != nil || fi.Size() > maxFileSize {
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files
}

// Search walks all repos and returns file:line matches for query.
func (m *Manager) Search(query string, maxResults int) []Match {
	if maxResults <= 0 {
		maxResults = 50
	}
	q := strings.ToLower(query)
	out := []Match{}
	for _, r := range m.Repos() {
		for _, path := range m.searchableFiles(r) {
			if len(out) >= maxResults {
				break
			}
			if m.matchFile(path, q, r.Name, maxResults, &out) {
				break
			}
		}
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

// contextLines is how many lines of surrounding context a match carries.
const contextLines = 2

// matchFile returns true when it reached the result cap while scanning the file.
func (m *Manager) matchFile(path, q, repoName string, max int, out *[]Match) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	rel := relPath(repoName, path)
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if sc.Err() != nil {
		return false
	}
	for i, text := range lines {
		if !strings.Contains(strings.ToLower(text), q) {
			continue
		}
		content := strings.TrimSpace(text)
		if len(content) > 300 {
			content = content[:300]
		}
		*out = append(*out, Match{
			Repo:    repoName,
			Path:    rel,
			Line:    i + 1,
			Content: content,
			Context: contextAround(lines, i, contextLines),
		})
		if len(*out) >= max {
			return true
		}
	}
	return false
}

// contextAround returns the trimmed text of the n lines surrounding index idx.
func contextAround(lines []string, idx, n int) string {
	lo := idx - n
	if lo < 0 {
		lo = 0
	}
	hi := idx + n
	if hi >= len(lines) {
		hi = len(lines) - 1
	}
	parts := make([]string, 0, hi-lo+1)
	for j := lo; j <= hi; j++ {
		t := strings.TrimSpace(lines[j])
		if len(t) > 300 {
			t = t[:300]
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, "\n")
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
		for _, path := range m.searchableFiles(r) {
			if len(out) >= maxResults {
				break
			}
			if m.matchFile(path, q, r.Name, maxResults, &out) {
				break
			}
		}
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
	for _, path := range m.searchableFiles(r) {
		if len(out) >= 25 {
			break
		}
		name := filepath.Base(path)
		base := strings.TrimSuffix(name, filepath.Ext(name))
		if !strings.EqualFold(base, host) {
			continue
		}
		content, err := readFirstLines(path, 50)
		if err != nil {
			continue
		}
		for i, line := range content {
			out = append(out, Match{Repo: r.Name, Path: relPath(r.Name, path), Line: i + 1, Content: line})
			if len(out) >= 25 {
				break
			}
		}
	}
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
	for _, path := range m.searchableFiles(r) {
		if len(out) >= maxResults {
			break
		}
		if m.matchFile(path, q, r.Name, maxResults, &out) {
			break
		}
	}
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
