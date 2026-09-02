package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
)

// This file implements expiry tracking for live, incoming Steam game-invite messages.
//
// Steam's own official client shows a "Play Game" button on a game invite that
// disappears once the inviter stops playing that game, or after 6 hours - whichever
// comes first. A Matrix message can't grey itself out on its own, so we replicate that
// behavior by editing the already-sent Matrix message to remove the "Join Game" link
// once it's no longer expected to be valid:
//
//   - If live rich-presence data confirms the inviter is currently playing the invited
//     app at invite-arrival time, the invite is presence-tracked: it's edited away the
//     moment presence shows they've stopped playing that app, with a 6-hour safety-net
//     deadline in case presence updates stop arriving for any reason.
//   - Otherwise (no presence confirmation at invite-arrival time), a flat 30-minute
//     timeout is used instead.
//
// This only applies to the live incoming-message path (handleIncomingMessage in
// messaging.go) - backfilled/historical invites (convertSteamMessageToBackfill in
// backfill.go) are intentionally never registered here; a weeks-old backfilled invite
// shouldn't get a fresh countdown.
//
// None of this is persisted: losing pending-invite tracking across a bridge restart is
// an acceptable, graceful degradation (the invite's Matrix message simply keeps showing
// its "Join Game" link indefinitely in that rare case), not a bug to solve.

const (
	// inviteExpiryFlatTimeout is the deadline used when we have no live rich-presence
	// confirmation that the inviter is currently playing the invited app at
	// invite-arrival time.
	inviteExpiryFlatTimeout = 30 * time.Minute

	// inviteExpiryPresenceSafetyNet is the deadline used for presence-tracked invites,
	// as a safety net in case a "stopped playing" presence update for the inviter never
	// arrives. Matches the ~6 hour window Steam's own client uses before a "Play Game"
	// button goes away regardless of presence.
	inviteExpiryPresenceSafetyNet = 6 * time.Hour

	// inviteExpirySweepInterval is how often the periodic sweep checks pending invites
	// against their deadline, as a backstop alongside the immediate presence-driven
	// expiry check in handlePresenceTopicEvent.
	inviteExpirySweepInterval = 2 * time.Minute
)

// pendingInvite tracks a single live game-invite Matrix message that was sent with a
// clickable "Join Game" link, so it can later be edited to remove that link once it's no
// longer expected to be valid.
type pendingInvite struct {
	Portal        networkid.PortalKey
	TargetMessage networkid.MessageID

	// Sender is the exact EventSender used when the original invite message was sent
	// (including IsFromMe for echoed self-sent invites), reused unchanged for the
	// expiry edit so it resolves to the same Matrix sender/intent as the original.
	Sender bridgev2.EventSender

	InviterSteamID uint64
	AppID          uint64
	Deadline       time.Time

	// PresenceTracked is true when Deadline is the 6-hour presence safety net (cleared
	// early by a matching presence change) and false when it's the flat 30-minute
	// timeout.
	PresenceTracked bool
}

// friendPresenceInfo is the last-known live presence for a Steam friend. It's cached
// purely so that a game invite arriving later can look up "is this person currently
// playing that game right now" without having to wait for a fresh presence event of its
// own to arrive after the invite.
type friendPresenceInfo struct {
	AppID           uint32
	HasRichPresence bool
	UpdatedAt       time.Time
}

// updateFriendPresenceCache records the given friend's last-known game/rich-presence
// state. This is called unconditionally on every live presence event, regardless of
// whether there's a pending invite for that friend right now, so the cache stays useful
// for invites that arrive later.
func (sc *SteamClient) updateFriendPresenceCache(steamID uint64, appID uint32, hasRichPresence bool) {
	sc.friendPresenceMu.Lock()
	defer sc.friendPresenceMu.Unlock()
	if sc.friendPresence == nil {
		sc.friendPresence = make(map[uint64]*friendPresenceInfo)
	}
	sc.friendPresence[steamID] = &friendPresenceInfo{
		AppID:           appID,
		HasRichPresence: hasRichPresence,
		UpdatedAt:       time.Now(),
	}
}

// registerPendingInvite records a newly-sent, live game-invite Matrix message for
// expiry tracking. Only call this for invites that actually produced a clickable join
// link (InviteLobbyId != "" || InviteConnect != "") - a plain-text-only invite has
// nothing to expire. Never call this for backfilled invites.
//
// portalKey and msgID must be the exact same values used to send the original invite
// message, since they're what later targets the expiry edit at that message.
func (sc *SteamClient) registerPendingInvite(portalKey networkid.PortalKey, msgID networkid.MessageID, sender bridgev2.EventSender, inviterSteamID, appID uint64) {
	sc.friendPresenceMu.Lock()
	presence, ok := sc.friendPresence[inviterSteamID]
	sc.friendPresenceMu.Unlock()

	presenceTracked := ok && presence.HasRichPresence && uint64(presence.AppID) == appID

	var deadline time.Time
	if presenceTracked {
		deadline = time.Now().Add(inviteExpiryPresenceSafetyNet)
	} else {
		deadline = time.Now().Add(inviteExpiryFlatTimeout)
	}

	inv := &pendingInvite{
		Portal:          portalKey,
		TargetMessage:   msgID,
		Sender:          sender,
		InviterSteamID:  inviterSteamID,
		AppID:           appID,
		Deadline:        deadline,
		PresenceTracked: presenceTracked,
	}

	sc.pendingInvitesMu.Lock()
	if sc.pendingInvites == nil {
		sc.pendingInvites = make(map[networkid.MessageID]*pendingInvite)
	}
	sc.pendingInvites[msgID] = inv
	sc.pendingInvitesMu.Unlock()

	sc.br.Log.Debug().
		Uint64("inviter_steam_id", inviterSteamID).
		Uint64("app_id", appID).
		Bool("presence_tracked", presenceTracked).
		Time("deadline", deadline).
		Msg("Registered pending game invite for expiry tracking")
}

