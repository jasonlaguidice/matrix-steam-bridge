package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2/status"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

// Constants for bridge state debouncing (following Signal bridge pattern)
const (
	disconnectDebounceDelay = 7 * time.Second // Debounce delay for transient disconnects
)

// Bridge state option helpers
func withReason(reason string) func(*status.BridgeState) {
	return func(state *status.BridgeState) {
		state.Reason = reason
	}
}

func withUserAction(action status.BridgeStateUserAction) func(*status.BridgeState) {
	return func(state *status.BridgeState) {
		state.UserAction = action
	}
}

func withInfo(info map[string]interface{}) func(*status.BridgeState) {
	return func(state *status.BridgeState) {
		state.Info = info
	}
}

// buildBridgeState builds a bridge state with common metadata
func (sc *SteamClient) buildBridgeState(state status.BridgeStateEvent, message string, opts ...func(*status.BridgeState)) status.BridgeState {
	bridgeState := status.BridgeState{
		StateEvent: state,
		Message:    message,
	}

	// Apply optional modifications
	for _, opt := range opts {
		opt(&bridgeState)
	}

	// Add remote profile information if available
	if meta := sc.getUserMetadata(); meta != nil {
		bridgeState.RemoteID = meta.RemoteID
		bridgeState.RemoteName = meta.PersonaName
		bridgeState.RemoteProfile = &status.RemoteProfile{
			Name: meta.PersonaName,
		}
	}

	return bridgeState
}

// getUserMetadata safely retrieves user metadata
func (sc *SteamClient) getUserMetadata() *UserLoginMetadata {
	if sc.UserLogin == nil || sc.UserLogin.Metadata == nil {
		return nil
	}

	if meta, ok := sc.UserLogin.Metadata.(*UserLoginMetadata); ok {
		return meta
	}

	return nil
}

// debouncedDisconnectState sends a debounced transient disconnect state following Signal bridge patterns
func (sc *SteamClient) debouncedDisconnectState() {
	sc.disconnectDebounceMutex.Lock()
	defer sc.disconnectDebounceMutex.Unlock()

	// Stop existing timer if running
	if sc.disconnectDebounceTimer != nil {
		sc.disconnectDebounceTimer.Stop()
	}

	// Start new debounce timer
	sc.disconnectDebounceTimer = time.AfterFunc(disconnectDebounceDelay, func() {
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect, "Disconnected from Steam"))
	})
}

// cancelDisconnectDebounce cancels any pending disconnect debounce timer
func (sc *SteamClient) cancelDisconnectDebounce() {
	sc.disconnectDebounceMutex.Lock()
	defer sc.disconnectDebounceMutex.Unlock()

	if sc.disconnectDebounceTimer != nil {
		sc.disconnectDebounceTimer.Stop()
		sc.disconnectDebounceTimer = nil
	}
}

// startConnectionMonitoring starts monitoring connection health with gRPC health checks
func (sc *SteamClient) startConnectionMonitoring(ctx context.Context) {
	sc.connectionMutex.Lock()
	defer sc.connectionMutex.Unlock()

	// Cancel existing monitoring if running
	if sc.connectionCancel != nil {
		sc.connectionCancel()
	}

	sc.connectionCtx, sc.connectionCancel = context.WithCancel(ctx)

	go sc.connectionMonitorLoop()
}

// connectionMonitorLoop periodically checks connection health
func (sc *SteamClient) connectionMonitorLoop() {
	ticker := time.NewTicker(30 * time.Second) // Check every 30 seconds
	defer ticker.Stop()

	for {
		select {
		case <-sc.connectionCtx.Done():
			sc.br.Log.Debug().Msg("Connection monitoring stopped")
			return
		case <-ticker.C:
			if err := sc.checkConnectionHealth(); err != nil {
				sc.br.Log.Warn().Err(err).Msg("Connection health check failed")
				// Don't immediately report disconnection - let the message stream handle it
			} else {
				sc.br.Log.Trace().Msg("gRPC heartbeat to SteamBridge service successful")
			}
		}
	}
}

