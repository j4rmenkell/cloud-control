// Package config loads non-secret settings from config.yaml and secrets from
// the environment (optionally via a .env file).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Timezone          string         `yaml:"timezone"`
	Currency          string         `yaml:"currency"`
	MonthlyBudget     float64        `yaml:"monthly_budget"`
	CriticalResources []string       `yaml:"critical_resources"`
	AutoStop          AutoStop       `yaml:"auto_stop"`
	Apps              map[string]App `yaml:"apps"`
	Database          string         `yaml:"database"`
	Dev               Dev            `yaml:"dev"`
	Azure             Azure          `yaml:"azure"`
	GCP               GCP            `yaml:"gcp"`
	Cloudflare        Cloudflare     `yaml:"cloudflare"`
	Resend            Resend         `yaml:"resend"`
	Secrets           Secrets        `yaml:"-"`
	Location          *time.Location `yaml:"-"`
}

type AutoStop struct {
	Enabled          bool    `yaml:"enabled"`
	DryRun           *bool   `yaml:"dry_run"` // nil → true; must be explicitly disabled
	ThresholdPercent float64 `yaml:"threshold_percent"`
	GraceMinutes     int     `yaml:"grace_minutes"`
}

// IsDryRun defaults to true when dry_run is not set.
func (a AutoStop) IsDryRun() bool { return a.DryRun == nil || *a.DryRun }

func (a AutoStop) Grace() time.Duration { return time.Duration(a.GraceMinutes) * time.Minute }

// App groups resources across providers. A resource belongs to the first app
// (in name order) that lists it in Resources or whose MatchTags all match.
type App struct {
	MatchTags       map[string]string `yaml:"match_tags"`
	Resources       []string          `yaml:"resources"`         // full resource IDs or "provider/name"
	AlertsChannelID string            `yaml:"alerts_channel_id"` // optional; falls back to the global alerts channel
	Critical        bool              `yaml:"critical"`          // auto-stop never touches this app
}

type Dev struct {
	FakeProvider bool `yaml:"fake_provider"` // register the demo fake provider
}

type Azure struct {
	SubscriptionID string            `yaml:"subscription_id"`
	HealthPaths    map[string]string `yaml:"health_paths"`
}

type GCP struct {
	ProjectID string `yaml:"project_id"`
	Vision    struct {
		FreeUnitsPerMonth float64 `yaml:"free_units_per_month"`
		PricePer1000      float64 `yaml:"price_per_1000"`
	} `yaml:"vision"`
}

type Cloudflare struct {
	AccountID string   `yaml:"account_id"`
	R2Buckets []string `yaml:"r2_buckets"`
	Pricing   struct {
		StorageGBMonth float64 `yaml:"storage_gb_month"`
		FreeStorageGB  float64 `yaml:"free_storage_gb"`
	} `yaml:"pricing"`
}

type Resend struct {
	Domains         []string `yaml:"domains"`
	MonthlyPlanCost float64  `yaml:"monthly_plan_cost"`
}

type Secrets struct {
	DiscordBotToken        string
	DiscordGuildID         string
	DiscordAlertsChannelID string
	DiscordDigestChannelID string
	OwnerDiscordUserID     string
	AzureTenantID          string
	AzureClientID          string
	AzureClientSecret      string
	GoogleCredentials      string
	CloudflareAPIToken     string
	ResendAPIKey           string
}

// Load reads the YAML file at path, loads envFile if it exists (existing
// environment variables win), applies defaults and validates.
func Load(path, envFile string) (*Config, error) {
	if envFile != "" {
		if err := godotenv.Load(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load %s: %w", envFile, err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	cfg.Secrets = SecretsFromEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Parse decodes YAML and applies defaults without reading secrets or validating.
func Parse(raw []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: %w", cfg.Timezone, err)
	}
	cfg.Location = loc
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if c.Currency == "" {
		c.Currency = "USD"
	}
	if c.AutoStop.ThresholdPercent == 0 {
		c.AutoStop.ThresholdPercent = 100
	}
	if c.Database == "" {
		c.Database = "cloud-control.db"
	}
}

func SecretsFromEnv() Secrets {
	return Secrets{
		DiscordBotToken:        os.Getenv("DISCORD_BOT_TOKEN"),
		DiscordGuildID:         os.Getenv("DISCORD_GUILD_ID"),
		DiscordAlertsChannelID: os.Getenv("DISCORD_ALERTS_CHANNEL_ID"),
		DiscordDigestChannelID: os.Getenv("DISCORD_DIGEST_CHANNEL_ID"),
		OwnerDiscordUserID:     os.Getenv("OWNER_DISCORD_USER_ID"),
		AzureTenantID:          os.Getenv("AZURE_TENANT_ID"),
		AzureClientID:          os.Getenv("AZURE_CLIENT_ID"),
		AzureClientSecret:      os.Getenv("AZURE_CLIENT_SECRET"),
		GoogleCredentials:      os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		CloudflareAPIToken:     os.Getenv("CLOUDFLARE_API_TOKEN"),
		ResendAPIKey:           os.Getenv("RESEND_API_KEY"),
	}
}

// Validate checks settings and required secrets.
func (c *Config) Validate() error {
	var errs []error
	if c.MonthlyBudget <= 0 {
		errs = append(errs, errors.New("monthly_budget must be > 0"))
	}
	if c.AutoStop.ThresholdPercent <= 0 {
		errs = append(errs, errors.New("auto_stop.threshold_percent must be > 0"))
	}
	if c.AutoStop.GraceMinutes < 0 {
		errs = append(errs, errors.New("auto_stop.grace_minutes must be >= 0"))
	}
	for name := range c.Apps {
		if strings.TrimSpace(name) == "" || len(name) > 50 {
			errs = append(errs, fmt.Errorf("app name %q must be 1-50 characters", name))
		}
	}
	if len(c.Apps) > 24 {
		errs = append(errs, errors.New("at most 24 apps are supported (Discord allows 25 choices, one is 'unassigned')"))
	}
	s := c.Secrets
	for _, req := range []struct{ name, v string }{
		{"DISCORD_BOT_TOKEN", s.DiscordBotToken},
		{"DISCORD_GUILD_ID", s.DiscordGuildID},
		{"DISCORD_ALERTS_CHANNEL_ID", s.DiscordAlertsChannelID},
		{"DISCORD_DIGEST_CHANNEL_ID", s.DiscordDigestChannelID},
		{"OWNER_DISCORD_USER_ID", s.OwnerDiscordUserID},
	} {
		if strings.TrimSpace(req.v) == "" {
			errs = append(errs, fmt.Errorf("%s is required", req.name))
		}
	}
	return errors.Join(errs...)
}

// AppNames returns configured app names in sorted order.
func (c *Config) AppNames() []string {
	names := make([]string, 0, len(c.Apps))
	for n := range c.Apps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LocalPath is the gitignored config with real IDs (channels, subscription).
// It is a full copy of config.yaml, not an overlay.
const LocalPath = "config.local.yaml"

// DefaultPath returns LocalPath if it exists in dir, otherwise config.yaml.
func DefaultPath(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, LocalPath)); err == nil {
		return filepath.Join(dir, LocalPath)
	}
	return filepath.Join(dir, "config.yaml")
}
