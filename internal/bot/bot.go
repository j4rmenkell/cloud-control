// Package bot is the Discord front end: slash commands, autocomplete,
// confirm buttons, and the notifier that posts alerts.
package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"cloud-control/internal/auth"
	"cloud-control/internal/autostop"
	"cloud-control/internal/config"
	"cloud-control/internal/provider"
	"cloud-control/internal/service"
)

const (
	confirmTTL    = 60 * time.Second
	confirmPrefix = "confirm:"
	abortPrefix   = "abort:"
)

type Bot struct {
	cfg     *config.Config
	svc     *service.Service
	engine  *autostop.Engine
	owner   auth.Owner
	log     *slog.Logger
	session *discordgo.Session

	ctx context.Context // lifetime of the bot; set by Open

	mu      sync.Mutex
	pending map[string]*pendingAction
}

type pendingAction struct {
	action      provider.Action
	resource    provider.Resource
	interaction *discordgo.Interaction // the slash command; its response holds the buttons
	timer       *time.Timer
}

func New(cfg *config.Config, svc *service.Service, log *slog.Logger) *Bot {
	return &Bot{
		cfg:     cfg,
		svc:     svc,
		owner:   auth.Owner{UserID: cfg.Secrets.OwnerDiscordUserID, GuildID: cfg.Secrets.DiscordGuildID},
		log:     log,
		pending: map[string]*pendingAction{},
		ctx:     context.Background(),
	}
}

// SetService attaches the core service (the service needs the bot as its
// notifier, so the two are wired after construction).
func (b *Bot) SetService(s *service.Service) { b.svc = s }

// SetEngine wires the auto-stop engine (created after the bot because the
// engine posts through the bot's notifier).
func (b *Bot) SetEngine(e *autostop.Engine) { b.engine = e }

// Open connects to the gateway and registers guild commands.
func (b *Bot) Open(ctx context.Context) error {
	b.ctx = ctx
	s, err := discordgo.New("Bot " + b.cfg.Secrets.DiscordBotToken)
	if err != nil {
		return err
	}
	s.Identify.Intents = discordgo.IntentsGuilds
	s.AddHandler(b.onInteraction)
	ready := make(chan struct{}, 1)
	s.AddHandlerOnce(func(_ *discordgo.Session, r *discordgo.Ready) {
		b.log.Info("discord connected", "user", r.User.Username)
		ready <- struct{}{}
	})
	if err := s.Open(); err != nil {
		return fmt.Errorf("discord open: %w", err)
	}
	b.session = s
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		return fmt.Errorf("discord: timed out waiting for READY")
	}
	cmds, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, b.cfg.Secrets.DiscordGuildID, b.commands())
	if err != nil {
		return fmt.Errorf("register commands: %w", err)
	}
	b.log.Info("registered slash commands", "count", len(cmds))
	return nil
}

func (b *Bot) Close() error {
	if b.session == nil {
		return nil
	}
	return b.session.Close()
}

func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("panic in interaction handler", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	user := interactionUserID(i)
	if !b.owner.Allowed(user, i.GuildID) {
		b.log.Warn("rejected interaction from non-owner", "user", user, "guild", i.GuildID, "type", i.Type.String())
		if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionApplicationCommandAutocompleteResult,
				Data: &discordgo.InteractionResponseData{Choices: []*discordgo.ApplicationCommandOptionChoice{}},
			})
			return
		}
		b.ephemeral(i, "⛔ This bot only answers its owner.")
		return
	}

	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		b.log.Info("command", "name", i.ApplicationCommandData().Name, "user", user)
		b.handleCommand(i)
	case discordgo.InteractionApplicationCommandAutocomplete:
		b.handleAutocomplete(i)
	case discordgo.InteractionMessageComponent:
		b.handleComponent(i, user)
	}
}

func (b *Bot) handleComponent(i *discordgo.InteractionCreate, user string) {
	id := i.MessageComponentData().CustomID
	switch {
	case strings.HasPrefix(id, confirmPrefix):
		b.onConfirm(i, user, strings.TrimPrefix(id, confirmPrefix))
	case strings.HasPrefix(id, abortPrefix):
		b.onAbort(i, strings.TrimPrefix(id, abortPrefix))
	case strings.HasPrefix(id, autostop.CancelPrefix):
		b.onCancelAutoStop(i, user, id)
	default:
		b.ephemeral(i, "Unknown button.")
	}
}

// ---- confirm flow for /stop and /start ----

func newToken() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func (b *Bot) askConfirm(i *discordgo.InteractionCreate, a provider.Action, r provider.Resource) {
	token := newToken()
	pa := &pendingAction{action: a, resource: r, interaction: i.Interaction}
	b.mu.Lock()
	b.pending[token] = pa
	pa.timer = time.AfterFunc(confirmTTL, func() { b.expire(token) })
	b.mu.Unlock()

	verb := strings.ToUpper(string(a)[:1]) + string(a)[1:]
	style := discordgo.PrimaryButton
	if a == provider.ActionStop {
		style = discordgo.DangerButton
	}
	err := b.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("**%s** `%s` in **%s**? (%s/%s)\nExpires %s.", verb, r.Name, appName(r.App),
				r.Provider, r.Type, relTime(time.Now().Add(confirmTTL))),
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.Button{Label: verb, Style: style, CustomID: confirmPrefix + token},
				discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, CustomID: abortPrefix + token},
			}}},
		},
	})
	if err != nil {
		b.log.Error("respond confirm", "err", err)
		b.take(token)
	}
}

