// Package storetest is a contract test suite for store.Store. Every
// implementation (SQLite, memstore, and later DynamoDB) runs the same tests,
// so core logic behaves identically whichever store backs it.
package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"cloud-control/internal/provider"
	"cloud-control/internal/store"
)

// Run runs the contract tests. newStore must return an empty store.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(t *testing.T, s store.Store)
	}{
		{"ResourcesReplacePerProvider", testResources},
		{"HealthPrevAndChangedAt", testHealth},
		{"LatestCostsPerProviderSince", testCosts},
		{"AuditNewestFirst", testAudit},
		{"BudgetAlertOncePerMonth", testBudgetAlerts},
		{"Settings", testSettings},
		{"RunLifecycle", testRuns},
		{"RunTransitionIsExclusive", testRunTransitionRace},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { s.Close() })
			tc.fn(t, s)
		})
	}
}

var t0 = time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)

func testResources(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.ReplaceResources(ctx, "azure", []provider.Resource{
		{ID: "a2", Provider: "azure", Name: "zeta", App: "library", Tags: map[string]string{"project": "athenaeum"}},
		{ID: "a1", Provider: "azure", Name: "alpha", App: "aquascale", Power: provider.PowerRunning},
	}, t0))
	must(t, s.ReplaceResources(ctx, "github", []provider.Resource{{ID: "g1", Provider: "github", Name: "ci", App: "aquascale"}}, t0))
	must(t, s.ReplaceResources(ctx, "azure", []provider.Resource{
		{ID: "a1", Provider: "azure", Name: "alpha", App: "aquascale", Power: provider.PowerStopped},
	}, t0.Add(time.Minute)))

	rs, err := s.Resources(ctx)
	must(t, err)
	if len(rs) != 2 {
		t.Fatalf("got %d resources, want 2 (azure snapshot replaced, github kept): %+v", len(rs), rs)
	}
	if rs[0].ID != "a1" || rs[1].ID != "g1" {
		t.Errorf("order = %s, %s; want a1 (aquascale/azure), g1 (aquascale/github)", rs[0].ID, rs[1].ID)
	}
	if rs[0].Power != provider.PowerStopped {
		t.Errorf("power = %q, want stopped", rs[0].Power)
	}
}

func testHealth(t *testing.T, s store.Store) {
	ctx := context.Background()
	_, had, err := s.RecordHealth(ctx, "r1", provider.HealthStatus{State: provider.Healthy, CheckedAt: t0})
	must(t, err)
	if had {
		t.Fatal("first record reported a previous state")
	}
	prev, had, err := s.RecordHealth(ctx, "r1", provider.HealthStatus{State: provider.Healthy, CheckedAt: t0.Add(5 * time.Minute)})
	must(t, err)
	if !had || prev != provider.Healthy {
		t.Fatalf("prev = %q had=%v", prev, had)
	}
	prev, _, _ = s.RecordHealth(ctx, "r1", provider.HealthStatus{State: provider.Down, Detail: "502", CheckedAt: t0.Add(10 * time.Minute)})
	if prev != provider.Healthy {
		t.Fatalf("prev = %q", prev)
	}
	cur, err := s.CurrentHealth(ctx)
	must(t, err)
	got := cur["r1"]
	if got.State != provider.Down || got.Detail != "502" {
		t.Errorf("current = %+v", got)
	}
	if !got.ChangedAt.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("ChangedAt = %v, want time of the Down check", got.ChangedAt)
	}
	must(t, s.PruneHealth(ctx, t0.Add(time.Hour)))
	if cur, _ := s.CurrentHealth(ctx); cur["r1"].State != provider.Down {
		t.Error("pruning history must not remove current state")
	}
}

func testCosts(t *testing.T, s store.Store) {
	ctx := context.Background()
	monthStart := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	must(t, s.RecordCost(ctx, provider.CostReport{Provider: "azure", MTD: 1, Currency: "USD", AsOf: t0}, t0))
	must(t, s.RecordCost(ctx, provider.CostReport{Provider: "azure", MTD: 2, Currency: "USD", AsOf: t0,
		ByResource: map[string]float64{"a1": 1.5}}, t0.Add(6*time.Hour)))
	must(t, s.RecordCost(ctx, provider.CostReport{Provider: "gcp", MTD: 9, Currency: "USD", Estimated: true, AsOf: t0},
		monthStart.Add(-time.Hour))) // last month
	got, err := s.LatestCosts(ctx, monthStart)
	must(t, err)
	if len(got) != 1 {
		t.Fatalf("got %v; want only azure (gcp's latest is from last month)", got)
	}
	if got["azure"].MTD != 2 || got["azure"].ByResource["a1"] != 1.5 {
		t.Errorf("azure = %+v, want the newest report", got["azure"])
	}
}

