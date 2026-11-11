// Package store defines how cloud-control persists inventory snapshots,
// health and cost history, the audit log, settings and auto-stop runs.
//
// Store is an interface so the same core logic runs on SQLite (local runs),
// in memory (tests, see memstore) and DynamoDB (serverless).
package store

import (
	"context"
	"time"

	"cloud-control/internal/provider"
)

// Store is the persistence contract. Implementations must be safe for
// concurrent use. storetest.Run checks any implementation against it.
type Store interface {
	// ReplaceResources stores the latest inventory snapshot for one provider,
	// replacing that provider's previous snapshot.
	ReplaceResources(ctx context.Context, providerName string, rs []provider.Resource, at time.Time) error
	// Resources returns the stored inventory of all providers, ordered by
	// app, provider, then name.
	Resources(ctx context.Context) ([]provider.Resource, error)

	// RecordHealth appends a check and updates the current state, returning
	// the previous state and whether there was one.
	RecordHealth(ctx context.Context, resourceID string, h provider.HealthStatus) (prev provider.HealthState, hadPrev bool, err error)
	// CurrentHealth returns the latest health per resource ID.
	CurrentHealth(ctx context.Context) (map[string]HealthRecord, error)
	// PruneHealth deletes check history older than before. Stores with
	// native expiry (DynamoDB TTL) may treat it as a no-op.
	PruneHealth(ctx context.Context, before time.Time) error

	RecordCost(ctx context.Context, c provider.CostReport, polledAt time.Time) error
	// LatestCosts returns each provider's most recent report, if that report
	// was polled at or after since.
	LatestCosts(ctx context.Context, since time.Time) (map[string]provider.CostReport, error)

	Audit(ctx context.Context, e AuditEntry) error
	// RecentAudit returns the last n entries, newest first.
	RecentAudit(ctx context.Context, n int) ([]AuditEntry, error)

	// MarkBudgetAlert records that a threshold fired for a month. It returns
	// false if it had already fired.
	MarkBudgetAlert(ctx context.Context, month string, percent float64, at time.Time) (bool, error)

	Setting(ctx context.Context, key string) (value string, ok bool, err error)
	SetSetting(ctx context.Context, key, value string) error

	// CreateRun inserts a run for its month; false if one already exists.
	CreateRun(ctx context.Context, r AutoStopRun) (bool, error)
	// Run returns the month's run, or nil if there is none.
	Run(ctx context.Context, month string) (*AutoStopRun, error)
	// RunsWithStatus returns every run in the given status, ordered by month.
	RunsWithStatus(ctx context.Context, st RunStatus) ([]AutoStopRun, error)
	// TransitionRun atomically moves a run from one status to another. It
	// returns false if the run was not in status from. This compare-and-set
	// is what guarantees a run executes at most once.
	TransitionRun(ctx context.Context, month string, from, to RunStatus, at time.Time) (bool, error)
	// DeleteRun removes the month's run if it is in one of the statuses.
	DeleteRun(ctx context.Context, month string, statuses ...RunStatus) (bool, error)

	Close() error
}

// HealthRecord is a resource's current health and when it last changed.
type HealthRecord struct {
	ResourceID string
	provider.HealthStatus
	ChangedAt time.Time
}

// AuditEntry is one control action.
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

type RunStatus string

const (
	RunPending   RunStatus = "pending"   // warning posted, waiting for grace period
	RunExecuting RunStatus = "executing" // grace expired, stopping resources
	RunDone      RunStatus = "done"      // finished (including dry runs)
	RunCancelled RunStatus = "cancelled" // owner cancelled; no auto-stop this month
)

// PlanItem is one resource an auto-stop run intends to stop.
type PlanItem struct {
	ResourceID string `json:"id"`
	Provider   string `json:"provider"`
	Name       string `json:"name"`
	App        string `json:"app"`
}

// AutoStopRun is the state of one month's auto-stop.
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
