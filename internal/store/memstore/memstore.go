// Package memstore is an in-memory store.Store for tests and local
// experiments. State lives only as long as the value.
package memstore

import (
	"context"
	"maps"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"cloud-control/internal/provider"
	"cloud-control/internal/store"
)

type Store struct {
	mu        sync.Mutex
	resources map[string][]provider.Resource // provider name → snapshot
	checks    []check
	current   map[string]store.HealthRecord
	costs     []costEntry
	audit     []store.AuditEntry
	alerts    map[string]bool
	settings  map[string]string
	runs      map[string]store.AutoStopRun
}

type check struct {
	resourceID string
	at         time.Time
}

type costEntry struct {
	report   provider.CostReport
	polledAt time.Time
}

var _ store.Store = (*Store)(nil)

func New() *Store {
	return &Store{
		resources: map[string][]provider.Resource{},
		current:   map[string]store.HealthRecord{},
		alerts:    map[string]bool{},
		settings:  map[string]string{},
		runs:      map[string]store.AutoStopRun{},
	}
}

func (s *Store) Close() error { return nil }

func cloneResource(r provider.Resource) provider.Resource {
	r.Tags = maps.Clone(r.Tags)
	return r
}

func cloneRun(r store.AutoStopRun) store.AutoStopRun {
	r.Plan = slices.Clone(r.Plan)
	return r
}

func (s *Store) ReplaceResources(_ context.Context, providerName string, rs []provider.Resource, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := make([]provider.Resource, len(rs))
	for i, r := range rs {
		snap[i] = cloneResource(r)
	}
	s.resources[providerName] = snap
	return nil
}

func (s *Store) Resources(context.Context) ([]provider.Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []provider.Resource
	for _, snap := range s.resources {
		for _, r := range snap {
			out = append(out, cloneResource(r))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.App != b.App {
			return a.App < b.App
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Name < b.Name
	})
	return out, nil
}

func (s *Store) RecordHealth(_ context.Context, resourceID string, h provider.HealthStatus) (provider.HealthState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, check{resourceID, h.CheckedAt})
	prev, had := s.current[resourceID]
	rec := store.HealthRecord{ResourceID: resourceID, HealthStatus: h, ChangedAt: h.CheckedAt}
	if had && prev.State == h.State {
		rec.ChangedAt = prev.ChangedAt
	}
	s.current[resourceID] = rec
	if !had {
		return "", false, nil
	}
	return prev.State, true, nil
}

func (s *Store) CurrentHealth(context.Context) (map[string]store.HealthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.current), nil
}

func (s *Store) PruneHealth(_ context.Context, before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = slices.DeleteFunc(s.checks, func(c check) bool { return c.at.Before(before) })
	return nil
}

func (s *Store) RecordCost(_ context.Context, c provider.CostReport, polledAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.ByResource = maps.Clone(c.ByResource)
	s.costs = append(s.costs, costEntry{c, polledAt})
	return nil
}

func (s *Store) LatestCosts(_ context.Context, since time.Time) (map[string]provider.CostReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := map[string]costEntry{}
	for _, e := range s.costs { // later entries win, like MAX(id)
		latest[e.report.Provider] = e
	}
	out := map[string]provider.CostReport{}
	for p, e := range latest {
		if !e.polledAt.Before(since) {
			r := e.report
			r.ByResource = maps.Clone(r.ByResource)
			out[p] = r
		}
	}
	return out, nil
}

func (s *Store) Audit(_ context.Context, e store.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.ID = int64(len(s.audit) + 1)
	s.audit = append(s.audit, e)
	return nil
}

func (s *Store) RecentAudit(_ context.Context, n int) ([]store.AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.AuditEntry
	for i := len(s.audit) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s.audit[i])
	}
	return out, nil
}

func (s *Store) MarkBudgetAlert(_ context.Context, month string, percent float64, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := month + "|" + strconv.FormatFloat(percent, 'f', -1, 64)
	if s.alerts[key] {
		return false, nil
	}
	s.alerts[key] = true
	return true, nil
}

func (s *Store) Setting(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.settings[key]
	return v, ok, nil
}

func (s *Store) SetSetting(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings[key] = value
	return nil
}

func (s *Store) CreateRun(_ context.Context, r store.AutoStopRun) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[r.Month]; exists {
		return false, nil
	}
	r.UpdatedAt = r.CreatedAt
	s.runs[r.Month] = cloneRun(r)
	return true, nil
}

func (s *Store) Run(_ context.Context, month string) (*store.AutoStopRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[month]
	if !ok {
		return nil, nil
	}
	r = cloneRun(r)
	return &r, nil
}

func (s *Store) RunsWithStatus(_ context.Context, st store.RunStatus) ([]store.AutoStopRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.AutoStopRun
	for _, r := range s.runs {
		if r.Status == st {
			out = append(out, cloneRun(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Month < out[j].Month })
	return out, nil
}

func (s *Store) TransitionRun(_ context.Context, month string, from, to store.RunStatus, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[month]
	if !ok || r.Status != from {
		return false, nil
	}
	r.Status, r.UpdatedAt = to, at
	s.runs[month] = r
	return true, nil
}

func (s *Store) DeleteRun(_ context.Context, month string, statuses ...store.RunStatus) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[month]
	if !ok || !slices.Contains(statuses, r.Status) {
		return false, nil
	}
	delete(s.runs, month)
	return true, nil
}
