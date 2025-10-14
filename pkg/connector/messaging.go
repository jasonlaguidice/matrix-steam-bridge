package connector

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

// startMessageSubscription starts a gRPC stream to receive real-time messages from Steam
// with robust reconnection logic and exponential backoff
func (sc *SteamClient) startMessageSubscription(ctx context.Context) {
	const (
		initialBackoff = 1 * time.Second
		maxBackoff     = 30 * time.Second
		backoffFactor  = 2.0
	)

	backoffDelay := initialBackoff

	for {
		select {
		case <-ctx.Done():
			sc.br.Log.Info().Msg("Message subscription context cancelled")
			return
		default:
			// Attempt to establish stream connection without immediate state reporting
			if err := sc.subscribeWithStreamRetry(ctx); err != nil {
				// Categorize error type
				if sc.isPermanentError(err) {
					sc.br.Log.Error().Err(err).Msg("Permanent error in message subscription, stopping")
					sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials,
						"Steam connection permanently failed",
						withReason(err.Error()),
						withUserAction(status.UserActionRelogin)))
					return
				}

				// Temporary error - update state and retry with backoff
				sc.br.Log.Warn().Err(err).
					Dur("retry_in", backoffDelay).
					Msg("Temporary error in message subscription, retrying")
				sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect,
					fmt.Sprintf("Steam connection lost, retrying in %v", backoffDelay),
					withReason(err.Error()),
					withInfo(map[string]interface{}{
						"retry_delay_seconds": backoffDelay.Seconds(),
						"error_type":          "temporary",
					})))

				// Wait for backoff delay or context cancellation
				select {
				case <-ctx.Done():
					sc.br.Log.Info().Msg("Message subscription context cancelled during backoff")
					return
				case <-time.After(backoffDelay):
					// Cancel any pending disconnect debounce and report reconnection attempt
					sc.cancelDisconnectDebounce()
					sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting, "Reconnecting to Steam"))

					// Increase backoff delay for next iteration, up to maximum
					backoffDelay = time.Duration(float64(backoffDelay) * backoffFactor)
					if backoffDelay > maxBackoff {
						backoffDelay = maxBackoff
					}
				}
			} else {
				// Successful connection - reset backoff delay and report success
				backoffDelay = initialBackoff
				sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnected, "Reconnected to Steam"))
			}
		}
	}
}

// subscribeWithStream handles a single stream connection lifecycle
func (sc *SteamClient) subscribeWithStream(ctx context.Context) error {
	// This method is now only used for initial connection setup
	// Don't send state updates here as they may overwrite the main connection state
	return sc.subscribeWithStreamRetry(ctx)
}

// subscribeWithStreamRetry handles stream connection attempts during reconnection
func (sc *SteamClient) subscribeWithStreamRetry(ctx context.Context) error {
	stream, err := sc.msgClient.SubscribeToMessages(ctx, &steamapi.MessageSubscriptionRequest{})
	if err != nil {
		return fmt.Errorf("failed to start message subscription: %w", err)
	}

	sc.br.Log.Info().Msg("Started Steam message subscription")
	// Cancel any pending disconnect debounce
	sc.cancelDisconnectDebounce()

	for {
		select {
		case <-ctx.Done():
			sc.br.Log.Info().Msg("Stream context cancelled")
			return ctx.Err()
		default:
			msgEvent, err := stream.Recv()
			if err != nil {
				return fmt.Errorf("error receiving message from stream: %w", err)
			}

			// Process incoming message
			sc.handleIncomingMessage(ctx, msgEvent)

			// Track message count for state reporting
			sc.messageCountMux.Lock()
			sc.messageCount++
			msgCount := sc.messageCount
			sc.messageCountMux.Unlock()

			// Only log periodically, don't spam bridge state
			if msgCount%100 == 0 {
				sc.br.Log.Debug().Uint64("message_count", msgCount).Msg("Message stream processing active")
			}
		}
	}
}

