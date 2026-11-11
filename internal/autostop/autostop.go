// Package autostop implements budget auto-stop (spec §6.1): when month-to-date
// spend crosses the threshold, warn, wait a cancellable grace period, then
// stop every running, non-critical resource. State lives in SQLite so a
// pending grace timer survives restarts.
package autostop

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud-control/internal/config"
	"cloud-control/internal/core"
	"cloud-control/internal/provider"
	"cloud-control/internal/store"
)

// Actor is the audit-log actor for every auto-stop action.
const Actor = "auto-stop"

// CancelPrefix prefixes the Cancel button's custom ID; the month follows.
const CancelPrefix = "autostop-cancel:"

const settingEnabled = "autostop.enabled"

// Controller is the subset of core.Service the engine needs.
type Controller interface {
	Inventory(ctx context.Context) []provider.Resource
	Can(r provider.Resource, a provider.Action) bool
	Control(ctx context.Context, actor string, r provider.Resource, a provider.Action) error
}

type Timer interface{ Stop() bool }

type Options struct {
	Config    *config.Config
	Store     store.Store
	Apps      *core.Resolver
	Control   Controller
	Notifier  core.Notifier
	Logger    *slog.Logger
	Now       func() time.Time
	AfterFunc func(d time.Duration, f func()) Timer // defaults to time.AfterFunc
}

type Engine struct {
	cfg       *config.Config
	store     store.Store
	apps      *core.Resolver
	ctl       Controller
	notifier  core.Notifier
	log       *slog.Logger
	now       func() time.Time
	afterFunc func(time.Duration, func()) Timer

	mu      sync.Mutex
	baseCtx context.Context
	timers  map[string]Timer
	wg      sync.WaitGroup
}

func New(o Options) *Engine {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.AfterFunc == nil {
		o.AfterFunc = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
	return &Engine{cfg: o.Config, store: o.Store, apps: o.Apps, ctl: o.Control, notifier: o.Notifier,
		log: o.Logger, now: o.Now, afterFunc: o.AfterFunc, timers: map[string]Timer{}, baseCtx: context.Background()}
}

// Start resumes pending grace timers (and finishes runs interrupted while
// executing). ctx bounds the work done by timers.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	e.baseCtx = ctx
	e.mu.Unlock()
	pending, err := e.store.RunsWithStatus(ctx, store.RunPending)
	if err != nil {
		return err
	}
	for _, r := range pending {
		e.log.Info("resuming auto-stop grace timer", "month", r.Month, "deadline", r.Deadline)
		e.schedule(r.Month, r.Deadline)
	}
	executing, err := e.store.RunsWithStatus(ctx, store.RunExecuting)
	if err != nil {
		return err
	}
	for _, r := range executing {
		e.log.Warn("auto-stop was interrupted mid-run; resuming", "month", r.Month)
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.execute(ctx, r.Month)
		}()
	}
	return nil
}

// Wait blocks until in-flight executions finish (for tests and shutdown).
func (e *Engine) Wait() { e.wg.Wait() }

// Stop cancels in-memory timers; persisted state is resumed on next Start.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for m, t := range e.timers {
		if t.Stop() {
			e.wg.Done() // the timer's func will never run
		}
		delete(e.timers, m)
	}
}

func (e *Engine) month() string { return e.now().In(e.cfg.Location).Format("2006-01") }

// Enabled returns whether auto-stop is on and where that came from
// ("config" or "runtime" for a /autostop override).
func (e *Engine) Enabled(ctx context.Context) (bool, string) {
	v, ok, err := e.store.Setting(ctx, settingEnabled)
	if err == nil && ok {
		b, perr := strconv.ParseBool(v)
		if perr == nil {
			return b, "runtime"
		}
	}
	return e.cfg.AutoStop.Enabled, "config"
}

