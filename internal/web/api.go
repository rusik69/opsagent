package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rusik69/opsagent/internal/incidents"
	"github.com/rusik69/opsagent/internal/model"
)

// Incident intake ---------------------------------------------------------

func (s *Server) handleAPICreateIncident(w http.ResponseWriter, r *http.Request) {
	var g incidents.GenericIncident
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		g.Host = r.Form.Get("host")
		g.Title = r.Form.Get("title")
		g.Severity = r.Form.Get("severity")
		g.Message = r.Form.Get("message")
		g.Source = r.Form.Get("source")
		g.ExternalID = r.Form.Get("external_id")
		if g.Labels == nil {
			g.Labels = map[string]string{}
		}
		for k, vs := range r.Form {
			if len(vs) > 0 {
				g.Labels[k] = vs[0]
			}
		}
		delete(g.Labels, "host")
		delete(g.Labels, "title")
		delete(g.Labels, "severity")
		delete(g.Labels, "message")
		delete(g.Labels, "source")
		delete(g.Labels, "external_id")
	} else {
		if err := readJSON(r, &g); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
	}
	inc, err := s.incidents.CreateGeneric(r.Context(), g)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		http.Redirect(w, r, "/incidents", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusCreated, inc)
}

func (s *Server) handleAPIAlertmanager(w http.ResponseWriter, r *http.Request) {
	var payload incidents.AlertmanagerPayload
	if err := readJSON(r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	created, err := s.incidents.CreateAlertmanager(r.Context(), payload)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"created": len(created), "incidents": created})
}

func (s *Server) handleAPIListIncidents(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	severity := r.URL.Query().Get("severity")
	query := r.URL.Query().Get("q")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	incs, err := s.store.ListIncidentsFiltered(r.Context(), status, severity, query, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, incs)
}

func (s *Server) incidentFromPath(w http.ResponseWriter, r *http.Request) *model.Incident {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid incident id"})
		return nil
	}
	inc, err := s.store.GetIncident(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return nil
	}
	if inc == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "incident not found"})
		return nil
	}
	return inc
}

func (s *Server) handleAPIGetIncident(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	writeJSON(w, http.StatusOK, inc)
}

// Diagnosis ---------------------------------------------------------------

func (s *Server) handleAPIDiagnose(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	if !s.cfg.LLM.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "LLM agent is disabled in config"})
		return
	}
	// Global concurrency limit: avoid starting an unbounded number of LLM
	// loops under a burst of alert notifications.
	select {
	case s.diagSem <- struct{}{}:
	default:
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many concurrent diagnoses"})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	if !s.pool.Start(inc.ID, cancel) {
		cancel()
		<-s.diagSem
		writeJSON(w, http.StatusConflict, map[string]string{"error": "diagnosis already running for this incident"})
		return
	}
	go func() {
		defer func() { <-s.diagSem }()
		defer s.pool.Stop(inc.ID)
		_, _ = s.agent.Diagnose(ctx, inc)
	}()
	if isHTMX(r) {
		s.renderIncidentPartial(w, r, inc)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"incident_id": inc.ID, "status": "diagnosing"})
}

// handleAPIUpdateStatus lets an operator transition an incident (e.g. to
// resolved). Body: {"status": "resolved"}.
func (s *Server) handleAPIUpdateStatus(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	var req struct {
		Status model.IncidentStatus `json:"status"`
	}
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		req.Status = model.IncidentStatus(r.Form.Get("status"))
	} else if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	switch req.Status {
	case model.IncidentOpen, model.IncidentDiagnosed, model.IncidentResolved, model.IncidentCancelled, model.IncidentError:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid status %q", req.Status)})
		return
	}
	switch req.Status {
	case model.IncidentResolved:
		if err := s.store.MarkResolved(r.Context(), inc.ID, "manual"); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_, _ = s.store.AddEvent(r.Context(), inc.ID, model.EventResolved, "resolved manually by operator")
	case model.IncidentCancelled:
		if err := s.store.UpdateIncidentStatus(r.Context(), inc.ID, req.Status); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_, _ = s.store.AddEvent(r.Context(), inc.ID, model.EventCancelled, "cancelled by operator")
	default:
		if err := s.store.UpdateIncidentStatus(r.Context(), inc.ID, req.Status); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	if isHTMX(r) {
		s.renderIncidentPartial(w, r, inc)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": inc.ID, "status": req.Status})
}

func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

func (s *Server) handleAPICancelDiagnosis(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	if !s.pool.Cancel(inc.ID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no diagnosis running for this incident"})
		return
	}
	_, _ = s.store.AddEvent(r.Context(), inc.ID, model.EventCancelled, "diagnosis cancelled by operator")
	if isHTMX(r) {
		s.renderIncidentPartial(w, r, inc)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) handleAPIGetDiagnosis(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	d, err := s.store.GetDiagnosis(r.Context(), inc.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if d == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no diagnosis yet"})
		return
	}
	runs, _ := s.store.ListCommandRuns(r.Context(), inc.ID)
	writeJSON(w, http.StatusOK, map[string]any{"diagnosis": d, "command_runs": runs, "running": s.pool.IsRunning(inc.ID)})
}

// Hosts -------------------------------------------------------------------

func (s *Server) handleAPIHosts(w http.ResponseWriter, r *http.Request) {
	type hostInfo struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		User    string `json:"user"`
		Port    int    `json:"port"`
	}
	hosts := []hostInfo{}
	for _, t := range s.resolver.Hosts() {
		hosts = append(hosts, hostInfo{Name: t.Name, Address: t.Address, User: t.User, Port: t.Port})
	}
	writeJSON(w, http.StatusOK, hosts)
}

func (s *Server) handleAPIHostCommands(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	if _, ok := s.resolver.Resolve(host); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "host not configured"})
		return
	}
	writeJSON(w, http.StatusOK, s.executor.Allowlist().List())
}

