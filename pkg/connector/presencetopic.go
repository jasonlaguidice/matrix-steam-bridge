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
// DM room topics (m.room.topic) in sync with their current game

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
// corresponding friend's DM room topic. On a stream error, it logs and returns
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
		Interface("rich_presence_tokens", ev.RichPresenceTokens).
		Msg("Received presence event")

	// Keep the last-known-presence cache fresh regardless of whether this friend has a
	// pending game invite right now - see inviteexpiry.go. Then, if they do have a
	// presence-tracked pending invite, check whether this fresh presence event shows
	// they've stopped playing the invited app, so it can be expired immediately instead
	// of waiting for the periodic sweep or the 6-hour safety net.
	sc.updateFriendPresenceCache(ev.SteamId, ev.GameAppId, ev.HasRichPresence)
	stillPlaying := ev.GameAppId != 0 && ev.Status != steamapi.PersonaState_OFFLINE && ev.Status != steamapi.PersonaState_INVISIBLE
	sc.checkPendingInviteExpiry(ev.SteamId, ev.GameAppId, stillPlaying)

	if err := sc.updateFriendDMTopic(ctx, ev.SteamId, ev.Status, ev.CurrentGame, ev.RichPresenceStatusText, ev.RichPresenceTokens); err != nil {
		sc.br.Log.Warn().Err(err).
			Uint64("steam_id", ev.SteamId).
			Msg("Failed to update DM room topic from presence event")
	}
}

// updateFriendDMTopic looks up the existing DM portal for the given friend
// and, if it exists, updates its topic to reflect the friend's current
// presence/game (and, when available, resolved or raw rich-presence flavor text). This
// is shared by the live presence stream handler (handlePresenceTopicEvent) and the
// startup friend sync (syncFriendsOnStartup) so both paths build and apply the topic
// identically.
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

	portal.UpdateInfo(ctx, &bridgev2.ChatInfo{Topic: ptr.Ptr(topic)}, sc.UserLogin, nil, time.Time{})

	return nil
}

// buildPresenceTopic builds the DM room topic string from a friend's presence status,
// current game, and (optionally) rich-presence flavor text:
//   - Empty when the friend is offline/invisible.
//   - "{game} | {rich presence text}" when both a game name and resolved rich-presence
//     status text are available.
//   - "{rich presence text}" alone when resolved text is available but no game name is
//   - "{game} | {raw token value}" / "{raw token value}" when only the token is available
//   - "{game}" alone if only the game name is available
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
		return fmt.Sprintf("%s | %s", currentGame, flavorText)
	case currentGame != "":
		return currentGame
	case flavorText != "":
		return flavorText
	default:
		return ""
	}
}
