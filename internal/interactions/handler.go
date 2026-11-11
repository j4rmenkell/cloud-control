package interactions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"cloud-control/internal/core"
	"cloud-control/internal/provider"
)

// Request is a transport-neutral HTTP request (Lambda Function URL or net/http).
type Request struct {
	Signature string // X-Signature-Ed25519
	Timestamp string // X-Signature-Timestamp
	Body      []byte
}

// Reply is what to send back.
type Reply struct {
	Status int
	Body   []byte
}

type Handler struct {
	Verifier *Verifier
	OwnerID  string
	GuildID  string
	Service  *core.Service
	Log      *slog.Logger
	Started  time.Time
	Region   string // shown by /ping
}

// Handle verifies, parses and answers one interaction.
func (h *Handler) Handle(ctx context.Context, req Request) Reply {
	if err := h.Verifier.Verify(req.Signature, req.Timestamp, req.Body); err != nil {
		// Discord's endpoint check deliberately sends bad signatures and
		// expects 401. Log at debug so probes don't flood the logs.
		h.Log.Debug("rejected interaction", "reason", err)
		return Reply{Status: 401, Body: []byte(err.Error())}
	}
	var in Interaction
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return Reply{Status: 400, Body: []byte("bad interaction payload")}
	}
	if in.Type == typePing {
		return h.json(Response{Type: respPong})
	}
	if in.Type != typeApplicationCommand || in.Data == nil {
		return h.json(ephemeral("Unsupported interaction."))
	}

	user := in.UserID()
	if user == "" || user != h.OwnerID || in.GuildID != h.GuildID {
		h.Log.Warn("rejected interaction from non-owner", "user", user, "guild", in.GuildID, "command", in.Data.Name)
		return h.json(ephemeral("⛔ This bot only answers its owner."))
	}
	h.Log.Info("command", "name", in.Data.Name)

	switch in.Data.Name {
	case "ping":
		return h.json(h.ping(ctx))
	case "status":
		return h.json(h.status(ctx, in.Data.Option("app")))
	default:
		return h.json(ephemeral(fmt.Sprintf("`/%s` isn't available in the serverless version yet.", in.Data.Name)))
	}
}

func (h *Handler) json(r Response) Reply {
	if r.Data != nil && r.Data.AllowedMentions == nil {
		r.Data.AllowedMentions = &AllowedMentions{Parse: []string{}}
	}
	b, err := json.Marshal(r)
	if err != nil {
		h.Log.Error("marshal response", "err", err)
		return Reply{Status: 500, Body: []byte("internal error")}
	}
	return Reply{Status: 200, Body: b}
}

func ephemeral(msg string) Response {
	return Response{Type: respChannelMessageWithSource, Data: &ResponseData{Content: msg, Flags: flagEphemeral}}
}

func (h *Handler) ping(ctx context.Context) Response {
	n := len(h.Service.Inventory(ctx))
	return ephemeral(fmt.Sprintf("🏓 Pong — served by AWS Lambda (%s)\nInstance up since <t:%d:R> · %d resources",
		h.Region, h.Started.Unix(), n))
}

// statusBudget keeps /status well inside Discord's 3-second reply deadline.
const statusBudget = 2 * time.Second

func (h *Handler) status(ctx context.Context, app string) Response {
	ctx, cancel := context.WithTimeout(ctx, statusBudget)
	defer cancel()
	if _, err := h.Service.PollHealth(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		h.Log.Warn("health poll from /status", "err", err)
	}
	rs := core.Filter(h.Service.Inventory(ctx), app, "")
	views, err := h.Service.HealthViews(ctx, rs)
	if err != nil {
		return ephemeral("❌ " + err.Error())
	}
	groups := map[string][]core.HealthView{}
	for _, v := range views {
		name := core.Display(v.Resource.App)
		groups[name] = append(groups[name], v)
	}
	var embeds []Embed
	for _, name := range append(h.Service.Apps().Names(), core.Unassigned) {
		g := groups[name]
		if len(g) == 0 {
			continue
		}
		worst := provider.Healthy
		var lines []string
		for _, v := range g {
			worst = worse(worst, v.Health.State)
			line := fmt.Sprintf("%s **%s** · %s/%s", core.HealthEmoji(v.Health.State), v.Resource.Name, v.Resource.Provider, v.Resource.Type)
			if v.Health.State != provider.Healthy && v.Health.State != provider.Stopped && v.Health.Detail != "" {
				line += "\n  ↳ " + clip(v.Health.Detail, 120)
			}
			lines = append(lines, line)
		}
		embeds = append(embeds, Embed{Title: name, Description: clip(strings.Join(lines, "\n"), 1500), Color: color(worst)})
		if len(embeds) == 10 {
			break
		}
	}
	if len(embeds) == 0 {
		return ephemeral("No resources found.")
	}
	return Response{Type: respChannelMessageWithSource, Data: &ResponseData{Embeds: embeds}}
}

var severity = map[provider.HealthState]int{provider.Healthy: 0, provider.Stopped: 0, provider.Unknown: 1, provider.Degraded: 2, provider.Down: 3}

func worse(a, b provider.HealthState) provider.HealthState {
	if severity[b] > severity[a] {
		return b
	}
	return a
}

func color(s provider.HealthState) int {
	switch s {
	case provider.Healthy, provider.Stopped:
		return 0x57F287
	case provider.Degraded, provider.Unknown:
		return 0xFEE75C
	}
	return 0xED4245
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
