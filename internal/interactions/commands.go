package interactions

import "cloud-control/internal/core"

// CommandDef is a slash command as registered with Discord.
type CommandDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Options     []OptionDef `json:"options,omitempty"`
	// "0" = only server admins can see/use it by default (owner check still
	// applies). Hides the commands from everyone else in the server.
	DefaultMemberPermissions *string `json:"default_member_permissions,omitempty"`
	Contexts                 []int   `json:"contexts,omitempty"` // 0 = guild only
}

type OptionDef struct {
	Type        int         `json:"type"` // 3 = string
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Required    bool        `json:"required,omitempty"`
	Choices     []ChoiceDef `json:"choices,omitempty"`
}

type ChoiceDef struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Commands returns the commands this handler serves. Register them with
// cmd/register whenever this list or the configured apps change.
func Commands(appNames []string) []CommandDef {
	adminsOnly := "0"
	app := OptionDef{Type: 3, Name: "app", Description: "Only this app"}
	for _, n := range append(append([]string(nil), appNames...), core.Unassigned) {
		app.Choices = append(app.Choices, ChoiceDef{Name: n, Value: n})
	}
	defs := []CommandDef{
		{Name: "ping", Description: "Health of the bot itself"},
		{Name: "status", Description: "Every resource, grouped by app, with health", Options: []OptionDef{app}},
	}
	for i := range defs {
		defs[i].DefaultMemberPermissions = &adminsOnly
		defs[i].Contexts = []int{0}
	}
	return defs
}
