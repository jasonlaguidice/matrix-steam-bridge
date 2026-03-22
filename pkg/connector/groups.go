package connector

import (
	"context"
	"fmt"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

// syncGroups fetches all Steam chat room groups for the current user and syncs them
// as Space portals (for the groups) and channel portals (for the channels within each group).
func (sc *SteamClient) syncGroups(ctx context.Context) error {
	if sc.groupClient == nil {
		return fmt.Errorf("group client not initialized")
	}

	sc.br.Log.Info().Msg("Fetching Steam chat room groups")

	resp, err := sc.groupClient.GetMyChatRoomGroups(ctx, &steamapi.GetGroupsRequest{})
	if err != nil {
		return fmt.Errorf("failed to get chat room groups: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("GetMyChatRoomGroups returned failure: %s", resp.ErrorMessage)
	}

	sc.br.Log.Info().Int("group_count", len(resp.Groups)).Msg("Syncing Steam chat room groups")

	// Sync all channels; bridgev2 auto-creates spaces from each channel's ParentID
	for _, group := range resp.Groups {
		if err := sc.syncGroupChannels(ctx, group); err != nil {
			sc.br.Log.Warn().
				Err(err).
				Uint64("chat_group_id", group.ChatGroupId).
				Str("group_name", group.Name).
				Msg("Failed to sync group channels, skipping")
		}
	}

	return nil
}

// syncGroupChannels loops all channels within the group and queues ChatResync events for each.
func (sc *SteamClient) syncGroupChannels(ctx context.Context, group *steamapi.ChatGroup) error {
	for _, channel := range group.Channels {
		if err := sc.syncChannel(ctx, group, channel); err != nil {
			sc.br.Log.Warn().
				Err(err).
				Uint64("chat_group_id", group.ChatGroupId).
				Uint64("chat_id", channel.ChatId).
				Msg("Failed to sync Steam channel, continuing")
		}
	}

	return nil
}

// syncChannel queues a ChatResync event for a single channel portal within a group.
func (sc *SteamClient) syncChannel(_ context.Context, group *steamapi.ChatGroup, channel *steamapi.ChatChannel) error {
	channelPortalID := makeChannelPortalID(group.ChatGroupId, channel.ChatId)
	channelPortalKey := networkid.PortalKey{
		ID:       channelPortalID,
		Receiver: sc.UserLogin.ID,
	}

	spacePortalID := makeSpacePortalID(group.ChatGroupId)
	chatInfo := buildChannelChatInfo(group, channel, spacePortalID)
	channelName := channel.Name
	if channelName == "" {
		channelName = fmt.Sprintf("channel-%d", channel.ChatId)
	}

	resyncEvt := &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			PortalKey:    channelPortalKey,
			CreatePortal: true,
		},
		GetChatInfoFunc: func(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
			if meta, ok := portal.Metadata.(*PortalMetadata); ok && meta != nil {
				meta.Name = channelName
			}
			return chatInfo, nil
		},
	}

	if !sc.UserLogin.QueueRemoteEvent(resyncEvt).Success {
		sc.br.Log.Warn().
			Str("portal_id", string(channelPortalID)).
			Msg("Failed to queue ChatResync event for channel")
	}

	return nil
}

// buildChannelChatInfo constructs a ChatInfo for a Steam chat channel portal.
func buildChannelChatInfo(group *steamapi.ChatGroup, channel *steamapi.ChatChannel, parentSpaceID networkid.PortalID) *bridgev2.ChatInfo {
	name := channel.Name
	if name == "" {
		name = fmt.Sprintf("channel-%d", channel.ChatId)
	}

	_ = group // group may be used for additional context in the future

	return &bridgev2.ChatInfo{
		Type:        ptr.Ptr(database.RoomTypeDefault),
		CanBackfill: true,
		Name:        ptr.Ptr(name),
		ParentID:    &parentSpaceID,
	}
}