// SetEnabled persists a runtime override. Turning it on clears this month's
// cancelled (or dry-run) run so auto-stop can fire again; turning it off
// cancels any pending run.
func (e *Engine) SetEnabled(ctx context.Context, on bool, actor string) error {
	if err := e.store.SetSetting(ctx, settingEnabled, strconv.FormatBool(on)); err != nil {
		return err
	}
	action := "autostop-off"
	if on {
		action = "autostop-on"
		month := e.month()
		run, err := e.store.Run(ctx, month)
		if err != nil {
			return err
		}
		if run != nil && (run.Status == store.RunCancelled || (run.Status == store.RunDone && run.DryRun)) {
			if _, err := e.store.DeleteRun(ctx, month, run.Status); err != nil {
				return err
			}
		}
	} else if _, err := e.Cancel(ctx, e.month(), actor); err != nil {
		return err
	}
	return e.store.Audit(ctx, store.AuditEntry{At: e.now(), Actor: actor, Action: action, Result: "ok"})
}

// Plan lists resources auto-stop would stop right now.
func (e *Engine) Plan(ctx context.Context) []store.PlanItem {
	var out []store.PlanItem
	for _, r := range e.ctl.Inventory(ctx) {
		if !e.ctl.Can(r, provider.ActionStop) || e.apps.IsCritical(r) {
			continue
		}
		out = append(out, store.PlanItem{ResourceID: r.ID, Provider: r.Provider, Name: r.Name, App: r.App})
	}
	return out
}

// OnCost implements core.CostHook: evaluate the trigger after a cost poll.
func (e *Engine) OnCost(ctx context.Context, sum core.CostSummary) {
	if err := e.evaluate(ctx, sum); err != nil {
		e.log.Error("auto-stop evaluate", "err", err)
	}
}

func (e *Engine) evaluate(ctx context.Context, sum core.CostSummary) error {
	if on, _ := e.Enabled(ctx); !on {
		return nil
	}
	// Unknown totals never trigger; the service already posted a warning.
	if !sum.Known || sum.Percent < e.cfg.AutoStop.ThresholdPercent {
		return nil
	}
	month := e.month()
	if existing, err := e.store.Run(ctx, month); err != nil || existing != nil {
		return err // at most one run per month
	}
	now := e.now()
	plan := e.Plan(ctx)
	run := store.AutoStopRun{
		Month: month, Status: store.RunPending, Deadline: now.Add(e.cfg.AutoStop.Grace()),
		Plan: plan, DryRun: e.cfg.AutoStop.IsDryRun(), Spend: sum.Total, CreatedAt: now,
	}
	if len(plan) == 0 {
		run.Status = store.RunDone
	}
	created, err := e.store.CreateRun(ctx, run)
	if err != nil || !created {
		return err
	}
	e.log.Warn("auto-stop triggered", "month", month, "spend", sum.Total, "percent", sum.Percent,
		"resources", len(plan), "dry_run", run.DryRun)

	if len(plan) == 0 {
		e.send(ctx, core.Message{
			Title:        "🛑 Auto-stop threshold reached — nothing to stop",
			Body:         e.spendLine(sum) + "\nNo running, non-critical resources support Stop.",
			Style:        core.Bad,
			MentionOwner: true,
		})
		return nil
	}

	title := "🛑 Auto-stop in " + humanDuration(e.cfg.AutoStop.Grace())
	if run.DryRun {
		title = "🧪 DRY RUN — auto-stop in " + humanDuration(e.cfg.AutoStop.Grace())
	}
	body := e.spendLine(sum) + "\n\n**Will stop:**\n" + planLines(plan) +
		fmt.Sprintf("\n\nRuns %s unless cancelled. Cancelling skips auto-stop for the rest of the month (re-enable with `/autostop on`).",
			core.RelTime(run.Deadline))
	if run.DryRun {
		body += "\n\n_Dry run: nothing will actually be stopped. Set `auto_stop.dry_run: false` to arm it._"
	}
	e.send(ctx, core.Message{
		Title: title, Body: body, Style: core.Bad, MentionOwner: true,
		Buttons: []core.Button{{Label: "Cancel auto-stop", CustomID: CancelPrefix + month, Danger: true}},
	})
	e.schedule(month, run.Deadline)
	return nil
}

