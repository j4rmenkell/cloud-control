package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloud-control/internal/config"
	"cloud-control/internal/provider"
	"cloud-control/internal/provider/fake"
	"cloud-control/internal/store"
	"cloud-control/internal/store/memstore"
)

type harness struct {
	svc  *Service
	fake *fake.Provider
	rec  *Recorder
	st   store.Store
}

func setup(t *testing.T) *harness {
	t.Helper()
	cfg, err := config.Parse([]byte(`
timezone: Asia/Manila
monthly_budget: 20
apps:
  aquascale: { match_tags: { project: aquascale }, alerts_channel_id: "aq" }
  buddibucks: { match_tags: { project: buddibucks } }
`))
	if err != nil {
		t.Fatal(err)
	}
	st := memstore.New()
	f := fake.Demo()
	rec := &Recorder{}
	svc := New(Options{Config: cfg, Store: st, Apps: NewResolver(cfg), Notifier: rec, Providers: []provider.Provider{f}})
	return &harness{svc, f, rec, st}
}

func TestInventoryAssignsApps(t *testing.T) {
	h := setup(t)
	inv, err := h.svc.RefreshInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range inv {
		got[r.Name] = r.App
	}
	if got["aquascale-api"] != "aquascale" || got["buddibucks-worker"] != "buddibucks" || got["scratch"] != "" {
		t.Errorf("apps = %v", got)
	}
	if inv[len(inv)-1].App != "" {
		t.Error("unassigned resources should sort last")
	}
}

func TestInventoryKeepsSnapshotWhenProviderFails(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	if _, err := h.svc.RefreshInventory(ctx); err != nil {
		t.Fatal(err)
	}
	h.fake.SetListErr(errors.New("boom"))
	inv, err := h.svc.RefreshInventory(ctx)
	if err == nil || len(inv) != 4 {
		t.Fatalf("got %d resources, err %v", len(inv), err)
	}
}

func TestHealthAlertsOnlyOnChange(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	if ts, _ := h.svc.PollHealth(ctx); len(ts) != 0 {
		t.Fatalf("first all-healthy poll should not alert, got %d", len(ts))
	}
	h.fake.SetHealth("fake:aca:aquascale-api", provider.Down)
	if ts, _ := h.svc.PollHealth(ctx); len(ts) != 1 || ts[0].From != provider.Healthy {
		t.Fatalf("transitions = %+v", ts)
	}
	if ts, _ := h.svc.PollHealth(ctx); len(ts) != 0 {
		t.Fatal("unchanged Down should not alert again")
	}
	h.fake.SetHealth("fake:aca:aquascale-api", provider.Healthy)
	if ts, _ := h.svc.PollHealth(ctx); len(ts) != 1 {
		t.Fatal("recovery should alert")
	}
	msgs := h.rec.Messages()
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages", len(msgs))
	}
	if msgs[0].Target.App != "aquascale" || !strings.Contains(msgs[0].Message.Title, "[aquascale]") {
		t.Errorf("alert not routed to app: %+v", msgs[0])
	}
}

func TestCostByAppAndBudgetAlerts(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	h.fake.SetCost(provider.CostReport{MTD: 17, ByResource: map[string]float64{
		"fake:aca:aquascale-api": 10, "fake:aca:buddibucks-worker": 5}})
	sum := h.svc.PollCost(ctx)
	if !sum.Known || sum.Percent != 85 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.ByApp["aquascale"] != 10 || sum.ByApp["buddibucks"] != 5 || sum.ByApp[""] != 2 {
		t.Errorf("by app = %v", sum.ByApp)
	}
	msgs := h.rec.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Message.Title, "80%") {
		t.Fatalf("expected one 80%% alert, got %+v", msgs)
	}
	h.rec.Reset()
	h.svc.PollCost(ctx)
	if len(h.rec.Messages()) != 0 {
		t.Error("budget alert fired twice in one month")
	}
	h.fake.SetCost(provider.CostReport{MTD: 21})
	h.svc.PollCost(ctx)
	if m := h.rec.Messages(); len(m) != 1 || !m[0].Message.MentionOwner {
		t.Errorf("100%% alert should mention owner: %+v", m)
	}
}

