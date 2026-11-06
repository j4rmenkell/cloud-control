// Package apps maps resources to the applications they belong to, so health,
// cost and alerts can be grouped per app rather than per provider.
package apps

import (
	"slices"
	"strings"

	"cloud-control/internal/config"
	"cloud-control/internal/provider"
)

// Unassigned is the display name for resources that match no app.
const Unassigned = "unassigned"

type Resolver struct {
	names    []string
	apps     map[string]config.App
	critical map[string]bool
}

func NewResolver(cfg *config.Config) *Resolver {
	r := &Resolver{names: cfg.AppNames(), apps: cfg.Apps, critical: map[string]bool{}}
	for _, id := range cfg.CriticalResources {
		r.critical[strings.ToLower(id)] = true
	}
	return r
}

// Names returns the configured app names, sorted.
func (r *Resolver) Names() []string { return slices.Clone(r.names) }

// Has reports whether name is a configured app (or Unassigned).
func (r *Resolver) Has(name string) bool {
	_, ok := r.apps[name]
	return ok || name == Unassigned
}

// AppFor returns the app a resource belongs to, or "" if none. Explicit
// resource listings win over tag matches; ties go to the first app by name.
func (r *Resolver) AppFor(res provider.Resource) string {
	key := strings.ToLower(res.Provider + "/" + res.Name)
	id := strings.ToLower(res.ID)
	for _, n := range r.names {
		for _, ref := range r.apps[n].Resources {
			ref = strings.ToLower(ref)
			if ref == id || ref == key {
				return n
			}
		}
	}
	for _, n := range r.names {
		if tagsMatch(r.apps[n].MatchTags, res.Tags) {
			return n
		}
	}
	return ""
}

func tagsMatch(want, have map[string]string) bool {
	if len(want) == 0 {
		return false
	}
	for k, v := range want {
		got, ok := lookupFold(have, k)
		if !ok || !strings.EqualFold(got, v) {
			return false
		}
	}
	return true
}

// Azure tag keys are case-insensitive, so match keys case-insensitively.
func lookupFold(m map[string]string, key string) (string, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// Assign sets App on each resource in place.
func (r *Resolver) Assign(rs []provider.Resource) {
	for i := range rs {
		rs[i].App = r.AppFor(rs[i])
	}
}

// IsCritical reports whether auto-stop must never touch res: the provider
// flagged it, it is listed in critical_resources, or its app is critical.
func (r *Resolver) IsCritical(res provider.Resource) bool {
	if res.Critical || r.critical[strings.ToLower(res.ID)] ||
		r.critical[strings.ToLower(res.Provider+"/"+res.Name)] {
		return true
	}
	app := res.App
	if app == "" {
		app = r.AppFor(res)
	}
	return app != "" && r.apps[app].Critical
}

// AlertsChannel returns the app-specific alerts channel, or fallback.
func (r *Resolver) AlertsChannel(app, fallback string) string {
	if a, ok := r.apps[app]; ok && a.AlertsChannelID != "" {
		return a.AlertsChannelID
	}
	return fallback
}

// Display returns the app's display name, mapping "" to Unassigned.
func Display(app string) string {
	if app == "" {
		return Unassigned
	}
	return app
}
