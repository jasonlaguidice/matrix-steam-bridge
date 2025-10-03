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
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials,
			"Steam session replaced by another login",
			withReason(event.Reason),
			withUserAction(status.UserActionRelogin)))

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
	sc.reconnectionMutex.Lock()
	defer sc.reconnectionMutex.Unlock()

	// If already reconnecting, just log and return
	if sc.isReconnecting {
		sc.br.Log.Debug().
			Str("message", message).
			Str("reason", reason).
			Msg("Reconnection already in progress, ignoring duplicate disconnect event")
		return
	}

	// Cancel any previous reconnection context
	if sc.reconnectionCancel != nil {
		sc.reconnectionCancel()
	}

	// Create new reconnection context
	reconnectCtx, cancel := context.WithCancel(ctx)
	sc.reconnectionCancel = cancel
	sc.isReconnecting = true
	sc.reconnectionAttempts = 0
	sc.lastReconnectTime = time.Now()

	// Send transient disconnect state
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect,
		message, withReason(reason)))

	// Start single managed reconnection process
	go sc.managedReconnectionLoop(reconnectCtx, message)
}

// managedReconnectionLoop implements centralized reconnection with iterative backoff
func (sc *SteamClient) managedReconnectionLoop(ctx context.Context, initialMessage string) {
	defer func() {
		sc.reconnectionMutex.Lock()
		sc.isReconnecting = false
		sc.reconnectionCancel = nil
		sc.reconnectionMutex.Unlock()
	}()

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

	// Iterative reconnection loop - continues indefinitely until success or cancellation
	for {
		select {
		case <-ctx.Done():
			sc.br.Log.Debug().Msg("Reconnection cancelled")
			return
		default:
		}

		// Calculate backoff delay
		var backoffDelay time.Duration
		if config.ReconnectDelay > 0 {
			backoffDelay = config.ReconnectDelay * time.Duration(sc.reconnectionAttempts+1)
		} else {
			// Exponential backoff: 5, 10, 20, 40, 60, 60 seconds (capped)
			backoffSeconds := min(60, 5*(1<<sc.reconnectionAttempts))
			backoffDelay = time.Duration(backoffSeconds) * time.Second
		}

		sc.br.Log.Info().
			Int("retry_count", sc.reconnectionAttempts).
			Dur("retry_in", backoffDelay).
			Msg("Attempting automatic reconnection")

		// Wait for backoff delay
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoffDelay):
		}

		// Check if already connected before attempting
		sc.stateMutex.RLock()
		if sc.isConnected {
			sc.stateMutex.RUnlock()
			sc.br.Log.Info().Msg("Already connected during reconnection wait")
			return
		}
		sc.stateMutex.RUnlock()

		// Update bridge state
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting,
			fmt.Sprintf("Reconnecting to Steam (attempt %d)", sc.reconnectionAttempts+1)))

		// Attempt connection with fresh context to avoid using cancelled contexts from previous sessions
		sc.Connect(context.Background())

		// Wait briefly and check result
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}

		sc.stateMutex.RLock()
		isConnected := sc.isConnected
		sc.stateMutex.RUnlock()

		if isConnected {
			sc.br.Log.Info().
				Int("retry_count", sc.reconnectionAttempts).
				Msg("Automatic reconnection successful")
			sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnected,
				"Successfully reconnected to Steam"))
			return
		}

		sc.reconnectionAttempts++
	}
}
