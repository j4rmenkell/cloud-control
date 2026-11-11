package interactions

import "encoding/json"

// The subset of Discord's interaction payloads cloud-control uses.
// https://discord.com/developers/docs/interactions/receiving-and-responding

const (
	typePing               = 1
	typeApplicationCommand = 2

	respPong                     = 1
	respChannelMessageWithSource = 4

	flagEphemeral = 1 << 6
)

type Interaction struct {
	Type    int          `json:"type"`
	ID      string       `json:"id"`
	Token   string       `json:"token"`
	GuildID string       `json:"guild_id"`
	Data    *CommandData `json:"data"`
	Member  *Member      `json:"member"`
	User    *User        `json:"user"`
}

type CommandData struct {
	Name    string   `json:"name"`
	Options []Option `json:"options"`
}

type Option struct {
	Name  string          `json:"name"`
	Type  int             `json:"type"`
	Value json.RawMessage `json:"value"`
}

type Member struct {
	User *User `json:"user"`
}

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// UserID returns the invoking user's ID (member.user in guilds, user in DMs).
func (i Interaction) UserID() string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// Option returns a string option's value, or "".
func (d *CommandData) Option(name string) string {
	if d == nil {
		return ""
	}
	for _, o := range d.Options {
		if o.Name == name {
			var s string
			if json.Unmarshal(o.Value, &s) == nil {
				return s
			}
		}
	}
	return ""
}

type Response struct {
	Type int           `json:"type"`
	Data *ResponseData `json:"data,omitempty"`
}

type ResponseData struct {
	Content         string           `json:"content,omitempty"`
	Embeds          []Embed          `json:"embeds,omitempty"`
	Flags           int              `json:"flags,omitempty"`
	AllowedMentions *AllowedMentions `json:"allowed_mentions,omitempty"`
}

// AllowedMentions with an empty Parse list means the message can't ping
// anyone, even if a resource name or error text contains @everyone.
type AllowedMentions struct {
	Parse []string `json:"parse"`
}

type Embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color,omitempty"`
	Fields      []EmbedField `json:"fields,omitempty"`
	Footer      *EmbedFooter `json:"footer,omitempty"`
}

type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type EmbedFooter struct {
	Text string `json:"text"`
}
