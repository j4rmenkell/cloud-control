package config

import (
	"os"
	"strings"
	"testing"
)

func TestRepoConfigParses(t *testing.T) {
	raw, err := os.ReadFile("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Location.String() != "Asia/Manila" {
		t.Errorf("location = %s", cfg.Location)
	}
	if !cfg.AutoStop.IsDryRun() {
		t.Error("committed config must default to dry run")
	}
	if got := cfg.AppNames(); len(got) != 2 || got[0] != "aquascale" {
		t.Errorf("apps = %v", got)
	}
	cfg.Secrets = Secrets{DiscordBotToken: "t", DiscordGuildID: "g", DiscordAlertsChannelID: "a",
		DiscordDigestChannelID: "d", OwnerDiscordUserID: "o"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse([]byte("monthly_budget: 10\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AutoStop.IsDryRun() {
		t.Error("dry_run should default to true when omitted")
	}
	if cfg.AutoStop.ThresholdPercent != 100 || cfg.Schedule.HealthEvery != "5m" || len(cfg.BudgetAlerts) != 3 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestExplicitDryRunFalse(t *testing.T) {
	cfg, err := Parse([]byte("monthly_budget: 10\nauto_stop:\n  dry_run: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoStop.IsDryRun() {
		t.Error("explicit dry_run: false ignored")
	}
}

func TestValidateReportsMissingSecretsAndBadValues(t *testing.T) {
	cfg, err := Parse([]byte("monthly_budget: 0\nschedule:\n  health_every: 10s\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"monthly_budget", "health_every", "OWNER_DISCORD_USER_ID", "DISCORD_BOT_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	if _, err := Parse([]byte("monthly_budget: 10\nmonthly_budjet: 5\n")); err == nil {
		t.Error("typo'd key should be rejected")
	}
}
