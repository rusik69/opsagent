package mcp

import "context"

type ctxKey int

const incidentKey ctxKey = iota

// WithIncident returns a context carrying the incident id for audit linkage.
func WithIncident(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, incidentKey, id)
}

// IncidentIDFrom returns the incident id carried in ctx, or 0 if unset.
func IncidentIDFrom(ctx context.Context) int64 {
	if v, ok := ctx.Value(incidentKey).(int64); ok {
		return v
	}
	return 0
}
