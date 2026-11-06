package core

import (
	"testing"

	"cloud-control/internal/config"
	"cloud-control/internal/provider"
)

func resolver(t *testing.T) *Resolver {
	t.Helper()
	cfg, err := config.Parse([]byte(`
monthly_budget: 10
critical_resources: [azure/legacy-db]
apps:
  aquascale:
    match_tags: { app: aquascale }
    resources: [cloudflare/aquascale-uploads, "resend:domain:aquascale.io"]
    alerts_channel_id: "123"
    critical: true
  buddibucks:
    match_tags: { app: buddibucks, env: prod }
`))
	if err != nil {
		t.Fatal(err)
	}
	return NewResolver(cfg)
}

func TestAppFor(t *testing.T) {
	r := resolver(t)
	cases := []struct {
		name string
		res  provider.Resource
		want string
	}{
		{"tag match, case-insensitive key", provider.Resource{Provider: "azure", Name: "api", Tags: map[string]string{"App": "AquaScale"}}, "aquascale"},
		{"explicit provider/name", provider.Resource{Provider: "cloudflare", Name: "aquascale-uploads"}, "aquascale"},
		{"explicit full ID", provider.Resource{ID: "resend:domain:aquascale.io", Provider: "resend", Name: "aquascale.io"}, "aquascale"},
		{"all tags must match", provider.Resource{Provider: "azure", Name: "w", Tags: map[string]string{"app": "buddibucks"}}, ""},
		{"multi-tag match", provider.Resource{Provider: "azure", Name: "w", Tags: map[string]string{"app": "buddibucks", "env": "prod"}}, "buddibucks"},
		{"no tags", provider.Resource{Provider: "gcp", Name: "vision"}, ""},
	}
	for _, c := range cases {
		if got := r.AppFor(c.res); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestIsCritical(t *testing.T) {
	r := resolver(t)
	if !r.IsCritical(provider.Resource{Provider: "azure", Name: "api", Tags: map[string]string{"app": "aquascale"}}) {
		t.Error("resource in critical app should be critical")
	}
	if !r.IsCritical(provider.Resource{Provider: "azure", Name: "legacy-db"}) {
		t.Error("critical_resources entry ignored")
	}
	if r.IsCritical(provider.Resource{Provider: "azure", Name: "x", Tags: map[string]string{"app": "buddibucks", "env": "prod"}}) {
		t.Error("non-critical app reported critical")
	}
}

func TestAlertsChannel(t *testing.T) {
	r := resolver(t)
	if got := r.AlertsChannel("aquascale", "global"); got != "123" {
		t.Errorf("got %q", got)
	}
	if got := r.AlertsChannel("buddibucks", "global"); got != "global" {
		t.Errorf("got %q", got)
	}
	if got := r.AlertsChannel("", "global"); got != "global" {
		t.Errorf("got %q", got)
	}
}
