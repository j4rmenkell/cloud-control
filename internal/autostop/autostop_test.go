package autostop

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud-control/internal/apps"
	"cloud-control/internal/config"
	"cloud-control/internal/notify"
	"cloud-control/internal/provider"
	"cloud-control/internal/provider/fake"
	"cloud-control/internal/service"
	"cloud-control/internal/store"
)

// ---- manual clock and timers ----

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type timers struct {
	mu   sync.Mutex
	list []*mtimer
}

type mtimer struct {
	mu             sync.Mutex
	d              time.Duration
	f              func()
	stopped, fired bool
}

func (t *mtimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

func (ts *timers) AfterFunc(d time.Duration, f func()) Timer {
	t := &mtimer{d: d, f: f}
	ts.mu.Lock()
	ts.list = append(ts.list, t)
	ts.mu.Unlock()
	return t
}

// FireAll runs every live timer, as if its deadline passed.
func (ts *timers) FireAll() int {
	ts.mu.Lock()
	list := append([]*mtimer(nil), ts.list...)
	ts.mu.Unlock()
	n := 0
	for _, t := range list {
		t.mu.Lock()
		live := !t.stopped && !t.fired
		t.fired = true
		t.mu.Unlock()
		if live {
			t.f()
			n++
		}
	}
	return n
}

// ---- harness ----

type harness struct {
	cfg    *config.Config
	st     *store.Store
	fake   *fake.Provider
	rec    *notify.Recorder
	svc    *service.Service
	engine *Engine
	clock  *clock
	timers *timers
}

const baseYAML = `
timezone: Asia/Manila
monthly_budget: 20
auto_stop:
  enabled: true
  threshold_percent: 100
  grace_minutes: 15
apps:
  aquascale: { match_tags: { app: aquascale }, critical: true }
  buddibucks: { match_tags: { app: buddibucks } }
`

func setup(t *testing.T, extraYAML string, dbPath string) *harness {
	t.Helper()
	cfg, err := config.Parse([]byte(baseYAML + extraYAML))
	if err != nil {
		t.Fatal(err)
	}
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "t.db")
	}
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &clock{now: time.Date(2026, 11, 20, 10, 0, 0, 0, cfg.Location)}
	f := fake.Demo()
	// A second running, non-critical, unassigned resource.
	f.AddResource(provider.Resource{ID: "fake:aca:scratch-job", Type: "containerapp", Name: "scratch-job", Power: provider.PowerRunning})
	rec := &notify.Recorder{}
	res := apps.NewResolver(cfg)
	svc := service.New(service.Options{Config: cfg, Store: st, Apps: res, Notifier: rec, Now: clk.Now,
		Providers: []provider.Provider{f}})
	tm := &timers{}
	eng := New(Options{Config: cfg, Store: st, Apps: res, Control: svc, Notifier: rec, Now: clk.Now, AfterFunc: tm.AfterFunc})
	svc.SetCostHook(eng)
	if err := eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &harness{cfg, st, f, rec, svc, eng, clk, tm}
}

func (h *harness) spend(v float64) { h.fake.SetCost(provider.CostReport{MTD: v}) }

func (h *harness) stopped() []string {
	var out []string
	for _, c := range h.fake.Calls() {
		if c.Action == provider.ActionStop {
			out = append(out, c.ResourceID)
		}
	}
	return out
}

func (h *harness) lastTitle() string {
	m := h.rec.Messages()
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1].Message.Title
}

func findTitle(msgs []notify.Sent, sub string) *notify.Message {
	for _, m := range msgs {
		if strings.Contains(m.Message.Title, sub) {
			return &m.Message
		}
	}
	return nil
}

// ---- tests ----

func TestBelowThresholdDoesNothing(t *testing.T) {
	h := setup(t, "  # armed\n", "")
	h.spend(19.99)
	h.svc.PollCost(context.Background())
	if run, _ := h.st.Run(context.Background(), "2026-11"); run != nil {
		t.Fatalf("run created below threshold: %+v", run)
	}
}

func TestDryRunWarnsButNeverStops(t *testing.T) {
	h := setup(t, "", "") // dry_run defaults to true
	ctx := context.Background()
	h.spend(20)
	h.svc.PollCost(ctx)

	warn := findTitle(h.rec.Messages(), "DRY RUN")
	if warn == nil {
		t.Fatalf("no dry-run warning; got %q", h.lastTitle())
	}
	if !warn.MentionOwner || len(warn.Buttons) != 1 || warn.Buttons[0].CustomID != CancelPrefix+"2026-11" {
		t.Errorf("warning missing mention or cancel button: %+v", warn)
	}
	if strings.Contains(warn.Body, "aquascale-api") {
		t.Error("critical app's resource in plan")
	}
	if !strings.Contains(warn.Body, "buddibucks-worker") || !strings.Contains(warn.Body, "scratch-job") {
		t.Errorf("plan incomplete: %s", warn.Body)
	}

	if n := h.timers.FireAll(); n != 1 {
		t.Fatalf("fired %d timers", n)
	}
	h.engine.Wait()
	if s := h.stopped(); len(s) != 0 {
		t.Fatalf("dry run stopped %v", s)
	}
	if !strings.Contains(h.lastTitle(), "dry run finished") {
		t.Errorf("last = %q", h.lastTitle())
	}
	entries, _ := h.st.RecentAudit(ctx, 10)
	if len(entries) != 2 || entries[0].Result != "dry-run" || entries[0].Actor != Actor {
		t.Errorf("audit = %+v", entries)
	}
}

func armed(t *testing.T, dbPath string) *harness {
	t.Helper()
	return setup(t, "", dbPath).withDryRun(false)
}

