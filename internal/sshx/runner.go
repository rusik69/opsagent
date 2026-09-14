package sshx

import (
	"context"
)

// Result is the outcome of a single command execution.
type Result struct {
	Stdout     string
	Stderr     string
	DurationMS int64
}

// Runner executes read-only commands on a remote host. Implementations must
// run commands without an interactive shell and without allowing shell
// interpretation of arguments (commands are allowlisted and validated).
type Runner interface {
	Run(ctx context.Context, host, command string) (Result, error)
}

// HostResolver maps host names to SSH connection targets.
type HostResolver interface {
	Resolve(host string) (HostTarget, bool)
	Hosts() []HostTarget
}

type HostTarget struct {
	Name    string
	Address string
	User    string
	Port    int
}

// FakeRunner is an in-memory Runner for tests and local development. When the
// host matches a key in Outputs, that output is returned verbatim.
type FakeRunner struct {
	Outputs map[string]string
}

func (f *FakeRunner) Run(_ context.Context, _ string, command string) (Result, error) {
	if out, ok := f.Outputs[command]; ok {
		return Result{Stdout: out}, nil
	}
	return Result{Stdout: ""}, nil
}

// HostsFromTargets builds a static HostResolver.
func HostsFromTargets(targets []HostTarget) HostResolver {
	return &staticResolver{targets: targets}
}

type staticResolver struct {
	targets []HostTarget
}

func (s *staticResolver) Resolve(name string) (HostTarget, bool) {
	for _, t := range s.targets {
		if t.Name == name {
			return t, true
		}
	}
	return HostTarget{}, false
}

func (s *staticResolver) Hosts() []HostTarget { return s.targets }
