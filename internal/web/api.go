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
	if !s.verifyWebhookSignature(w, r) {
		return
	}
	var g incidents.GenericIncident
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		g.Host = r.Form.Get("host")
		g.Title = r.Form.Get("title")
		g.Severity = r.Form.Get("severity")
		g.Message = r.Form.Get("message")
		g.Source = r.Form.Get("source")
		g.ExternalID = r.Form.Get("external_id")
		g.Owner = r.Form.Get("owner")
		g.Team = r.Form.Get("team")
		if t := r.Form.Get("tags"); t != "" {
			g.Tags = splitComma(t)
		}
		if g.Labels == nil {
			g.Labels = map[string]string{}
		}
		for k, vs := range r.Form {
			if len(vs) > 0 {
				g.Labels[k] = vs[0]
			}
		}
		for _, k := range []string{"host", "title", "severity", "message", "source", "external_id", "owner", "team", "tags"} {
			delete(g.Labels, k)
		}
	} else {
		if err := readJSON(r, &g); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}
	inc, err := s.incidents.CreateGeneric(r.Context(), g)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		http.Redirect(w, r, "/incidents", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusCreated, inc)
}

func (s *Server) handleAPIAlertmanager(w http.ResponseWriter, r *http.Request) {
	if !s.verifyWebhookSignature(w, r) {
		return
	}
	var payload incidents.AlertmanagerPayload
	if err := readJSON(r, &payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	created, err := s.incidents.CreateAlertmanager(r.Context(), payload)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"created": len(created), "incidents": created})
}

func (s *Server) handleAPIListIncidents(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	severity := r.URL.Query().Get("severity")
	query := r.URL.Query().Get("q")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 100
	}
	incs, total, err := s.store.ListIncidentsPage(r.Context(), status, severity, query, limit, offset)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("X-Total-Count", fmt.Sprintf("%d", total))
	writeJSON(w, http.StatusOK, incs)
}

func (s *Server) incidentFromPath(w http.ResponseWriter, r *http.Request) *model.Incident {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid incident id")
		return nil
	}
	inc, err := s.store.GetIncident(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	if inc == nil {
		writeAPIError(w, http.StatusNotFound, "incident not found")
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
		writeAPIError(w, http.StatusBadRequest, "LLM agent is disabled in config")
		return
	}
	// Global concurrency limit: avoid starting an unbounded number of LLM
	// loops under a burst of alert notifications.
	select {
	case s.diagSem <- struct{}{}:
	default:
		writeAPIError(w, http.StatusTooManyRequests, "too many concurrent diagnoses")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	if !s.pool.Start(inc.ID, cancel) {
		cancel()
		<-s.diagSem
		writeAPIError(w, http.StatusConflict, "diagnosis already running for this incident")
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
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Status = model.IncidentStatus(r.Form.Get("status"))
	} else if err := readJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	switch req.Status {
	case model.IncidentOpen, model.IncidentDiagnosed, model.IncidentResolved, model.IncidentCancelled, model.IncidentError:
	default:
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("invalid status %q", req.Status))
		return
	}
	switch req.Status {
	case model.IncidentResolved:
		if err := s.store.MarkResolved(r.Context(), inc.ID, "manual"); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_, _ = s.store.AddEvent(r.Context(), inc.ID, model.EventResolved, "resolved manually by operator")
	case model.IncidentCancelled:
		if err := s.store.UpdateIncidentStatus(r.Context(), inc.ID, req.Status); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_, _ = s.store.AddEvent(r.Context(), inc.ID, model.EventCancelled, "cancelled by operator")
	default:
		if err := s.store.UpdateIncidentStatus(r.Context(), inc.ID, req.Status); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
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
		writeAPIError(w, http.StatusNotFound, "no diagnosis running for this incident")
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
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d == nil {
		writeAPIError(w, http.StatusNotFound, "no diagnosis yet")
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
		writeAPIError(w, http.StatusNotFound, "host not configured")
		return
	}
	writeJSON(w, http.StatusOK, s.executor.Allowlist().List())
}

func (s *Server) handleAPIHostRunCommand(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	if _, ok := s.resolver.Resolve(host); !ok {
		writeAPIError(w, http.StatusBadRequest, "host not configured")
		return
	}
	var req struct {
		CommandID string            `json:"command_id"`
		Params    map[string]string `json:"params"`
	}
	if err := readJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
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
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": out})
}

func (s *Server) handleAPIReposSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeAPIError(w, http.StatusBadRequest, "q is required")
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
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	items, total, err := s.store.ListMemoriesPage(r.Context(), limit, offset)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("X-Total-Count", fmt.Sprintf("%d", total))
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
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if m.Topic == "" || m.Content == "" {
		writeAPIError(w, http.StatusBadRequest, "topic and content are required")
		return
	}
	created, err := s.store.CreateMemory(r.Context(), &m)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		http.Redirect(w, r, "/memory", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleAPIInstructions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	items, total, err := s.store.ListInstructionsPage(r.Context(), limit, offset)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("X-Total-Count", fmt.Sprintf("%d", total))
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
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if i.Content == "" {
		writeAPIError(w, http.StatusBadRequest, "content is required")
		return
	}
	created, err := s.store.CreateInstruction(r.Context(), &i)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
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
		writeAPIError(w, http.StatusBadRequest, "invalid instruction id")
		return
	}
	if err := s.store.MarkInstructionApplied(r.Context(), id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
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

// handleAPIStats returns dashboard aggregates.
func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.IncidentStats(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleMetrics exposes a small dependency-free Prometheus text-format
// endpoint backed by the store aggregates.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.IncidentStats(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var sb strings.Builder
	sb.WriteString("# HELP opsagent_incidents Incidents by status.\n")
	sb.WriteString("# TYPE opsagent_incidents gauge\n")
	for _, sc := range []struct {
		label string
		n     int64
	}{
		{"open", stats.Open}, {"diagnosing", stats.Diagnosing}, {"diagnosed", stats.Diagnosed},
		{"resolved", stats.Resolved}, {"cancelled", stats.Cancelled}, {"error", stats.Error},
		{"deleted", stats.Deleted},
	} {
		fmt.Fprintf(&sb, "opsagent_incidents{status=%q} %d\n", sc.label, sc.n)
	}
	sb.WriteString("# HELP opsagent_incidents_critical Critical incidents.\n")
	sb.WriteString("# TYPE opsagent_incidents_critical gauge\n")
	fmt.Fprintf(&sb, "opsagent_incidents_critical %d\n", stats.Critical)
	sb.WriteString("# HELP opsagent_incident_recurrences Total recurrence events.\n")
	sb.WriteString("# TYPE opsagent_incident_recurrences counter\n")
	fmt.Fprintf(&sb, "opsagent_incident_recurrences %d\n", stats.Recurrences)
	sb.WriteString("# HELP opsagent_command_avg_duration_ms Average successful command duration.\n")
	sb.WriteString("# TYPE opsagent_command_avg_duration_ms gauge\n")
	fmt.Fprintf(&sb, "opsagent_command_avg_duration_ms %d\n", stats.AvgDiagnosisMS)
	_, _ = w.Write([]byte(sb.String()))
}

// Groups & correlation ------------------------------------------------------

func (s *Server) handleAPIGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListGroups(r.Context(), 200)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
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
		writeAPIError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	groups, err := s.store.ListGroups(r.Context(), 1000)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
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
		writeAPIError(w, http.StatusNotFound, "group not found")
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
		writeAPIError(w, http.StatusBadRequest, "correlation is not configured")
		return
	}
	if err := s.correlate.Run(r.Context()); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
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

// handleAPIDeleteIncident soft-deletes an incident.
func (s *Server) handleAPIDeleteIncident(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	if err := s.store.DeleteIncident(r.Context(), inc.ID); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = s.store.AddEvent(r.Context(), inc.ID, model.EventCancelled, "incident deleted by operator")
	writeJSON(w, http.StatusOK, map[string]any{"id": inc.ID, "status": model.IncidentDeleted})
}

type bulkRequest struct {
	IDs    []int64              `json:"ids"`
	Status model.IncidentStatus `json:"status"`
}

func decodeBulk(w http.ResponseWriter, r *http.Request) (*bulkRequest, bool) {
	var req bulkRequest
	if err := readJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return nil, false
	}
	if len(req.IDs) == 0 {
		writeAPIError(w, http.StatusBadRequest, "ids is required")
		return nil, false
	}
	return &req, true
}

