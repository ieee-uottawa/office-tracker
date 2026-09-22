package commands

import (
	"fmt"
	"sort"
	"strings"

	"office/internal/logging"
	"office/internal/service"

	"github.com/bwmarrin/discordgo"
)

var jingleLog = logging.Component("discord.commands.jingle")

// knownJingles maps the identifier stored on a user (and sent to the ESP32
// in the scan response) to a human-readable label. The ESP32 firmware
// (lib/feedback/feedback.cpp) has its own local table of Note sequences
// keyed by these same identifiers - this list must stay in sync with that
// table. Deliberately a small, curated set rather than an open "name any
// song" lookup: see the firmware's per-jingle comments for why each one
// was judged safe to include.
var knownJingles = map[string]string{
	"mario":          "Super Mario Bros. Ground Theme (opening hook)",
	"imperial-march": "The Imperial March (Star Wars)",
	"canada":         "O Canada (public domain, 1880)",
	"jingle-bells":   "Jingle Bells (public domain, 1857)",
	"sad-trombone":   "Sad trombone (generic fail sting)",
	"checkmark":      "Checkmark - a short generic confirm beep",
	"jana":           "Morse code for \"JANA\"",
	"default":        "Reset to the device default",
}

// JingleCommands handles the /jingle slash command.
type JingleCommands struct {
	userSvc *service.UserService
}

// NewJingleCommands creates a new JingleCommands handler.
func NewJingleCommands(userSvc *service.UserService) *JingleCommands {
	return &JingleCommands{userSvc: userSvc}
}

// GetApplicationCommands returns the slash command definition for /jingle.
func (jc *JingleCommands) GetApplicationCommands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "jingle",
			Description: "Set your personal sign-in jingle",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "name",
					Description: "Jingle name - use /jingle with no name to see the list",
					Type:        discordgo.ApplicationCommandOptionString,
					Required:    false,
				},
			},
		},
	}
}

// HandleCommand routes the /jingle command.
func (jc *JingleCommands) HandleCommand(s *discordgo.Session, i *discordgo.InteractionCreate, cmdName string) {
	if cmdName != "jingle" {
		return
	}

	if jc.userSvc == nil {
		respondEphemeral(s, i, "❌ User service not configured")
		return
	}

	user := interactionUser(i)
	if user == nil {
		respondEphemeral(s, i, "❌ Unable to identify user.")
		return
	}

	appUser, err := jc.userSvc.GetUserByDiscordID(user.ID)
	if err != nil {
		jingleLog.Error("failed to find user for jingle command", "discord_id", user.ID, "err", err)
		respondEphemeral(s, i, "❌ No linked user account found.")
		return
	}

	var requested string
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "name" {
			requested = strings.TrimSpace(opt.StringValue())
		}
	}

	// No name given - show the list of known jingles.
	if requested == "" {
		respondEphemeral(s, i, "🎵 Known jingles:\n"+jingleList())
		return
	}

	key := normalizeJingleName(requested)
	label, ok := knownJingles[key]
	if !ok {
		jingleLog.Info("unknown jingle requested", "user", appUser.Name, "requested", requested)
		respondEphemeral(s, i, fmt.Sprintf(
			"🚩 \"%s\" isn't a jingle I have. Known jingles:\n%s",
			requested, jingleList(),
		))
		return
	}

	storeValue := key
	if key == "default" {
		storeValue = ""
	}

	if _, err := jc.userSvc.UpdateJingle(appUser.ID, storeValue); err != nil {
		jingleLog.Error("failed to update jingle", "user", appUser.Name, "err", err)
		respondEphemeral(s, i, "❌ Failed to save your jingle.")
		return
	}

	jingleLog.Info("jingle set", "user", appUser.Name, "jingle", key)
	respondEphemeral(s, i, fmt.Sprintf("✅ Your sign-in jingle is now: %s", label))
}

// normalizeJingleName lowercases and collapses spaces/underscores to
// hyphens so "Imperial March", "imperial_march", and "imperial-march" all
// match the same key.
func normalizeJingleName(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	lower = strings.ReplaceAll(lower, "_", "-")
	lower = strings.ReplaceAll(lower, " ", "-")
	for strings.Contains(lower, "--") {
		lower = strings.ReplaceAll(lower, "--", "-")
	}
	return lower
}

func jingleList() string {
	keys := make([]string, 0, len(knownJingles))
	for k := range knownJingles {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "• `%s` — %s\n", k, knownJingles[k])
	}
	return b.String()
}
