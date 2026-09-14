package sshx

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rusik69/opsagent/internal/store"
)

// errorRunner fails all runs.
type errorRunner struct{}

func (errorRunner) Run(_ context.Context, _, _ string) (Result, error) {
	return Result{Stderr: "boom"}, errors.New("command failed")
}

func TestExecutorRecordsAudit(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/ex.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	al, _ := NewAllowlist(DefaultAllowlist())
	runner := &FakeRunner{Outputs: map[string]string{"uptime": "load average: 0.5"}}
	ex := NewExecutor(al, runner, st)

	run, err := ex.Run(context.Background(), nil, "web-01", "uptime", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != "success" || run.Stdout == "" {
		t.Fatalf("unexpected run: %+v", run)
	}
	all, err := st.ListAllCommandRuns(context.Background(), 10)
	if err != nil || len(all) != 1 {
		t.Fatalf("expected 1 audit row, got %d (%v)", len(all), err)
	}
}

func TestExecutorRejectsDisallowed(t *testing.T) {
	st, _ := store.Open(t.TempDir() + "/ex.db")
	defer st.Close()
	al, _ := NewAllowlist(DefaultAllowlist())
	ex := NewExecutor(al, &FakeRunner{}, st)
	if _, err := ex.Run(context.Background(), nil, "web-01", "rm_rf", nil); err == nil {
		t.Fatal("expected rejection of non-allowlisted command")
	}
	if _, err := ex.Run(context.Background(), nil, "web-01", "systemctl_status", map[string]string{"service": "a;reboot"}); err == nil {
		t.Fatal("expected rejection of injection attempt")
	}
}

func TestExecutorErrorRunRecorded(t *testing.T) {
	st, _ := store.Open(t.TempDir() + "/ex.db")
	defer st.Close()
	al, _ := NewAllowlist(DefaultAllowlist())
	ex := NewExecutor(al, errorRunner{}, st)
	run, err := ex.Run(context.Background(), nil, "web-01", "uptime", nil)
	if err == nil {
		t.Fatal("expected error from failing runner")
	}
	if run == nil || run.Status != "error" {
		t.Fatalf("expected error run recorded, got %+v", run)
	}
	all, _ := st.ListAllCommandRuns(context.Background(), 10)
	if len(all) != 1 || all[0].Status != "error" {
		t.Fatalf("expected 1 error audit row, got %+v", all)
	}
}

func TestExecutorRejectsConcurrentPerHost(t *testing.T) {
	st, _ := store.Open(t.TempDir() + "/ex.db")
	defer st.Close()
	al, _ := NewAllowlist(DefaultAllowlist())
	runner := &slowRunner{delay: 100 * time.Millisecond, out: "done"}
	ex := NewExecutor(al, runner, st)

	var wg sync.WaitGroup
	var successes, rejections int32
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ex.Run(context.Background(), nil, "web-01", "uptime", nil)
			if err != nil {
				atomic.AddInt32(&rejections, 1)
			} else {
				atomic.AddInt32(&successes, 1)
			}
		}()
	}
	wg.Wait()
	// One run wins the per-host lock; the others must be rejected.
	if successes != 1 {
		t.Fatalf("expected exactly 1 successful run, got %d (rejections %d)", successes, rejections)
	}
	if rejections != 2 {
		t.Fatalf("expected 2 rejections, got %d", rejections)
	}
	// Only the successful run is audited.
	all, _ := st.ListAllCommandRuns(context.Background(), 10)
	if len(all) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(all))
	}
}

type slowRunner struct {
	delay time.Duration
	out   string
}

func (s *slowRunner) Run(_ context.Context, _, _ string) (Result, error) {
	time.Sleep(s.delay)
	return Result{Stdout: s.out}, nil
}

func TestAllowlistSortedIDs(t *testing.T) {
	al, _ := NewAllowlist(DefaultAllowlist())
	ids := al.SortedIDs()
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Fatalf("ids not sorted: %v", ids)
		}
	}
}