// checkConnectionHealth performs a gRPC heartbeat to verify SteamBridge service is responsive
func (sc *SteamClient) checkConnectionHealth() error {
	if sc.userClient == nil {
		return fmt.Errorf("user client not initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use a non-destructive method to check if the gRPC connection is healthy
	// Get our own user info as a health check - this doesn't change any state
	meta := sc.getUserMetadata()
	if meta == nil {
		return fmt.Errorf("no user metadata for health check")
	}

	_, err := sc.userClient.GetUserInfo(ctx, &steamapi.UserInfoRequest{
		SteamId: meta.SteamID,
	})
	return err
}

// stopConnectionMonitoring stops connection monitoring
func (sc *SteamClient) stopConnectionMonitoring() {
	sc.connectionMutex.Lock()
	defer sc.connectionMutex.Unlock()

	if sc.connectionCancel != nil {
		sc.connectionCancel()
		sc.connectionCancel = nil
	}
}

// Connect establishes a connection to Steam and starts message streaming
func (sc *SteamClient) Connect(ctx context.Context) {
	// Set connection state to prevent duplicate connections
	sc.stateMutex.Lock()
	if sc.isConnecting || sc.isConnected {
		sc.stateMutex.Unlock()
		sc.br.Log.Debug().Msg("Already connecting or connected, skipping duplicate Connect() call")
		return
	}
	sc.isConnecting = true
	sc.stateMutex.Unlock()

	// Ensure we clean up connection state on exit
	defer func() {
		sc.stateMutex.Lock()
		sc.isConnecting = false
		sc.stateMutex.Unlock()
	}()

	sc.br.Log.Info().Msg("Connect() - Connecting to Steam")

	// Consolidate initial connection state messages
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting, "Connecting to Steam"))

	// Steam requires a persistent connection - setup that connection here
	// using gRPC API. This is missing from other attempts
	if sc.authClient == nil {
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials, "You're not logged into Steam",
			withUserAction(status.UserActionRelogin)))
	return
	}

	meta := sc.getUserMetadata()
	if meta == nil {
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials, "No user metadata found",
			withUserAction(status.UserActionRelogin)))
		return
	}

	// Always attempt re-authentication when stored credentials exist
	// This ensures proper coordination with C# service initialization and friends list waiting
	if meta.AccessToken != "" && meta.RefreshToken != "" && (meta.AccountName != "" || meta.Username != "") {
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting, "Re-authenticating with stored credentials"))

		// Use AccountName for authentication, fallback to Username for backward compatibility
		authUsername := meta.AccountName
		if authUsername == "" {
			authUsername = meta.Username
		}

		sc.br.Log.Info().Str("username", authUsername).Msg("Attempting re-authentication with stored tokens")

		reAuthReq := &steamapi.TokenReAuthRequest{
			AccessToken:  meta.AccessToken, // Send tokens as stored
			RefreshToken: meta.RefreshToken,
			Username:     authUsername,
		}

		resp, err := sc.authClient.ReAuthenticateWithTokens(ctx, reAuthReq)
		if err != nil {
			sc.br.Log.Err(err).Msg("Failed to re-authenticate with stored tokens")
			
			// Check if this is a network connectivity issue
			if strings.Contains(err.Error(), "connect") || strings.Contains(err.Error(), "connection") || 
			   strings.Contains(err.Error(), "network") || strings.Contains(err.Error(), "timeout") {
				// Network issue - trigger transient disconnect handling instead of credential failure
				sc.br.Log.Warn().Msg("Network connectivity issue during re-authentication, treating as transient disconnect")
				go sc.handleTransientDisconnect(ctx, "Steam network connectivity issue during login", err.Error())
				return
			}
			
			sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateUnknownError,
				"Re-authentication failed - please check Steam service connection",
				withReason(err.Error()),
				withUserAction(status.UserActionRestart)))
			return
		}

		if !resp.Success || resp.State != steamapi.AuthStatusResponse_AUTHENTICATED {
			sc.br.Log.Warn().Str("auth_state", resp.State.String()).Str("error", resp.ErrorMessage).Msg("Token re-authentication failed")

			// Check if the failure is due to network connectivity rather than bad credentials
			if strings.Contains(resp.ErrorMessage, "Failed to connect to Steam network") ||
			   strings.Contains(resp.ErrorMessage, "Failed to connect to Steam within timeout") ||
			   strings.Contains(resp.ErrorMessage, "network") ||
			   strings.Contains(resp.ErrorMessage, "timeout") ||
			   strings.Contains(resp.ErrorMessage, "connection") {
				// Network connectivity issue - treat as transient disconnect
				sc.br.Log.Warn().Msg("Network connectivity issue in auth response, treating as transient disconnect")
				go sc.handleTransientDisconnect(ctx, "Steam network connectivity issue", resp.ErrorMessage)
				return
			}

			var userAction status.BridgeStateUserAction = status.UserActionRelogin
			var message string

			switch resp.State {
			case steamapi.AuthStatusResponse_EXPIRED:
				message = "Stored credentials expired - please log in again"
			case steamapi.AuthStatusResponse_FAILED:
				message = "Stored credentials invalid - please log in again"
			default:
				message = "Re-authentication failed - please log in again"
			}

			sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials, message,
				withUserAction(userAction),
				withInfo(map[string]interface{}{
					"auth_state":    resp.State.String(),
					"session_type":  meta.SessionType,
					"error_message": resp.ErrorMessage,
				})))
			return
		}

		// Re-authentication successful - update metadata
		sc.br.Log.Info().Msg("Successfully re-authenticated with stored tokens")

		// Update tokens if they were refreshed
		if resp.NewAccessToken != "" {
			meta.AccessToken = resp.NewAccessToken
		}
		if resp.NewRefreshToken != "" {
			meta.RefreshToken = resp.NewRefreshToken
		}

		// Update user info if provided
		if resp.UserInfo != nil {
			meta.PersonaName = resp.UserInfo.PersonaName
			meta.ProfileURL = resp.UserInfo.ProfileUrl
			meta.AvatarHash = resp.UserInfo.AvatarHash // Use hash instead of URL
		}

		meta.IsValid = true
		meta.LastValidated = time.Now()
		sc.UserLogin.Save(ctx)
	} else {
		// No stored credentials found
		sc.br.Log.Warn().Msg("No stored credentials found for re-authentication")
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials,
			"No stored credentials - please log in",
			withUserAction(status.UserActionRelogin),
			withInfo(map[string]interface{}{
				"reason":       "missing_credentials",
				"session_type": meta.SessionType,
			})))
		return
	}

	// Verify Steam is actually logged in before reporting connected
	sc.br.Log.Info().Msg("Verifying Steam authentication state before reporting connected")
	verified, err := sc.verifysteamAuthentication(ctx)
	if err != nil {
		sc.br.Log.Err(err).Msg("Failed to verify Steam authentication state")
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateUnknownError,
			"Failed to verify Steam authentication",
			withReason(err.Error()),
			withUserAction(status.UserActionRestart)))
		return
	}

	if !verified {
		sc.br.Log.Warn().Msg("Steam authentication verification failed - not actually logged in")
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials,
			"Steam authentication incomplete - please log in again",
			withUserAction(status.UserActionRelogin),
			withInfo(map[string]interface{}{
				"verification_failed": true,
				"session_type":        meta.SessionType,
			})))
		return
	}

	sc.br.Log.Info().Msg("Steam authentication verified successfully")

	// Cancel any pending disconnect debounce
	sc.cancelDisconnectDebounce()

	// Mark as connected in state tracking
	sc.stateMutex.Lock()
	sc.isConnected = true
	sc.stateMutex.Unlock()

	// Clean up any ongoing reconnection process
	sc.reconnectionMutex.Lock()
	if sc.isReconnecting {
		if sc.reconnectionCancel != nil {
			sc.reconnectionCancel()
		}
		sc.isReconnecting = false
		sc.reconnectionCancel = nil
		sc.reconnectionAttempts = 0
	}
	sc.reconnectionMutex.Unlock()

	// Start connection monitoring
	sc.startConnectionMonitoring(ctx)

	// Initialize message stream BEFORE reporting final connected state
	// This prevents the race condition where stream setup overwrites the connected state
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting, "Establishing Steam message stream"))
	
	// Start message subscription synchronously to ensure it's established
	messageStreamReady := make(chan bool, 1)
	go func() {
		if err := sc.initializeMessageStream(ctx); err != nil {
			sc.br.Log.Error().Err(err).Msg("Failed to initialize message stream")
			sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateUnknownError,
				"Failed to establish message stream",
				withReason(err.Error()),
				withUserAction(status.UserActionRestart)))
			messageStreamReady <- false
			return
		}
		messageStreamReady <- true
	}()

	// Wait for message stream initialization
	select {
	case success := <-messageStreamReady:
		if !success {
			return
		}
	case <-time.After(10 * time.Second):
		sc.br.Log.Warn().Msg("Message stream initialization timed out")
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateUnknownError,
			"Message stream setup timed out",
			withUserAction(status.UserActionRestart)))
		return
	}

	// Now report final connected state - this won't be overwritten
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnected, "Connected to Steam"))

	// Initialize and start presence manager
	if sc.presenceManager == nil && sc.connector != nil {
		sc.presenceManager = NewPresenceManager(sc, &sc.connector.Config.Presence)
	}
	if sc.presenceManager != nil {
		sc.presenceManager.Start(ctx)
	}

	// Sync existing portals for backfill after re-authentication
	go sc.syncExistingPortals(ctx)

	// Sync all friends on startup if configured
	if sc.connector != nil && sc.connector.Config.SyncFriendsOnStartup {
		go sc.syncFriendsOnStartup(ctx)
	}

	// Start session event subscription for logout notifications
	go sc.startSessionEventSubscription(ctx)
}

