package connector

import (
	"maunium.net/go/mautrix/bridgev2/commands"
)

// CommandInvisible sets Steam presence to invisible
var CommandInvisible = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		// Get the user's login
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("You're not logged in to Steam")
			return
		}

		// Get the Steam client
		client, ok := login.Client.(*SteamClient)
		if !ok {
			ce.Reply("Failed to get Steam client")
			return
		}

		// Check if presence manager is available
		if client.presenceManager == nil {
			ce.Reply("Presence tracking is not enabled. Please enable it in the bridge configuration.")
			return
		}

		// Set invisible mode
		err := client.presenceManager.SetInvisible(ce.Ctx, true)
		if err != nil {
			ce.Reply("Failed to set invisible mode: %v", err)
			return
		}

		ce.Reply("✅ Steam status set to **INVISIBLE** (appear offline). Use `visible` to return to normal presence tracking.")
	},
	Name: "invisible",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Set your Steam status to INVISIBLE (appear offline)",
	},
}

// CommandVisible exits invisible mode and returns to normal presence tracking
var CommandVisible = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		// Get the user's login
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("You're not logged in to Steam")
			return
		}

		// Get the Steam client
		client, ok := login.Client.(*SteamClient)
		if !ok {
			ce.Reply("Failed to get Steam client")
			return
		}

		// Check if presence manager is available
		if client.presenceManager == nil {
			ce.Reply("Presence tracking is not enabled. Please enable it in the bridge configuration.")
			return
		}

		// Exit invisible mode
		err := client.presenceManager.SetInvisible(ce.Ctx, false)
		if err != nil {
			ce.Reply("Failed to exit invisible mode: %v", err)
			return
		}

		ce.Reply("✅ Exiting **INVISIBLE** mode. Your Steam status will now sync with your Matrix presence.")
	},
	Name: "visible",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Exit INVISIBLE mode and resume normal presence tracking",
	},
}

// RegisterPresenceCommands registers the presence-related commands with the bridge
func RegisterPresenceCommands(proc *commands.Processor) {
	proc.AddHandlers(CommandInvisible, CommandVisible)
}
