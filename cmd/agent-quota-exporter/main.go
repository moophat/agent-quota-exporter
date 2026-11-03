// Command agent-quota-exporter is a read-only Prometheus exporter for AI coding-agent quota and usage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/app"
	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-quota-exporter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", os.Getenv("AGENT_QUOTA_EXPORTER_CONFIG"), "config file (YAML)")
	once := fs.Bool("once", false, "collect local sources once, print /metrics to stdout and exit")
	onceQuota := fs.Bool("once-with-quota", false, "with --once, also ask the vendor quota endpoint if its 15 min gate allows")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "agent-quota-exporter %s (contract %s)\n", version, core.ContractVersion)
		return 0
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	host := cfg.Host
	if host == "" {
		if host, err = os.Hostname(); err != nil {
			host = "unknown"
		}
	}
	profiles, warns := cfg.Resolve(os.Getenv)
	for _, w := range warns {
		log.Warn(w)
	}
	for _, p := range profiles {
		log.Info("profile", "harness", p.Harness, "alias", p.Alias, "origin", p.Origin, "quota", p.QuotaOptIn)
	}
	built := app.Build(cfg, profiles, host, version)

	now := core.RealClock.Now()
	st, err := core.OpenState(cfg.StateDir, now)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()
	if st.Quarantined {
		log.Warn("state.json was unreadable and has been moved aside; vendor sources start backed off")
	}
	sched := core.NewScheduler(built.Sources, st, core.RealClock, log, cfg.Defaults.StaleAfter.D())
	exp := &core.Exposition{Host: host, Static: built.Static, Version: version, ProcessStart: now,
		Profiles: built.Profiles, Sched: sched, Clock: core.RealClock}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Accounts first, so every other plugin labels its first values with the resolved account.
	sched.Tick(ctx, app.IsAccount, true, true)

	if *once {
		sched.Tick(ctx, app.OnceFilter(*onceQuota), true, true)
		if err := exp.Render(stdout); err != nil {
			return 1
		}
		return 0
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "listen %s: %v\n", cfg.Listen, err)
		return 1
	}
	srv := &http.Server{Handler: exp.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	}()
	go sched.Loop(ctx)
	log.Info("listening", "addr", ln.Addr().String(), "version", version, "profiles", len(profiles))
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, err)
		return 1
	}
	sched.Wait()
	return 0
}
