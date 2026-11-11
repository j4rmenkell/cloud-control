package discord

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"cloud-control/internal/autostop"
	"cloud-control/internal/core"
	"cloud-control/internal/provider"
	"cloud-control/internal/store"
)

func (b *Bot) appOption() *discordgo.ApplicationCommandOption {
	opt := &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: "app", Description: "Only this app",
	}
	for _, n := range append(b.svc.Apps().Names(), core.Unassigned) {
		opt.Choices = append(opt.Choices, &discordgo.ApplicationCommandOptionChoice{Name: n, Value: n})
	}
	return opt
}

func (b *Bot) commands() []*discordgo.ApplicationCommand {
	provOpt := &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: "provider", Description: "Only this provider",
	}
	for _, p := range b.svc.ProviderNames() {
		provOpt.Choices = append(provOpt.Choices, &discordgo.ApplicationCommandOptionChoice{Name: p, Value: p})
	}
	resourceOpt := func(verb string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type: discordgo.ApplicationCommandOptionString, Name: "resource", Required: true, Autocomplete: true,
			Description: "Resource to " + verb,
		}
	}
	minN, maxN := 1.0, 50.0
	return []*discordgo.ApplicationCommand{
		{Name: "ping", Description: "Health of the bot itself"},
		{Name: "status", Description: "Every resource, grouped by app, with health",
			Options: []*discordgo.ApplicationCommandOption{b.appOption()}},
		{Name: "resources", Description: "List resources with IDs and available actions",
			Options: []*discordgo.ApplicationCommandOption{provOpt, b.appOption()}},
		{Name: "cost", Description: "Month-to-date spend and forecast, per provider and per app",
			Options: []*discordgo.ApplicationCommandOption{b.appOption()}},
		{Name: "stop", Description: "Stop a resource (asks for confirmation)",
			Options: []*discordgo.ApplicationCommandOption{resourceOpt("stop")}},
		{Name: "start", Description: "Start a resource (asks for confirmation)",
			Options: []*discordgo.ApplicationCommandOption{resourceOpt("start")}},
		{Name: "audit", Description: "Recent control actions",
			Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionInteger, Name: "n", Description: "How many (default 10)",
				MinValue: &minN, MaxValue: maxN,
			}}},
		{Name: "autostop", Description: "Budget auto-stop",
			Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionString, Name: "action", Description: "on, off or status", Required: true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "status", Value: "status"}, {Name: "on", Value: "on"}, {Name: "off", Value: "off"},
				},
			}}},
	}
}

type opts map[string]*discordgo.ApplicationCommandInteractionDataOption

func optionMap(i *discordgo.InteractionCreate) opts {
	m := opts{}
	for _, o := range i.ApplicationCommandData().Options {
		m[o.Name] = o
	}
	return m
}

func (o opts) str(name string) string {
	if v, ok := o[name]; ok {
		return v.StringValue()
	}
	return ""
}

func (o opts) integer(name string, def int) int {
	if v, ok := o[name]; ok {
		return int(v.IntValue())
	}
	return def
}

func (b *Bot) handleCommand(i *discordgo.InteractionCreate) {
	o := optionMap(i)
	switch i.ApplicationCommandData().Name {
	case "ping":
		b.cmdPing(i)
	case "status":
		b.cmdStatus(i, o.str("app"))
	case "resources":
		b.cmdResources(i, o.str("provider"), o.str("app"))
	case "cost":
		b.cmdCost(i, o.str("app"))
	case "stop":
		b.cmdControl(i, provider.ActionStop, o.str("resource"))
	case "start":
		b.cmdControl(i, provider.ActionStart, o.str("resource"))
	case "audit":
		b.cmdAudit(i, o.integer("n", 10))
	case "autostop":
		b.cmdAutoStop(i, o.str("action"), interactionUserID(i))
	default:
		b.ephemeral(i, "Unknown command.")
	}
}

func (b *Bot) cmdPing(i *discordgo.InteractionCreate) {
	health, cost := b.svc.LastPolls()
	b.ephemeral(i, fmt.Sprintf("🏓 Pong — gateway latency %s\nUp since %s · last health poll %s · last cost poll %s · %d resources",
		b.session.HeartbeatLatency().Round(time.Millisecond), relTime(b.svc.Started()), relTime(health), relTime(cost),
		len(b.svc.Inventory(b.ctx))))
}

