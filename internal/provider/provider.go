// Package provider defines the contract every cloud provider implements and
// the shared types that flow through the rest of the system.
package provider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"slices"
	"time"
)

// ErrUnsupported is returned by Do when a provider cannot perform an action.
var ErrUnsupported = errors.New("action not supported")

// Action is a control operation on a resource.
type Action string

const (
	ActionStart Action = "start"
	ActionStop  Action = "stop"
)

// PowerState is the running state of a controllable resource.
type PowerState string

const (
	PowerUnknown PowerState = ""
	PowerRunning PowerState = "running"
	PowerStopped PowerState = "stopped"
)

// Resource is one thing running in a cloud account.
type Resource struct {
	ID       string            // provider-unique, stable ID (e.g. Azure ARM ID)
	Provider string            // provider Name()
	Type     string            // e.g. "containerapp", "staticwebapp", "r2bucket"
	Name     string            // human name
	Region   string            //
	Tags     map[string]string //
	Critical bool              // provider-level hint; config can also mark resources critical
	Power    PowerState        // for resources that can be started/stopped

	// App is filled in by the apps resolver, not by providers.
	App string
}

// ShortID is a compact, stable key for a resource. Discord limits
// autocomplete values and button custom IDs to 100 characters, and Azure ARM
// IDs regularly exceed that.
func (r Resource) ShortID() string { return ShortID(r.ID) }

// ShortID hashes a full resource ID into a 12-character key.
func ShortID(id string) string {
	sum := sha1.Sum([]byte(id))
	return hex.EncodeToString(sum[:6])
}

// HealthState is the coarse health of a resource.
type HealthState string

const (
	Healthy  HealthState = "healthy"
	Degraded HealthState = "degraded"
	Down     HealthState = "down"
	Unknown  HealthState = "unknown"
	// Stopped means intentionally stopped (by the owner, auto-stop, or outside
	// cloud-control). It is not an outage, so it never pages anyone.
	Stopped HealthState = "stopped"
)

// HealthStatus is the result of one health check.
type HealthStatus struct {
	State     HealthState
	Detail    string
	CheckedAt time.Time
}

// Period is a cost reporting window.
type Period struct {
	Start time.Time
	End   time.Time
}

// MonthToDate returns the period from the start of the month containing now
// (in now's location) up to now.
func MonthToDate(now time.Time) Period {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return Period{Start: start, End: now}
}

// CostReport is one provider's spend for a period.
type CostReport struct {
	Provider  string
	MTD       float64
	Forecast  float64
	Currency  string
	Estimated bool // computed from usage × configured price, not a billing API
	AsOf      time.Time

	// ByResource optionally attributes MTD spend to resource IDs. Spend not
	// attributed here is reported as unassigned when grouping by app.
	ByResource map[string]float64
}

// Provider is implemented by each cloud integration.
type Provider interface {
	Name() string
	ListResources(ctx context.Context) ([]Resource, error)
	Health(ctx context.Context, r Resource) (HealthStatus, error)
	Cost(ctx context.Context, period Period) (CostReport, error)
	Capabilities(r Resource) []Action
	Do(ctx context.Context, r Resource, a Action) error
}

// Can reports whether p advertises action a for r.
func Can(p Provider, r Resource, a Action) bool {
	return slices.Contains(p.Capabilities(r), a)
}
