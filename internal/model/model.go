package model

import "time"

type IncidentStatus string

const (
	IncidentOpen       IncidentStatus = "open"
	IncidentDiagnosing IncidentStatus = "diagnosing"
	IncidentDiagnosed  IncidentStatus = "diagnosed"
	IncidentError      IncidentStatus = "error"
	IncidentResolved   IncidentStatus = "resolved"
	IncidentCancelled  IncidentStatus = "cancelled"
)

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

type Incident struct {
	ID          int64             `json:"id"`
	ExternalID  string            `json:"external_id"`
	Source      string            `json:"source"`
	Host        string            `json:"host"`
	Severity    Severity          `json:"severity"`
	Title       string            `json:"title"`
	Message     string            `json:"message"`
	Labels      map[string]string `json:"labels"`
	Status      IncidentStatus    `json:"status"`
	Solution    string            `json:"solution"`
	MRURL       string            `json:"mr_url"`
	RootCause   string            `json:"root_cause"`
	Confidence  string            `json:"confidence"`
	ResolvedVia string            `json:"resolved_via"`
	ResolvedAt  *time.Time        `json:"resolved_at,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// IncidentEventKind enumerates append-only timeline event types.
const (
	EventCreated        = "created"
	EventDiagnosisStart = "diagnosis_started"
	EventDiagnosed      = "diagnosed"
	EventError          = "error"
	EventMRCreated      = "mr_created"
	EventMRMerged       = "mr_merged"
	EventResolved       = "resolved"
	EventCancelled      = "cancelled"
	EventCorrelated     = "correlated"
	EventNote           = "note"
	EventOutcomeSet     = "outcome_set"
	EventRecurrence     = "recurrence"
	EventMRClosed       = "mr_closed"
)

type IncidentEvent struct {
	ID         int64     `json:"id"`
	IncidentID int64     `json:"incident_id"`
	Kind       string    `json:"kind"`
	Detail     string    `json:"detail"`
	CreatedAt  time.Time `json:"created_at"`
}

// GroupKind enumerates incident correlation methods.
const (
	GroupHost      = "host"
	GroupAlertname = "alertname"
	GroupLabel     = "label"
	GroupRootCause = "rootcause"
	GroupAgent     = "agent"
)

type IncidentGroup struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Key       string    `json:"key"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
	// IncidentIDs is populated by list queries (not stored).
	IncidentIDs []int64 `json:"incident_ids,omitempty"`
}

type Retrospective struct {
	ID                  int64     `json:"id"`
	WindowStart         time.Time `json:"window_start"`
	WindowEnd           time.Time `json:"window_end"`
	IncidentsReviewd    int       `json:"incidents_reviewed"`
	Summary             string    `json:"summary"`
	MemoriesCreated     int       `json:"memories_created"`
	InstructionsCreated int       `json:"instructions_created"`
	CreatedAt           time.Time `json:"created_at"`
}

type Diagnosis struct {
	ID         int64           `json:"id"`
	IncidentID int64           `json:"incident_id"`
	Status     string          `json:"status"`
	Report     string          `json:"report"`
	Summary    string          `json:"summary"`
	Steps      []DiagnosisStep `json:"steps"`
	Logs       string          `json:"logs"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type DiagnosisStep struct {
	Step      int       `json:"step"`
	Tool      string    `json:"tool"`
	Input     string    `json:"input"`
	Output    string    `json:"output"`
	Timestamp time.Time `json:"timestamp"`
}

type CommandRun struct {
	ID         int64             `json:"id"`
	IncidentID *int64            `json:"incident_id,omitempty"`
	Host       string            `json:"host"`
	CommandID  string            `json:"command_id"`
	Params     map[string]string `json:"params"`
	Command    string            `json:"command"`
	Status     string            `json:"status"`
	Stdout     string            `json:"stdout"`
	Stderr     string            `json:"stderr"`
	DurationMS int64             `json:"duration_ms"`
	CreatedAt  time.Time         `json:"created_at"`
}

type Memory struct {
	ID        int64     `json:"id"`
	Topic     string    `json:"topic"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"created_at"`
}

type Instruction struct {
	ID        int64     `json:"id"`
	Content   string    `json:"content"`
	Priority  int       `json:"priority"`
	Source    string    `json:"source"`
	Applied   bool      `json:"applied"`
	CreatedAt time.Time `json:"created_at"`
}
