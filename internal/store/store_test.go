package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cloud-control/internal/provider"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBudgetAlertFiresOncePerMonth(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Now()
	for i, want := range []bool{true, false} {
		got, err := s.MarkBudgetAlert(ctx, "2026-11", 80, now)
		if err != nil || got != want {
			t.Fatalf("call %d: got %v, %v", i, got, err)
		}
	}
	if got, _ := s.MarkBudgetAlert(ctx, "2026-12", 80, now); !got {
		t.Error("new month should fire again")
	}
}

func TestHealthPrevState(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Now()
	_, had, err := s.RecordHealth(ctx, "r1", provider.HealthStatus{State: provider.Healthy, CheckedAt: now})
	if err != nil || had {
		t.Fatalf("first record: had=%v err=%v", had, err)
	}
	prev, had, _ := s.RecordHealth(ctx, "r1", provider.HealthStatus{State: provider.Down, CheckedAt: now.Add(time.Minute)})
	if !had || prev != provider.Healthy {
		t.Fatalf("prev = %v had=%v", prev, had)
	}
	cur, _ := s.CurrentHealth(ctx)
	if cur["r1"].State != provider.Down || !cur["r1"].ChangedAt.Equal(now.Add(time.Minute).UTC()) {
		t.Errorf("current = %+v", cur["r1"])
	}
}

func TestRunTransitionsAreAtomic(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Now()
	run := AutoStopRun{Month: "2026-11", Status: RunPending, Deadline: now, DryRun: true, CreatedAt: now,
		Plan: []PlanItem{{ResourceID: "a", Name: "a"}}}
	if ok, err := s.CreateRun(ctx, run); !ok || err != nil {
		t.Fatalf("create: %v %v", ok, err)
	}
	if ok, _ := s.CreateRun(ctx, run); ok {
		t.Fatal("second create for same month should be ignored")
	}
	if ok, _ := s.TransitionRun(ctx, "2026-11", RunPending, RunExecuting, now); !ok {
		t.Fatal("pending → executing failed")
	}
	if ok, _ := s.TransitionRun(ctx, "2026-11", RunPending, RunCancelled, now); ok {
		t.Fatal("cancel after executing must fail")
	}
	got, _ := s.Run(ctx, "2026-11")
	if got.Status != RunExecuting || len(got.Plan) != 1 || !got.DryRun {
		t.Errorf("run = %+v", got)
	}
}

func TestAuditRoundTrip(t *testing.T) {
	s, ctx := open(t), context.Background()
	for _, a := range []string{"stop", "start"} {
		if err := s.Audit(ctx, AuditEntry{Actor: "42", Action: a, ResourceName: "api", Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RecentAudit(ctx, 1)
	if err != nil || len(got) != 1 || got[0].Action != "start" {
		t.Fatalf("got %+v %v", got, err)
	}
}
