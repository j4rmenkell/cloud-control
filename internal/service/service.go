// Package service is the core: it owns the resource inventory, runs health
// and cost polls, enforces budget alerts and performs audited control actions.
// The Discord bot and the scheduler are thin layers over it.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud-control/internal/apps"
	"cloud-control/internal/config"
	"cloud-control/internal/format"
	"cloud-control/internal/notify"
	"cloud-control/internal/provider"
	"cloud-control/internal/store"
)

// CostHook is called after every cost poll (the auto-stop engine).
type CostHook interface {
	OnCost(ctx context.Context, s CostSummary)
}

type Service struct {
	cfg       *config.Config
	store     *store.Store
	apps      *apps.Resolver
	notifier  notify.Notifier
	log       *slog.Logger
	providers map[string]provider.Provider
	order     []string
	now       func() time.Time
	hook      CostHook
	started   time.Time

	mu         sync.RWMutex
	inventory  []provider.Resource
	lastHealth time.Time
	lastCost   time.Time
}

type Options struct {
	Config    *config.Config
	Store     *store.Store
	Apps      *apps.Resolver
	Notifier  notify.Notifier
	Logger    *slog.Logger
	Providers []provider.Provider
	Now       func() time.Time // defaults to time.Now
}

func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	s := &Service{cfg: o.Config, store: o.Store, apps: o.Apps, notifier: o.Notifier, log: o.Logger,
		providers: map[string]provider.Provider{}, now: o.Now}
	for _, p := range o.Providers {
		s.providers[p.Name()] = p
		s.order = append(s.order, p.Name())
	}
	s.started = s.now()
	return s
}

// SetCostHook registers the hook run after each cost poll.
func (s *Service) SetCostHook(h CostHook) { s.hook = h }

func (s *Service) Config() *config.Config { return s.cfg }
func (s *Service) Store() *store.Store    { return s.store }
func (s *Service) Apps() *apps.Resolver   { return s.apps }
func (s *Service) ProviderNames() []string {
	return append([]string(nil), s.order...)
}
func (s *Service) Started() time.Time { return s.started }

// Now returns the current time in the configured timezone.
func (s *Service) Now() time.Time { return s.now().In(s.cfg.Location) }

// MonthKey is the configured-timezone month, e.g. "2026-11".
func (s *Service) MonthKey() string { return s.Now().Format("2006-01") }

func (s *Service) LastPolls() (health, cost time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastHealth, s.lastCost
}

// ---- inventory ----

// RefreshInventory lists resources from every provider. A provider that
// fails keeps its last stored snapshot, so one outage doesn't hide resources.
func (s *Service) RefreshInventory(ctx context.Context) ([]provider.Resource, error) {
	now := s.now()
	stored, err := s.store.Resources(ctx)
	if err != nil {
		return nil, err
	}
	var all []provider.Resource
	var errs []error
	for _, name := range s.order {
		lctx, cancel := context.WithTimeout(ctx, time.Minute)
		rs, err := s.providers[name].ListResources(lctx)
		cancel()
		if err != nil {
			s.log.Warn("list resources failed; using last snapshot", "provider", name, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			for _, r := range stored {
				if r.Provider == name {
					all = append(all, r)
				}
			}
			continue
		}
		for i := range rs {
			rs[i].Provider = name
		}
		s.apps.Assign(rs)
		if err := s.store.ReplaceResources(ctx, name, rs, now); err != nil {
			return nil, err
		}
		all = append(all, rs...)
	}
	sortResources(all)
	s.mu.Lock()
	s.inventory = all
	s.mu.Unlock()
	return all, errors.Join(errs...)
}

// Inventory returns the cached inventory, loading or refreshing it if empty.
func (s *Service) Inventory(ctx context.Context) []provider.Resource {
	s.mu.RLock()
	inv := s.inventory
	s.mu.RUnlock()
	if inv != nil {
		return append([]provider.Resource(nil), inv...)
	}
	inv, err := s.RefreshInventory(ctx)
	if err != nil {
		s.log.Warn("inventory refresh", "err", err)
	}
	return inv
}

func sortResources(rs []provider.Resource) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if apps.Display(a.App) != apps.Display(b.App) {
			// unassigned sorts last
			if a.App == "" || b.App == "" {
				return b.App == ""
			}
			return a.App < b.App
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Name < b.Name
	})
}

