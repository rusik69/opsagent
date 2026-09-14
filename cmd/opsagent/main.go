package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
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
		log.Fatalf("config: %v", err)
	}

	// Storage.
	st, err := store.Open(cfg.Storage.Path)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	defer st.Close()

	// SSH allowlist + executor.
	al, err := sshx.NewAllowlist(sshx.DefaultAllowlist())
	if err != nil {
		log.Fatalf("allowlist: %v", err)
	}
	if len(cfg.Allowlist.Commands) > 0 {
		extra := make([]sshx.Command, 0, len(cfg.Allowlist.Commands))
		for _, c := range cfg.Allowlist.Commands {
			extra = append(extra, sshx.Command{ID: c.ID, Description: c.Description, Template: c.Template, Params: c.Params})
		}
		merged := append(sshx.DefaultAllowlist(), extra...)
		al, err = sshx.NewAllowlist(merged)
		if err != nil {
			log.Fatalf("allowlist: %v", err)
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
		log.Printf("warning: no hosts configured; using fake runner (all commands return empty)")
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
			log.Fatalf("ssh: %v", err)
		}
		defer sshClient.Close()
		runner = sshClient
	}

	executor := sshx.NewExecutor(al, runner, st)

	// Repos.
	reposMgr, err := repos.NewManager(cfg.Repos, "./data/repos")
	if err != nil {
		log.Fatalf("repos: %v", err)
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		out, err := reposMgr.Sync(ctx)
		cancel()
		if err != nil {
			log.Printf("repos initial sync: %v", err)
		} else {
			log.Printf("repos sync:\n%s", out)
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
		log.Printf("gitlab configured: %s", cfg.GitLab.BaseURL)
	} else {
		log.Printf("warning: gitlab not configured; create_gitlab_mr tool will be unavailable")
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
		log.Fatalf("mcp: %v", err)
	}
	log.Printf("MCP tools: %d registered", len(mcpSrv.MCPServer().ListTools()))

	// Agent.
	llm := agent.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.Model)
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
		log.Fatalf("web: %v", err)
	}

	// Self-improvement reviewer (background interval + manual API trigger).
	var rv *review.Reviewer
	if cfg.Review.Enabled && cfg.LLM.Enabled {
		rv = review.New(st, llm, mcpSrv, review.Config{
			Limit: cfg.Review.Limit, MinIncidents: cfg.Review.MinIncidents, MaxSummaryLen: cfg.Review.MaxSummaryLen,
		})
		ws.SetReviewer(rv)
		go func() {
			interval := time.Duration(cfg.Review.IntervalHours) * time.Hour
			if interval <= 0 {
				interval = 6 * time.Hour
			}
			runReview := func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				retro, err := rv.Run(ctx)
				if err != nil {
					log.Printf("review: %v", err)
				} else if retro != nil {
					log.Printf("review: %d incidents reviewed, %d memories, %d instructions", retro.IncidentsReviewd, retro.MemoriesCreated, retro.InstructionsCreated)
				}
			}
			runReview()
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for range tick.C {
				runReview()
			}
		}()
	}

	go func() {
		if err := ws.ListenAndServe(); err != nil {
			log.Fatalf("server: %v", err)
		}
	}()

	// Background correlation ticker.
	if corr != nil {
		go func() {
			interval := time.Duration(cfg.Correlate.IntervalMinutes) * time.Minute
			if interval <= 0 {
				interval = 15 * time.Minute
			}
			runCorr := func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				if err := corr.Run(ctx); err != nil {
					log.Printf("correlation: %v", err)
				}
			}
			runCorr()
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for range tick.C {
				runCorr()
			}
		}()
	}

	// MR merge poller: when an agent-created MR is merged, mark the incident
	// resolved via mr_merged (outcome feedback for the review loop).
	if gl != nil && cfg.GitLab.PollMRSeconds > 0 {
		go func() {
			interval := time.Duration(cfg.GitLab.PollMRSeconds) * time.Second
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for range tick.C {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				pollMRMerges(ctx, gl, st, cfg.GitLab.DefaultProjectID)
				cancel()
			}
		}()
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	<-shutdown
	log.Printf("shutting down")
}

// pollMRMerges checks agent-created MRs for merge and marks incidents resolved.
func pollMRMerges(ctx context.Context, gl *gitlab.Client, st *store.Store, projectID string) {
	incs, err := st.ListIncidentsWithMR(ctx, 100)
	if err != nil {
		log.Printf("mr poll: list: %v", err)
		return
	}
	for _, inc := range incs {
		iid, ok := gitlab.ParseMRID(inc.MRURL)
		if !ok {
			continue
		}
		state, err := gl.GetMRState(ctx, projectID, iid)
		if err != nil {
			log.Printf("mr poll: state of #%d (mr %d): %v", inc.ID, iid, err)
			continue
		}
		switch state {
		case "merged":
			if err := st.MarkResolved(ctx, inc.ID, "mr_merged"); err != nil {
				log.Printf("mr poll: resolve #%d: %v", inc.ID, err)
				continue
			}
			_, _ = st.AddEvent(ctx, inc.ID, model.EventMRMerged, inc.MRURL)
			log.Printf("mr poll: incident #%d resolved via merged MR %d", inc.ID, iid)
		case "closed":
			has, _ := st.HasEvent(ctx, inc.ID, model.EventMRClosed)
			if !has {
				_, _ = st.AddEvent(ctx, inc.ID, model.EventMRClosed, inc.MRURL)
				log.Printf("mr poll: incident #%d MR %d closed without merge", inc.ID, iid)
			}
		}
	}
}