func (s *Server) handleAPIHostRunCommand(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	if _, ok := s.resolver.Resolve(host); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "host not configured"})
		return
	}
	var req struct {
		CommandID string            `json:"command_id"`
		Params    map[string]string `json:"params"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	run, err := s.executor.Run(r.Context(), nil, host, req.CommandID, req.Params)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "run": run})
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// Repos -------------------------------------------------------------------

func (s *Server) handleAPIRepos(w http.ResponseWriter, r *http.Request) {
	type repoInfo struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Root string `json:"root"`
		URL  string `json:"url"`
	}
	out := []repoInfo{}
	for _, rp := range s.repos.Repos() {
		out = append(out, repoInfo{Name: rp.Name, Type: rp.Type, Root: rp.Root, URL: rp.URL})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAPIReposSync(w http.ResponseWriter, r *http.Request) {
	out, err := s.repos.Sync(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": out})
}

func (s *Server) handleAPIReposSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q is required"})
		return
	}
	max, _ := strconv.Atoi(r.URL.Query().Get("max"))
	if r.URL.Query().Get("scope") == "docs" {
		writeJSON(w, http.StatusOK, s.repos.DocsSearch(q, max))
		return
	}
	writeJSON(w, http.StatusOK, s.repos.Search(q, max))
}

// Memory & instructions ---------------------------------------------------

func (s *Server) handleAPIMemories(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListMemories(r.Context(), 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleAPICreateMemory(w http.ResponseWriter, r *http.Request) {
	var m model.Memory
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		_ = r.ParseForm()
		m.Topic = r.Form.Get("topic")
		m.Content = r.Form.Get("content")
		if t := r.Form.Get("tags"); t != "" {
			for _, tag := range splitComma(t) {
				m.Tags = append(m.Tags, tag)
			}
		}
	} else if err := readJSON(r, &m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if m.Topic == "" || m.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "topic and content are required"})
		return
	}
	created, err := s.store.CreateMemory(r.Context(), &m)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		http.Redirect(w, r, "/memory", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleAPIInstructions(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListInstructions(r.Context(), 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleAPICreateInstruction(w http.ResponseWriter, r *http.Request) {
	var i model.Instruction
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		_ = r.ParseForm()
		i.Content = r.Form.Get("content")
		i.Source = r.Form.Get("source")
		i.Priority, _ = strconv.Atoi(r.Form.Get("priority"))
	} else if err := readJSON(r, &i); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if i.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content is required"})
		return
	}
	created, err := s.store.CreateInstruction(r.Context(), &i)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		http.Redirect(w, r, "/memory", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// handleAPIApplyInstruction marks an instruction as applied so it stops being
// injected into every diagnosis prompt.
func (s *Server) handleAPIApplyInstruction(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid instruction id"})
		return
	}
	if err := s.store.MarkInstructionApplied(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if isHTMX(r) {
		s.handleInstructionsPartial(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "applied": true})
}

func splitComma(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Groups & correlation ------------------------------------------------------

func (s *Server) handleAPIGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListGroups(r.Context(), 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := []map[string]any{}
	for _, g := range groups {
		members, _ := s.store.GroupMemberIDs(r.Context(), g.ID)
		out = append(out, map[string]any{
			"id": g.ID, "kind": g.Kind, "key": g.Key, "label": g.Label,
			"created_at": g.CreatedAt, "incident_ids": members, "member_count": g.MemberCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAPIGroupDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid group id"})
		return
	}
	groups, err := s.store.ListGroups(r.Context(), 1000)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var g *model.IncidentGroup
	for _, gr := range groups {
		if gr.ID == id {
			g = gr
			break
		}
	}
	if g == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "group not found"})
		return
	}
	members, _ := s.store.GroupMemberIDs(r.Context(), id)
	incs := []*model.Incident{}
	for _, mid := range members {
		if inc, err := s.store.GetIncident(r.Context(), mid); err == nil && inc != nil {
			incs = append(incs, inc)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g, "incidents": incs})
}

func (s *Server) handleAPIRunCorrelation(w http.ResponseWriter, r *http.Request) {
	if s.correlate == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "correlation is not configured"})
		return
	}
	if err := s.correlate.Run(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "correlated"})
}

func (s *Server) handleAPIRelated(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	groups, _ := s.store.GroupByIncident(r.Context(), inc.ID)
	related, _ := s.store.RelatedIncidentIDs(r.Context(), inc.ID)
	incs := []*model.Incident{}
	for _, rid := range related {
		if i, err := s.store.GetIncident(r.Context(), rid); err == nil && i != nil {
			incs = append(incs, i)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups, "incidents": incs})
}

func (s *Server) handleAPIIncidentEvents(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	events, err := s.store.ListEvents(r.Context(), inc.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// Review & retrospectives ---------------------------------------------------

func (s *Server) handleAPIRetrospectives(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListRetrospectives(r.Context(), 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleAPIRunReview(w http.ResponseWriter, r *http.Request) {
	if s.reviewer == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reviewer is not configured"})
		return
	}
	retro, err := s.reviewer.Run(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if retro == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "skipped", "reason": "not enough completed incidents yet"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "done", "retrospective": retro})
}
