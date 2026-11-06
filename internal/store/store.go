// Package store persists snapshots, health and cost history, the audit log
// and auto-stop state in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"cloud-control/internal/provider"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS resources (
	id TEXT PRIMARY KEY, provider TEXT NOT NULL, type TEXT NOT NULL, name TEXT NOT NULL,
	region TEXT NOT NULL DEFAULT '', app TEXT NOT NULL DEFAULT '', power TEXT NOT NULL DEFAULT '',
	critical INTEGER NOT NULL DEFAULT 0, tags TEXT NOT NULL DEFAULT '{}', seen_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS health_checks (
	id INTEGER PRIMARY KEY AUTOINCREMENT, resource_id TEXT NOT NULL, state TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT '', checked_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS health_checks_resource ON health_checks(resource_id, checked_at);
CREATE TABLE IF NOT EXISTS health_current (
	resource_id TEXT PRIMARY KEY, state TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '',
	checked_at TEXT NOT NULL, changed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS cost_reports (
	id INTEGER PRIMARY KEY AUTOINCREMENT, provider TEXT NOT NULL, mtd REAL NOT NULL, forecast REAL NOT NULL,
	currency TEXT NOT NULL, estimated INTEGER NOT NULL, as_of TEXT NOT NULL,
	by_resource TEXT NOT NULL DEFAULT '{}', polled_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS cost_reports_provider ON cost_reports(provider, polled_at);
CREATE TABLE IF NOT EXISTS audit_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL,
	resource_id TEXT NOT NULL DEFAULT '', resource_name TEXT NOT NULL DEFAULT '', app TEXT NOT NULL DEFAULT '',
	result TEXT NOT NULL, detail TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS budget_alerts (
	month TEXT NOT NULL, percent REAL NOT NULL, fired_at TEXT NOT NULL, PRIMARY KEY (month, percent)
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS autostop_runs (
	month TEXT PRIMARY KEY, status TEXT NOT NULL, deadline TEXT NOT NULL, plan TEXT NOT NULL,
	dry_run INTEGER NOT NULL, spend REAL NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
`

// Open opens (creating if needed) the SQLite database at path and migrates it.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serialize to avoid SQLITE_BUSY
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(v string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

// ---- resources ----

// ReplaceResources stores the latest inventory snapshot for one provider.
func (s *Store) ReplaceResources(ctx context.Context, providerName string, rs []provider.Resource, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `DELETE FROM resources WHERE provider = ?`, providerName); err != nil {
		return err
	}
	for _, r := range rs {
		tags, _ := json.Marshal(r.Tags)
		if _, err := tx.ExecContext(ctx, `INSERT INTO resources (id, provider, type, name, region, app, power, critical, tags, seen_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, r.ID, r.Provider, r.Type, r.Name, r.Region, r.App, string(r.Power),
			boolInt(r.Critical), string(tags), ts(at)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Resources returns the last stored inventory (all providers).
func (s *Store) Resources(ctx context.Context) ([]provider.Resource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, provider, type, name, region, app, power, critical, tags
		FROM resources ORDER BY app, provider, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []provider.Resource
	for rows.Next() {
		var r provider.Resource
		var power, tags string
		var crit int
		if err := rows.Scan(&r.ID, &r.Provider, &r.Type, &r.Name, &r.Region, &r.App, &power, &crit, &tags); err != nil {
			return nil, err
		}
		r.Power = provider.PowerState(power)
		r.Critical = crit != 0
		_ = json.Unmarshal([]byte(tags), &r.Tags)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- health ----

type HealthRecord struct {
	ResourceID string
	provider.HealthStatus
	ChangedAt time.Time
}

// RecordHealth appends a check and updates the current state. It returns the
// previous state (and whether there was one).
func (s *Store) RecordHealth(ctx context.Context, resourceID string, h provider.HealthStatus) (prev provider.HealthState, hadPrev bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback() //nolint:errcheck
	var p string
	switch err := tx.QueryRowContext(ctx, `SELECT state FROM health_current WHERE resource_id = ?`, resourceID).Scan(&p); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", false, err
	default:
		prev, hadPrev = provider.HealthState(p), true
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO health_checks (resource_id, state, detail, checked_at) VALUES (?,?,?,?)`,
		resourceID, string(h.State), h.Detail, ts(h.CheckedAt)); err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO health_current (resource_id, state, detail, checked_at, changed_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(resource_id) DO UPDATE SET
			changed_at = CASE WHEN health_current.state = excluded.state THEN health_current.changed_at ELSE excluded.changed_at END,
			state = excluded.state, detail = excluded.detail, checked_at = excluded.checked_at`,
		resourceID, string(h.State), h.Detail, ts(h.CheckedAt), ts(h.CheckedAt)); err != nil {
		return "", false, err
	}
	return prev, hadPrev, tx.Commit()
}

// CurrentHealth returns the latest health per resource ID.
func (s *Store) CurrentHealth(ctx context.Context) (map[string]HealthRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT resource_id, state, detail, checked_at, changed_at FROM health_current`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]HealthRecord{}
	for rows.Next() {
		var h HealthRecord
		var state, checked, changed string
		if err := rows.Scan(&h.ResourceID, &state, &h.Detail, &checked, &changed); err != nil {
			return nil, err
		}
		h.State, h.CheckedAt, h.ChangedAt = provider.HealthState(state), parseTS(checked), parseTS(changed)
		out[h.ResourceID] = h
	}
	return out, rows.Err()
}

// PruneHealth deletes check history older than before.
func (s *Store) PruneHealth(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM health_checks WHERE checked_at < ?`, ts(before))
	return err
}

// ---- cost ----

func (s *Store) RecordCost(ctx context.Context, c provider.CostReport, polledAt time.Time) error {
	by, _ := json.Marshal(c.ByResource)
	_, err := s.db.ExecContext(ctx, `INSERT INTO cost_reports (provider, mtd, forecast, currency, estimated, as_of, by_resource, polled_at)
		VALUES (?,?,?,?,?,?,?,?)`, c.Provider, c.MTD, c.Forecast, c.Currency, boolInt(c.Estimated), ts(c.AsOf), string(by), ts(polledAt))
	return err
}

// LatestCosts returns the most recent report per provider polled at or after since.
func (s *Store) LatestCosts(ctx context.Context, since time.Time) (map[string]provider.CostReport, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, mtd, forecast, currency, estimated, as_of, by_resource
		FROM cost_reports c WHERE polled_at >= ? AND id = (SELECT MAX(id) FROM cost_reports WHERE provider = c.provider)`, ts(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]provider.CostReport{}
	for rows.Next() {
		var c provider.CostReport
		var est int
		var asOf, by string
		if err := rows.Scan(&c.Provider, &c.MTD, &c.Forecast, &c.Currency, &est, &asOf, &by); err != nil {
			return nil, err
		}
		c.Estimated, c.AsOf = est != 0, parseTS(asOf)
		_ = json.Unmarshal([]byte(by), &c.ByResource)
		out[c.Provider] = c
	}
	return out, rows.Err()
}

// ---- audit ----

type AuditEntry struct {
	ID           int64
	At           time.Time
	Actor        string // Discord user ID, or "auto-stop"
	Action       string
	ResourceID   string
	ResourceName string
	App          string
	Result       string // "ok", "error", "dry-run", "skipped", "cancelled"
	Detail       string
}

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log (at, actor, action, resource_id, resource_name, app, result, detail)
		VALUES (?,?,?,?,?,?,?,?)`, ts(e.At), e.Actor, e.Action, e.ResourceID, e.ResourceName, e.App, e.Result, e.Detail)
	return err
}

// RecentAudit returns the last n entries, newest first.
func (s *Store) RecentAudit(ctx context.Context, n int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, actor, action, resource_id, resource_name, app, result, detail
		FROM audit_log ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.ResourceID, &e.ResourceName, &e.App, &e.Result, &e.Detail); err != nil {
			return nil, err
		}
		e.At = parseTS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- budget alerts ----

// MarkBudgetAlert records that the percent threshold fired for month. It
// returns false if it had already fired.
func (s *Store) MarkBudgetAlert(ctx context.Context, month string, percent float64, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO budget_alerts (month, percent, fired_at) VALUES (?,?,?)`, month, percent, ts(at))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ---- settings ----

func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---- auto-stop runs ----

type RunStatus string

const (
	RunPending   RunStatus = "pending"   // warning posted, waiting for grace period
	RunExecuting RunStatus = "executing" // grace expired, stopping resources
	RunDone      RunStatus = "done"      // finished (including dry runs)
	RunCancelled RunStatus = "cancelled" // owner cancelled; no auto-stop this month
)

type PlanItem struct {
	ResourceID string `json:"id"`
	Provider   string `json:"provider"`
	Name       string `json:"name"`
	App        string `json:"app"`
}

type AutoStopRun struct {
	Month     string
	Status    RunStatus
	Deadline  time.Time
	Plan      []PlanItem
	DryRun    bool
	Spend     float64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateRun inserts a run for its month. It returns false if one exists.
func (s *Store) CreateRun(ctx context.Context, r AutoStopRun) (bool, error) {
	plan, _ := json.Marshal(r.Plan)
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO autostop_runs
		(month, status, deadline, plan, dry_run, spend, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		r.Month, string(r.Status), ts(r.Deadline), string(plan), boolInt(r.DryRun), r.Spend, ts(r.CreatedAt), ts(r.CreatedAt))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Run returns the run for month, if any.
func (s *Store) Run(ctx context.Context, month string) (*AutoStopRun, error) {
	runs, err := s.queryRuns(ctx, `WHERE month = ?`, month)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// RunsWithStatus returns all runs in the given status.
func (s *Store) RunsWithStatus(ctx context.Context, st RunStatus) ([]AutoStopRun, error) {
	return s.queryRuns(ctx, `WHERE status = ?`, string(st))
}

func (s *Store) queryRuns(ctx context.Context, where string, args ...any) ([]AutoStopRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT month, status, deadline, plan, dry_run, spend, created_at, updated_at
		FROM autostop_runs `+where+` ORDER BY month`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AutoStopRun
	for rows.Next() {
		var r AutoStopRun
		var st, dl, plan, ca, ua string
		var dry int
		if err := rows.Scan(&r.Month, &st, &dl, &plan, &dry, &r.Spend, &ca, &ua); err != nil {
			return nil, err
		}
		r.Status, r.Deadline, r.DryRun = RunStatus(st), parseTS(dl), dry != 0
		r.CreatedAt, r.UpdatedAt = parseTS(ca), parseTS(ua)
		_ = json.Unmarshal([]byte(plan), &r.Plan)
		out = append(out, r)
	}
	return out, rows.Err()
}

// TransitionRun moves a run from one status to another atomically. It
// returns false if the run was not in status from.
func (s *Store) TransitionRun(ctx context.Context, month string, from, to RunStatus, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE autostop_runs SET status = ?, updated_at = ? WHERE month = ? AND status = ?`,
		string(to), ts(at), month, string(from))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteRun removes the month's run if it is in one of the given statuses,
// allowing auto-stop to fire again that month.
func (s *Store) DeleteRun(ctx context.Context, month string, statuses ...RunStatus) (bool, error) {
	if len(statuses) == 0 {
		return false, nil
	}
	q := `DELETE FROM autostop_runs WHERE month = ? AND status IN (?` + repeat(",?", len(statuses)-1) + `)`
	args := []any{month}
	for _, st := range statuses {
		args = append(args, string(st))
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