// groupByApp returns app display names in configured order (unassigned last).
func (b *Bot) groupByApp(rs []provider.Resource) ([]string, map[string][]provider.Resource) {
	groups := map[string][]provider.Resource{}
	for _, r := range rs {
		groups[appName(r.App)] = append(groups[appName(r.App)], r)
	}
	var order []string
	for _, n := range append(b.svc.Apps().Names(), core.Unassigned) {
		if len(groups[n]) > 0 {
			order = append(order, n)
		}
	}
	return order, groups
}

func (b *Bot) cmdStatus(i *discordgo.InteractionCreate, app string) {
	if !b.deferReply(i) {
		return
	}
	ctx := b.ctx
	rs := core.Filter(b.svc.Inventory(ctx), app, "")
	views, err := b.svc.HealthViews(ctx, rs)
	if err != nil {
		b.editReply(i, "❌ "+err.Error(), nil)
		return
	}
	byID := map[string]core.HealthView{}
	for _, v := range views {
		byID[v.Resource.ID] = v
	}
	order, groups := b.groupByApp(rs)
	var embeds []*discordgo.MessageEmbed
	limit := descLimit(len(order))
	for _, name := range order {
		worst := provider.Healthy
		var lines []string
		for _, r := range groups[name] {
			v := byID[r.ID]
			worst = worse(worst, v.Health.State)
			line := fmt.Sprintf("%s **%s** · %s/%s", core.HealthEmoji(v.Health.State), r.Name, r.Provider, r.Type)
			if v.Health.State != provider.Healthy && v.Health.State != provider.Stopped && v.Health.Detail != "" {
				line += "\n  ↳ " + clip(v.Health.Detail, 120)
			}
			lines = append(lines, line)
		}
		embeds = append(embeds, &discordgo.MessageEmbed{
			Title: name, Description: clip(strings.Join(lines, "\n"), limit), Color: healthColor(worst),
		})
	}
	health, _ := b.svc.LastPolls()
	content := "Last health poll " + relTime(health)
	if len(embeds) == 0 {
		content = "No resources found" + appSuffix(app) + "."
	}
	b.editReply(i, content, embeds)
}

func (b *Bot) cmdResources(i *discordgo.InteractionCreate, prov, app string) {
	if !b.deferReply(i) {
		return
	}
	rs := core.Filter(b.svc.Inventory(b.ctx), app, prov)
	order, groups := b.groupByApp(rs)
	limit := descLimit(len(order))
	var embeds []*discordgo.MessageEmbed
	for _, name := range order {
		var lines []string
		for _, r := range groups[name] {
			var acts []string
			for _, a := range []provider.Action{provider.ActionStop, provider.ActionStart} {
				if b.svc.Can(r, a) {
					acts = append(acts, string(a))
				}
			}
			actions := "—"
			if len(acts) > 0 {
				actions = strings.Join(acts, ", ")
			}
			crit := ""
			if b.svc.Apps().IsCritical(r) {
				crit = " 🛡️"
			}
			lines = append(lines, fmt.Sprintf("`%s` **%s**%s · %s/%s · %s · actions: %s",
				r.ShortID(), r.Name, crit, r.Provider, r.Type, orDash(r.Region), actions))
		}
		embeds = append(embeds, &discordgo.MessageEmbed{Title: name, Description: clip(strings.Join(lines, "\n"), limit), Color: 0x5865F2})
	}
	content := fmt.Sprintf("%d resources · 🛡️ = critical (auto-stop never touches it)", len(rs))
	if len(rs) == 0 {
		content = "No resources found."
	}
	b.editReply(i, content, embeds)
}