// checkPendingInviteExpiry is called from handlePresenceTopicEvent on every live
// presence update for a friend, after updateFriendPresenceCache. If that friend has one
// or more presence-tracked pending invites and the fresh presence event shows they're no
// longer playing the invited app - either a different (or no) app, or not actually
// playing at all - those invites are expired immediately rather than waiting for the
// periodic sweep or the 6-hour safety net.
func (sc *SteamClient) checkPendingInviteExpiry(steamID uint64, currentAppID uint32, stillPlaying bool) {
	var toExpire []*pendingInvite

	sc.pendingInvitesMu.Lock()
	for id, inv := range sc.pendingInvites {
		if !inv.PresenceTracked || inv.InviterSteamID != steamID {
			continue
		}
		if !stillPlaying || uint64(currentAppID) != inv.AppID {
			toExpire = append(toExpire, inv)
			delete(sc.pendingInvites, id)
		}
	}
	sc.pendingInvitesMu.Unlock()

	for _, inv := range toExpire {
		sc.expireInvite(inv, "presence")
	}
}

// startInviteExpirySweep runs a periodic backstop check for pending game invites whose
// deadline has passed - the flat 30-minute timeout for non-presence-tracked invites, or
// the 6-hour safety net for presence-tracked ones whose "stopped playing" presence
// update never arrived. It runs for the lifetime of the client connection, stopping when
// ctx is cancelled, following the same lifecycle convention as processPresenceStream and
// processMessageStream.
//
// Called from every Connect()/login path, including automatic reconnects - those reuse
// context.Background() (never cancelled), so without inviteSweepStarted each reconnect
// would spawn and permanently leak another copy of this goroutine. sync.Once ensures only
// the first call's ctx actually starts a (single, long-lived) sweep loop.
func (sc *SteamClient) startInviteExpirySweep(ctx context.Context) {
	sc.inviteSweepStarted.Do(func() {
		go func() {
			ticker := time.NewTicker(inviteExpirySweepInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					sc.sweepExpiredInvites()
				}
			}
		}()
	})
}

// sweepExpiredInvites expires (and removes from tracking) every pending invite whose
// deadline has already passed.
func (sc *SteamClient) sweepExpiredInvites() {
	now := time.Now()
	var toExpire []*pendingInvite

	sc.pendingInvitesMu.Lock()
	for id, inv := range sc.pendingInvites {
		if now.After(inv.Deadline) {
			toExpire = append(toExpire, inv)
			delete(sc.pendingInvites, id)
		}
	}
	sc.pendingInvitesMu.Unlock()

	for _, inv := range toExpire {
		sc.expireInvite(inv, "timeout")
	}
}

// expireInvite sends a Matrix edit that replaces a game invite's clickable content with
// plain, non-clickable text - the bridge's substitute for Steam's own client greying out
// its "Play Game" button once an invite is no longer valid. reason is used only for
// logging ("presence" or "timeout").
func (sc *SteamClient) expireInvite(inv *pendingInvite, reason string) {
	sc.br.Log.Debug().
		Uint64("inviter_steam_id", inv.InviterSteamID).
		Uint64("app_id", inv.AppID).
		Str("reason", reason).
		Msg("Expiring game invite")

	editEvt := &simplevent.Message[any]{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventEdit,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Uint64("inviter_steam_id", inv.InviterSteamID).
					Uint64("app_id", inv.AppID).
					Str("expiry_reason", reason)
			},
			PortalKey: inv.Portal,
			Sender:    inv.Sender,
		},
		TargetMessage: inv.TargetMessage,
		ConvertEditFunc: func(_ context.Context, _ *bridgev2.Portal, _ bridgev2.MatrixAPI, existing []*database.Message, _ any) (*bridgev2.ConvertedEdit, error) {
			if len(existing) == 0 {
				return nil, fmt.Errorf("no existing message parts found for expiring game invite")
			}
			return &bridgev2.ConvertedEdit{
				ModifiedParts: []*bridgev2.ConvertedEditPart{{
					Part: existing[0],
					Type: event.EventMessage,
					Content: &event.MessageEventContent{
						MsgType: event.MsgText,
						Body:    "🎮 Game invite expired",
					},
				}},
			}, nil
		},
	}

	sc.br.QueueRemoteEvent(sc.UserLogin, editEvt)
}