// initializeMessageStream establishes the message stream without sending bridge state updates
// This prevents race conditions with the main connection state reporting
func (sc *SteamClient) initializeMessageStream(ctx context.Context) error {
	if sc.msgClient == nil {
		return fmt.Errorf("message client not available")
	}

	stream, err := sc.msgClient.SubscribeToMessages(ctx, &steamapi.MessageSubscriptionRequest{})
	if err != nil {
		return fmt.Errorf("failed to start message subscription: %w", err)
	}

	sc.br.Log.Info().Msg("Steam message stream established successfully")

	// Start the message processing goroutine
	go sc.processMessageStream(ctx, stream)
	
	return nil
}

// processMessageStream handles the actual message processing without state updates
func (sc *SteamClient) processMessageStream(ctx context.Context, stream steamapi.SteamMessagingService_SubscribeToMessagesClient) {
	defer func() {
		if r := recover(); r != nil {
			sc.br.Log.Error().Interface("panic", r).Msg("Panic in message stream processing")
		}
	}()

	messageCount := 0
	for {
		select {
		case <-ctx.Done():
			sc.br.Log.Info().Msg("Message stream context cancelled")
			return
		default:
			msgEvent, err := stream.Recv()
			if err != nil {
				sc.br.Log.Error().Err(err).Msg("Error receiving message from stream")
				// Report disconnection and attempt reconnection
				sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect, 
					"Steam message stream disconnected", withReason(err.Error())))
				
				// Trigger reconnection logic
				go sc.handleTransientDisconnect(ctx, "Steam message stream disconnected", err.Error())
				return
			}

			// Process incoming message
			sc.handleIncomingMessage(ctx, msgEvent)
			messageCount++

			// Periodically log message processing (but don't spam bridge state)
			if messageCount%100 == 0 {
				sc.br.Log.Debug().Int("message_count", messageCount).Msg("Message stream processing active")
			}
		}
	}
}

