package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/rusik69/opsagent/internal/agent"
	"github.com/rusik69/opsagent/internal/config"
	"github.com/rusik69/opsagent/internal/correlate"
	"github.com/rusik69/opsagent/internal/gitlab"
	"github.com/rusik69/opsagent/internal/incidents"
	"github.com/rusik69/opsagent/internal/mcp"
	"github.com/rusik69/opsagent/internal/model"
	"github.com/rusik69/opsagent/internal/repos"
	"github.com/rusik69/opsagent/internal/review"
	"github.com/rusik69/opsagent/internal/sshx"
	"github.com/rusik69/opsagent/internal/store"
	"github.com/rusik69/opsagent/internal/web"
)

func main() {
	configPath := flag.String("config", "", "path to config.yaml (defaults to built-in defaults)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error(fmt.Sprintf("config: %v", err))
		os.Exit(1)
	}

	// Storage.
	st, err := store.Open(cfg.Storage.Path)
	if err != nil {
		slog.Error(fmt.Sprintf("storage: %v", err))
		os.Exit(1)
	}
	defer st.Close()

	// SSH allowlist + executor.
	al, err := sshx.NewAllowlist(sshx.DefaultAllowlist())
	if err != nil {
		slog.Error(fmt.Sprintf("allowlist: %v", err))
		os.Exit(1)
	}
	if len(cfg.Allowlist.Commands) > 0 {
		extra := make([]sshx.Command, 0, len(cfg.Allowlist.Commands))
		for _, c := range cfg.Allowlist.Commands {
			extra = append(extra, sshx.Command{ID: c.ID, Description: c.Description, Template: c.Template, Params: c.Params})
		}
		merged := append(sshx.DefaultAllowlist(), extra...)
		al, err = sshx.NewAllowlist(merged)
		if err != nil {
			slog.Error(fmt.Sprintf("allowlist: %v", err))
			os.Exit(1)
		}
	}

	// Hosts.
	targets := []sshx.HostTarget{}
	for _, h := range cfg.SSH.Hosts {
		port := h.Port
		if port == 0 {
			port = 22
		}
		targets = append(targets, sshx.HostTarget{Name: h.Name, Address: h.Address, User: h.User, Port: port})
	}
	resolver := sshx.HostsFromTargets(targets)

	var runner sshx.Runner
	if len(targets) == 0 {
		slog.Warn(fmt.Sprintf("warning: no hosts configured; using fake runner (all commands return empty)"))
		runner = &sshx.FakeRunner{Outputs: map[string]string{}}
	} else {
		sshClient, err := sshx.NewSSHClient(sshx.Options{
			UseAgent:       cfg.SSH.UseAgent,
			PrivateKeyPath: cfg.SSH.PrivateKey,
			KnownHosts:     cfg.SSH.KnownHosts,
			ConnectTimeout: time.Duration(cfg.SSH.TimeoutSeconds) * time.Second,
			CommandTimeout: time.Duration(cfg.SSH.TimeoutSeconds) * time.Second,
			Resolver:       resolver,
		})
		if err != nil {
			slog.Error(fmt.Sprintf("ssh: %v", err))
			os.Exit(1)
		}
		defer sshClient.Close()
		runner = sshClient
	}

	executor := sshx.NewExecutor(al, runner, st)

	// Repos.
	reposMgr, err := repos.NewManager(cfg.Repos, "./data/repos")
	if err != nil {
		slog.Error(fmt.Sprintf("repos: %v", err))
		os.Exit(1)
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		out, err := reposMgr.Sync(ctx)
		cancel()
		if err != nil {
			slog.Info(fmt.Sprintf("repos initial sync: %v", err))
		} else {
			slog.Info(fmt.Sprintf("repos sync:\n%s", out))
		}
	}

	// GitLab client (optional; enables create_gitlab_mr).
	var gl *gitlab.Client
	if cfg.GitLab.BaseURL != "" && cfg.GitLab.Token != "" {
		gl = gitlab.New(cfg.GitLab.BaseURL, cfg.GitLab.Token)
		gl.DefaultProjectID = cfg.GitLab.DefaultProjectID
		if cfg.GitLab.SourceBranchPrefix != "" {
			gl.SourceBranchPrefix = cfg.GitLab.SourceBranchPrefix
		}
		if cfg.GitLab.TargetBranch != "" {
			gl.TargetBranch = cfg.GitLab.TargetBranch
		}
		slog.Info(fmt.Sprintf("gitlab configured: %s", cfg.GitLab.BaseURL))
	} else {
		slog.Warn(fmt.Sprintf("warning: gitlab not configured; create_gitlab_mr tool will be unavailable"))
	}

	// Correlation engine.
	var corr *correlate.Engine
	if cfg.Correlate.Enabled {
		corrCfg := correlate.DefaultConfig()
		if cfg.Correlate.WindowMinutes > 0 {
			corrCfg.Window = time.Duration(cfg.Correlate.WindowMinutes) * time.Minute
		}
		if len(cfg.Correlate.Methods) > 0 {
			corrCfg.Methods = cfg.Correlate.Methods
		}
		if len(cfg.Correlate.LabelKeys) > 0 {
			corrCfg.LabelKeys = cfg.Correlate.LabelKeys
		}
		corr = correlate.NewEngine(st, corrCfg)
	}

	// MCP server.
	mcpSrv, err := mcp.NewServer("opsagent", "1.0.0", mcp.Deps{
		Executor:  executor,
		Repos:     reposMgr,
		Store:     st,
		Resolver:  resolver,
		GitLab:    gl,
		Correlate: corr,
	})
	if err != nil {
		slog.Error(fmt.Sprintf("mcp: %v", err))
		os.Exit(1)
	}
	slog.Info(fmt.Sprintf("MCP tools: %d registered", len(mcpSrv.MCPServer().ListTools())))

	// Agent.
	llm := agent.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.Model)
	llm.MaxRetries = cfg.LLM.MaxRetries
	llm.MaxTokens = cfg.LLM.MaxTokens
	ag := agent.New(llm, mcpSrv, st, agent.Options{
		MaxSteps:         cfg.LLM.MaxSteps,
		Timeout:          time.Duration(cfg.LLM.TimeoutSecs) * time.Second,
		HostConfig:       reposMgr.HostConfig,
		InstructionsFile: cfg.Agent.InstructionsFile,
	})

	// Incident service.
	inc := incidents.NewService(st)

	// Web server.
	ws, err := web.New(cfg, st, executor, resolver, reposMgr, mcpSrv, ag, inc, corr)
	if err != nil {
		slog.Error(fmt.Sprintf("web: %v", err))
		os.Exit(1)
	}

	// Self-improvement reviewer (background interval + manual API trigger).
	var rv *review.Reviewer
	if cfg.Review.Enabled && cfg.LLM.Enabled {
		rv = review.New(st, llm, mcpSrv, review.Config{
			Limit: cfg.Review.Limit, MinIncidents: cfg.Review.MinIncidents, MaxSummaryLen: cfg.Review.MaxSummaryLen,
		})
		ws.SetReviewer(rv)
	}

	// Background jobs: each runs on a ticker until the stop channel closes,
	// so shutdown is graceful (tickers stop before the store is closed).
	var wg sync.WaitGroup
	stop := make(chan struct{})

	startTicker := func(interval time.Duration, first bool, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if first {
				fn()
			}
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					fn()
				case <-stop:
					return
				}
			}
		}()
	}

	withCtx := func(timeout time.Duration, fn func(context.Context)) func() {
		return func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			fn(ctx)
		}
	}

	// Review loop.
	if rv != nil {
		interval := time.Duration(cfg.Review.IntervalHours) * time.Hour
		if interval <= 0 {
			interval = 6 * time.Hour
		}
		startTicker(interval, true, withCtx(5*time.Minute, func(ctx context.Context) {
			retro, err := rv.Run(ctx)
			if err != nil {
				slog.Info(fmt.Sprintf("review: %v", err))
			} else if retro != nil {
				slog.Info(fmt.Sprintf("review: %d incidents reviewed, %d memories, %d instructions", retro.IncidentsReviewd, retro.MemoriesCreated, retro.InstructionsCreated))
			}
		}))
	}

	// Correlation loop.
	if corr != nil {
		interval := time.Duration(cfg.Correlate.IntervalMinutes) * time.Minute
		if interval <= 0 {
			interval = 15 * time.Minute
		}
		startTicker(interval, true, withCtx(2*time.Minute, func(ctx context.Context) {
			if err := corr.Run(ctx); err != nil {
				slog.Info(fmt.Sprintf("correlation: %v", err))
			}
		}))
	}

	// MR merge poller: when an agent-created MR is merged, mark the incident
	// resolved via mr_merged (outcome feedback for the review loop).
	if gl != nil && cfg.GitLab.PollMRSeconds > 0 {
		interval := time.Duration(cfg.GitLab.PollMRSeconds) * time.Second
		startTicker(interval, false, withCtx(2*time.Minute, func(ctx context.Context) {
			pollMRMerges(ctx, gl, st, cfg.GitLab.DefaultProjectID)
		}))
	}

	// Periodic repo sync keeps cloned config repos fresh.
	if cfg.Maintenance.RepoSyncMinutes > 0 {
		interval := time.Duration(cfg.Maintenance.RepoSyncMinutes) * time.Minute
		startTicker(interval, false, withCtx(5*time.Minute, func(ctx context.Context) {
			if out, err := reposMgr.Sync(ctx); err != nil {
				slog.Info(fmt.Sprintf("repos sync: %v", err))
			} else {
				slog.Info(fmt.Sprintf("repos sync:\n%s", out))
			}
		}))
	}

	// Stale incidents: auto-close open/diagnosing incidents older than
	// maintenance.auto_close_hours.
	if cfg.Maintenance.AutoCloseHours > 0 {
		startTicker(30*time.Minute, true, withCtx(2*time.Minute, func(ctx context.Context) {
			cutoff := time.Now().UTC().Add(-time.Duration(cfg.Maintenance.AutoCloseHours) * time.Hour)
			stale, err := st.ListStaleIncidents(ctx, cutoff, 500)
			if err != nil {
				slog.Info(fmt.Sprintf("auto-close: %v", err))
				return
			}
			for _, inc := range stale {
				if err := st.UpdateIncidentStatus(ctx, inc.ID, model.IncidentCancelled); err != nil {
					slog.Info(fmt.Sprintf("auto-close: #%d: %v", inc.ID, err))
					continue
				}
				_, _ = st.AddEvent(ctx, inc.ID, model.EventCancelled,
					fmt.Sprintf("auto-closed after %d hours without resolution", cfg.Maintenance.AutoCloseHours))
				slog.Info(fmt.Sprintf("auto-close: incident #%d closed (stale)", inc.ID))
			}
		}))
	}

	// Retention: prune old command runs and events.
	if cfg.Storage.RetentionDays > 0 {
		startTicker(time.Hour, false, withCtx(2*time.Minute, func(ctx context.Context) {
			cutoff := time.Now().UTC().Add(-time.Duration(cfg.Storage.RetentionDays) * 24 * time.Hour)
			runs, evs, err := st.PruneOlderThan(ctx, cutoff)
			if err != nil {
				slog.Info(fmt.Sprintf("retention: %v", err))
			} else if runs+evs > 0 {
				slog.Info(fmt.Sprintf("retention: pruned %d command runs, %d events", runs, evs))
			}
		}))
	}

	// Serve until the server exits or a signal arrives.
	srvErr := make(chan error, 1)
	go func() { srvErr <- ws.ListenAndServe() }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-srvErr:
		if err != nil && err != http.ErrServerClosed {
			slog.Error(fmt.Sprintf("server: %v", err))
			os.Exit(1)
		}
	case <-signals:
		slog.Info(fmt.Sprintf("shutting down"))
	}

	// Graceful shutdown: stop tickers, drain the HTTP server, then close the
	// store (deferred).
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ws.Shutdown(ctx)
	wg.Wait()
}

