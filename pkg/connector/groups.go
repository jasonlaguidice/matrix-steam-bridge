package connector

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

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

	// Pass 1: create all spaces
	for _, group := range resp.Groups {
		if err := sc.syncGroupSpace(ctx, group); err != nil {
			sc.br.Log.Warn().
				Err(err).
				Uint64("chat_group_id", group.ChatGroupId).
				Str("group_name", group.Name).
				Msg("Failed to sync group space, skipping")
		}
	}

	// Wait for bridgev2 to process space ChatResync events and create Matrix rooms
	// before queuing channels, to avoid duplicate space creation race condition
	sc.br.Log.Debug().Msg("Waiting for space portals to be created before syncing channels")
	time.Sleep(3 * time.Second)

	// Pass 2: create all channels
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

// syncGroupSpace queues a ChatResync event for the space portal representing this group only.
func (sc *SteamClient) syncGroupSpace(_ context.Context, group *steamapi.ChatGroup) error {
	spacePortalID := makeSpacePortalID(group.ChatGroupId)
	spacePortalKey := networkid.PortalKey{
		ID:       spacePortalID,
		Receiver: sc.UserLogin.ID,
	}

	chatInfo := sc.buildSpaceChatInfo(group)
	groupName := group.Name

	resyncEvt := &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			PortalKey:    spacePortalKey,
			CreatePortal: true,
		},
		GetChatInfoFunc: func(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
			if meta, ok := portal.Metadata.(*PortalMetadata); ok && meta != nil {
				meta.Name = groupName
			}
			return chatInfo, nil
		},
	}

	if !sc.UserLogin.QueueRemoteEvent(resyncEvt).Success {
		sc.br.Log.Warn().
			Str("portal_id", string(spacePortalID)).
			Msg("Failed to queue ChatResync event for group space")
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

// buildSpaceChatInfo constructs a ChatInfo for a Steam chat group space portal.
func (sc *SteamClient) buildSpaceChatInfo(group *steamapi.ChatGroup) *bridgev2.ChatInfo {
	info := &bridgev2.ChatInfo{
		Type:        ptr.Ptr(database.RoomTypeSpace),
		CanBackfill: false,
		Name:        ptr.Ptr(group.Name),
	}

	if group.Tagline != "" {
		info.Topic = ptr.Ptr(group.Tagline)
	}

	if group.AvatarUrl != "" || len(group.AvatarSha) > 0 {
		var avatarID networkid.AvatarID
		if len(group.AvatarSha) > 0 {
			avatarID = networkid.AvatarID(hex.EncodeToString(group.AvatarSha))
		}
		var capturedURL string
		if group.AvatarUrl != "" {
			capturedURL = group.AvatarUrl
			if avatarID == "" {
				avatarID = networkid.AvatarID(group.AvatarUrl)
			}
		} else {
			capturedURL = fmt.Sprintf("https://avatars.steamstatic.com/%s_full.jpg", string(avatarID))
		}
		info.Avatar = &bridgev2.Avatar{
			ID: avatarID,
			Get: func(ctx context.Context) ([]byte, error) {
				return sc.downloadImageFromURL(ctx, capturedURL)
			},
		}
	}

	return info
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
