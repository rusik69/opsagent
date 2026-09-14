package sshx

import (
	"context"
	"fmt"
	"sync"

	"github.com/rusik69/opsagent/internal/model"
	"github.com/rusik69/opsagent/internal/store"
)

// Executor runs allowlisted commands on configured hosts and records an audit
// trail in the store. It is the single execution path for the agent and API.
type Executor struct {
	mu        sync.Mutex
	allowlist *Allowlist
	runner    Runner
	store     *store.Store
	// Busy prevents concurrent command runs per host (simple safety valve).
	busy map[string]bool
}

func NewExecutor(allowlist *Allowlist, runner Runner, st *store.Store) *Executor {
	return &Executor{allowlist: allowlist, runner: runner, store: st, busy: map[string]bool{}}
}

func (e *Executor) Allowlist() *Allowlist { return e.allowlist }

// Run validates and executes an allowlisted command, persisting a CommandRun
// record with stdout/stderr and duration. Read-only access only.
func (e *Executor) Run(ctx context.Context, incidentID *int64, host, commandID string, params map[string]string) (*model.CommandRun, error) {
	command, err := e.allowlist.Render(commandID, params)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	if e.busy[host] {
		e.mu.Unlock()
		return nil, fmt.Errorf("host %q already has a running command", host)
	}
	e.busy[host] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.busy, host)
		e.mu.Unlock()
	}()

	run := &model.CommandRun{
		IncidentID: incidentID,
		Host:       host,
		CommandID:  commandID,
		Params:     params,
		Command:    command,
		Status:     "running",
	}
	run, err = e.store.CreateCommandRun(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("record command run: %w", err)
	}

	res, err := e.runner.Run(ctx, host, command)
	run.Stdout = res.Stdout
	run.Stderr = res.Stderr
	run.DurationMS = res.DurationMS
	if err != nil {
		run.Status = "error"
	} else {
		run.Status = "success"
	}
	if e.store != nil {
		_ = e.store.UpdateCommandRun(ctx, run)
	}
	if err != nil {
		return run, fmt.Errorf("run command: %w", err)
	}
	return run, nil
}

func (e *Executor) Runner() Runner { return e.runner }
