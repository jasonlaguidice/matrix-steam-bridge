package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/simplevent"
)

// SteamChatResync represents a chat resync event for existing Steam portals.
// This implements RemoteChatResyncBackfill to enable automatic backfill.
type SteamChatResync struct {
	simplevent.ChatResync
}

// Verify interface implementation at compile time
var _ bridgev2.RemoteChatResyncBackfill = (*SteamChatResync)(nil)

// CheckNeedsBackfill determines if this portal needs backfill for catchup messages.
// For existing portals after bridge restart, we always want to check for missed messages.
func (evt *SteamChatResync) CheckNeedsBackfill(ctx context.Context, latestMessage *database.Message) (bool, error) {
	// Always request backfill for existing portals to catch up on missed messages
	// The backfill system will fetch messages since the last message we have
	return true, nil
}

// syncExistingPortals creates ChatResync events for all existing portals belonging to this user.
// This triggers backfill task creation for catchup messages after bridge restart.
//
// This function runs on every bridge startup to ensure messages missed during downtime are fetched.
// The bridge framework's CheckNeedsBackfill() determines if actual backfill is needed based on
// the latest message timestamp in each portal.
func (sc *SteamClient) syncExistingPortals(ctx context.Context) {
	sc.br.Log.Info().Msg("Syncing existing portals for backfill")

	// Get all portals for this user login from database
	userPortals, err := sc.br.DB.UserPortal.GetAllForLogin(ctx, sc.UserLogin.UserLogin)
	if err != nil {
		sc.br.Log.Err(err).Msg("Failed to get user portals from database")
		return
	}

	sc.br.Log.Info().
		Int("portal_count", len(userPortals)).
		Msg("Found existing portals to sync")

	// Create ChatResync events for each existing portal
	for _, userPortal := range userPortals {
		portal, err := sc.br.DB.Portal.GetByKey(ctx, userPortal.Portal)
		if err != nil {
			sc.br.Log.Err(err).
				Str("portal_id", string(userPortal.Portal.ID)).
				Msg("Failed to get portal from database")
			continue
		}

		if portal == nil {
			sc.br.Log.Warn().
				Str("portal_id", string(userPortal.Portal.ID)).
				Msg("Portal not found in database, skipping")
			continue
		}

		sc.br.Log.Debug().
			Str("portal_id", string(portal.PortalKey.ID)).
			Str("room_id", string(portal.MXID)).
			Msg("Creating ChatResync event for existing portal")

		// Create ChatResync event with backfill support
		resyncEvt := &SteamChatResync{
			ChatResync: simplevent.ChatResync{
				EventMeta: simplevent.EventMeta{
					Type:         bridgev2.RemoteEventChatResync,
					PortalKey:    portal.PortalKey,
					CreatePortal: false, // Don't create - portal already exists
				},
				ChatInfo: nil, // Will be fetched if needed by the framework
			},
		}

		// Queue the event for processing
		if !sc.UserLogin.QueueRemoteEvent(resyncEvt).Success {
			sc.br.Log.Warn().
				Str("portal_id", string(portal.PortalKey.ID)).
				Msg("Failed to queue ChatResync event")
			return
		}
	}

	sc.br.Log.Info().
		Int("synced_count", len(userPortals)).
		Msg("Completed portal sync")
}
