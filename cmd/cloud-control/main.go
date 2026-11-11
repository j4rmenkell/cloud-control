// Command cloud-control discovers cloud resources, tracks health and cost,
// and exposes owner-only controls through a private Discord server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"cloud-control/internal/autostop"
	"cloud-control/internal/config"
	"cloud-control/internal/core"
	"cloud-control/internal/discord"
	"cloud-control/internal/provider"
	"cloud-control/internal/provider/fake"
	"cloud-control/internal/store"
)

func main() {
	cfgPath := flag.String("config", "", "path to the config file (default: config.local.yaml if present, else config.yaml)")
	envPath := flag.String("env", ".env", "path to .env (optional)")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()
	if *cfgPath == "" {
		*cfgPath = config.DefaultPath(".")
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := run(*cfgPath, *envPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath, envPath string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(cfgPath, envPath)
	if err != nil {
		return err
	}
	log.Info("loaded config", "path", cfgPath)
	st, err := store.OpenSQLite(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	providers, err := buildProviders(cfg, log)
	if err != nil {
		return err
	}

	resolver := core.NewResolver(cfg)
	b := discord.New(cfg, nil, log) // service is attached below; bot is the notifier
	svc := core.New(core.Options{
		Config: cfg, Store: st, Apps: resolver, Notifier: b, Logger: log, Providers: providers,
	})
	b.SetService(svc)
	engine := autostop.New(autostop.Options{
		Config: cfg, Store: st, Apps: resolver, Control: svc, Notifier: b, Logger: log,
	})
	svc.SetCostHook(engine)
	b.SetEngine(engine)

	if err := b.Open(ctx); err != nil {
		return err
	}
	defer b.Close()

	if err := engine.Start(ctx); err != nil {
		return fmt.Errorf("resume auto-stop: %w", err)
	}
	defer engine.Stop()

	// Initial polls so /status and /cost have data right away.
	go func() {
		if _, err := svc.PollHealth(ctx); err != nil {
			log.Warn("initial health poll", "err", err)
		}
		svc.PollCost(ctx)
	}()
	stopJobs, err := startJobs(ctx, cfg, svc, log)
	if err != nil {
		return err
	}
	log.Info("cloud-control running", "providers", svc.ProviderNames(), "apps", resolver.Names(),
		"auto_stop", cfg.AutoStop.Enabled, "dry_run", cfg.AutoStop.IsDryRun())

	<-ctx.Done()
	log.Info("shutting down")
	stopJobs()
	return nil
}

func buildProviders(cfg *config.Config, log *slog.Logger) ([]provider.Provider, error) {
	var ps []provider.Provider
	if cfg.Dev.FakeProvider {
		log.Warn("using the demo fake provider (dev.fake_provider: true)")
		ps = append(ps, fake.Demo())
	}
	// Real providers (Azure, GCP, Cloudflare, Resend) register here as they land.
	if len(ps) == 0 {
		return nil, errors.New("no providers enabled: set dev.fake_provider: true until real providers are implemented")
	}
	return ps, nil
}
