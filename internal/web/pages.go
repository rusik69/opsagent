package web

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"

	"github.com/rusik69/opsagent/internal/model"
)

type dashboardData struct {
	Total       int
	Open        int
	Critical    int
	NeedsReview int
	Recent      []*model.Incident
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListIncidents(r.Context(), "", 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var open, critical, needsReview int
	recent := []*model.Incident{}
	for i, inc := range all {
		if inc.Status == model.IncidentOpen || inc.Status == model.IncidentDiagnosing {
			open++
		}
		if inc.Severity == model.SeverityCritical {
			critical++
		}
		if inc.Status == model.IncidentDiagnosed && inc.MRURL == "" {
			needsReview++
		}
		if i < 10 {
			recent = append(recent, inc)
		}
	}
	s.render(w, "dashboard", dashboardData{
		Total:       len(all),
		Open:        open,
		Critical:    critical,
		NeedsReview: needsReview,
		Recent:      recent,
	})
}

func (s *Server) handleIncidentsPage(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	severity := r.URL.Query().Get("severity")
	query := r.URL.Query().Get("q")
	incs, err := s.store.ListIncidentsFiltered(r.Context(), status, severity, query, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "incidents", map[string]any{"Incidents": incs, "Status": status, "Severity": severity, "Query": query})
}

func (s *Server) handleIncidentPage(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	s.renderIncidentDetail(w, r, inc, "incident")
}

func (s *Server) handleHostsPage(w http.ResponseWriter, r *http.Request) {
	hosts := s.resolver.Hosts()
	commands := s.executor.Allowlist().List()
	cmdsJSON, _ := json.Marshal(commands)
	s.render(w, "hosts", map[string]any{
		"Hosts": hosts, "Commands": commands,
		"CommandsJSON": template.JS(cmdsJSON),
	})
}

func (s *Server) handleReposPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "repos", map[string]any{"Repos": s.repos.Repos()})
}

func (s *Server) handleMemoryPage(w http.ResponseWriter, r *http.Request) {
	mem, _ := s.store.ListMemories(r.Context(), 200)
	instr, _ := s.store.ListInstructions(r.Context(), 200)
	s.render(w, "memory", map[string]any{"Memories": mem, "Instructions": instr})
}

// Partials (HTMX) ----------------------------------------------------------

// incidentDetailData collects everything needed to render an incident.
func (s *Server) incidentDetailData(ctx context.Context, inc *model.Incident) map[string]any {
	d, _ := s.store.GetDiagnosis(ctx, inc.ID)
	runs, _ := s.store.ListCommandRuns(ctx, inc.ID)
	events, _ := s.store.ListEvents(ctx, inc.ID)
	groups, _ := s.store.GroupByIncident(ctx, inc.ID)
	relatedIDs, _ := s.store.RelatedIncidentIDs(ctx, inc.ID)
	related := []*model.Incident{}
	for _, rid := range relatedIDs {
		if i, err := s.store.GetIncident(ctx, rid); err == nil && i != nil {
			related = append(related, i)
		}
	}
	return map[string]any{
		"Incident": inc, "Diagnosis": d, "CommandRuns": runs,
		"Events": events, "Groups": groups, "Related": related,
		"Running": s.pool.IsRunning(inc.ID),
	}
}

// renderIncidentDetail renders the incident root (full page or partial).
func (s *Server) renderIncidentDetail(w http.ResponseWriter, r *http.Request, inc *model.Incident, templateName string) {
	s.render(w, templateName, s.incidentDetailData(r.Context(), inc))
}

// renderIncidentPartial renders the incident root (metadata + diagnosis +
// command runs + actions) with the current running state.
func (s *Server) renderIncidentPartial(w http.ResponseWriter, r *http.Request, inc *model.Incident) {
	s.render(w, "incident_partial", s.incidentDetailData(r.Context(), inc))
}

func (s *Server) handleIncidentPartial(w http.ResponseWriter, r *http.Request) {
	inc := s.incidentFromPath(w, r)
	if inc == nil {
		return
	}
	s.renderIncidentPartial(w, r, inc)
}

func (s *Server) handleGroupsPage(w http.ResponseWriter, r *http.Request) {
	groups, _ := s.store.ListGroups(r.Context(), 200)
	type groupView struct {
		ID    int64
		Kind  string
		Label string
		Count int
	}
	views := []groupView{}
	for _, g := range groups {
		members, _ := s.store.GroupMemberIDs(r.Context(), g.ID)
		views = append(views, groupView{ID: g.ID, Kind: g.Kind, Label: g.Label, Count: len(members)})
	}
	s.render(w, "groups", map[string]any{"Groups": views})
}

func (s *Server) handleHistoryPage(w http.ResponseWriter, r *http.Request) {
	retros, _ := s.store.ListRetrospectives(r.Context(), 50)
	s.render(w, "history", map[string]any{"Retrospectives": retros})
}

func (s *Server) handleDiagnosisPartial(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	d, err := s.store.GetDiagnosis(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	runs, _ := s.store.ListCommandRuns(r.Context(), id)
	s.render(w, "diagnosis_partial", map[string]any{
		"Diagnosis": d, "CommandRuns": runs, "Running": s.pool.IsRunning(id),
	})
}

func (s *Server) handleMemoryPartial(w http.ResponseWriter, r *http.Request) {
	mem, _ := s.store.ListMemories(r.Context(), 200)
	s.render(w, "memory_partial", map[string]any{"Memories": mem})
}

func (s *Server) handleInstructionsPartial(w http.ResponseWriter, r *http.Request) {
	instr, _ := s.store.ListInstructions(r.Context(), 200)
	s.render(w, "instructions_partial", map[string]any{"Instructions": instr})
}
