package connector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.shadowdrake.org/steam/pkg/steamapi"
	"maunium.net/go/mautrix/event"
)

// PresenceManager handles Matrix→Steam presence synchronization
type PresenceManager struct {
	client *SteamClient
	mu     sync.RWMutex

	// Current state tracking
	currentState    steamapi.PersonaState
	manualInvisible bool

	// Inactivity detection
	lastActivity      time.Time
	inactivityTimer   *time.Timer
	inactivityTimeout time.Duration
	inactivityState   steamapi.PersonaState // State to set after timeout

	// Configuration
	enabled                   bool
	typingResetsPresence      bool
	readReceiptsResetPresence bool
}

// NewPresenceManager creates a new presence manager
func NewPresenceManager(client *SteamClient, config *PresenceConfig) *PresenceManager {
	timeout := time.Duration(config.InactivityTimeout) * time.Minute
	if timeout == 0 {
		timeout = 15 * time.Minute // Default
	}

	// Parse inactivity status (default to SNOOZE if not specified or invalid)
	inactivityState := steamapi.PersonaState_SNOOZE
	if config.InactivityStatus == "invisible" {
		inactivityState = steamapi.PersonaState_INVISIBLE
	}

	// Set defaults for new boolean options if not explicitly set
	typingResets := config.TypingResetsPresence
	if config.TypingResetsPresence == false && config.InactivityTimeout > 0 {
		// Default to true if inactivity tracking is enabled
		typingResets = true
	}

	return &PresenceManager{
		client:                    client,
		currentState:              steamapi.PersonaState_ONLINE,
		inactivityTimeout:         timeout,
		inactivityState:           inactivityState,
		enabled:                   config.Enabled,
		typingResetsPresence:      typingResets,
		readReceiptsResetPresence: config.ReadReceiptsResetPresence,
		lastActivity:              time.Now(),
	}
}

// Start begins presence tracking
func (pm *PresenceManager) Start(ctx context.Context) {
	if !pm.enabled {
		pm.client.br.Log.Info().Msg("Presence tracking disabled")
		return
	}

	pm.client.br.Log.Info().Msg("Starting presence tracking")

	// Set initial state to ONLINE
	pm.setPersonaState(ctx, steamapi.PersonaState_ONLINE)

	// Start inactivity timer
	pm.resetInactivityTimer(ctx)
}

// Stop stops presence tracking
func (pm *PresenceManager) Stop() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.inactivityTimer != nil {
		pm.inactivityTimer.Stop()
		pm.inactivityTimer = nil
	}
}

