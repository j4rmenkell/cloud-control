// Package scheduler runs the periodic jobs: health poll, cost poll (which
// drives budget alerts and auto-stop) and the daily digest.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"cloud-control/internal/config"
	"cloud-control/internal/service"
)

type Scheduler struct {
	c   *cron.Cron
	svc *service.Service
	log *slog.Logger
}

func New(ctx context.Context, cfg *config.Config, svc *service.Service, log *slog.Logger) (*Scheduler, error) {
	cl := cronLogger{log}
	c := cron.New(cron.WithLocation(cfg.Location), cron.WithChain(cron.Recover(cl), cron.SkipIfStillRunning(cl)))
	s := &Scheduler{c: c, svc: svc, log: log}
	hour, minute, err := cfg.DigestTime()
	if err != nil {
		return nil, err
	}
	jobs := []struct {
		name, spec string
		run        func(context.Context)
	}{
		{"health", "@every " + cfg.Schedule.HealthEvery, func(ctx context.Context) {
			if _, err := svc.PollHealth(ctx); err != nil {
				log.Warn("health poll", "err", err)
			}
		}},
		{"cost", "@every " + cfg.Schedule.CostEvery, func(ctx context.Context) { svc.PollCost(ctx) }},
		{"digest", fmt.Sprintf("%d %d * * *", minute, hour), func(ctx context.Context) {
			if err := svc.Digest(ctx); err != nil {
				log.Error("daily digest", "err", err)
			}
		}},
	}
	for _, j := range jobs {
		if _, err := c.AddFunc(j.spec, s.wrap(ctx, j.name, j.run)); err != nil {
			return nil, fmt.Errorf("schedule %s (%q): %w", j.name, j.spec, err)
		}
	}
	return s, nil
}

func (s *Scheduler) wrap(ctx context.Context, name string, f func(context.Context)) func() {
	return func() {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		f(ctx)
		s.log.Info("job finished", "job", name, "took", time.Since(start).Round(time.Millisecond))
	}
}

func (s *Scheduler) Start() { s.c.Start() }

// Stop stops scheduling and waits for running jobs.
func (s *Scheduler) Stop() { <-s.c.Stop().Done() }

type cronLogger struct{ l *slog.Logger }

func (c cronLogger) Info(msg string, kv ...any) { c.l.Debug("cron: "+msg, kv...) }
func (c cronLogger) Error(err error, msg string, kv ...any) {
	c.l.Error("cron: "+msg, append(kv, "err", err)...)
}