func testAudit(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, a := range []string{"stop", "start", "autostop-on"} {
		must(t, s.Audit(ctx, store.AuditEntry{At: t0, Actor: "42", Action: a, Result: "ok"}))
	}
	got, err := s.RecentAudit(ctx, 2)
	must(t, err)
	if len(got) != 2 || got[0].Action != "autostop-on" || got[1].Action != "start" {
		t.Fatalf("got %+v", got)
	}
	if got[0].ID == 0 || got[0].ID == got[1].ID {
		t.Errorf("IDs must be set and unique: %d, %d", got[0].ID, got[1].ID)
	}
}

func testBudgetAlerts(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i, want := range []bool{true, false} {
		got, err := s.MarkBudgetAlert(ctx, "2026-11", 80, t0)
		must(t, err)
		if got != want {
			t.Fatalf("call %d = %v, want %v", i, got, want)
		}
	}
	if got, _ := s.MarkBudgetAlert(ctx, "2026-11", 100, t0); !got {
		t.Error("different threshold should fire")
	}
	if got, _ := s.MarkBudgetAlert(ctx, "2026-12", 80, t0); !got {
		t.Error("new month should fire again")
	}
}

func testSettings(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, ok, err := s.Setting(ctx, "k"); ok || err != nil {
		t.Fatalf("missing key: ok=%v err=%v", ok, err)
	}
	must(t, s.SetSetting(ctx, "k", "true"))
	must(t, s.SetSetting(ctx, "k", "false"))
	if v, ok, _ := s.Setting(ctx, "k"); !ok || v != "false" {
		t.Errorf("got %q %v", v, ok)
	}
}

func testRuns(t *testing.T, s store.Store) {
	ctx := context.Background()
	if r, err := s.Run(ctx, "2026-11"); r != nil || err != nil {
		t.Fatalf("empty store returned %+v, %v", r, err)
	}
	run := store.AutoStopRun{Month: "2026-11", Status: store.RunPending, Deadline: t0.Add(15 * time.Minute),
		Plan:   []store.PlanItem{{ResourceID: "a1", Provider: "azure", Name: "alpha", App: "aquascale"}},
		DryRun: true, Spend: 21.5, CreatedAt: t0}
	if ok, err := s.CreateRun(ctx, run); !ok || err != nil {
		t.Fatalf("create: %v %v", ok, err)
	}
	if ok, _ := s.CreateRun(ctx, run); ok {
		t.Fatal("second create for the same month must be ignored")
	}

	got, _ := s.Run(ctx, "2026-11")
	if got.Status != store.RunPending || !got.DryRun || got.Spend != 21.5 || len(got.Plan) != 1 ||
		got.Plan[0].App != "aquascale" || !got.Deadline.Equal(run.Deadline) {
		t.Errorf("run = %+v", got)
	}
	got.Plan[0].Name = "mutated"
	if again, _ := s.Run(ctx, "2026-11"); again.Plan[0].Name != "alpha" {
		t.Error("returned run aliases stored state")
	}

	if pend, _ := s.RunsWithStatus(ctx, store.RunPending); len(pend) != 1 {
		t.Errorf("pending runs = %d", len(pend))
	}
	if ok, _ := s.TransitionRun(ctx, "2026-11", store.RunPending, store.RunCancelled, t0); !ok {
		t.Fatal("pending → cancelled failed")
	}
	if ok, _ := s.DeleteRun(ctx, "2026-11", store.RunDone); ok {
		t.Error("deleted a run whose status was not listed")
	}
	if ok, _ := s.DeleteRun(ctx, "2026-11", store.RunDone, store.RunCancelled); !ok {
		t.Error("delete of cancelled run failed")
	}
	if r, _ := s.Run(ctx, "2026-11"); r != nil {
		t.Error("run still present after delete")
	}
}

// Two timers (or a timer and the overdue sweep) racing to execute the same
// run: exactly one must win the pending → executing transition.
func testRunTransitionRace(t *testing.T, s store.Store) {
	ctx := context.Background()
	if ok, err := s.CreateRun(ctx, store.AutoStopRun{Month: "2026-11", Status: store.RunPending, CreatedAt: t0}); !ok || err != nil {
		t.Fatalf("create: %v %v", ok, err)
	}
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.TransitionRun(ctx, "2026-11", store.RunPending, store.RunExecuting, t0)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d goroutines won the transition, want exactly 1", wins)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