func TestCostFailureIsUnknown(t *testing.T) {
	h := setup(t)
	h.fake.SetCost(provider.CostReport{MTD: 50})
	h.fake.SetCostErr(errors.New("billing api down"))
	sum := h.svc.PollCost(context.Background())
	if sum.Known {
		t.Fatal("failed provider must make total unknown")
	}
	msgs := h.rec.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Message.Title, "unknown") {
		t.Errorf("expected unknown-total warning, got %+v", msgs)
	}
}

func TestControlAuditsAndChecksCapability(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	r, ok := h.svc.Find(ctx, provider.ShortID("fake:aca:aquascale-api"))
	if !ok {
		t.Fatal("find by short ID failed")
	}
	if err := h.svc.Control(ctx, "42", r, provider.ActionStart); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("start on running resource: %v", err)
	}
	if err := h.svc.Control(ctx, "42", r, provider.ActionStop); err != nil {
		t.Fatal(err)
	}
	entries, _ := h.st.RecentAudit(ctx, 5)
	if len(entries) != 2 || entries[0].Result != "ok" || entries[1].Result != "error" || entries[0].Actor != "42" {
		t.Errorf("audit = %+v", entries)
	}
	r, _ = h.svc.Find(ctx, "fake/aquascale-api")
	if r.Power != provider.PowerStopped {
		t.Error("inventory not refreshed after control")
	}
}

func TestDigestGroupsByApp(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	h.svc.PollHealth(ctx) //nolint:errcheck
	h.svc.PollCost(ctx)
	h.rec.Reset()
	if err := h.svc.Digest(ctx); err != nil {
		t.Fatal(err)
	}
	m := h.rec.Messages()
	if len(m) != 1 || m[0].Target.Kind != Digest {
		t.Fatalf("digest = %+v", m)
	}
	var names []string
	for _, f := range m[0].Message.Fields {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "aquascale,buddibucks,unassigned" {
		t.Errorf("fields = %v", names)
	}
}

func TestShouldAlert(t *testing.T) {
	H, D, G, S, U := provider.Healthy, provider.Down, provider.Degraded, provider.Stopped, provider.Unknown
	cases := []struct {
		prev    provider.HealthState
		hadPrev bool
		now     provider.HealthState
		want    bool
		why     string
	}{
		{"", false, H, false, "first sight, healthy"},
		{"", false, S, false, "first sight, already stopped (e.g. stopped to save cost)"},
		{"", false, D, true, "first sight, down"},
		{H, true, H, false, "no change"},
		{D, true, D, false, "still down: no repeat page"},
		{H, true, D, true, "outage"},
		{H, true, G, true, "degraded"},
		{D, true, H, true, "recovery"},
		{H, true, S, false, "stopped on purpose"},
		{S, true, H, false, "started normally"},
		{S, true, D, true, "started but broken"},
		{H, true, U, true, "check failing: worth knowing"},
	}
	for _, c := range cases {
		if got := shouldAlert(c.prev, c.hadPrev, c.now); got != c.want {
			t.Errorf("%s: shouldAlert(%q, %v, %q) = %v, want %v", c.why, c.prev, c.hadPrev, c.now, got, c.want)
		}
	}
}

func TestStoppingAndStartingStayQuiet(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	h.svc.PollHealth(ctx) //nolint:errcheck
	r, _ := h.svc.Find(ctx, "fake/aquascale-api")

	if err := h.svc.Control(ctx, "42", r, provider.ActionStop); err != nil {
		t.Fatal(err)
	}
	h.rec.Reset()
	if ts, _ := h.svc.PollHealth(ctx); len(ts) != 0 {
		t.Fatalf("stopping produced health alerts: %+v", ts)
	}
	views, _ := h.svc.HealthViews(ctx, []provider.Resource{r})
	if views[0].Health.State != provider.Stopped {
		t.Errorf("state = %q, want stopped", views[0].Health.State)
	}

	r, _ = h.svc.Find(ctx, "fake/aquascale-api")
	if err := h.svc.Control(ctx, "42", r, provider.ActionStart); err != nil {
		t.Fatal(err)
	}
	if ts, _ := h.svc.PollHealth(ctx); len(ts) != 0 {
		t.Fatalf("a normal start produced health alerts: %+v", ts)
	}
}
