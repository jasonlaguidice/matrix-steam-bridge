package connector

import (
	"fmt"

	"maunium.net/go/mautrix/bridgev2/networkid"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

// steamMessageID is the single source of truth for the networkid.MessageID of a
// Steam chat message. Live events, backfilled history and messages sent from
// Matrix must all be keyed through it so the bridgev2 database dedup sees the
// same ID for the same Steam message no matter which path stored it first.
//
//	DM chat message  {sender}:{ts}_{ordinal}
//	DM game invite   {sender}:{ts}_{ordinal}:invite
//	group message    {ts}_{ordinal}
//
// sender is the Steam ID of the message author (our own ID for outgoing messages).
func steamMessageID(idType PortalIDType, sender uint64, timestamp int64, ordinal uint32, msgType steamapi.MessageType) networkid.MessageID {
	if idType == PortalIDTypeChannel {
		return networkid.MessageID(fmt.Sprintf("%d_%d", timestamp, ordinal))
	}
	if msgType == steamapi.MessageType_INVITE_GAME {
		return networkid.MessageID(fmt.Sprintf("%d:%d_%d:invite", sender, timestamp, ordinal))
	}
	return networkid.MessageID(fmt.Sprintf("%d:%d_%d", sender, timestamp, ordinal))
}

// legacySteamMessageIDs returns the IDs earlier bridge versions stored for the
// message that steamMessageID now keys canonically. They are only used to
// recognise an already-bridged pagination anchor row; nothing writes them.
//
//	DM backfilled message   {ts}_{ordinal}
//	DM outgoing message     {target}:{ts}_{ordinal}:out
//	group outgoing message  {chatGroupID}:{chatID}:{ts}_{ordinal}
func legacySteamMessageIDs(portalKey networkid.PortalKey, idType PortalIDType, sender, self uint64, timestamp int64, ordinal uint32, msgType steamapi.MessageType) []networkid.MessageID {
	if msgType == steamapi.MessageType_INVITE_GAME && idType == PortalIDTypeDM {
		return nil
	}
	ids := []networkid.MessageID{}
	_, first, second, err := parsePortalID(portalKey.ID)
	if err != nil {
		return ids
	}
	switch idType {
	case PortalIDTypeDM:
		ids = append(ids, networkid.MessageID(fmt.Sprintf("%d_%d", timestamp, ordinal)))
		if sender == self {
			ids = append(ids, networkid.MessageID(fmt.Sprintf("%d:%d_%d:out", first, timestamp, ordinal)))
		}
	case PortalIDTypeChannel:
		if sender == self {
			ids = append(ids, networkid.MessageID(fmt.Sprintf("%d:%d:%d_%d", first, second, timestamp, ordinal)))
		}
	}
	return ids
}
