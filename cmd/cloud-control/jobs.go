package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"cloud-control/internal/config"
	"cloud-control/internal/core"
)

// Schedules from spec §6. The digest runs at 09:00 in the configured timezone.
const (
	healthEvery = "@every 5m"
	costEvery   = "@every 6h"
	digestAt    = "0 9 * * *"
)

// startJobs schedules the health poll, cost poll (which drives budget alerts
// and auto-stop) and daily digest. Call the returned func to stop scheduling
// and wait for running jobs.
func startJobs(ctx context.Context, cfg *config.Config, svc *core.Service, log *slog.Logger) (stop func(), err error) {
	cl := cronLogger{log}
	c := cron.New(cron.WithLocation(cfg.Location), cron.WithChain(cron.Recover(cl), cron.SkipIfStillRunning(cl)))
	jobs := []struct {
		name, spec string
		run        func(context.Context)
	}{
		{"health", healthEvery, func(ctx context.Context) {
			if _, err := svc.PollHealth(ctx); err != nil {
				log.Warn("health poll", "err", err)
			}
		}},
		{"cost", costEvery, func(ctx context.Context) { svc.PollCost(ctx) }},
		{"digest", digestAt, func(ctx context.Context) {
			if err := svc.Digest(ctx); err != nil {
				log.Error("daily digest", "err", err)
			}
		}},
	}
	for _, j := range jobs {
		if _, err := c.AddFunc(j.spec, func() {
			if ctx.Err() != nil {
				return
			}
			start := time.Now()
			j.run(ctx)
			log.Info("job finished", "job", j.name, "took", time.Since(start).Round(time.Millisecond))
		}); err != nil {
			return nil, err
		}
	}
	c.Start()
	return func() { <-c.Stop().Done() }, nil
}

type cronLogger struct{ l *slog.Logger }

func (c cronLogger) Info(msg string, kv ...any) { c.l.Debug("cron: "+msg, kv...) }
func (c cronLogger) Error(err error, msg string, kv ...any) {
	c.l.Error("cron: "+msg, append(kv, "err", err)...)
}