// Find looks up a resource by short ID, full ID or "provider/name".
func (s *Service) Find(ctx context.Context, key string) (provider.Resource, bool) {
	key = strings.TrimSpace(key)
	for _, r := range s.Inventory(ctx) {
		if r.ShortID() == key || r.ID == key || strings.EqualFold(r.Provider+"/"+r.Name, key) {
			return r, true
		}
	}
	return provider.Resource{}, false
}

// Can reports whether action a is currently available on r.
func (s *Service) Can(r provider.Resource, a provider.Action) bool {
	p, ok := s.providers[r.Provider]
	return ok && provider.Can(p, r, a)
}

// Filter returns resources matching app ("" = all, apps.Unassigned = none)
// and provider ("" = all).
func Filter(rs []provider.Resource, app, prov string) []provider.Resource {
	var out []provider.Resource
	for _, r := range rs {
		if app != "" && apps.Display(r.App) != app {
			continue
		}
		if prov != "" && r.Provider != prov {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ---- health ----

type Transition struct {
	Resource provider.Resource
	From     provider.HealthState // "" if first observation
	To       provider.HealthStatus
}

// PollHealth refreshes the inventory, checks every resource, persists the
// results and alerts on state changes (grouped per app).
func (s *Service) PollHealth(ctx context.Context) ([]Transition, error) {
	inv, invErr := s.RefreshInventory(ctx)
	if inv == nil && invErr != nil {
		return nil, invErr
	}
	type result struct {
		r provider.Resource
		h provider.HealthStatus
	}
	results := make([]result, len(inv))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, r := range inv {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = result{r, s.checkOne(ctx, r)}
		}()
	}
	wg.Wait()

	var transitions []Transition
	for _, res := range results {
		prev, hadPrev, err := s.store.RecordHealth(ctx, res.r.ID, res.h)
		if err != nil {
			return nil, err
		}
		changed := hadPrev && prev != res.h.State
		firstBad := !hadPrev && (res.h.State == provider.Down || res.h.State == provider.Degraded)
		if changed || firstBad {
			from := prev
			if !hadPrev {
				from = ""
			}
			transitions = append(transitions, Transition{Resource: res.r, From: from, To: res.h})
		}
	}
	if err := s.store.PruneHealth(ctx, s.now().AddDate(0, 0, -30)); err != nil {
		s.log.Warn("prune health history", "err", err)
	}
	s.mu.Lock()
	s.lastHealth = s.now()
	s.mu.Unlock()
	s.alertTransitions(ctx, transitions)
	return transitions, invErr
}

func (s *Service) checkOne(ctx context.Context, r provider.Resource) provider.HealthStatus {
	p, ok := s.providers[r.Provider]
	if !ok {
		return provider.HealthStatus{State: provider.Unknown, Detail: "provider not loaded", CheckedAt: s.now()}
	}
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	h, err := p.Health(hctx, r)
	if err != nil {
		h = provider.HealthStatus{State: provider.Unknown, Detail: "check failed: " + err.Error()}
	}
	if h.CheckedAt.IsZero() {
		h.CheckedAt = s.now()
	}
	if h.State == "" {
		h.State = provider.Unknown
	}
	return h
}

func (s *Service) alertTransitions(ctx context.Context, ts []Transition) {
	byApp := map[string][]Transition{}
	var order []string
	for _, t := range ts {
		if _, ok := byApp[t.Resource.App]; !ok {
			order = append(order, t.Resource.App)
		}
		byApp[t.Resource.App] = append(byApp[t.Resource.App], t)
	}
	for _, app := range order {
		group := byApp[app]
		style := notify.Good
		var lines []string
		for _, t := range group {
			if t.To.State == provider.Down {
				style = notify.Bad
			} else if t.To.State != provider.Healthy && style != notify.Bad {
				style = notify.Warn
			}
			from := "new"
			if t.From != "" {
				from = format.HealthEmoji(t.From) + " " + string(t.From)
			}
			line := fmt.Sprintf("**%s** (%s) %s → %s %s", t.Resource.Name, t.Resource.Provider, from,
				format.HealthEmoji(t.To.State), t.To.State)
			if t.To.Detail != "" && t.To.State != provider.Healthy {
				line += "\n> " + truncate(t.To.Detail, 200)
			}
			lines = append(lines, line)
		}
		title := fmt.Sprintf("[%s] Health changed", apps.Display(app))
		if len(group) == 1 {
			title = fmt.Sprintf("[%s] %s is %s", apps.Display(app), group[0].Resource.Name, group[0].To.State)
		}
		s.send(ctx, notify.Target{Kind: notify.Alerts, App: app}, notify.Message{
			Title: title, Body: strings.Join(lines, "\n"), Style: style,
		})
	}
}

// HealthView pairs resources with their latest health.
type HealthView struct {
	Resource provider.Resource
	Health   store.HealthRecord
	Known    bool
}

func (s *Service) HealthViews(ctx context.Context, rs []provider.Resource) ([]HealthView, error) {
	cur, err := s.store.CurrentHealth(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]HealthView, len(rs))
	for i, r := range rs {
		h, ok := cur[r.ID]
		out[i] = HealthView{Resource: r, Health: h, Known: ok}
		if !ok {
			out[i].Health.State = provider.Unknown
		}
	}
	return out, nil
}

// ---- cost ----

type CostSummary struct {
	Month     string
	Reports   map[string]provider.CostReport // provider → report
	Failed    map[string]string              // provider → error message
	Total     float64                        // sum of MTD across reports
	Forecast  float64
	Known     bool // every provider reported; Total is trustworthy
	Estimated bool // at least one report is estimated
	ByApp     map[string]float64 // app ("" = unassigned) → MTD
	Budget    float64
	Percent   float64 // Total / Budget × 100
	Currency  string
	AsOf      time.Time // oldest report's AsOf (billing data lags)
}

// PollCost fetches cost from every provider, persists it, fires budget
// alerts and then runs the cost hook (auto-stop).
func (s *Service) PollCost(ctx context.Context) CostSummary {
	now := s.Now()
	period := provider.MonthToDate(now)
	reports := map[string]provider.CostReport{}
	failed := map[string]string{}
	for _, name := range s.order {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		c, err := s.providers[name].Cost(cctx, period)
		cancel()
		if err != nil {
			s.log.Warn("cost fetch failed", "provider", name, "err", err)
			failed[name] = err.Error()
			continue
		}
		c.Provider = name
		if c.AsOf.IsZero() {
			c.AsOf = now
		}
		if err := s.store.RecordCost(ctx, c, now); err != nil {
			s.log.Error("record cost", "err", err)
		}
		reports[name] = c
	}
	sum := s.summarize(ctx, reports, failed)
	s.mu.Lock()
	s.lastCost = s.now()
	s.mu.Unlock()

	if !sum.Known {
		var lines []string
		for _, p := range sortedKeys(failed) {
			lines = append(lines, fmt.Sprintf("**%s**: %s", p, truncate(failed[p], 200)))
		}
		s.send(ctx, notify.Target{Kind: notify.Alerts}, notify.Message{
			Title: "⚠️ Cost fetch failed — total spend unknown",
			Body:  strings.Join(lines, "\n") + "\n\nBudget alerts and auto-stop are skipped until all providers report.",
			Style: notify.Warn,
		})
	} else {
		s.checkBudget(ctx, sum)
	}
	if s.hook != nil {
		s.hook.OnCost(ctx, sum)
	}
	return sum
}

// CurrentCost builds a summary from this month's stored reports without
// calling providers. Providers with no report this month count as unknown.
func (s *Service) CurrentCost(ctx context.Context) (CostSummary, error) {
	since := provider.MonthToDate(s.Now()).Start
	reports, err := s.store.LatestCosts(ctx, since)
	if err != nil {
		return CostSummary{}, err
	}
	failed := map[string]string{}
	for _, name := range s.order {
		if _, ok := reports[name]; !ok {
			failed[name] = "no cost data yet this month"
		}
	}
	for name := range reports {
		if _, ok := s.providers[name]; !ok {
			delete(reports, name) // provider removed from config
		}
	}
	return s.summarize(ctx, reports, failed), nil
}

func (s *Service) summarize(ctx context.Context, reports map[string]provider.CostReport, failed map[string]string) CostSummary {
	sum := CostSummary{
		Month: s.MonthKey(), Reports: reports, Failed: failed, Known: len(failed) == 0,
		ByApp: map[string]float64{}, Budget: s.cfg.MonthlyBudget, Currency: s.cfg.Currency,
	}
	appOf := map[string]string{}
	for _, r := range s.Inventory(ctx) {
		appOf[r.ID] = r.App
	}
	for _, c := range reports {
		sum.Total += c.MTD
		sum.Forecast += c.Forecast
		sum.Estimated = sum.Estimated || c.Estimated
		if sum.AsOf.IsZero() || c.AsOf.Before(sum.AsOf) {
			sum.AsOf = c.AsOf
		}
		attributed := 0.0
		for id, v := range c.ByResource {
			sum.ByApp[appOf[id]] += v
			attributed += v
		}
		if rest := c.MTD - attributed; rest > 0.005 {
			sum.ByApp[""] += rest
		}
	}
	if sum.Budget > 0 {
		sum.Percent = sum.Total / sum.Budget * 100
	}
	return sum
}

func (s *Service) checkBudget(ctx context.Context, sum CostSummary) {
	var crossed []float64
	for _, p := range s.cfg.BudgetAlerts {
		if sum.Percent < p {
			continue
		}
		fresh, err := s.store.MarkBudgetAlert(ctx, sum.Month, p, s.now())
		if err != nil {
			s.log.Error("mark budget alert", "err", err)
			continue
		}
		if fresh {
			crossed = append(crossed, p)
		}
	}
	if len(crossed) == 0 {
		return
	}
	top := crossed[len(crossed)-1]
	style := notify.Warn
	if top >= 100 {
		style = notify.Bad
	}
	s.send(ctx, notify.Target{Kind: notify.Alerts}, notify.Message{
		Title: fmt.Sprintf("💸 Budget %.0f%% reached", top),
		Body: fmt.Sprintf("Month-to-date spend is **%s** of %s (%.0f%%)\n%s\nForecast: %s",
			format.Money(sum.Total, sum.Currency, sum.Estimated), format.Amount(sum.Budget, sum.Currency), sum.Percent,
			format.Bar(sum.Percent), format.Money(sum.Forecast, sum.Currency, sum.Estimated)),
		Style:        style,
		MentionOwner: top >= 100,
		Footer:       "as of " + sum.AsOf.In(s.cfg.Location).Format("Jan 2 15:04 MST"),
	})
}

// ---- control ----

// Control performs action a on r on behalf of actor, writes the audit log
// and echoes the result to the app's alerts channel.
func (s *Service) Control(ctx context.Context, actor string, r provider.Resource, a provider.Action) error {
	p, ok := s.providers[r.Provider]
	var err error
	switch {
	case !ok:
		err = fmt.Errorf("provider %q not loaded", r.Provider)
	case !provider.Can(p, r, a):
		err = fmt.Errorf("%s is not available for %s (power state %q): %w", a, r.Name, r.Power, provider.ErrUnsupported)
	default:
		dctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err = p.Do(dctx, r, a)
		cancel()
	}
	s.recordAction(ctx, actor, string(a), r, err)
	if _, rerr := s.RefreshInventory(ctx); rerr != nil {
		s.log.Warn("refresh after control", "err", rerr)
	}
	return err
}

func (s *Service) recordAction(ctx context.Context, actor, action string, r provider.Resource, err error) {
	result, detail := "ok", ""
	if err != nil {
		result, detail = "error", err.Error()
	}
	if aerr := s.store.Audit(ctx, store.AuditEntry{At: s.now(), Actor: actor, Action: action, ResourceID: r.ID,
		ResourceName: r.Name, App: r.App, Result: result, Detail: detail}); aerr != nil {
		s.log.Error("audit", "err", aerr)
	}
	s.log.Info("control action", "actor", actor, "action", action, "resource", r.ID, "result", result, "err", err)
	msg := notify.Message{
		Title: fmt.Sprintf("[%s] %s %s → %s", apps.Display(r.App), action, r.Name, result),
		Body:  fmt.Sprintf("By %s · %s/%s", ActorMention(actor), r.Provider, r.Type),
		Style: notify.Good,
	}
	if err != nil {
		msg.Style = notify.Bad
		msg.Body += "\n> " + truncate(err.Error(), 300)
	}
	s.send(ctx, notify.Target{Kind: notify.Alerts, App: r.App}, msg)
}

// ActorMention renders a Discord user ID as a mention; system actors as-is.
func ActorMention(actor string) string {
	if actor != "" && strings.Trim(actor, "0123456789") == "" {
		return "<@" + actor + ">"
	}
	return "`" + actor + "`"
}

// ---- digest ----

func (s *Service) Digest(ctx context.Context) error {
	sum, err := s.CurrentCost(ctx)
	if err != nil {
		return err
	}
	views, err := s.HealthViews(ctx, s.Inventory(ctx))
	if err != nil {
		return err
	}
	counts := map[string]map[provider.HealthState]int{}
	for _, v := range views {
		app := apps.Display(v.Resource.App)
		if counts[app] == nil {
			counts[app] = map[provider.HealthState]int{}
		}
		counts[app][v.Health.State]++
	}
	msg := notify.Message{
		Title: "📊 Daily digest — " + s.Now().Format("Mon Jan 2"),
		Body: fmt.Sprintf("**MTD:** %s of %s (%.0f%%) %s\n**Forecast:** %s",
			format.Money(sum.Total, sum.Currency, sum.Estimated), format.Amount(sum.Budget, sum.Currency),
			sum.Percent, format.Bar(sum.Percent), format.Money(sum.Forecast, sum.Currency, sum.Estimated)),
		Footer: "as of " + sum.AsOf.In(s.cfg.Location).Format("Jan 2 15:04 MST"),
	}
	if !sum.Known {
		msg.Body += "\n⚠️ Missing cost data: " + strings.Join(sortedKeys(sum.Failed), ", ")
		msg.Style = notify.Warn
	}
	for _, app := range append(s.apps.Names(), apps.Unassigned) {
		c, ok := counts[app]
		cost, hasCost := sum.ByApp[appKey(app)]
		if !ok && !hasCost {
			continue
		}
		val := fmt.Sprintf("%s %d  %s %d  %s %d  %s %d\nMTD %s",
			format.HealthEmoji(provider.Healthy), c[provider.Healthy], format.HealthEmoji(provider.Degraded), c[provider.Degraded],
			format.HealthEmoji(provider.Down), c[provider.Down], format.HealthEmoji(provider.Unknown), c[provider.Unknown],
			format.Amount(cost, sum.Currency))
		msg.Fields = append(msg.Fields, notify.Field{Name: app, Value: val, Inline: true})
	}
	return s.notifier.Send(ctx, notify.Target{Kind: notify.Digest}, msg)
}

func appKey(display string) string {
	if display == apps.Unassigned {
		return ""
	}
	return display
}

func (s *Service) send(ctx context.Context, t notify.Target, m notify.Message) {
	if err := s.notifier.Send(ctx, t, m); err != nil {
		s.log.Error("send notification", "title", m.Title, "err", err)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