// pollMRMerges checks agent-created MRs for merge and marks incidents resolved.
// Checks run concurrently (bounded) since each is an independent HTTP round trip.
func pollMRMerges(ctx context.Context, gl *gitlab.Client, st *store.Store, projectID string) {
	incs, err := st.ListIncidentsWithMR(ctx, 100)
	if err != nil {
		slog.Info(fmt.Sprintf("mr poll: list: %v", err))
		return
	}
	const workers = 8
	jobs := make(chan *model.Incident)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for inc := range jobs {
				pollOneMR(ctx, gl, st, projectID, inc)
			}
		}()
	}
	for _, inc := range incs {
		select {
		case jobs <- inc:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

// pollOneMR checks the state of a single incident's merge request.
func pollOneMR(ctx context.Context, gl *gitlab.Client, st *store.Store, projectID string, inc *model.Incident) {
	iid, ok := gitlab.ParseMRID(inc.MRURL)
	if !ok {
		return
	}
	state, err := gl.GetMRState(ctx, projectID, iid)
	if err != nil {
		slog.Info(fmt.Sprintf("mr poll: state of #%d (mr %d): %v", inc.ID, iid, err))
		return
	}
	switch state {
	case "merged":
		if err := st.MarkResolved(ctx, inc.ID, "mr_merged"); err != nil {
			slog.Info(fmt.Sprintf("mr poll: resolve #%d: %v", inc.ID, err))
			return
		}
		_, _ = st.AddEvent(ctx, inc.ID, model.EventMRMerged, inc.MRURL)
		slog.Info(fmt.Sprintf("mr poll: incident #%d resolved via merged MR %d", inc.ID, iid))
	case "closed":
		has, _ := st.HasEvent(ctx, inc.ID, model.EventMRClosed)
		if !has {
			_, _ = st.AddEvent(ctx, inc.ID, model.EventMRClosed, inc.MRURL)
			slog.Info(fmt.Sprintf("mr poll: incident #%d MR %d closed without merge", inc.ID, iid))
		}
	}
}