func (h *harness) withDryRun(v bool) *harness {
	h.cfg.AutoStop.DryRun = &v
	return h
}

func TestArmedRun(t *testing.T) {
	h := armed(t, "")
	ctx := context.Background()
	h.fake.SetDoErr("fake:aca:buddibucks-worker", errors.New("arm 409"))
	h.spend(25)
	h.svc.PollCost(ctx)
	if findTitle(h.rec.Messages(), "Auto-stop in 15 min") == nil {
		t.Fatalf("no warning; last %q", h.lastTitle())
	}
	if len(h.stopped()) != 0 {
		t.Fatal("stopped before grace period ended")
	}
	h.timers.FireAll()
	h.engine.Wait()

	got := strings.Join(h.stopped(), ",")
	if got != "fake:aca:buddibucks-worker,fake:aca:scratch-job" {
		t.Fatalf("stop calls = %s", got)
	}
	summary := h.rec.Messages()[len(h.rec.Messages())-1].Message
	if !strings.Contains(summary.Body, "Stopped (1)") || !strings.Contains(summary.Body, "Failed (1)") || !summary.MentionOwner {
		t.Errorf("summary = %+v", summary)
	}
	run, _ := h.st.Run(ctx, "2026-11")
	if run.Status != store.RunDone {
		t.Errorf("status = %s", run.Status)
	}
	entries, _ := h.st.RecentAudit(ctx, 10)
	for _, e := range entries {
		if e.Actor != Actor {
			t.Errorf("audit actor = %q", e.Actor)
		}
	}

	// Once per month: spend keeps rising, nothing new happens.
	h.rec.Reset()
	h.spend(40)
	h.svc.PollCost(ctx)
	if findTitle(h.rec.Messages(), "Auto-stop") != nil {
		t.Error("auto-stop fired twice in one month")
	}
	// Never auto-start.
	for _, c := range h.fake.Calls() {
		if c.Action == provider.ActionStart {
			t.Error("auto-stop started something")
		}
	}
}

func TestCancelSkipsRestOfMonthUntilReenabled(t *testing.T) {
	h := armed(t, "")
	ctx := context.Background()
	h.spend(25)
	h.svc.PollCost(ctx)
	ok, err := h.engine.Cancel(ctx, "2026-11", "42")
	if err != nil || !ok {
		t.Fatalf("cancel: %v %v", ok, err)
	}
	if n := h.timers.FireAll(); n != 0 {
		t.Fatalf("cancelled timer still fired (%d)", n)
	}
	h.engine.Wait()
	if len(h.stopped()) != 0 {
		t.Fatal("stopped after cancel")
	}
	if ok, _ := h.engine.Cancel(ctx, "2026-11", "42"); ok {
		t.Error("double cancel succeeded")
	}

	h.svc.PollCost(ctx)
	if h.timers.FireAll() != 0 {
		t.Fatal("re-triggered after cancel in the same month")
	}

	if err := h.engine.SetEnabled(ctx, true, "42"); err != nil {
		t.Fatal(err)
	}
	h.svc.PollCost(ctx)
	if h.timers.FireAll() != 1 {
		t.Fatal("/autostop on should allow a new run")
	}
	h.engine.Wait()
	if len(h.stopped()) != 2 {
		t.Errorf("stopped = %v", h.stopped())
	}
}

func TestDisabledAndUnknownNeverTrigger(t *testing.T) {
	h := armed(t, "")
	ctx := context.Background()
	h.spend(100)
	h.fake.SetCostErr(errors.New("billing down"))
	h.svc.PollCost(ctx)
	if run, _ := h.st.Run(ctx, "2026-11"); run != nil {
		t.Fatal("triggered on unknown total")
	}

	h.fake.SetCostErr(nil)
	if err := h.engine.SetEnabled(ctx, false, "42"); err != nil {
		t.Fatal(err)
	}
	h.svc.PollCost(ctx)
	if run, _ := h.st.Run(ctx, "2026-11"); run != nil {
		t.Fatal("triggered while disabled")
	}
	if on, src := h.engine.Enabled(ctx); on || src != "runtime" {
		t.Errorf("enabled = %v (%s)", on, src)
	}
}

func TestGraceTimerSurvivesRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "shared.db")
	h1 := armed(t, db)
	ctx := context.Background()
	h1.spend(25)
	h1.svc.PollCost(ctx)
	h1.engine.Stop() // process dies before the deadline
	h1.st.Close()

	h2 := armed(t, db) // restart: Start() resumes the pending run
	if n := h2.timers.FireAll(); n != 1 {
		t.Fatalf("resumed %d timers", n)
	}
	h2.engine.Wait()
	if len(h2.stopped()) != 2 {
		t.Fatalf("stopped after restart = %v", h2.stopped())
	}
}

func TestNewMonthCanFireAgain(t *testing.T) {
	h := armed(t, "")
	ctx := context.Background()
	h.spend(25)
	h.svc.PollCost(ctx)
	h.timers.FireAll()
	h.engine.Wait()

	h.clock.Advance(15 * 24 * time.Hour) // into December
	h.svc.PollCost(ctx)
	if run, _ := h.st.Run(ctx, "2026-12"); run == nil {
		t.Fatal("no run in the new month")
	}
}

func TestParseCancelID(t *testing.T) {
	if m, ok := ParseCancelID(CancelPrefix + "2026-11"); !ok || m != "2026-11" {
		t.Error("valid id rejected")
	}
	if _, ok := ParseCancelID(CancelPrefix + "nope"); ok {
		t.Error("invalid month accepted")
	}
}
