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

	"cloud-control/internal/apps"
	"cloud-control/internal/autostop"
	"cloud-control/internal/bot"
	"cloud-control/internal/config"
	"cloud-control/internal/provider"
	"cloud-control/internal/provider/fake"
	"cloud-control/internal/scheduler"
	"cloud-control/internal/service"
	"cloud-control/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	envPath := flag.String("env", ".env", "path to .env (optional)")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()

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
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	providers, err := buildProviders(cfg, log)
	if err != nil {
		return err
	}

	resolver := apps.NewResolver(cfg)
	b := bot.New(cfg, nil, log) // service is attached below; bot is the notifier
	svc := service.New(service.Options{
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

	sched, err := scheduler.New(ctx, cfg, svc, log)
	if err != nil {
		return err
	}

	// Initial polls so /status and /cost have data right away.
	go func() {
		if _, err := svc.PollHealth(ctx); err != nil {
			log.Warn("initial health poll", "err", err)
		}
		svc.PollCost(ctx)
	}()
	sched.Start()
	log.Info("cloud-control running", "providers", svc.ProviderNames(), "apps", resolver.Names(),
		"auto_stop", cfg.AutoStop.Enabled, "dry_run", cfg.AutoStop.IsDryRun())

	<-ctx.Done()
	log.Info("shutting down")
	sched.Stop()
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