// take removes and returns a pending action, stopping its expiry timer.
func (b *Bot) take(token string) *pendingAction {
	b.mu.Lock()
	defer b.mu.Unlock()
	pa, ok := b.pending[token]
	if !ok {
		return nil
	}
	delete(b.pending, token)
	pa.timer.Stop()
	return pa
}

func (b *Bot) expire(token string) {
	pa := b.take(token)
	if pa == nil {
		return
	}
	content := fmt.Sprintf("⌛ Confirmation to %s `%s` expired.", pa.action, pa.resource.Name)
	if _, err := b.session.InteractionResponseEdit(pa.interaction, &discordgo.WebhookEdit{
		Content: &content, Components: &[]discordgo.MessageComponent{},
	}); err != nil {
		b.log.Warn("edit expired confirmation", "err", err)
	}
}

func (b *Bot) onAbort(i *discordgo.InteractionCreate, token string) {
	pa := b.take(token)
	msg := "Already handled or expired."
	if pa != nil {
		msg = fmt.Sprintf("✖️ Cancelled — `%s` was not %s.", pa.resource.Name, strings.ToLower(pastVerb(pa.action)))
	}
	b.updateMessage(i, msg)
}

func (b *Bot) onConfirm(i *discordgo.InteractionCreate, user, token string) {
	pa := b.take(token)
	if pa == nil {
		b.updateMessage(i, "⌛ This confirmation expired or was already used. Run the command again.")
		return
	}
	r := pa.resource
	b.updateMessage(i, fmt.Sprintf("⏳ %s `%s`…", progressVerb(pa.action), r.Name))
	go func() {
		ctx := b.ctx
		// Re-resolve: the resource may have changed state since the prompt.
		if cur, ok := b.svc.Find(ctx, r.ID); ok {
			r = cur
		}
		err := b.svc.Control(ctx, user, r, pa.action)
		content := fmt.Sprintf("✅ %s `%s` — done.", pastVerb(pa.action), r.Name)
		if err != nil {
			content = fmt.Sprintf("❌ Failed to %s `%s`: %s", pa.action, r.Name, clip(err.Error(), 1500))
		}
		if _, eerr := b.session.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: &content, Components: &[]discordgo.MessageComponent{},
		}); eerr != nil {
			b.log.Warn("edit control result", "err", eerr)
		}
	}()
}

func (b *Bot) onCancelAutoStop(i *discordgo.InteractionCreate, user, id string) {
	month, ok := autostop.ParseCancelID(id)
	if !ok || b.engine == nil {
		b.ephemeral(i, "Invalid auto-stop button.")
		return
	}
	cancelled, err := b.engine.Cancel(b.ctx, month, user)
	if err != nil {
		b.ephemeral(i, "❌ Cancel failed: "+err.Error())
		return
	}
	note := "✅ Auto-stop cancelled."
	if !cancelled {
		note = "Nothing to cancel — auto-stop already ran or was cancelled."
	}
	embeds := i.Message.Embeds
	_ = b.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content: note, Embeds: embeds, Components: []discordgo.MessageComponent{},
		},
	})
}

// ---- response helpers ----

func (b *Bot) ephemeral(i *discordgo.InteractionCreate, content string) {
	if err := b.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content, Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		b.log.Warn("respond", "err", err)
	}
}

func (b *Bot) updateMessage(i *discordgo.InteractionCreate, content string) {
	if err := b.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: content, Components: []discordgo.MessageComponent{}},
	}); err != nil {
		b.log.Warn("update message", "err", err)
	}
}

// defer acknowledges within Discord's 3-second window; follow with editReply.
func (b *Bot) deferReply(i *discordgo.InteractionCreate) bool {
	if err := b.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		b.log.Warn("defer", "err", err)
		return false
	}
	return true
}

func (b *Bot) editReply(i *discordgo.InteractionCreate, content string, embeds []*discordgo.MessageEmbed) {
	if len(embeds) > 10 {
		embeds = embeds[:10]
		content = strings.TrimSpace(content + "\n_(showing first 10 groups)_")
	}
	if _, err := b.session.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &content, Embeds: &embeds,
	}); err != nil {
		b.log.Warn("edit reply", "err", err)
	}
}

func progressVerb(a provider.Action) string {
	if a == provider.ActionStop {
		return "Stopping"
	}
	return "Starting"
}

func pastVerb(a provider.Action) string {
	if a == provider.ActionStop {
		return "Stopped"
	}
	return "Started"
}
