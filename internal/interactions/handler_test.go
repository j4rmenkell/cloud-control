package interactions

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud-control/internal/config"
	"cloud-control/internal/core"
	"cloud-control/internal/provider"
	"cloud-control/internal/provider/fake"
	"cloud-control/internal/store/memstore"
)

const (
	owner = "111111111111111111"
	guild = "222222222222222222"
)

type harness struct {
	h    *Handler
	priv ed25519.PrivateKey
	now  time.Time
}

func setup(t *testing.T) *harness {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)
	v, err := NewVerifier(hex.EncodeToString(pub), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte("timezone: Asia/Manila\nmonthly_budget: 20\napps:\n  aquascale: { match_tags: { project: aquascale } }\n  buddibucks: { match_tags: { project: buddibucks } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := core.New(core.Options{Config: cfg, Store: memstore.New(), Apps: core.NewResolver(cfg),
		Notifier: core.Discard{}, Logger: log, Providers: []provider.Provider{fake.Demo()}})
	return &harness{
		h:    &Handler{Verifier: v, OwnerID: owner, GuildID: guild, Service: svc, Log: log, Started: now, Region: "ap-southeast-1"},
		priv: priv, now: now,
	}
}

// signed builds a request Discord would send at time at.
func (hs *harness) signed(body string, at time.Time) Request {
	ts := strconv.FormatInt(at.Unix(), 10)
	sig := ed25519.Sign(hs.priv, []byte(ts+body))
	return Request{Signature: hex.EncodeToString(sig), Timestamp: ts, Body: []byte(body)}
}

func command(name, user, guildID string) string {
	b, _ := json.Marshal(map[string]any{
		"type": 2, "id": "1", "token": "tok", "guild_id": guildID,
		"member": map[string]any{"user": map[string]any{"id": user}},
		"data":   map[string]any{"name": name},
	})
	return string(b)
}

func decode(t *testing.T, r Reply) Response {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d: %s", r.Status, r.Body)
	}
	var resp Response
	if err := json.Unmarshal(r.Body, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSignatureChecks(t *testing.T) {
	hs := setup(t)
	ctx := context.Background()
	body := `{"type":1}`
	good := hs.signed(body, hs.now)

	cases := []struct {
		name string
		req  Request
		want int
	}{
		{"valid PING", good, 200},
		{"no headers", Request{Body: []byte(body)}, 401},
		{"garbage signature", Request{Signature: "zz", Timestamp: good.Timestamp, Body: good.Body}, 401},
		{"tampered body", Request{Signature: good.Signature, Timestamp: good.Timestamp, Body: []byte(`{"type":2}`)}, 401},
		{"tampered timestamp", Request{Signature: good.Signature, Timestamp: "1", Body: good.Body}, 401},
		{"replayed after 10 min", hs.signed(body, hs.now.Add(-10*time.Minute)), 401},
		{"from the future", hs.signed(body, hs.now.Add(10*time.Minute)), 401},
		{"slight clock skew ok", hs.signed(body, hs.now.Add(-time.Minute)), 200},
	}
	for _, c := range cases {
		if got := hs.h.Handle(ctx, c.req).Status; got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	wrongKey := setup(t) // different key pair
	if got := hs.h.Handle(ctx, wrongKey.signed(body, hs.now)).Status; got != 401 {
		t.Errorf("signed by another key: status %d, want 401", got)
	}
}

func TestPingHandshake(t *testing.T) {
	hs := setup(t)
	r := hs.h.Handle(context.Background(), hs.signed(`{"type":1}`, hs.now))
	if strings.TrimSpace(string(r.Body)) != `{"type":1}` {
		t.Errorf("PONG body = %s", r.Body)
	}
}

func TestOwnerOnly(t *testing.T) {
	hs := setup(t)
	ctx := context.Background()
	for _, c := range []struct{ name, user, guild string }{
		{"stranger", "999999999999999999", guild},
		{"owner in another server", owner, "333333333333333333"},
		{"owner in DMs", owner, ""},
	} {
		resp := decode(t, hs.h.Handle(ctx, hs.signed(command("status", c.user, c.guild), hs.now)))
		if resp.Data == nil || !strings.Contains(resp.Data.Content, "only answers its owner") || resp.Data.Flags != flagEphemeral {
			t.Errorf("%s: not rejected: %+v", c.name, resp.Data)
		}
	}
}

func TestCommands(t *testing.T) {
	hs := setup(t)
	ctx := context.Background()

	ping := decode(t, hs.h.Handle(ctx, hs.signed(command("ping", owner, guild), hs.now)))
	if ping.Type != respChannelMessageWithSource || !strings.Contains(ping.Data.Content, "Pong") ||
		!strings.Contains(ping.Data.Content, "ap-southeast-1") {
		t.Errorf("ping = %+v", ping.Data)
	}
	if ping.Data.AllowedMentions == nil || ping.Data.AllowedMentions.Parse == nil {
		t.Error("replies must disable mentions")
	}

	status := decode(t, hs.h.Handle(ctx, hs.signed(command("status", owner, guild), hs.now)))
	var titles []string
	for _, e := range status.Data.Embeds {
		titles = append(titles, e.Title)
	}
	if strings.Join(titles, ",") != "aquascale,buddibucks,unassigned" {
		t.Errorf("status groups = %v", titles)
	}
	if !strings.Contains(status.Data.Embeds[0].Description, "aquascale-api") {
		t.Errorf("aquascale embed = %q", status.Data.Embeds[0].Description)
	}

	other := decode(t, hs.h.Handle(ctx, hs.signed(command("stop", owner, guild), hs.now)))
	if !strings.Contains(other.Data.Content, "isn't available") {
		t.Errorf("unknown command = %+v", other.Data)
	}
}

func TestMentionsDisabledInJSON(t *testing.T) {
	hs := setup(t)
	r := hs.h.Handle(context.Background(), hs.signed(command("ping", owner, guild), hs.now))
	if !strings.Contains(string(r.Body), `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("body = %s", r.Body)
	}
}