// isPermanentError determines if a gRPC error is permanent and should not be retried
func (sc *SteamClient) isPermanentError(err error) bool {
	if err == nil {
		return false
	}

	// Context cancellation is NOT permanent - it's expected behavior in bridgev2
	// Let bridgev2 handle connection lifecycle and retries
	if err == context.Canceled || err == context.DeadlineExceeded {
		return false
	}

	// Check gRPC status codes
	if grpcStatus, ok := grpcstatus.FromError(err); ok {
		switch grpcStatus.Code() {
		case codes.Canceled:
			return false // Context cancellation should be retryable
		case codes.InvalidArgument:
			return true
		case codes.NotFound:
			return true
		case codes.PermissionDenied:
			return true
		case codes.Unauthenticated:
			return true
		case codes.Unimplemented:
			return true
		default:
			// All other errors are considered temporary
			return false
		}
	}

	// Unknown errors are considered temporary to be safe
	return false
}

// HandleMatrixMessage implements bridgev2.NetworkAPI.
func (sc *SteamClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (message *bridgev2.MatrixMessageResponse, err error) {
	sc.br.Log.Info().Str("event_type", msg.Event.Type.String()).Msg("HandleMatrixMessage() - Processing Matrix message")

	// Track activity for presence management (any Matrix message counts as activity)
	if sc.presenceManager != nil {
		sc.presenceManager.HandleActivity(ctx)
	}

	sc.br.Log.Debug().
		Str("event_type", msg.Event.Type.String()).
		Str("event_id", string(msg.Event.ID)).
		Str("sender", string(msg.Event.Sender)).
		Interface("raw_content", msg.Event.Content.Raw).
		Msg("HandleMatrixMessage - Raw event details")

	// Parse target Steam ID from portal ID
	targetSteamID, err := parseSteamIDFromPortalID(msg.Portal.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to parse target Steam ID from portal %s: %w", msg.Portal.ID, err)
	}

	switch msg.Event.Type {
	case event.EventMessage:
		content := msg.Event.Content.AsMessage()
		sc.br.Log.Debug().
			Interface("parsed_content", content).
			Bool("content_is_nil", content == nil).
			Msg("HandleMatrixMessage - Parsed message content")
		
		if content != nil && content.MsgType == event.MsgImage {
			sc.br.Log.Debug().
				Str("msgtype", string(content.MsgType)).
				Str("url", string(content.URL)).
				Msg("HandleMatrixMessage - Detected image message, routing to handleImageMessage")
			return sc.handleImageMessage(ctx, msg, targetSteamID)
		}
		return sc.handleTextMessage(ctx, msg, targetSteamID)
	case event.EventSticker:
		return sc.handleStickerMessage(ctx, msg, targetSteamID)
	default:
		sc.br.Log.Warn().Str("event_type", msg.Event.Type.String()).Msg("Unsupported message type")
		return nil, fmt.Errorf("unsupported message type: %v", msg.Event.Type)
	}
}

// handleTextMessage processes text messages and sends them to Steam
func (sc *SteamClient) handleTextMessage(ctx context.Context, msg *bridgev2.MatrixMessage, targetSteamID uint64) (*bridgev2.MatrixMessageResponse, error) {
	content := msg.Event.Content.AsMessage()
	if content == nil {
		return nil, fmt.Errorf("failed to parse message content")
	}

	// Extract text content
	var messageText string
	switch content.MsgType {
	case event.MsgText:
		messageText = content.Body
	case event.MsgEmote:
		// Steam emotes are handled differently - format as action
		messageText = content.Body
	case event.MsgNotice:
		messageText = content.Body
	default:
		return nil, fmt.Errorf("unsupported message type: %s", content.MsgType)
	}

	if messageText == "" {
		return nil, fmt.Errorf("empty message text")
	}

	// Determine Steam message type
	var steamMsgType steamapi.MessageType
	switch content.MsgType {
	case event.MsgEmote:
		steamMsgType = steamapi.MessageType_EMOTE
	default:
		steamMsgType = steamapi.MessageType_CHAT_MESSAGE
	}

	// Send message via gRPC
	resp, err := sc.msgClient.SendMessage(ctx, &steamapi.SendMessageRequest{
		TargetSteamId: targetSteamID,
		Message:       messageText,
		MessageType:   steamMsgType,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to send message to Steam: %w", err)
	}

	if !resp.Success {
		return nil, fmt.Errorf("steam message send failed: %s", resp.ErrorMessage)
	}

	// Create message metadata
	msgMeta := &MessageMetadata{
		SteamMessageType: steamMsgType.String(),
		IsEcho:           false, // This is our outgoing message
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        networkid.MessageID(fmt.Sprintf("%d:%d", targetSteamID, resp.Timestamp)),
			MXID:      msg.Event.ID,
			Timestamp: time.Unix(resp.Timestamp, 0),
			Metadata:  msgMeta,
		},
	}, nil
}

// handleStickerMessage processes sticker messages (Steam doesn't natively support stickers, so convert to text)
func (sc *SteamClient) handleStickerMessage(ctx context.Context, msg *bridgev2.MatrixMessage, targetSteamID uint64) (*bridgev2.MatrixMessageResponse, error) {
	content := msg.Event.Content.AsMessage()
	if content == nil {
		return nil, fmt.Errorf("failed to parse sticker content")
	}

	// Convert sticker to text message since Steam doesn't support stickers natively
	stickerText := fmt.Sprintf("[Sticker: %s]", content.Body)

	resp, err := sc.msgClient.SendMessage(ctx, &steamapi.SendMessageRequest{
		TargetSteamId: targetSteamID,
		Message:       stickerText,
		MessageType:   steamapi.MessageType_CHAT_MESSAGE,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to send sticker message to Steam: %w", err)
	}

	if !resp.Success {
		return nil, fmt.Errorf("steam sticker message send failed: %s", resp.ErrorMessage)
	}

	msgMeta := &MessageMetadata{
		SteamMessageType: "STICKER_AS_TEXT",
		IsEcho:           false,
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        networkid.MessageID(fmt.Sprintf("%d:%d", targetSteamID, resp.Timestamp)),
			MXID:      msg.Event.ID,
			Timestamp: time.Unix(resp.Timestamp, 0),
			Metadata:  msgMeta,
		},
	}, nil
}

// handleImageMessage processes image messages from Matrix and sends them to Steam
func (sc *SteamClient) handleImageMessage(ctx context.Context, msg *bridgev2.MatrixMessage, targetSteamID uint64) (*bridgev2.MatrixMessageResponse, error) {
	sc.br.Log.Debug().
		Str("event_type", msg.Event.Type.String()).
		Str("event_id", string(msg.Event.ID)).
		Interface("raw_content", msg.Event.Content.Raw).
		Msg("Raw Matrix event for image message")

	content := msg.Event.Content.AsMessage()
	if content == nil {
		return nil, fmt.Errorf("failed to parse image content")
	}

	// Extract the media URL - handle both regular and encrypted formats
	var mediaURL id.ContentURIString
	if content.URL != "" {
		// Regular format: url field directly
		mediaURL = content.URL
	} else if content.File != nil && content.File.URL != "" {
		// Encrypted format: file.url field
		mediaURL = content.File.URL
	}

	sc.br.Log.Info().
		Str("image_url", string(content.URL)).
		Str("file_url", string(content.File.URL)).
		Str("resolved_media_url", string(mediaURL)).
		Str("mime_type", content.Info.MimeType).
		Int("size", content.Info.Size).
		Str("caption", content.Body).
		Str("msgtype", string(content.MsgType)).
		Interface("info_object", content.Info).
		Bool("is_encrypted", content.File != nil).
		Msg("Processing image message from Matrix")

	// Check if we have a valid media URL
	if mediaURL == "" {
		return nil, fmt.Errorf("no media URL found in image message (neither url nor file.url present)")
	}

	// Parse MXC URL for logging
	var serverName, mediaID string
	mxcStr := string(mediaURL)
	if strings.HasPrefix(mxcStr, "mxc://") {
		parts := strings.SplitN(mxcStr[6:], "/", 2) // Remove "mxc://" and split
		if len(parts) == 2 {
			serverName = parts[0]
			mediaID = parts[1]
		}
	}

	// Log the exact MXC URL that will be passed to GetPublicMediaAddress
	sc.br.Log.Info().
		Str("input_mxc_url", string(mediaURL)).
		Bool("is_encrypted_media", content.File != nil).
		Str("server_name", serverName).
		Str("media_id", mediaID).
		Msg("MXC URL → GetPublicMediaAddress INPUT")

	// Try to use public media if available (preferred approach)
	if matrixConn, ok := sc.br.Matrix.(bridgev2.MatrixConnectorWithPublicMedia); ok {
		sc.br.Log.Debug().
			Str("media_url", string(mediaURL)).
			Bool("is_encrypted", content.File != nil).
			Msg("Matrix connector supports public media interface, attempting to get public URL")
		
		publicURL := matrixConn.GetPublicMediaAddress(mediaURL)
		
		sc.br.Log.Info().
			Str("input_mxc_url", string(mediaURL)).
			Str("output_public_url", publicURL).
			Bool("has_public_url", publicURL != "").
			Bool("is_encrypted_media", content.File != nil).
			Int("url_length", len(publicURL)).
			Msg("MXC URL → GetPublicMediaAddress OUTPUT")
		
		if publicURL != "" {
			// Log the exact URL that will be sent to Steam
			sc.br.Log.Info().
				Str("public_url", publicURL).
				Str("original_mxc", string(mediaURL)).
				Msg("Final URL that will be sent to Steam")

			// Send caption as separate message if present and not just the filename
			var captionResp *steamapi.SendMessageResponse
			var err error
			if content.Body != "" && content.Body != content.FileName {
				sc.br.Log.Debug().
					Str("caption", content.Body).
					Str("filename", content.FileName).
					Msg("Sending image caption as separate message")
				
				captionResp, err = sc.msgClient.SendMessage(ctx, &steamapi.SendMessageRequest{
					TargetSteamId: targetSteamID,
					Message:       content.Body,
					MessageType:   steamapi.MessageType_CHAT_MESSAGE,
				})
				if err != nil {
					return nil, fmt.Errorf("failed to send image caption to Steam: %w", err)
				}
				if !captionResp.Success {
					return nil, fmt.Errorf("steam caption message send failed: %s", captionResp.ErrorMessage)
				}
			}

			// Send image URL as separate message
			resp, err := sc.msgClient.SendMessage(ctx, &steamapi.SendMessageRequest{
				TargetSteamId: targetSteamID,
				Message:       publicURL,
				MessageType:   steamapi.MessageType_CHAT_MESSAGE,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to send image URL to Steam: %w", err)
			}

			if !resp.Success {
				return nil, fmt.Errorf("steam image URL send failed: %s", resp.ErrorMessage)
			}

			msgMeta := &MessageMetadata{
				SteamMessageType: "IMAGE_PUBLIC_URL",
				IsEcho:           false,
				ImageURL:         publicURL,
			}

			sc.br.Log.Info().
				Str("sent_public_url", publicURL).
				Str("source_mxc_url", string(mediaURL)).
				Int64("timestamp", resp.Timestamp).
				Bool("was_encrypted", content.File != nil).
				Msg("Image message sent to Steam successfully")

			return &bridgev2.MatrixMessageResponse{
				DB: &database.Message{
					ID:        networkid.MessageID(fmt.Sprintf("%d:%d", targetSteamID, resp.Timestamp)),
					MXID:      msg.Event.ID,
					Timestamp: time.Unix(resp.Timestamp, 0),
					Metadata:  msgMeta,
				},
			}, nil
		}

		sc.br.Log.Error().
			Str("input_mxc_url", string(mediaURL)).
			Str("empty_public_url", publicURL).
			Bool("is_encrypted", content.File != nil).
			Str("server_name", serverName).
			Str("media_id", mediaID).
			Msg("GetPublicMediaAddress returned empty URL")
	} else {
		sc.br.Log.Error().
			Str("matrix_connector_type", fmt.Sprintf("%T", sc.br.Matrix)).
			Msg("Matrix connector does not implement MatrixConnectorWithPublicMedia interface")
	}

	// No public media available - Matrix→Steam image sharing not supported
	// Steam blocks UGC uploads from third-party clients, so we cannot upload images to Steam
	sc.br.Log.Error().
		Str("reason", "public_media_unavailable").
		Str("failed_mxc_url", string(mediaURL)).
		Bool("was_encrypted", content.File != nil).
		Msg("Cannot send image to Steam: public media not configured or GetPublicMediaAddress failed")

	return nil, fmt.Errorf("image sharing to Steam requires public media configuration. Please enable 'public_media.enabled: true' and set 'appservice.public_address' in bridge config")
}

// extractFilenameFromURL extracts a filename from a URL path
func extractFilenameFromURL(imageURL string) string {
	// Simple extraction - get the last path component
	if idx := strings.LastIndex(imageURL, "/"); idx != -1 && idx < len(imageURL)-1 {
		return imageURL[idx+1:]
	}
	return ""
}

// getFileExtensionFromMimeType returns the appropriate file extension for a MIME type
func getFileExtensionFromMimeType(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return ""
	}
}

// handleIncomingMessage processes incoming messages from Steam and sends them to Matrix
func (sc *SteamClient) handleIncomingMessage(_ context.Context, msgEvent *steamapi.MessageEvent) {
	sc.br.Log.Info().
		Uint64("sender_steam_id", msgEvent.SenderSteamId).
		Str("message_type", msgEvent.MessageType.String()).
		Int64("timestamp", msgEvent.Timestamp).
		Msg("Received message from Steam")

	// Detect echo messages from other Steam clients
	if msgEvent.IsEcho {
		sc.br.Log.Debug().
			Uint64("sender_steam_id", msgEvent.SenderSteamId).
			Msg("Processing echo message from other Steam client")
		// Continue processing instead of skipping
	}

	// Generate message ID
	var msgID string
	switch msgEvent.MessageType {
	case steamapi.MessageType_INVITE_GAME:
		msgID = fmt.Sprintf("%d:%d:invite", msgEvent.SenderSteamId, msgEvent.Timestamp)
	default:
		msgID = fmt.Sprintf("%d:%d", msgEvent.SenderSteamId, msgEvent.Timestamp)
	}

	// Get current user's Steam ID to determine if this is a DM
	meta := sc.UserLogin.Metadata.(*UserLoginMetadata)
	if meta == nil {
		sc.br.Log.Error().Msg("No user metadata found for handling incoming message")
		return
	}

	// Determine portal ID - for DMs, use the other user's Steam ID
	var portalID networkid.PortalID
	if msgEvent.TargetSteamId == meta.SteamID {
		// This is a message sent to us, portal is the sender
		portalID = makePortalID(msgEvent.SenderSteamId)
	} else {
		// This is a message we sent from another client, portal is the target
		portalID = makePortalID(msgEvent.TargetSteamId)
	}

	// Create portal key
	portalKey := networkid.PortalKey{
		ID:       portalID,
		Receiver: sc.UserLogin.ID,
	}

	// Determine sender ID
	senderID := makeUserID(msgEvent.SenderSteamId)

	// Create appropriate EventSender based on whether this is an echo message
	var eventSender bridgev2.EventSender
	if msgEvent.IsEcho {
		// Echo message from our other Steam clients - show as "from me"
		eventSender = bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: sc.UserLogin.ID,
			Sender:      senderID,
		}
	} else {
		// Regular incoming message from another user
		eventSender = bridgev2.EventSender{
			Sender: senderID,
		}
	}

	// Message metadata will be created in the conversion function

	// Create appropriate remote event based on message type
	switch msgEvent.MessageType {
	case steamapi.MessageType_CHAT_MESSAGE:
		remoteMsg := &simplevent.Message[*steamapi.MessageEvent]{
			EventMeta: simplevent.EventMeta{
				Type: bridgev2.RemoteEventMessage,
				LogContext: func(c zerolog.Context) zerolog.Context {
					return c.Uint64("sender_steam_id", msgEvent.SenderSteamId).
						Int64("timestamp", msgEvent.Timestamp)
				},
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       eventSender,
				Timestamp:    time.Unix(msgEvent.Timestamp, 0),
			},
			Data:               msgEvent,
			ID:                 networkid.MessageID(msgID),
			ConvertMessageFunc: sc.convertSteamMessage,
		}
		sc.br.QueueRemoteEvent(sc.UserLogin, remoteMsg)

	case steamapi.MessageType_EMOTE:
		remoteMsg := &simplevent.Message[*steamapi.MessageEvent]{
			EventMeta: simplevent.EventMeta{
				Type: bridgev2.RemoteEventMessage,
				LogContext: func(c zerolog.Context) zerolog.Context {
					return c.Uint64("sender_steam_id", msgEvent.SenderSteamId).
						Int64("timestamp", msgEvent.Timestamp)
				},
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       eventSender,
				Timestamp:    time.Unix(msgEvent.Timestamp, 0),
			},
			Data:               msgEvent,
			ID:                 networkid.MessageID(msgID),
			ConvertMessageFunc: sc.convertSteamMessage,
		}
		sc.br.QueueRemoteEvent(sc.UserLogin, remoteMsg)

	case steamapi.MessageType_TYPING:
		// Handle typing notifications - create typing event
		typingEvent := &simplevent.Typing{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventTyping,
				PortalKey: portalKey,
				Sender:    eventSender,
				Timestamp: time.Unix(msgEvent.Timestamp, 0),
			},
			Timeout: 5 * time.Second,
			Type:    bridgev2.TypingTypeText,
		}
		sc.br.QueueRemoteEvent(sc.UserLogin, typingEvent)

	case steamapi.MessageType_INVITE_GAME:
		// Handle game invites as special messages
		remoteMsg := &simplevent.Message[*steamapi.MessageEvent]{
			EventMeta: simplevent.EventMeta{
				Type: bridgev2.RemoteEventMessage,
				LogContext: func(c zerolog.Context) zerolog.Context {
					return c.Uint64("sender_steam_id", msgEvent.SenderSteamId).
						Int64("timestamp", msgEvent.Timestamp).
						Str("message_type", "game_invite")
				},
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       eventSender,
				Timestamp:    time.Unix(msgEvent.Timestamp, 0),
			},
			Data:               msgEvent,
			ID:                 networkid.MessageID(msgID),
			ConvertMessageFunc: sc.convertSteamMessage,
		}
		sc.br.QueueRemoteEvent(sc.UserLogin, remoteMsg)

	default:
		sc.br.Log.Warn().Str("message_type", msgEvent.MessageType.String()).Msg("Unsupported message type")
	}
}

