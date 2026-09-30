// Destination abstraction for Matrix → Steam media sends: a 1:1 chat or a
// group channel. It carries everything the shared media handler needs so the
// image/video path is one code path for both chat kinds (the upload commit
// fields differ only by UploadTarget, the caption and public-URL messages by
// the SendMessage addressing).
package connector

import (
	"go.shadowdrake.org/steam/pkg/steamapi"
)

type mediaSendTarget struct {
	upload      UploadTarget
	dmSteamID   uint64 // 1:1 partner; 0 for group channels
	chatGroupID uint64 // group channel; 0 for 1:1 chats
	chatID      uint64
}

func dmMediaTarget(steamID uint64) mediaSendTarget {
	return mediaSendTarget{upload: DMUploadTarget{FriendSteamID: steamID}, dmSteamID: steamID}
}

func groupMediaTarget(chatGroupID, chatID uint64) mediaSendTarget {
	return mediaSendTarget{
		upload:      GroupUploadTarget{ChatGroupID: chatGroupID, ChatID: chatID},
		chatGroupID: chatGroupID,
		chatID:      chatID,
	}
}

// textRequest builds a chat message addressed to the target.
func (t mediaSendTarget) textRequest(text string, callerSteamID uint64) *steamapi.SendMessageRequest {
	return &steamapi.SendMessageRequest{
		TargetSteamId: t.dmSteamID,
		ChatGroupId:   t.chatGroupID,
		ChatId:        t.chatID,
		Message:       text,
		MessageType:   steamapi.MessageType_CHAT_MESSAGE,
		CallerSteamId: callerSteamID,
	}
}

// portalIDType is the kind of Steam chat the target addresses, for keying the
// bridge's own sent messages through steamMessageID.
func (t mediaSendTarget) portalIDType() PortalIDType {
	if t.chatGroupID != 0 {
		return PortalIDTypeChannel
	}
	return PortalIDTypeDM
}