// Disconnect cleanly disconnects from Steam
func (sc *SteamClient) Disconnect() {
	sc.br.Log.Info().Msg("Disconnect() - Disconnecting from Steam")

	// Update connection state
	sc.stateMutex.Lock()
	sc.isConnected = false
	sc.isConnecting = false
	sc.stateMutex.Unlock()

	// Stop presence manager
	if sc.presenceManager != nil {
		sc.presenceManager.Stop()
	}

	// Stop connection monitoring
	sc.stopConnectionMonitoring()

	// Use debounced disconnect state following Signal bridge pattern
	sc.debouncedDisconnectState()

	// Note: gRPC connections are managed by the SteamConnector
	// Individual client disconnection doesn't close the shared connection
}

// IsLoggedIn implements bridgev2.NetworkAPI.
func (sc *SteamClient) IsLoggedIn() bool {
	sc.br.Log.Info().Msg("IsLoggedIn() - Checking if session is still valid")

	// Must confirm if network session is still valid
	// This could be as simple as checking a single variable
	meta := sc.UserLogin.Metadata.(*UserLoginMetadata)
	return meta != nil && meta.IsValid
}

// LogoutRemote implements bridgev2.NetworkAPI.
// This performs a full logout that invalidates the session server-side.
func (sc *SteamClient) LogoutRemote(ctx context.Context) {
	sc.br.Log.Info().Msg("Logging out from Steam network")

	// Stop connection monitoring
	sc.stopConnectionMonitoring()

	// Report logout initiation
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateLoggedOut, "Logging out of Steam"))

	// Invalidate credentials with remote network and logout
	if sc.authClient != nil {
		_, err := sc.authClient.Logout(ctx, &steamapi.LogoutRequest{})
		if err != nil {
			sc.br.Log.Err(err).Msg("Failed to logout from Steam")
			sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateUnknownError,
				"Failed to complete Steam logout",
				withReason(err.Error())))
			return
		}
	}

	// Mark metadata as invalid
	if meta := sc.getUserMetadata(); meta != nil {
		meta.IsValid = false
		meta.AccessToken = ""
		meta.RefreshToken = ""
		sc.UserLogin.Save(ctx)
	}

	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateLoggedOut, "Successfully logged out from Steam"))
}