// detectImageURL scans a message for image URLs and returns the first one found
func detectImageURL(message string) string {
	if message == "" {
		return ""
	}
	
	// Steam image URL patterns
	imagePatterns := []string{
		// Steam's native image uploads
		`https://images\.steamusercontent\.com/ugc/\d+/[A-F0-9]+/?`,
		// Steam community screenshots  
		`https://steamcommunity\.com/sharedfiles/filedetails/\?id=\d+`,
		// Steam CDN images
		`https://steamcdn-a\.akamaihd\.net/.*\.(jpg|jpeg|png|gif|webp)`,
		// Steam user images
		`https://steamuserimages-a\.akamaihd\.net/.*\.(jpg|jpeg|png|gif|webp)`,
		// Common external image hosts
		`https://(?:i\.)?imgur\.com/[a-zA-Z0-9]+(?:\.(jpg|jpeg|png|gif|webp))?`,
		// Direct image URLs
		`https?://.*\.(jpg|jpeg|png|gif|webp)(?:\?.*)?$`,
	}
	
	for _, pattern := range imagePatterns {
		if re, err := regexp.Compile("(?i)" + pattern); err == nil {
			if match := re.FindString(message); match != "" {
				return match
			}
		}
	}
	
	return ""
}

// convertSteamMessage converts a Steam message event to a Matrix message
func (sc *SteamClient) convertSteamMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *steamapi.MessageEvent) (*bridgev2.ConvertedMessage, error) {
	var content *event.MessageEventContent

	switch data.MessageType {
	case steamapi.MessageType_CHAT_MESSAGE:
		// Auto-detect image URLs in Steam messages if not already set
		if data.ImageUrl == "" {
			if detectedURL := detectImageURL(data.Message); detectedURL != "" {
				data.ImageUrl = detectedURL
				sc.br.Log.Info().
					Str("detected_image_url", detectedURL).
					Str("original_message", data.Message).
					Msg("Auto-detected image URL in Steam message")
			}
		}
		
		// Check if this message contains an image URL
		if data.ImageUrl != "" {
			return sc.convertImageMessage(ctx, portal, intent, data)
		}

		content = &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    data.Message,
		}
	case steamapi.MessageType_EMOTE:
		content = &event.MessageEventContent{
			MsgType: event.MsgEmote,
			Body:    data.Message,
		}
	case steamapi.MessageType_INVITE_GAME:
		content = &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    fmt.Sprintf("🎮 Game Invite: %s", data.Message),
		}
	default:
		return nil, fmt.Errorf("unsupported message type: %s", data.MessageType.String())
	}

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

