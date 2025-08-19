package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

// PaginationCursor represents a cursor for message pagination
type PaginationCursor struct {
	Time    uint32 `json:"time"`
	Ordinal uint32 `json:"ordinal"`
}

// FetchMessages implements bridgev2.BackfillingNetworkAPI
func (sc *SteamClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	sc.br.Log.Debug().
		Str("portal_id", string(params.Portal.PortalKey.ID)).
		Str("receiver", string(params.Portal.PortalKey.Receiver)).
		Bool("forward", params.Forward).
		Int("count", params.Count).
		Msg("Fetching message history")

	if sc.msgClient == nil {
		return nil, fmt.Errorf("messaging client not initialized")
	}

	// Extract chat IDs from portal key
	chatGroupID, chatID, err := sc.extractChatIDs(params.Portal.PortalKey)
	if err != nil {
		return nil, fmt.Errorf("failed to extract chat IDs: %w", err)
	}

	// Prepare pagination cursor
	var lastTime, lastOrdinal uint32
	if params.Cursor != "" {
		cursor := parsePaginationCursor(params.Cursor)
		lastTime = cursor.Time
		lastOrdinal = cursor.Ordinal
	} else if params.AnchorMessage != nil {
		// Use anchor message timestamp if no cursor provided
		lastTime = uint32(params.AnchorMessage.Timestamp.Unix())
		// Extract ordinal from message metadata if available
		if msgMeta, ok := params.AnchorMessage.Metadata.(*MessageMetadata); ok && msgMeta != nil {
			lastOrdinal = msgMeta.Ordinal
		}
	}

	// Call gRPC service
	historyResp, err := sc.msgClient.GetChatMessageHistory(ctx, &steamapi.ChatMessageHistoryRequest{
		ChatGroupId: chatGroupID,
		ChatId:      chatID,
		LastTime:    lastTime,
		LastOrdinal: lastOrdinal,
		MaxCount:    uint32(params.Count),
		Forward:     params.Forward,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get chat history: %w", err)
	}

	if !historyResp.Success {
		return nil, fmt.Errorf("steam API error: %s", historyResp.ErrorMessage)
	}

	// Convert Steam messages to BackfillMessages
	messages := make([]*bridgev2.BackfillMessage, 0, len(historyResp.Messages))
	for _, steamMsg := range historyResp.Messages {
		backfillMsg, err := sc.convertSteamMessageToBackfill(ctx, steamMsg, params.Portal)
		if err != nil {
			sc.br.Log.Warn().Err(err).
				Uint64("sender", steamMsg.SenderSteamId).
				Uint32("timestamp", steamMsg.Timestamp).
				Msg("Failed to convert Steam message, skipping")
			continue
		}
		messages = append(messages, backfillMsg)
	}

	// Generate next cursor
	var nextCursor networkid.PaginationCursor
	if historyResp.HasMore && historyResp.NextTime > 0 {
		nextCursor = createPaginationCursor(historyResp.NextTime, historyResp.NextOrdinal)
	}

	response := &bridgev2.FetchMessagesResponse{
		Messages: messages,
		Cursor:   nextCursor,
		HasMore:  historyResp.HasMore,
		Forward:  params.Forward,
	}

	sc.br.Log.Debug().
		Int("message_count", len(messages)).
		Bool("has_more", historyResp.HasMore).
		Msg("Message history fetched successfully")

	return response, nil
}

// extractChatIDs extracts Steam chat group ID and chat ID from portal key
func (sc *SteamClient) extractChatIDs(portalKey networkid.PortalKey) (chatGroupID, chatID uint64, err error) {
	// For Steam, the portal ID typically contains the Steam ID
	// In 1-to-1 chats, the chat_group_id is 0 and chat_id is the Steam ID
	// In group chats, both would be set appropriately

	// Parse the portal ID as a Steam ID
	steamIDStr := string(portalKey.ID)
	steamID, parseErr := strconv.ParseUint(steamIDStr, 10, 64)
	if parseErr != nil {
		return 0, 0, fmt.Errorf("failed to parse portal ID as Steam ID: %w", parseErr)
	}

	// For now, assume all chats are 1-to-1 (chat_group_id = 0, chat_id = steam_id)
	// TODO: Add support for group chats with proper chat_group_id extraction
	return 0, steamID, nil
}

// parsePaginationCursor parses a pagination cursor string
func parsePaginationCursor(cursor networkid.PaginationCursor) PaginationCursor {
	// Parse JSON cursor format
	var parsed PaginationCursor
	if err := json.Unmarshal([]byte(cursor), &parsed); err != nil {
		// If parsing fails, return empty cursor
		return PaginationCursor{}
	}
	return parsed
}

// createPaginationCursor creates a pagination cursor string
func createPaginationCursor(time, ordinal uint32) networkid.PaginationCursor {
	cursor := PaginationCursor{
		Time:    time,
		Ordinal: ordinal,
	}
	data, _ := json.Marshal(cursor) // Ignore error, return empty string if marshal fails
	return networkid.PaginationCursor(data)
}

// convertSteamMessageToBackfill converts a Steam API message to bridgev2 BackfillMessage
func (sc *SteamClient) convertSteamMessageToBackfill(ctx context.Context, steamMsg *steamapi.ChatHistoryMessage, portal *bridgev2.Portal) (*bridgev2.BackfillMessage, error) {
	// Create sender information
	senderID := makeUserID(steamMsg.SenderSteamId)
	sender := bridgev2.EventSender{
		IsFromMe: steamMsg.SenderSteamId == sc.getUserID(),
		Sender:   senderID,
	}

	// Convert timestamp
	timestamp := time.Unix(int64(steamMsg.Timestamp), 0)

	// Convert message content
	var convertedMsg *bridgev2.ConvertedMessage
	var err error

	if steamMsg.ImageUrl != nil && *steamMsg.ImageUrl != "" {
		// Handle image message
		convertedMsg, err = sc.convertImageMessageFromHistory(ctx, steamMsg.MessageContent, *steamMsg.ImageUrl, portal)
	} else {
		// Handle text message
		convertedMsg, err = sc.convertTextMessageFromHistory(ctx, steamMsg.MessageContent, portal)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to convert message content: %w", err)
	}

	// Create message metadata to store ordinal for pagination
	messageMetadata := &MessageMetadata{
		SteamID:   steamMsg.SenderSteamId,
		Ordinal:   steamMsg.Ordinal,
		Timestamp: timestamp,
	}

	backfillMsg := &bridgev2.BackfillMessage{
		ConvertedMessage: convertedMsg,
		Sender:           sender,
		ID:               networkid.MessageID(fmt.Sprintf("%d_%d", steamMsg.Timestamp, steamMsg.Ordinal)),
		Timestamp:        timestamp,
		StreamOrder:      int64(steamMsg.Ordinal),        // Use ordinal for ordering
		Reactions:        []*bridgev2.BackfillReaction{}, // TODO: Add reaction support
	}

	// Store metadata in the first part if available
	if len(backfillMsg.Parts) > 0 {
		backfillMsg.Parts[0].DBMetadata = messageMetadata
	}

	return backfillMsg, nil
}

// getUserID gets the current user's Steam ID
func (sc *SteamClient) getUserID() uint64 {
	if metadata := sc.getUserMetadata(); metadata != nil {
		return metadata.SteamID
	}
	return 0
}

// convertTextMessageFromHistory converts a text message from history to bridgev2 format
func (sc *SteamClient) convertTextMessageFromHistory(ctx context.Context, content string, portal *bridgev2.Portal) (*bridgev2.ConvertedMessage, error) {
	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{
			{
				Type: event.EventMessage,
				Content: &event.MessageEventContent{
					MsgType: event.MsgText,
					Body:    content,
				},
				ID: networkid.PartID("text"),
			},
		},
	}, nil
}

// convertImageMessageFromHistory converts an image message from history to bridgev2 format
func (sc *SteamClient) convertImageMessageFromHistory(ctx context.Context, caption, imageURL string, portal *bridgev2.Portal) (*bridgev2.ConvertedMessage, error) {
	parts := []*bridgev2.ConvertedMessagePart{}

	// Add image part
	imageContent := &event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    "Image",
		URL:     id.ContentURIString(imageURL), // This will need proper handling
	}

	parts = append(parts, &bridgev2.ConvertedMessagePart{
		Type:    event.EventMessage,
		Content: imageContent,
		ID:      networkid.PartID("image"),
	})

	// Add caption if present
	if caption != "" {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    caption,
			},
			ID: networkid.PartID("caption"),
		})
	}

	return &bridgev2.ConvertedMessage{
		Parts: parts,
	}, nil
}
