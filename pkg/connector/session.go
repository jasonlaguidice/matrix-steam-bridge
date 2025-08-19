package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2/status"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

// startSessionEventSubscription starts a gRPC stream to receive session events (logout notifications)
func (sc *SteamClient) startSessionEventSubscription(ctx context.Context) {
	sc.br.Log.Info().Msg("Starting Steam session event subscription")

	// Check if sessionClient is nil before using it
	if sc.sessionClient == nil {
		sc.br.Log.Error().Msg("Session client is nil, cannot start session event subscription")
		return
	}

	stream, err := sc.sessionClient.SubscribeToSessionEvents(ctx, &steamapi.SessionSubscriptionRequest{})
	if err != nil {
		sc.br.Log.Error().Err(err).Msg("Failed to start session event subscription")
		return
	}

	for {
		select {
		case <-ctx.Done():
			sc.br.Log.Info().Msg("Session event subscription context cancelled")
			return
		default:
			sessionEvent, err := stream.Recv()
			if err != nil {
				sc.br.Log.Error().Err(err).Msg("Error receiving session event from stream")
				return
			}

			sc.handleSessionEvent(ctx, sessionEvent)
		}
	}
}

// handleSessionEvent processes incoming session events and updates bridge state
func (sc *SteamClient) handleSessionEvent(ctx context.Context, event *steamapi.SessionEvent) {
	sc.br.Log.Info().
		Str("event_type", event.EventType.String()).
		Str("reason", event.Reason).
		Int64("timestamp", event.Timestamp).
		Msg("Received session event from Steam")

	// Update connection state
	sc.stateMutex.Lock()
	sc.isConnected = false
	sc.isConnecting = false
	sc.stateMutex.Unlock()

	// Stop connection monitoring since we're disconnected
	sc.stopConnectionMonitoring()

	// Send appropriate bridge state based on event type
	switch event.EventType {
	case steamapi.SessionEventType_SESSION_REPLACED:
		// Session replacement can happen during relogin or multiple connections
		// Treat as transient disconnect to allow reconnection instead of permanent failure
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect,
			"Steam session replaced by another login",
			withReason(event.Reason)))

	case steamapi.SessionEventType_TOKEN_EXPIRED:
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials,
			"Steam credentials expired",
			withReason(event.Reason),
			withUserAction(status.UserActionRelogin)))

	case steamapi.SessionEventType_ACCOUNT_DISABLED:
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials,
			"Steam account disabled",
			withReason(event.Reason),
			withUserAction(status.UserActionRelogin)))

	case steamapi.SessionEventType_CONNECTION_LOST:
		// For connection loss, use transient disconnect with potential for reconnection
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect,
			"Steam connection lost",
			withReason(event.Reason)))

		// TODO: Implement automatic reconnection logic if desired

	case steamapi.SessionEventType_LOGGED_OFF:
	default:
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateLoggedOut,
			"Logged out from Steam",
			withReason(event.Reason)))
	}

	// Invalidate user metadata if the session is permanently invalid
	// Note: SESSION_REPLACED is now treated as transient, so don't invalidate metadata
	if event.EventType == steamapi.SessionEventType_TOKEN_EXPIRED ||
		event.EventType == steamapi.SessionEventType_ACCOUNT_DISABLED {

		if meta := sc.getUserMetadata(); meta != nil {
			meta.IsValid = false
			sc.UserLogin.Save(ctx)
			sc.br.Log.Info().Msg("Invalidated user metadata due to session event")
		}
	}
}
