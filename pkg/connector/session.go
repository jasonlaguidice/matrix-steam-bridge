package connector

import (
	"context"
	"fmt"
	"time"

	"maunium.net/go/mautrix/bridgev2/status"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

// startSessionEventSubscription starts a gRPC stream to receive session events (logout notifications)
func (sc *SteamClient) startSessionEventSubscription(ctx context.Context) {
	sc.br.Log.Info().Msg("Starting Steam session event subscription")

	// Check if sessionClient is nil before using it
	if sc.sessionClient == nil {
		sc.br.Log.Error().Msg("Session client is nil, cannot start session event subscription")
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateUnknownError,
			"Session monitoring unavailable - session client not initialized",
			withUserAction(status.UserActionRestart)))
		return
	}

	stream, err := sc.sessionClient.SubscribeToSessionEvents(ctx, &steamapi.SessionSubscriptionRequest{})
	if err != nil {
		sc.br.Log.Error().Err(err).Msg("Failed to start session event subscription")
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateUnknownError,
			"Failed to establish session monitoring",
			withReason(err.Error()),
			withUserAction(status.UserActionRestart)))
		return
	}

	sc.br.Log.Info().Msg("Session event subscription established successfully")

	for {
		select {
		case <-ctx.Done():
			sc.br.Log.Info().Msg("Session event subscription context cancelled")
			return
		default:
			sessionEvent, err := stream.Recv()
			if err != nil {
				sc.br.Log.Error().Err(err).Msg("Error receiving session event from stream")
				// Session event stream failure is not critical enough to disconnect entirely
				// Just log the error and exit this goroutine
				// The main message stream will handle connection issues
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
		// Treat as transient disconnect and attempt automatic reconnection
		go sc.handleTransientDisconnect(ctx, "Steam session replaced by another login", event.Reason)

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
		// For connection loss, implement automatic reconnection with debounce
		go sc.handleTransientDisconnect(ctx, "Steam connection lost", event.Reason)

	case steamapi.SessionEventType_LOGGED_OFF:
	default:
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateLoggedOut,
			"Logged out from Steam",
			withReason(event.Reason)))
	}

	// Invalidate user metadata if the session is permanently invalid
	// Note: SESSION_REPLACED and CONNECTION_LOST are now treated as transient, so don't invalidate metadata
	if event.EventType == steamapi.SessionEventType_TOKEN_EXPIRED ||
		event.EventType == steamapi.SessionEventType_ACCOUNT_DISABLED {

		if meta := sc.getUserMetadata(); meta != nil {
			meta.IsValid = false
			sc.UserLogin.Save(ctx)
			sc.br.Log.Info().Msg("Invalidated user metadata due to session event")
		}
	}
}

// handleTransientDisconnect manages automatic reconnection for transient disconnects
func (sc *SteamClient) handleTransientDisconnect(ctx context.Context, message, reason string) {
	// Wait briefly to see if connection recovers naturally (debounce)
	time.Sleep(5 * time.Second)
	
	// Check if we're already reconnected
	sc.stateMutex.Lock()
	if sc.isConnected {
		sc.stateMutex.Unlock()
		sc.br.Log.Debug().Msg("Connection recovered during debounce period, skipping reconnection")
		return
	}
	sc.stateMutex.Unlock()
	
	// Send transient disconnect state
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect,
		message, withReason(reason)))
	
	// Attempt automatic reconnection with exponential backoff
	go sc.attemptReconnection(ctx, 0)
}

// attemptReconnection implements automatic reconnection with exponential backoff
func (sc *SteamClient) attemptReconnection(ctx context.Context, retryCount int) {
	// Get Steam connector config
	steamConnector := sc.br.Network.(*SteamConnector)
	config := steamConnector.Config
	
	// Check if auto-reconnect is disabled
	if !config.AutoReconnect {
		sc.br.Log.Debug().Msg("Auto-reconnect disabled in configuration, skipping reconnection")
		return
	}
	
	maxRetries := config.MaxReconnectTries
	if maxRetries <= 0 {
		maxRetries = 6 // Default fallback
	}
	
	if retryCount >= maxRetries {
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateUnknownError,
			"Failed to reconnect after multiple attempts",
			withUserAction(status.UserActionRestart)))
		return
	}
	
	// Use configured delay or exponential backoff
	var backoffDelay time.Duration
	if config.ReconnectDelay > 0 {
		backoffDelay = config.ReconnectDelay * time.Duration(retryCount+1)
	} else {
		// Exponential backoff: 2, 4, 8, 16, 32, 64 seconds
		backoffDelay = time.Duration(2<<retryCount) * time.Second
	}
	
	sc.br.Log.Info().
		Int("retry_count", retryCount).
		Dur("retry_in", backoffDelay).
		Msg("Attempting automatic reconnection")
	
	time.Sleep(backoffDelay)
	
	// Check if we're already connected (maybe user manually reconnected)
	sc.stateMutex.Lock()
	if sc.isConnected {
		sc.stateMutex.Unlock()
		sc.br.Log.Debug().Msg("Already connected, skipping reconnection attempt")
		return
	}
	sc.stateMutex.Unlock()
	
	// Update bridge state to show reconnection attempt
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting,
		fmt.Sprintf("Reconnecting to Steam (attempt %d/%d)", retryCount+1, maxRetries)))
	
	// Attempt to reconnect
	sc.Connect(ctx)
	
	// Wait a moment to see if connection succeeded
	time.Sleep(3 * time.Second)
	
	sc.stateMutex.Lock()
	isConnected := sc.isConnected
	sc.stateMutex.Unlock()
	
	if !isConnected {
		// Connection failed, schedule next retry
		go sc.attemptReconnection(ctx, retryCount+1)
	} else {
		sc.br.Log.Info().
			Int("retry_count", retryCount).
			Msg("Automatic reconnection successful")
		
		// Ensure user sees final reconnection success state
		// This is critical as Connect() might not have sent the final state due to stream setup
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnected, 
			"Successfully reconnected to Steam"))
	}
}