func (e *Engine) spendLine(sum core.CostSummary) string {
	return fmt.Sprintf("Month-to-date spend **%s** is %.0f%% of the %s budget (threshold %.0f%%).",
		core.Money(sum.Total, sum.Currency, sum.Estimated), sum.Percent,
		core.Amount(sum.Budget, sum.Currency), e.cfg.AutoStop.ThresholdPercent)
}

func (e *Engine) schedule(month string, deadline time.Time) {
	d := max(deadline.Sub(e.now()), 0)
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, ok := e.timers[month]; ok {
		t.Stop()
	}
	ctx := e.baseCtx
	e.wg.Add(1)
	var t Timer
	t = e.afterFunc(d, func() {
		defer e.wg.Done()
		e.mu.Lock()
		if e.timers[month] == t {
			delete(e.timers, month)
		}
		e.mu.Unlock()
		e.fire(ctx, month)
	})
	e.timers[month] = t
}

// Cancel cancels a pending run. It returns false if nothing was pending.
func (e *Engine) Cancel(ctx context.Context, month, actor string) (bool, error) {
	ok, err := e.store.TransitionRun(ctx, month, store.RunPending, store.RunCancelled, e.now())
	if err != nil || !ok {
		return false, err
	}
	e.mu.Lock()
	if t, found := e.timers[month]; found && t.Stop() {
		delete(e.timers, month)
		e.wg.Done() // the timer's func will never run
	}
	e.mu.Unlock()
	if err := e.store.Audit(ctx, store.AuditEntry{At: e.now(), Actor: actor, Action: "autostop-cancel",
		Result: "cancelled", Detail: "month " + month}); err != nil {
		e.log.Error("audit", "err", err)
	}
	e.send(ctx, core.Message{
		Title: "✅ Auto-stop cancelled",
		Body: fmt.Sprintf("Cancelled by %s. Auto-stop is skipped for %s unless re-enabled with `/autostop on`.",
			core.ActorMention(actor), month),
		Style: core.Good,
	})
	return true, nil
}

// fire runs when the grace period ends.
func (e *Engine) fire(ctx context.Context, month string) {
	ok, err := e.store.TransitionRun(ctx, month, store.RunPending, store.RunExecuting, e.now())
	if err != nil {
		e.log.Error("auto-stop transition", "err", err)
		return
	}
	if !ok {
		return // cancelled meanwhile
	}
	e.execute(ctx, month)
}

type outcome struct {
	item   store.PlanItem
	result string // stopped, failed, skipped, dry-run
	detail string
}

func (e *Engine) execute(ctx context.Context, month string) {
	run, err := e.store.Run(ctx, month)
	if err != nil || run == nil {
		e.log.Error("auto-stop load run", "month", month, "err", err)
		return
	}
	inv := map[string]provider.Resource{}
	for _, r := range e.ctl.Inventory(ctx) {
		inv[r.ID] = r
	}
	var outcomes []outcome
	for _, item := range run.Plan {
		r, found := inv[item.ResourceID]
		switch {
		case !found:
			outcomes = append(outcomes, outcome{item, "skipped", "no longer in inventory"})
		case e.apps.IsCritical(r):
			outcomes = append(outcomes, outcome{item, "skipped", "now marked critical"})
		case !e.ctl.Can(r, provider.ActionStop):
			outcomes = append(outcomes, outcome{item, "skipped", "not running"})
		case run.DryRun:
			outcomes = append(outcomes, outcome{item, "dry-run", "would stop"})
			if err := e.store.Audit(ctx, store.AuditEntry{At: e.now(), Actor: Actor, Action: "stop", ResourceID: r.ID,
				ResourceName: r.Name, App: r.App, Result: "dry-run"}); err != nil {
				e.log.Error("audit", "err", err)
			}
		default:
			// One at a time; continue on individual failures. Control audits.
			if err := e.ctl.Control(ctx, Actor, r, provider.ActionStop); err != nil {
				outcomes = append(outcomes, outcome{item, "failed", err.Error()})
			} else {
				outcomes = append(outcomes, outcome{item, "stopped", ""})
			}
		}
	}
	if _, err := e.store.TransitionRun(ctx, month, store.RunExecuting, store.RunDone, e.now()); err != nil {
		e.log.Error("auto-stop finish", "err", err)
	}
	e.send(ctx, summaryMessage(run.DryRun, outcomes))
}