// HandlePresenceEvent processes Matrix presence events
func (pm *PresenceManager) HandlePresenceEvent(ctx context.Context, presence event.PresenceEventContent) {
	if !pm.enabled {
		return
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Don't change state if manually invisible
	if pm.manualInvisible {
		pm.client.br.Log.Debug().Msg("Ignoring presence event - manual invisible mode active")
		return
	}

	// Map Matrix presence to Steam PersonaState
	var targetState steamapi.PersonaState
	switch presence.Presence {
	case event.PresenceOnline:
		targetState = steamapi.PersonaState_ONLINE
		pm.resetInactivityTimerLocked(ctx)
		pm.client.br.Log.Debug().Msg("Matrix presence: online → Steam ONLINE")

	case event.PresenceUnavailable:
		targetState = steamapi.PersonaState_SNOOZE
		pm.stopInactivityTimerLocked()
		pm.client.br.Log.Debug().Msg("Matrix presence: unavailable → Steam SNOOZE")

	case event.PresenceOffline:
		// Don't change state for offline - user may still be connected via other clients
		pm.client.br.Log.Debug().Msg("Matrix presence: offline → Steam no change")
		return

	default:
		pm.client.br.Log.Warn().Str("presence", string(presence.Presence)).Msg("Unknown Matrix presence state")
		return
	}

	pm.setPersonaStateLocked(ctx, targetState)
}

// HandleActivity is called on any Matrix activity (messages, sync events)
func (pm *PresenceManager) HandleActivity(ctx context.Context) {
	if !pm.enabled {
		return
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Don't change state if manually invisible
	if pm.manualInvisible {
		return
	}

	pm.lastActivity = time.Now()

	// If we're in the inactivity state, wake up
	if pm.currentState == pm.inactivityState {
		pm.setPersonaStateLocked(ctx, steamapi.PersonaState_ONLINE)
	}

	// Reset inactivity timer
	pm.resetInactivityTimerLocked(ctx)
}

// SetInvisible sets manual invisible mode
func (pm *PresenceManager) SetInvisible(ctx context.Context, invisible bool) error {
	if !pm.enabled {
		return fmt.Errorf("presence tracking is disabled")
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.manualInvisible = invisible

	if invisible {
		pm.client.br.Log.Info().Msg("Setting manual invisible mode")
		pm.stopInactivityTimerLocked()
		pm.setPersonaStateLocked(ctx, steamapi.PersonaState_INVISIBLE)
	} else {
		pm.client.br.Log.Info().Msg("Exiting manual invisible mode")
		pm.setPersonaStateLocked(ctx, steamapi.PersonaState_ONLINE)
		pm.resetInactivityTimerLocked(ctx)
	}

	// Store in metadata
	if meta := pm.client.getUserMetadata(); meta != nil {
		meta.ManualInvisible = invisible
	}

	return nil
}

// setPersonaState sets the Steam persona state (internal, not locked)
func (pm *PresenceManager) setPersonaState(ctx context.Context, state steamapi.PersonaState) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.setPersonaStateLocked(ctx, state)
}

// setPersonaStateLocked sets the Steam persona state (must be called with lock held)
func (pm *PresenceManager) setPersonaStateLocked(ctx context.Context, state steamapi.PersonaState) {
	// Don't send redundant updates
	if pm.currentState == state {
		return
	}

	pm.client.br.Log.Info().
		Str("old_state", pm.currentState.String()).
		Str("new_state", state.String()).
		Msg("Changing Steam PersonaState")

	// Ensure we have a presence client
	if pm.client.presenceClient == nil {
		pm.client.br.Log.Error().Msg("Presence client not initialized")
		return
	}

	// Call gRPC to update Steam
	resp, err := pm.client.presenceClient.SetPersonaState(ctx, &steamapi.SetPersonaStateRequest{
		State: state,
	})

	if err != nil {
		pm.client.br.Log.Error().Err(err).Msg("Failed to set Steam PersonaState")
		return
	}

	if !resp.Success {
		pm.client.br.Log.Error().
			Str("error", resp.ErrorMessage).
			Msg("Steam rejected PersonaState change")
		return
	}

	pm.currentState = state

	// Update metadata
	if meta := pm.client.getUserMetadata(); meta != nil {
		meta.CurrentPersonaState = state
	}
}

// resetInactivityTimer resets the inactivity timer
func (pm *PresenceManager) resetInactivityTimer(ctx context.Context) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.resetInactivityTimerLocked(ctx)
}

// resetInactivityTimerLocked resets the inactivity timer (must be called with lock held)
func (pm *PresenceManager) resetInactivityTimerLocked(ctx context.Context) {
	if pm.inactivityTimer != nil {
		pm.inactivityTimer.Stop()
	}

	if pm.inactivityTimeout > 0 {
		pm.inactivityTimer = time.AfterFunc(pm.inactivityTimeout, func() {
			pm.onInactivityTimeout(ctx)
		})
	}
}

// stopInactivityTimerLocked stops the inactivity timer (must be called with lock held)
func (pm *PresenceManager) stopInactivityTimerLocked() {
	if pm.inactivityTimer != nil {
		pm.inactivityTimer.Stop()
		pm.inactivityTimer = nil
	}
}

// onInactivityTimeout is called when the inactivity timer expires
func (pm *PresenceManager) onInactivityTimeout(ctx context.Context) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Don't change if manually invisible
	if pm.manualInvisible {
		return
	}

	// Check if we're still inactive
	if time.Since(pm.lastActivity) >= pm.inactivityTimeout {
		pm.client.br.Log.Info().
			Dur("inactive_duration", time.Since(pm.lastActivity)).
			Str("target_state", pm.inactivityState.String()).
			Msg("Inactivity timeout - changing Steam status")

		pm.setPersonaStateLocked(ctx, pm.inactivityState)
	}
}