func (b *Bot) cmdCost(i *discordgo.InteractionCreate, app string) {
	if !b.deferReply(i) {
		return
	}
	ctx := b.ctx
	sum, err := b.svc.CurrentCost(ctx)
	if err != nil {
		b.editReply(i, "❌ "+err.Error(), nil)
		return
	}
	if len(sum.Reports) == 0 {
		sum = b.svc.PollCost(ctx) // nothing stored this month yet
	}
	cur := sum.Currency
	e := &discordgo.MessageEmbed{Title: "💸 Cost — " + sum.Month, Color: costColor(sum.Percent)}
	if app != "" {
		v := sum.ByApp[appKey(app)]
		e.Title += " · " + app
		e.Description = fmt.Sprintf("**MTD:** %s\n%.0f%% of the total %s budget",
			core.Money(v, cur, sum.Estimated), pct(v, sum.Budget), core.Amount(sum.Budget, cur))
	} else {
		e.Description = fmt.Sprintf("**MTD:** %s of %s (%.0f%%)\n%s\n**Forecast:** %s",
			core.Money(sum.Total, cur, sum.Estimated), core.Amount(sum.Budget, cur), sum.Percent,
			core.Bar(sum.Percent), core.Money(sum.Forecast, cur, sum.Estimated))
		names := make([]string, 0, len(sum.Reports))
		for n := range sum.Reports {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			c := sum.Reports[n]
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: n, Inline: true,
				Value: fmt.Sprintf("%s\nforecast %s\nas of %s", core.Money(c.MTD, c.Currency, c.Estimated),
					core.Amount(c.Forecast, c.Currency), relTime(c.AsOf))})
		}
		var byApp []string
		for _, n := range append(b.svc.Apps().Names(), core.Unassigned) {
			if v, ok := sum.ByApp[appKey(n)]; ok {
				byApp = append(byApp, fmt.Sprintf("**%s** %s", n, core.Amount(v, cur)))
			}
		}
		if len(byApp) > 0 {
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "By app", Value: clip(strings.Join(byApp, "\n"), 1024)})
		}
	}
	if !sum.Known {
		var missing []string
		for p, msg := range sum.Failed {
			missing = append(missing, fmt.Sprintf("%s (%s)", p, clip(msg, 80)))
		}
		sort.Strings(missing)
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "⚠️ Missing", Value: clip(strings.Join(missing, "\n"), 1024)})
	}
	if !sum.AsOf.IsZero() {
		e.Footer = &discordgo.MessageEmbedFooter{Text: "Billing data lags · as of " + sum.AsOf.In(b.cfg.Location).Format("Jan 2 15:04 MST")}
	}
	b.editReply(i, "", []*discordgo.MessageEmbed{e})
}

func (b *Bot) cmdControl(i *discordgo.InteractionCreate, a provider.Action, key string) {
	r, ok := b.svc.Find(b.ctx, key)
	if !ok {
		b.ephemeral(i, "Resource not found. Pick one from the autocomplete list.")
		return
	}
	if !b.svc.Can(r, a) {
		b.ephemeral(i, fmt.Sprintf("Can't %s `%s` right now (power state: %s).", a, r.Name, orDash(string(r.Power))))
		return
	}
	b.askConfirm(i, a, r)
}

func (b *Bot) handleAutocomplete(i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	action := provider.Action(data.Name)
	var query string
	for _, o := range data.Options {
		if o.Focused {
			query = strings.ToLower(o.StringValue())
		}
	}
	choices := []*discordgo.ApplicationCommandOptionChoice{}
	for _, r := range b.svc.Inventory(b.ctx) {
		if len(choices) == 25 {
			break
		}
		if !b.svc.Can(r, action) {
			continue
		}
		label := fmt.Sprintf("[%s] %s (%s)", appName(r.App), r.Name, r.Provider)
		if query != "" && !strings.Contains(strings.ToLower(label), query) {
			continue
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: clip(label, 100), Value: r.ShortID()})
	}
	if err := b.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	}); err != nil {
		b.log.Warn("autocomplete", "err", err)
	}
}

func (b *Bot) cmdAudit(i *discordgo.InteractionCreate, n int) {
	entries, err := b.svc.Store().RecentAudit(b.ctx, n)
	if err != nil {
		b.ephemeral(i, "❌ "+err.Error())
		return
	}
	if len(entries) == 0 {
		b.ephemeral(i, "No actions recorded yet.")
		return
	}
	var lines []string
	for _, e := range entries {
		target := e.ResourceName
		if e.App != "" {
			target = "[" + e.App + "] " + target
		}
		line := fmt.Sprintf("%s %s **%s** %s → %s", absTime(e.At), core.ActorMention(e.Actor), e.Action,
			strings.TrimSpace(target), resultEmoji(e.Result)+e.Result)
		if e.Detail != "" && e.Result == "error" {
			line += " — " + clip(e.Detail, 100)
		}
		lines = append(lines, line)
	}
	b.respondEmbed(i, &discordgo.MessageEmbed{Title: fmt.Sprintf("📜 Last %d actions", len(entries)),
		Description: clip(strings.Join(lines, "\n"), 4000), Color: 0x5865F2})
}