// handleAPIBulkStatus transitions many incidents to a status at once.
func (s *Server) handleAPIBulkStatus(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBulk(w, r)
	if !ok {
		return
	}
	switch req.Status {
	case model.IncidentOpen, model.IncidentDiagnosed, model.IncidentResolved, model.IncidentCancelled, model.IncidentError:
	default:
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("invalid status %q", req.Status))
		return
	}
	if req.Status == model.IncidentResolved {
		for _, id := range req.IDs {
			_ = s.store.MarkResolved(r.Context(), id, "manual")
			_, _ = s.store.AddEvent(r.Context(), id, model.EventResolved, "resolved in bulk by operator")
		}
		writeJSON(w, http.StatusOK, map[string]any{"updated": len(req.IDs), "status": req.Status})
		return
	}
	n, err := s.store.BulkUpdateStatus(r.Context(), req.IDs, req.Status)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": n, "status": req.Status})
}

// handleAPIBulkDelete soft-deletes many incidents at once.
func (s *Server) handleAPIBulkDelete(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBulk(w, r)
	if !ok {
		return
	}
	n, err := s.store.BulkUpdateStatus(r.Context(), req.IDs, model.IncidentDeleted)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

// handleAPIAddNote appends an operator note to an incident's timeline.
func (s *Server) handleAPIAddNote(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		_ = r.ParseForm()
		req.Note = r.Form.Get("note")
	} else if err := readJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	if req.Note == "" {
		writeAPIError(w, http.StatusBadRequest, "note is required")
		return
	}
	if _, err := s.store.AddEvent(r.Context(), inc.ID, model.EventNote, req.Note); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if isHTMX(r) {
		s.renderIncidentPartial(w, r, inc)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": inc.ID, "note": req.Note})
}

func (s *Server) handleAPIIncidentEvents(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	events, err := s.store.ListEvents(r.Context(), inc.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// Review & retrospectives ---------------------------------------------------

func (s *Server) handleAPIRetrospectives(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListRetrospectives(r.Context(), 50)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleAPIRunReview(w http.ResponseWriter, r *http.Request) {
	if s.reviewer == nil {
		writeAPIError(w, http.StatusBadRequest, "reviewer is not configured")
		return
	}
	retro, err := s.reviewer.Run(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if retro == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "skipped", "reason": "not enough completed incidents yet"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "done", "retrospective": retro})
}
