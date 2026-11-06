// Package fake is an in-memory Provider for tests and local development.
package fake

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"cloud-control/internal/provider"
)

// Provider is safe for concurrent use. Zero values are usable after New.
type Provider struct {
	name string

	mu        sync.Mutex
	resources []provider.Resource
	health    map[string]provider.HealthState
	cost      provider.CostReport
	listErr   error
	costErr   error
	doErr     map[string]error // resource ID → error returned by Do
	calls     []Call
	now       func() time.Time
}

// Call records one Do invocation.
type Call struct {
	ResourceID string
	Action     provider.Action
}

func New(name string) *Provider {
	return &Provider{
		name:   name,
		health: map[string]provider.HealthState{},
		doErr:  map[string]error{},
		now:    time.Now,
		cost:   provider.CostReport{Provider: name, Currency: "USD"},
	}
}

func (p *Provider) Name() string { return p.name }

// AddResource registers r; Provider is set to the fake's name.
func (p *Provider) AddResource(r provider.Resource) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r.Provider = p.name
	p.resources = append(p.resources, r)
	if _, ok := p.health[r.ID]; !ok {
		p.health[r.ID] = provider.Healthy
	}
}

func (p *Provider) SetHealth(id string, s provider.HealthState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.health[id] = s
}

func (p *Provider) SetCost(c provider.CostReport) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c.Provider = p.name
	if c.Currency == "" {
		c.Currency = "USD"
	}
	p.cost = c
}

func (p *Provider) SetListErr(err error) { p.mu.Lock(); p.listErr = err; p.mu.Unlock() }
func (p *Provider) SetCostErr(err error) { p.mu.Lock(); p.costErr = err; p.mu.Unlock() }
func (p *Provider) SetDoErr(id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.doErr[id] = err
}

// Calls returns every Do call made so far.
func (p *Provider) Calls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Call(nil), p.calls...)
}

func (p *Provider) ListResources(context.Context) ([]provider.Resource, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listErr != nil {
		return nil, p.listErr
	}
	out := make([]provider.Resource, len(p.resources))
	for i, r := range p.resources {
		r.Tags = maps.Clone(r.Tags)
		out[i] = r
	}
	return out, nil
}

func (p *Provider) Health(_ context.Context, r provider.Resource) (provider.HealthStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.health[r.ID]
	if !ok {
		return provider.HealthStatus{State: provider.Unknown, Detail: "not found", CheckedAt: p.now()}, nil
	}
	if p.powerLocked(r.ID) == provider.PowerStopped {
		return provider.HealthStatus{State: provider.Down, Detail: "stopped", CheckedAt: p.now()}, nil
	}
	return provider.HealthStatus{State: st, Detail: "fake", CheckedAt: p.now()}, nil
}

func (p *Provider) Cost(context.Context, provider.Period) (provider.CostReport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.costErr != nil {
		return provider.CostReport{}, p.costErr
	}
	c := p.cost
	c.ByResource = maps.Clone(c.ByResource)
	if c.AsOf.IsZero() {
		c.AsOf = p.now()
	}
	return c, nil
}

// Capabilities: resources with a known power state can be toggled.
func (p *Provider) Capabilities(r provider.Resource) []provider.Action {
	switch r.Power {
	case provider.PowerRunning:
		return []provider.Action{provider.ActionStop}
	case provider.PowerStopped:
		return []provider.Action{provider.ActionStart}
	}
	return nil
}

func (p *Provider) Do(_ context.Context, r provider.Resource, a provider.Action) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, Call{ResourceID: r.ID, Action: a})
	if err := p.doErr[r.ID]; err != nil {
		return err
	}
	for i := range p.resources {
		if p.resources[i].ID != r.ID {
			continue
		}
		switch a {
		case provider.ActionStop:
			p.resources[i].Power = provider.PowerStopped
		case provider.ActionStart:
			p.resources[i].Power = provider.PowerRunning
		default:
			return provider.ErrUnsupported
		}
		return nil
	}
	return fmt.Errorf("resource %s not found", r.ID)
}

func (p *Provider) powerLocked(id string) provider.PowerState {
	for _, r := range p.resources {
		if r.ID == id {
			return r.Power
		}
	}
	return provider.PowerUnknown
}

// Demo returns a fake provider seeded with resources spanning two apps, for
// running the bot locally without cloud credentials.
func Demo() *Provider {
	p := New("fake")
	p.AddResource(provider.Resource{ID: "fake:aca:aquascale-api", Type: "containerapp", Name: "aquascale-api",
		Region: "southeastasia", Tags: map[string]string{"app": "aquascale"}, Power: provider.PowerRunning})
	p.AddResource(provider.Resource{ID: "fake:swa:aquascale-web", Type: "staticwebapp", Name: "aquascale-web",
		Region: "eastasia", Tags: map[string]string{"app": "aquascale"}})
	p.AddResource(provider.Resource{ID: "fake:aca:buddibucks-worker", Type: "containerapp", Name: "buddibucks-worker",
		Region: "southeastasia", Tags: map[string]string{"app": "buddibucks"}, Power: provider.PowerRunning})
	p.AddResource(provider.Resource{ID: "fake:r2:scratch", Type: "r2bucket", Name: "scratch"})
	p.SetCost(provider.CostReport{MTD: 4.20, Forecast: 9.80, ByResource: map[string]float64{
		"fake:aca:aquascale-api": 3.00, "fake:aca:buddibucks-worker": 1.10,
	}})
	return p
}