func (b *Bot) cmdAutoStop(i *discordgo.InteractionCreate, action, user string) {
	if b.engine == nil {
		b.ephemeral(i, "Auto-stop engine not running.")
		return
	}
	ctx := b.ctx
	switch action {
	case "on", "off":
		if err := b.engine.SetEnabled(ctx, action == "on", user); err != nil {
			b.ephemeral(i, "❌ "+err.Error())
			return
		}
		msg := "✅ Auto-stop **enabled**. This month's cancellation (if any) was cleared."
		if action == "off" {
			msg = "⏸️ Auto-stop **disabled**. Any pending auto-stop was cancelled."
		}
		b.respondText(i, msg)
	default:
		st, err := b.engine.Status(ctx)
		if err != nil {
			b.ephemeral(i, "❌ "+err.Error())
			return
		}
		sum, err := b.svc.CurrentCost(ctx)
		if err != nil {
			b.ephemeral(i, "❌ "+err.Error())
			return
		}
		state := "🟢 on"
		if !st.Enabled {
			state = "⚪ off"
		}
		spend := core.Money(sum.Total, sum.Currency, sum.Estimated) + fmt.Sprintf(" (%.0f%%)", sum.Percent)
		if !sum.Known {
			spend += " — ⚠️ incomplete, won't trigger"
		}
		run := "not triggered this month"
		if st.Run != nil {
			run = fmt.Sprintf("%s (spend %s)", st.Run.Status, core.Amount(st.Run.Spend, sum.Currency))
			if st.Run.Status == store.RunPending {
				run += " · runs " + relTime(st.Run.Deadline)
			}
			if st.Run.DryRun {
				run += " · dry run"
			}
		}
		e := &discordgo.MessageEmbed{
			Title: "🛑 Auto-stop", Color: 0x5865F2,
			Fields: []*discordgo.MessageEmbedField{
				{Name: "State", Value: fmt.Sprintf("%s (%s)", state, st.Source), Inline: true},
				{Name: "Dry run", Value: yesNo(st.DryRun), Inline: true},
				{Name: "Threshold", Value: fmt.Sprintf("%.0f%% of %s", st.Threshold, core.Amount(sum.Budget, sum.Currency)), Inline: true},
				{Name: "Grace", Value: st.Grace.String(), Inline: true},
				{Name: "Current spend", Value: spend, Inline: true},
				{Name: "This month", Value: run, Inline: true},
				{Name: fmt.Sprintf("Would stop now (%d)", len(st.Plan)), Value: clip(autostop.PlanLines(st.Plan), 1024)},
			},
		}
		b.respondEmbed(i, e)
	}
}

func (b *Bot) respondText(i *discordgo.InteractionCreate, content string) {
	if err := b.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content},
	}); err != nil {
		b.log.Warn("respond", "err", err)
	}
}

func (b *Bot) respondEmbed(i *discordgo.InteractionCreate, e *discordgo.MessageEmbed) {
	if err := b.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{e}},
	}); err != nil {
		b.log.Warn("respond", "err", err)
	}
}

// ---- small helpers ----

func appName(app string) string { return core.Display(app) }

func appKey(display string) string {
	if display == core.Unassigned {
		return ""
	}
	return display
}

func appSuffix(app string) string {
	if app == "" {
		return ""
	}
	return " for " + app
}

// descLimit keeps the total across embeds under Discord's 6000-char cap.
func descLimit(n int) int {
	if n <= 1 {
		return 4000
	}
	return min(4000, 5000/n)
}

var severity = map[provider.HealthState]int{provider.Healthy: 0, provider.Stopped: 0, provider.Unknown: 1, provider.Degraded: 2, provider.Down: 3}

func worse(a, b provider.HealthState) provider.HealthState {
	if severity[b] > severity[a] {
		return b
	}
	return a
}

func healthColor(s provider.HealthState) int {
	switch s {
	case provider.Healthy:
		return 0x57F287
	case provider.Degraded, provider.Unknown:
		return 0xFEE75C
	}
	return 0xED4245
}

func costColor(p float64) int {
	switch {
	case p >= 100:
		return 0xED4245
	case p >= 80:
		return 0xFEE75C
	}
	return 0x57F287
}

func pct(v, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return v / total * 100
}

func resultEmoji(r string) string {
	switch r {
	case "ok":
		return "✅ "
	case "error":
		return "❌ "
	case "dry-run":
		return "🧪 "
	case "cancelled":
		return "✖️ "
	}
	return ""
}

func relTime(t time.Time) string { return core.RelTime(t) }
func absTime(t time.Time) string { return core.AbsTime(t) }

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