// convertImageMessage converts a Steam image message to a Matrix image message
func (sc *SteamClient) convertImageMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *steamapi.MessageEvent) (*bridgev2.ConvertedMessage, error) {
	if data.ImageUrl == "" {
		return nil, fmt.Errorf("no image URL provided in message")
	}

	imageURL := data.ImageUrl
	sc.br.Log.Info().
		Str("image_url", imageURL).
		Str("caption", data.Message).
		Msg("Converting Steam image message to Matrix")

	// Download image from Steam
	downloadResp, err := sc.msgClient.DownloadImageFromSteam(ctx, &steamapi.DownloadImageRequest{
		ImageUrl: imageURL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download image from Steam: %w", err)
	}

	if !downloadResp.Success {
		return nil, fmt.Errorf("steam image download failed: %s", downloadResp.ErrorMessage)
	}

	// Extract filename from URL or use default
	filename := extractFilenameFromURL(imageURL)
	if filename == "" {
		filename = "image"
	}

	// Add file extension based on MIME type
	if ext := getFileExtensionFromMimeType(downloadResp.MimeType); ext != "" {
		filename += "." + ext
	}

	// Upload image to Matrix
	mxcURL, encryptedFile, err := intent.UploadMedia(ctx, portal.MXID, downloadResp.ImageData, filename, downloadResp.MimeType)
	if err != nil {
		return nil, fmt.Errorf("failed to upload image to Matrix: %w", err)
	}

	// Create Matrix image message content
	content := &event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    data.Message, // Use Steam message as caption
		URL:     mxcURL,
		File:    encryptedFile,
		Info: &event.FileInfo{
			MimeType: downloadResp.MimeType,
			Size:     len(downloadResp.ImageData),
		},
	}

	// If caption is empty, use filename as body
	if content.Body == "" {
		content.Body = filename
	}

	sc.br.Log.Info().
		Str("matrix_mxc_url", string(mxcURL)).
		Str("filename", filename).
		Int("size", len(downloadResp.ImageData)).
		Msg("Image converted and uploaded to Matrix successfully")

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

// HandleMatrixTyping implements bridgev2.TypingHandlingNetworkAPI.
// This method handles typing notifications from Matrix and sends them to Steam.
func (sc *SteamClient) HandleMatrixTyping(ctx context.Context, msg *bridgev2.MatrixTyping) error {
	if !msg.IsTyping {
		// Steam doesn't support explicit "stop typing" notifications
		// Steam typing notifications automatically timeout
		return nil
	}

	// Parse the Steam ID from the portal ID
	steamID, err := parseSteamIDFromPortalID(msg.Portal.ID)
	if err != nil {
		return fmt.Errorf("failed to parse Steam ID from portal ID %q: %w", msg.Portal.ID, err)
	}

	// Send typing notification to Steam via gRPC
	resp, err := sc.msgClient.SendTypingNotification(ctx, &steamapi.TypingNotificationRequest{
		TargetSteamId: steamID,
		IsTyping:      true,
	})
	if err != nil {
		return fmt.Errorf("failed to send typing notification to Steam: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("Steam typing notification failed")
	}

	return nil
}