// verifysteamAuthentication verifies that Steam is actually logged in and ready
func (sc *SteamClient) verifysteamAuthentication(ctx context.Context) (bool, error) {
	if sc.userClient == nil {
		return false, fmt.Errorf("user client not available")
	}

	// Call GetUserInfo to verify we can actually communicate with Steam
	// This will fail if Steam is not logged in properly
	meta := sc.getUserMetadata()
	if meta == nil {
		return false, fmt.Errorf("user metadata not available")
	}

	req := &steamapi.UserInfoRequest{
		SteamId: meta.SteamID,
	}

	resp, err := sc.userClient.GetUserInfo(ctx, req)
	if err != nil {
		sc.br.Log.Err(err).Msg("Steam user info request failed during verification")
		return false, err
	}

	if resp.UserInfo == nil {
		sc.br.Log.Warn().Msg("Steam user info is null - authentication not complete")
		return false, nil
	}

	// Verify the returned user info matches our stored metadata
	if resp.UserInfo.SteamId != meta.SteamID {
		sc.br.Log.Warn().
			Str("expected", fmt.Sprintf("%d", meta.SteamID)).
			Str("received", fmt.Sprintf("%d", resp.UserInfo.SteamId)).
			Msg("Steam ID mismatch in verification response")
		return false, fmt.Errorf("Steam ID mismatch: expected %d, got %d", meta.SteamID, resp.UserInfo.SteamId)
	}

	// Success - update metadata with latest info
	meta.PersonaName = resp.UserInfo.PersonaName
	meta.ProfileURL = resp.UserInfo.ProfileUrl
	meta.AvatarHash = resp.UserInfo.AvatarHash
	meta.LastValidated = time.Now()
	sc.UserLogin.Save(ctx)

	sc.br.Log.Info().
		Str("persona_name", resp.UserInfo.PersonaName).
		Str("steam_id", fmt.Sprintf("%d", resp.UserInfo.SteamId)).
		Msg("Steam authentication verification successful")

	return true, nil
}
