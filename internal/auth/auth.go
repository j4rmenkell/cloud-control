// Package auth holds the owner-only access check applied to every command,
// autocomplete request and button press.
package auth

import "strings"

type Owner struct {
	UserID  string
	GuildID string
}

// Allowed reports whether an interaction from userID in guildID may proceed.
// Interactions outside the configured guild (including DMs) are rejected.
func (o Owner) Allowed(userID, guildID string) bool {
	return strings.TrimSpace(o.UserID) != "" &&
		userID == o.UserID &&
		guildID == o.GuildID
}
