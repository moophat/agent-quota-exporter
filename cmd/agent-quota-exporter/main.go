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

// processStart is taken as the binary initializes, before config loading, so process_start_time_seconds covers
// the whole run.
var processStart = time.Now()

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-quota-exporter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", os.Getenv("AGENT_QUOTA_EXPORTER_CONFIG"), "config file (YAML)")
	once := fs.Bool("once", false, "collect local sources once, print /metrics to stdout and exit")
	onceQuota := fs.Bool("once-with-quota", false, "with --once, also run the opt-in quota sources (Claude's quota endpoint, the agy client) if their gates allow")
	checkConfig := fs.Bool("check-config", false, "load and validate the config, print the result and exit 0 (valid) or 1 (invalid); listens on nothing and writes nothing")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "agent-quota-exporter %s (contract %s)\n", version, core.ContractVersion)
		return 0
	}
	if *onceQuota && !*once {
		// Alone, the flag was ignored by the one-shot path while the server still started, so a probe meant
		// as a single request became a long-lived exporter that polls the vendor.
		fmt.Fprintln(stderr, "--once-with-quota requires --once")
		return 2
	}
	if *checkConfig {
		if _, err := config.Load(*cfgPath); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "config ok")
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
		log.Info("profile", "harness", p.Harness, "alias", p.Alias, "origin", p.Origin, "quota", p.AnyQuota())
	}
	built, err := app.Build(cfg, profiles, host, version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer built.Events.Close()

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
	exp := &core.Exposition{Host: host, Static: built.Static, Version: version, ProcessStart: processStart,
		Profiles: built.Profiles, Sched: sched, Clock: core.RealClock, ExtraSamples: built.EventSamples}

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
