package connector

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

// initializePresenceStream establishes the presence stream used to keep Steam friends'
// DM room topics (m.room.topic) in sync with their current game (Phase 1: game name
// only, no rich presence text yet).
//
// Unlike the message stream, loss of the presence stream is not fatal to the bridge
// connection: on error we log and return without tearing down the main Steam
// connection or triggering reconnection via handleTransientDisconnect. Presence-driven
// topic updates simply stop until the bridge (and this stream) reconnect.
func (sc *SteamClient) initializePresenceStream(ctx context.Context) error {
	if sc.connector == nil || !sc.connector.Config.PresenceTopic.Enabled {
		sc.br.Log.Debug().Msg("Presence topic feature disabled, not starting presence stream")
		return nil
	}

	if sc.presenceClient == nil {
		return fmt.Errorf("presence client not available")
	}

	stream, err := sc.presenceClient.SubscribeToPresence(ctx, &steamapi.PresenceSubscriptionRequest{
		SteamId:             sc.steamID(),
		RichPresenceEnabled: sc.connector.Config.PresenceTopic.RichPresenceEnabled,
	})
	if err != nil {
		return fmt.Errorf("failed to start presence subscription: %w", err)
	}

	sc.br.Log.Info().Msg("Steam presence topic stream established successfully")

	go sc.processPresenceStream(ctx, stream)

	return nil
}

// processPresenceStream reads presence events from the stream and updates the
// corresponding friend's DM room topic. On a stream error, it logs and returns -
// presence topic loss does not affect the main Steam connection.
func (sc *SteamClient) processPresenceStream(ctx context.Context, stream steamapi.SteamPresenceService_SubscribeToPresenceClient) {
	defer func() {
		if r := recover(); r != nil {
			sc.br.Log.Error().Interface("panic", r).Msg("Panic in presence topic stream processing")
		}
	}()

	for {
		select {
		case <-ctx.Done():
			sc.br.Log.Info().Msg("Presence topic stream context cancelled")
			return
		default:
			ev, err := stream.Recv()
			if err != nil {
				sc.br.Log.Warn().Err(err).Msg("Presence topic stream disconnected; presence-driven topic updates stopped until reconnect")
				return
			}

			sc.handlePresenceTopicEvent(ctx, ev)
		}
	}
}

// handlePresenceTopicEvent updates a Steam friend's DM room topic in response to a
// live presence event received from the presence stream.
func (sc *SteamClient) handlePresenceTopicEvent(ctx context.Context, ev *steamapi.PresenceEvent) {
	if ev == nil {
		return
	}

	sc.br.Log.Debug().
		Uint64("steam_id", ev.SteamId).
		Str("status", ev.Status.String()).
		Str("current_game", ev.CurrentGame).
		Bool("has_rich_presence", ev.HasRichPresence).
		Str("rich_presence_status_text", ev.RichPresenceStatusText).
		Msg("Received presence event")

	if err := sc.updateFriendDMTopic(ctx, ev.SteamId, ev.Status, ev.CurrentGame, ev.RichPresenceStatusText, ev.RichPresenceTokens); err != nil {
		sc.br.Log.Warn().Err(err).
			Uint64("steam_id", ev.SteamId).
			Msg("Failed to update DM room topic from presence event")
	}
}