func summaryMessage(dry bool, outs []outcome) core.Message {
	groups := map[string][]outcome{}
	for _, o := range outs {
		groups[o.result] = append(groups[o.result], o)
	}
	var parts []string
	for _, k := range []string{"stopped", "dry-run", "failed", "skipped"} {
		g := groups[k]
		if len(g) == 0 {
			continue
		}
		label := map[string]string{"stopped": "✅ Stopped", "dry-run": "🧪 Would stop", "failed": "❌ Failed", "skipped": "⏭️ Skipped"}[k]
		lines := []string{fmt.Sprintf("**%s (%d)**", label, len(g))}
		for _, o := range g {
			l := fmt.Sprintf("• [%s] %s (%s)", core.Display(o.item.App), o.item.Name, o.item.Provider)
			if o.detail != "" && k != "dry-run" {
				l += " — " + o.detail
			}
			lines = append(lines, l)
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	m := core.Message{Title: "🛑 Auto-stop finished", Body: strings.Join(parts, "\n\n"), Style: core.Good}
	if dry {
		m.Title = "🧪 Auto-stop dry run finished — nothing was stopped"
		m.Style = core.Info
	}
	if len(groups["failed"]) > 0 {
		m.Style = core.Bad
		m.MentionOwner = true
	}
	return m
}

// Status describes auto-stop for /autostop status.
type Status struct {
	Enabled   bool
	Source    string
	DryRun    bool
	Threshold float64
	Grace     time.Duration
	Run       *store.AutoStopRun // this month's run, if any
	Plan      []store.PlanItem   // what would be stopped now
}

func (e *Engine) Status(ctx context.Context) (Status, error) {
	on, src := e.Enabled(ctx)
	run, err := e.store.Run(ctx, e.month())
	if err != nil {
		return Status{}, err
	}
	return Status{Enabled: on, Source: src, DryRun: e.cfg.AutoStop.IsDryRun(), Threshold: e.cfg.AutoStop.ThresholdPercent,
		Grace: e.cfg.AutoStop.Grace(), Run: run, Plan: e.Plan(ctx)}, nil
}

// ParseCancelID extracts the month from a Cancel button custom ID.
func ParseCancelID(id string) (string, bool) {
	m, ok := strings.CutPrefix(id, CancelPrefix)
	if !ok {
		return "", false
	}
	if _, err := time.Parse("2006-01", m); err != nil {
		return "", false
	}
	return m, true
}

func planLines(plan []store.PlanItem) string {
	sorted := append([]store.PlanItem(nil), plan...)
	sort.SliceStable(sorted, func(i, j int) bool { return core.Display(sorted[i].App) < core.Display(sorted[j].App) })
	var lines []string
	for _, p := range sorted {
		lines = append(lines, fmt.Sprintf("• [%s] %s (%s)", core.Display(p.App), p.Name, p.Provider))
	}
	return strings.Join(lines, "\n")
}

// PlanLines renders a plan for display.
func PlanLines(plan []store.PlanItem) string {
	if len(plan) == 0 {
		return "_nothing_"
	}
	return planLines(plan)
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%d min", int(d.Minutes()))
}

func (e *Engine) send(ctx context.Context, m core.Message) {
	if err := e.notifier.Send(ctx, core.Target{Kind: core.Alerts}, m); err != nil {
		e.log.Error("send auto-stop notification", "err", err)
	}
}
