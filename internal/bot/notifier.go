package bot

import (
	"context"
	"errors"

	"github.com/bwmarrin/discordgo"

	"cloud-control/internal/notify"
)

// Send implements notify.Notifier, routing alerts to the app's own channel
// when configured and falling back to the global alerts channel.
func (b *Bot) Send(_ context.Context, t notify.Target, m notify.Message) error {
	if b.session == nil {
		return errors.New("discord session not open")
	}
	ch := b.channelFor(t)
	msg := &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{toEmbed(m)},
		Components:      toComponents(m.Buttons),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	}
	if m.MentionOwner {
		msg.Content = "<@" + b.owner.UserID + ">"
		msg.AllowedMentions.Users = []string{b.owner.UserID}
	}
	_, err := b.session.ChannelMessageSendComplex(ch, msg)
	return err
}

func (b *Bot) channelFor(t notify.Target) string {
	sec := b.cfg.Secrets
	if t.Kind == notify.Digest {
		return sec.DiscordDigestChannelID
	}
	return b.svc.Apps().AlertsChannel(t.App, sec.DiscordAlertsChannelID)
}

var styleColor = map[notify.Style]int{
	notify.Info: 0x5865F2,
	notify.Good: 0x57F287,
	notify.Warn: 0xFEE75C,
	notify.Bad:  0xED4245,
}

func toEmbed(m notify.Message) *discordgo.MessageEmbed {
	e := &discordgo.MessageEmbed{
		Title:       clip(m.Title, 256),
		Description: clip(m.Body, 4000),
		Color:       styleColor[m.Style],
	}
	for i, f := range m.Fields {
		if i == 25 {
			break
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: clip(f.Name, 256), Value: clip(f.Value, 1024), Inline: f.Inline})
	}
	if m.Footer != "" {
		e.Footer = &discordgo.MessageEmbedFooter{Text: clip(m.Footer, 2048)}
	}
	return e
}

func toComponents(bs []notify.Button) []discordgo.MessageComponent {
	if len(bs) == 0 {
		return nil
	}
	row := discordgo.ActionsRow{}
	for i, b := range bs {
		if i == 5 {
			break
		}
		style := discordgo.SecondaryButton
		if b.Danger {
			style = discordgo.DangerButton
		}
		row.Components = append(row.Components, discordgo.Button{Label: b.Label, CustomID: b.CustomID, Style: style})
	}
	return []discordgo.MessageComponent{row}
}

// clip truncates s to at most n runes.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