// updateFriendDMTopic looks up the existing DM portal for the given friend (without
// creating one) and, if it exists, updates its topic to reflect the friend's current
// presence/game (and, when available, resolved or raw rich-presence flavor text). This
// is shared by the live presence stream handler (handlePresenceTopicEvent) and the
// startup friend sync (syncFriendsOnStartup) so both paths build and apply the topic
// identically.
//
// richPresenceStatusText and rawTokens come from the live presence stream's
// PresenceEvent; syncFriendsOnStartup's seed path only has basic friend-list data
// (GetFriendsList's Friend message was not extended with rich-presence fields), so it
// passes an empty string / nil map - live rich-presence data will arrive shortly after
// via the stream once Steam sends it, same as it does today for basic presence changes.
func (sc *SteamClient) updateFriendDMTopic(ctx context.Context, steamID uint64, presenceStatus steamapi.PersonaState, currentGame string, richPresenceStatusText string, rawTokens map[string]string) error {
	if sc.connector == nil || !sc.connector.Config.PresenceTopic.Enabled {
		return nil
	}

	portalKey := networkid.PortalKey{
		ID:       makePortalID(steamID),
		Receiver: sc.UserLogin.ID,
	}

	portal, err := sc.UserLogin.Bridge.GetExistingPortalByKey(ctx, portalKey)
	if err != nil {
		return fmt.Errorf("failed to look up existing portal: %w", err)
	}
	if portal == nil {
		// No DM room has ever been opened with this friend - don't force-create one
		// just because their presence/game state changed.
		sc.br.Log.Debug().
			Uint64("steam_id", steamID).
			Msg("Skipping DM room topic update - no existing DM room with this friend")
		return nil
	}

	topic := buildPresenceTopic(presenceStatus, currentGame, richPresenceStatusText, sc.connector.Config.PresenceTopic.ShowRawTokens, rawTokens)

	sc.br.Log.Info().
		Uint64("steam_id", steamID).
		Str("portal_id", string(portal.ID)).
		Str("previous_topic", portal.Topic).
		Str("new_topic", topic).
		Msg("Updating DM room topic from Steam presence")

	// A zero time.Time (not time.Now()) matches mautrix-signal's convention for live info
	// updates (e.g. groupinfo.go, handlesignal.go) - it omits the Matrix "ts" timestamp
	// override entirely (only set when > 0), letting the homeserver stamp the event with
	// its own receipt time. Explicit ts overrides are for backfill/historical events with a
	// known true origin time, which doesn't apply to a live presence-driven update.
	portal.UpdateInfo(ctx, &bridgev2.ChatInfo{Topic: ptr.Ptr(topic)}, sc.UserLogin, nil, time.Time{})

	return nil
}

// buildPresenceTopic builds the DM room topic string from a friend's presence status,
// current game, and (optionally) rich-presence flavor text:
//   - Empty when the friend is offline/invisible.
//   - "{game} — {rich presence text}" when both a game name and resolved rich-presence
//     status text are available.
//   - "{rich presence text}" alone when resolved text is available but no game name is
//     (Steam doesn't always include the human-readable game name on packets that do
//     carry rich presence data - observed empirically - so requiring both would blank
//     the topic even when we have real, useful text to show).
//   - "{game} — {raw token value}" / "{raw token value}" alone as a best-effort fallback
//     when resolved text isn't available but showRawTokens is enabled and a raw "status"
//     token is present. This is an opt-in stopgap: raw tokens are unlocalized loc-file
//     keys (e.g. "#DeadlockRP_HideoutSnack") and may look cryptic, hence gated off by
//     default.
//   - "{game}" alone if only the game name is available (Phase 1 behavior, unchanged).
//   - Empty if nothing at all is available.
func buildPresenceTopic(presenceStatus steamapi.PersonaState, currentGame string, richPresenceStatusText string, showRawTokens bool, rawTokens map[string]string) string {
	if presenceStatus == steamapi.PersonaState_OFFLINE || presenceStatus == steamapi.PersonaState_INVISIBLE {
		return ""
	}

	flavorText := richPresenceStatusText
	if flavorText == "" && showRawTokens {
		if rawStatus, ok := rawTokens["status"]; ok && rawStatus != "" {
			flavorText = rawStatus
		}
	}

	switch {
	case currentGame != "" && flavorText != "":
		return fmt.Sprintf("%s — %s", currentGame, flavorText)
	case currentGame != "":
		return currentGame
	case flavorText != "":
		return flavorText
	default:
		return ""
	}
}
