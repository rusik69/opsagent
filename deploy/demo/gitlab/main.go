// Command mockgitlab is an in-memory fake of the subset of the GitLab REST
// API that opsagent's gitlab client uses. It lets the k8s demo exercise the
// full "create MR -> poll for merge -> resolved via mr_merged" loop without a
// real GitLab. All state is lost on restart.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

type mr struct {
	IID    int    `json:"iid"`
	Title  string `json:"title"`
	Source string `json:"source_branch"`
	Target string `json:"target_branch"`
	State  string `json:"state"`
	WebURL string `json:"web_url"`
}

type state struct {
	mu       sync.Mutex
	branches map[string]bool   // project+branch
	files    map[string]string // project+branch+path -> content
	mrs      map[int]*mr
	nextIID  int
}

func newState() *state {
	return &state{
		branches: map[string]bool{},
		files:    map[string]string{},
		mrs:      map[int]*mr{},
		nextIID:  1,
	}
}

func main() {
	addr := flag.String("listen", ":8080", "HTTP listen address")
	flag.Parse()
	st := newState()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handle(st, w, r)
	})
	log.Printf("mockgitlab listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("mockgitlab: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"message": msg})
}

// handle dispatches on the escaped path so project paths like acme%2Finfra
// stay a single segment.
func handle(st *state, w http.ResponseWriter, r *http.Request) {
	seg := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
	if len(seg) == 0 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if seg[0] == "_mock" {
		handleMock(st, w, r, seg)
		return
	}
	if seg[0] == "projects" {
		handleProjects(st, w, r, seg)
		return
	}
	writeErr(w, http.StatusNotFound, "not found")
}

// handleMock serves demo helper endpoints (not part of the GitLab API).
func handleMock(st *state, w http.ResponseWriter, r *http.Request, seg []string) {
	if !(r.Method == http.MethodPost && len(seg) == 3 && seg[1] == "merge") {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	iid, err := strconv.Atoi(seg[2])
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad iid")
		return
	}
	st.mu.Lock()
	m, ok := st.mrs[iid]
	if ok {
		m.State = "merged"
	}
	st.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "mr not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"iid": iid, "state": "merged"})
}

func handleProjects(st *state, w http.ResponseWriter, r *http.Request, seg []string) {
	if len(seg) < 2 {
		writeErr(w, http.StatusBadRequest, "project id required")
		return
	}
	proj, err := url.PathUnescape(seg[1])
	if err != nil || proj == "" {
		writeErr(w, http.StatusBadRequest, "bad project id")
		return
	}
	rest := seg[2:]
	switch {
	case len(rest) >= 2 && rest[0] == "repository" && rest[1] == "branches":
		handleBranches(st, w, r, proj, rest[2:])
	case len(rest) >= 2 && rest[0] == "repository" && rest[1] == "files":
		handleFiles(st, w, r, proj, rest[2:])
	case len(rest) >= 1 && rest[0] == "merge_requests":
		handleMRs(st, w, r, proj, rest[1:])
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func handleBranches(st *state, w http.ResponseWriter, r *http.Request, proj string, rest []string) {
	switch r.Method {
	case http.MethodGet:
		if len(rest) < 1 {
			writeErr(w, http.StatusBadRequest, "branch required")
			return
		}
		br, _ := url.PathUnescape(rest[0])
		st.mu.Lock()
		exists := st.branches[proj+"/"+br]
		st.mu.Unlock()
		if !exists {
			writeErr(w, http.StatusNotFound, "Branch does not exist")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": br})
	case http.MethodPost:
		var body struct {
			Branch string `json:"branch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Branch == "" {
			writeErr(w, http.StatusBadRequest, "branch is required")
			return
		}
		st.mu.Lock()
		st.branches[proj+"/"+body.Branch] = true
		st.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"name": body.Branch})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleFiles(st *state, w http.ResponseWriter, r *http.Request, proj string, rest []string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if len(rest) < 1 {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	path, err := url.PathUnescape(rest[0])
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad path")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad body")
		return
	}
	var body struct {
		Branch  string `json:"branch"`
		Content string `json:"content"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.Branch == "" {
		writeErr(w, http.StatusBadRequest, "branch is required")
		return
	}
	key := proj + "/" + body.Branch + "/" + path
	st.mu.Lock()
	if r.Method == http.MethodPost {
		if _, exists := st.files[key]; exists {
			st.mu.Unlock()
			writeErr(w, http.StatusBadRequest, "A file with this name already exists")
			return
		}
	}
	st.files[key] = body.Content
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"file_path": path, "branch": body.Branch})
}

func handleMRs(st *state, w http.ResponseWriter, r *http.Request, proj string, rest []string) {
	switch r.Method {
	case http.MethodPost:
		var body struct {
			SourceBranch string `json:"source_branch"`
			TargetBranch string `json:"target_branch"`
			Title        string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.SourceBranch == "" || body.Title == "" {
			writeErr(w, http.StatusBadRequest, "source_branch and title are required")
			return
		}
		st.mu.Lock()
		m := &mr{
			IID:    st.nextIID,
			Title:  body.Title,
			Source: body.SourceBranch,
			Target: body.TargetBranch,
			State:  "opened",
			WebURL: "http://gitlab/projects/" + proj + "/merge_requests/" + strconv.Itoa(st.nextIID),
		}
		st.mrs[m.IID] = m
		st.nextIID++
		st.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"iid": m.IID, "title": m.Title, "web_url": m.WebURL, "state": m.State})
	case http.MethodGet:
		if len(rest) < 1 {
			writeErr(w, http.StatusBadRequest, "iid required")
			return
		}
		iid, err := strconv.Atoi(rest[0])
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad iid")
			return
		}
		st.mu.Lock()
		m, ok := st.mrs[iid]
		st.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "MR not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"state": m.State, "title": m.Title})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
