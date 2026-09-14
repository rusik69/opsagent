package agent

import (
	"context"
	"sync"
)

// Pool tracks in-flight diagnoses so they can be de-duplicated and cancelled.
type Pool struct {
	mu      sync.Mutex
	running map[int64]context.CancelFunc
}

func NewPool() *Pool {
	return &Pool{running: map[int64]context.CancelFunc{}}
}

// Start registers a diagnosis. Returns false if one is already running.
func (p *Pool) Start(incidentID int64, cancel context.CancelFunc) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.running[incidentID]; ok {
		return false
	}
	p.running[incidentID] = cancel
	return true
}

// Stop removes and cancels a diagnosis.
func (p *Pool) Stop(incidentID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cancel, ok := p.running[incidentID]; ok {
		cancel()
		delete(p.running, incidentID)
	}
}

// Cancel cancels a running diagnosis if present.
func (p *Pool) Cancel(incidentID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cancel, ok := p.running[incidentID]; ok {
		cancel()
		delete(p.running, incidentID)
		return true
	}
	return false
}

// IsRunning reports whether a diagnosis is in flight.
func (p *Pool) IsRunning(incidentID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.running[incidentID]
	return ok
}
