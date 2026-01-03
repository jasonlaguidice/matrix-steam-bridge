# Presence Tracking Implementation

## Overview

This document describes the implementation of **unidirectional** presence tracking between Matrix and Steam networks. The bridge automatically manages Steam presence (PersonaState) based on Matrix user activity and presence state.

### Key Principle
**Matrix drives Steam** - The Matrix user's presence and activity determines their Steam status. Steam presence is never propagated back to Matrix.

---

## Architecture

### Matrix Presence System
Matrix supports three presence states:
- `online` - User is actively present
- `unavailable` - User is away/idle
- `offline` - User is offline

Matrix servers may or may not have presence tracking enabled. When disabled, we fall back to activity-based presence.

### Steam PersonaState System
Steam uses the `PersonaState` enum with these values:
- `OFFLINE` (0) - Not logged in
- `ONLINE` (1) - Available and active
- `BUSY` (2) - Busy, do not disturb
- `AWAY` (3) - Away from computer
- `SNOOZE` (4) - Away for extended period
- `LOOKING_TO_TRADE` (5) - Looking to trade items
- `LOOKING_TO_PLAY` (6) - Looking for game
- `INVISIBLE` (7) - Appear offline to others

---

## State Mapping

### Matrix → Steam Mapping

| Matrix Event/State | Steam PersonaState | Trigger | Notes |
|-------------------|-------------------|---------|-------|
| Presence: `online` | ONLINE (1) | Matrix presence event | User actively online |
| Presence: `unavailable` | SNOOZE (4) | Matrix presence event | User marked as away (maps to SNOOZE per user request) |
| Presence: `offline` | No change | Matrix presence event | Keep current state, user may still be connected |
| Activity detected | ONLINE (1) | Sync events received | Fallback when server doesn't track presence |
| Inactivity timeout | SNOOZE (4) | 15 minutes without activity | Auto-away when no Matrix activity |
| Bridge connect/reconnect | ONLINE (1) | Connection established | Default state |
| Command: `invisible` | INVISIBLE (7) | Bot command | Manual invisible mode |
| Command: `visible` | ONLINE (1) | Bot command | Exit invisible mode |

### Rationale for SNOOZE vs AWAY

Steam has both AWAY (3) and SNOOZE (4):
- **AWAY** is typically for short-term absence (stepped away from desk)
- **SNOOZE** is for extended idle/sleep periods

Matrix's `unavailable` state and our 15-minute inactivity timeout both indicate extended absence, so SNOOZE is more semantically appropriate.

### Why No Debouncing?

Matrix presence events are already rate-limited by the homeserver (typically to once per few seconds). Additional debouncing would make the bridge feel less responsive without providing meaningful benefits. The inactivity timer provides natural throttling for the activity-based fallback.

---

## State Transition Diagram

```
┌─────────────────────── NORMAL MODE ───────────────────────┐
│                                                            │
│    ┌──────────────┐                                       │
│    │   ONLINE (1) │ ←─────────────────┐                  │
│    └──────┬───────┘                    │                  │
│           │                             │                  │
│           │  Presence: unavailable     │  Presence:       │
│           │  OR                         │  online OR       │
│           │  15min inactivity          │  Activity        │
│           │                             │                  │
│           ↓                             │                  │
│    ┌──────────────┐                    │                  │
│    │  SNOOZE (4)  │ ───────────────────┘                  │
│    └──────────────┘                                       │
│                                                            │
└────────────────────────────────────────────────────────────┘

┌──────────────────── MANUAL INVISIBLE MODE ─────────────────┐
│                                                             │
│    ┌──────────────────┐                                    │
│    │  INVISIBLE (7)   │  ← Command: invisible              │
│    │                  │    Persists until explicit change  │
│    └─────────┬────────┘                                    │
│              │                                              │
│              │  Command: visible                            │
│              │  OR Activity after command                   │
│              ↓                                              │
│    ┌──────────────┐                                        │
│    │  ONLINE (1)  │  Resume normal presence tracking       │
│    └──────────────┘                                        │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

---

## Implementation Details

### 1. Protobuf Definitions

**File: `SteamBridge/Proto/steam_bridge.proto`**

**Location:** After the `SteamMessagingService` service definition (around line 40)

**Add:**
```protobuf
// Presence Management Service
service SteamPresenceService {
  rpc SetPersonaState(SetPersonaStateRequest) returns (SetPersonaStateResponse);
}
```

**Location:** After the `SessionEvent` message definition (around line 327)

**Add:**
```protobuf
// Presence Messages
message SetPersonaStateRequest {
  PersonaState state = 1;
}

message SetPersonaStateResponse {
  bool success = 1;
  string error_message = 2;
}
```

**Rationale:** Keeping presence as a separate service follows the Single Responsibility Principle and makes the API more modular. The SetPersonaState operation is distinct from user information queries.

---

### 2. C# SteamPresenceService Implementation

**File: `SteamBridge/Services/SteamPresenceService.cs` (NEW)**

**Complete Implementation:**
```csharp
using Grpc.Core;
using Microsoft.Extensions.Logging;
using SteamBridge.Proto;
using SteamKit2;

namespace SteamBridge.Services;

public class SteamPresenceService : Proto.SteamPresenceService.SteamPresenceServiceBase
{
    private readonly ILogger<SteamPresenceService> _logger;
    private readonly SteamClientManager _steamClientManager;

    public SteamPresenceService(
        ILogger<SteamPresenceService> logger,
        SteamClientManager steamClientManager)
    {
        _logger = logger;
        _steamClientManager = steamClientManager;
    }

    public override Task<SetPersonaStateResponse> SetPersonaState(
        SetPersonaStateRequest request,
        ServerCallContext context)
    {
        try
        {
            // Validate client is logged on
            if (!_steamClientManager.IsLoggedOn)
            {
                _logger.LogWarning("Attempt to set persona state while not logged on");
                return Task.FromResult(new SetPersonaStateResponse
                {
                    Success = false,
                    ErrorMessage = "Not logged into Steam"
                });
            }

            // Validate PersonaState value
            if (!Enum.IsDefined(typeof(EPersonaState), (int)request.State))
            {
                _logger.LogWarning("Invalid PersonaState value: {State}", request.State);
                return Task.FromResult(new SetPersonaStateResponse
                {
                    Success = false,
                    ErrorMessage = $"Invalid PersonaState value: {request.State}"
                });
            }

            var steamState = (EPersonaState)request.State;

            _logger.LogInformation("Setting Steam PersonaState to: {State}", steamState);

            // Call SteamKit2 to set the persona state
            _steamClientManager.SteamFriends.SetPersonaState(steamState);

            return Task.FromResult(new SetPersonaStateResponse
            {
                Success = true,
                ErrorMessage = string.Empty
            });
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error setting persona state to {State}", request.State);
            return Task.FromResult(new SetPersonaStateResponse
            {
                Success = false,
                ErrorMessage = $"Internal error: {ex.Message}"
            });
        }
    }
}
```

**Rationale:**
- Validates logged-on state to prevent errors
- Enum validation ensures only valid PersonaState values are accepted
- Comprehensive logging for debugging
- Exception handling prevents service crashes
- Simple pass-through to SteamKit2 - no complex logic needed

**File: `SteamBridge/Program.cs`**

**Location:** After the other service registrations (around line 40-50, where AddGrpcService calls are)

**Add:**
```csharp
builder.Services.AddGrpc();
// ... existing services ...
builder.Services.AddGrpc().AddServiceOptions<SteamPresenceService>(options =>
{
    options.MaxReceiveMessageSize = 1024 * 1024; // 1MB
});

// Later in app.MapGrpcService calls:
app.MapGrpcService<SteamPresenceService>();
```

---

### 3. Go Presence Manager Implementation

**File: `pkg/connector/presence.go` (NEW)**

```go
package connector

import (
	"context"
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

	// Configuration
	enabled bool
}

// NewPresenceManager creates a new presence manager
func NewPresenceManager(client *SteamClient, config *PresenceConfig) *PresenceManager {
	timeout := time.Duration(config.InactivityTimeout) * time.Minute
	if timeout == 0 {
		timeout = 15 * time.Minute // Default
	}

	return &PresenceManager{
		client:            client,
		currentState:      steamapi.PersonaState_ONLINE,
		inactivityTimeout: timeout,
		enabled:           config.Enabled,
		lastActivity:      time.Now(),
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
		pm.client.br.Log.Debug().Msg("Matrix presence: offline → No change")
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

	// If we're snoozed due to inactivity, wake up
	if pm.currentState == steamapi.PersonaState_SNOOZE {
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

	// Call gRPC to update Steam
	resp, err := pm.client.msgClient.SetPersonaState(ctx, &steamapi.SetPersonaStateRequest{
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
			Msg("Inactivity timeout - setting Steam to SNOOZE")

		pm.setPersonaStateLocked(ctx, steamapi.PersonaState_SNOOZE)
	}
}
```

**Rationale:**
- **Thread-safe:** Uses mutex to protect state
- **Non-blocking:** Timer callbacks don't block main goroutines
- **Configurable:** Timeout and enable/disable controlled by config
- **Smart defaults:** 15-minute inactivity timeout
- **Redundancy prevention:** Only sends gRPC calls when state actually changes
- **Metadata tracking:** Persists state across bridge restarts

---

### 4. Integration with SteamClient

**File: `pkg/connector/client.go`**

**Location 1:** Add field to SteamClient struct (around line 72)

```go
type SteamClient struct {
	// ... existing fields ...

	// Presence management
	presenceManager *PresenceManager
}
```

**Location 2:** In Connect() method, after successful connection (around line 420)

```go
func (sc *SteamClient) Connect(ctx context.Context) {
	// ... existing connection logic ...

	// Initialize and start presence manager
	if sc.presenceManager == nil {
		config := sc.Main.Config.Presence // Assumes config is added
		sc.presenceManager = NewPresenceManager(sc, &config)
	}
	sc.presenceManager.Start(ctx)

	// ... rest of connect logic ...
}
```

**Location 3:** In Disconnect() method (add cleanup)

```go
func (sc *SteamClient) Disconnect() {
	// ... existing disconnect logic ...

	// Stop presence manager
	if sc.presenceManager != nil {
		sc.presenceManager.Stop()
	}
}
```

**Location 4:** Create new method to handle sync events

```go
// handleSyncEvent is called for each successful sync
func (sc *SteamClient) handleSyncEvent(ctx context.Context, resp *mautrix.RespSync) {
	// Handle presence events from sync
	if resp.Presence.Events != nil {
		for _, evt := range resp.Presence.Events {
			if content := evt.Content.AsPresence(); content != nil {
				// Only handle our own presence
				if evt.Sender == sc.UserLogin.UserMXID {
					sc.presenceManager.HandlePresenceEvent(ctx, *content)
				}
			}
		}
	}

	// Any sync activity counts as Matrix activity
	sc.presenceManager.HandleActivity(ctx)
}
```

**Location 5:** Wire into existing message/sync handling

Find where messages are processed and add activity tracking:
```go
// In your message handling function
sc.presenceManager.HandleActivity(ctx)
```

---

### 5. Configuration

**File: `pkg/connector/config.go`**

**Location:** Add to Config struct

```go
type Config struct {
	// ... existing fields ...

	Presence PresenceConfig `yaml:"presence"`
}

type PresenceConfig struct {
	// Enable presence synchronization from Matrix to Steam
	Enabled bool `yaml:"enabled"`

	// Inactivity timeout in minutes before setting Steam to SNOOZE
	// Only used when Matrix server doesn't support presence tracking
	// Set to 0 to disable activity-based presence
	InactivityTimeout int `yaml:"inactivity_timeout"`
}
```

**File: `pkg/connector/example-config.yaml`**

**Location:** Add section (near end of file)

```yaml
# Presence synchronization settings
presence:
  # Enable presence tracking from Matrix to Steam
  # When enabled, your Steam status will automatically change based on
  # your Matrix presence and activity
  enabled: true

  # Inactivity timeout in minutes before setting Steam to SNOOZE
  # This is used as a fallback when your Matrix server doesn't support
  # presence tracking. After this many minutes without Matrix activity,
  # your Steam status will change to SNOOZE (away)
  # Set to 0 to disable automatic away
  inactivity_timeout: 15
```

---

### 6. Metadata Updates

**File: `pkg/connector/metadata.go`**

**Location:** Add to UserLoginMetadata struct

```go
type UserLoginMetadata struct {
	// ... existing fields ...

	// Presence tracking state
	CurrentPersonaState steamapi.PersonaState `json:"current_persona_state"`
	ManualInvisible     bool                  `json:"manual_invisible"`
}
```

**Rationale:** Persisting state allows the bridge to remember manual invisible mode and current state across restarts.

---

### 7. Bot Commands (Optional)

**File: `pkg/connector/commands.go`** (or create if doesn't exist)

```go
package connector

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
)

func (sc *SteamConnector) registerPresenceCommands(proc *commands.Processor) {
	proc.AddHandlers(
		&commands.FullHandler{
			Func: func(ce *commands.Event) {
				client := ce.User.GetDefaultLogin().Client.(*SteamClient)
				err := client.presenceManager.SetInvisible(ce.Ctx, true)
				if err != nil {
					ce.Reply("Failed to set invisible mode: %v", err)
				} else {
					ce.Reply("Steam status set to INVISIBLE. Use `visible` to return to normal presence tracking.")
				}
			},
			Name: "invisible",
			Help: commands.HelpMeta{
				Section:     commands.HelpSectionGeneral,
				Description: "Set your Steam status to INVISIBLE (appear offline)",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) {
				client := ce.User.GetDefaultLogin().Client.(*SteamClient)
				err := client.presenceManager.SetInvisible(ce.Ctx, false)
				if err != nil {
					ce.Reply("Failed to exit invisible mode: %v", err)
				} else {
					ce.Reply("Exiting INVISIBLE mode. Your Steam status will now sync with your Matrix presence.")
				}
			},
			Name: "visible",
			Help: commands.HelpMeta{
				Section:     commands.HelpSectionGeneral,
				Description: "Exit INVISIBLE mode and resume normal presence tracking",
			},
		},
	)
}
```

**Location:** Call `registerPresenceCommands()` in the connector initialization where other commands are registered.

---

## Testing Scenarios

### Test 1: With Matrix Presence Support
1. Connect bridge → Verify Steam shows ONLINE
2. Set Matrix presence to "unavailable" → Verify Steam shows SNOOZE
3. Set Matrix presence to "online" → Verify Steam shows ONLINE
4. Send messages while "online" → Verify Steam stays ONLINE

### Test 2: Without Matrix Presence Support
1. Connect bridge → Verify Steam shows ONLINE
2. Send Matrix messages → Verify Steam stays ONLINE and timer resets
3. Wait 15 minutes idle → Verify Steam changes to SNOOZE
4. Send message → Verify Steam returns to ONLINE

### Test 3: Manual Invisible Mode
1. Send `invisible` command → Verify Steam shows INVISIBLE
2. Set Matrix presence to "online" → Verify Steam stays INVISIBLE
3. Send messages → Verify Steam stays INVISIBLE
4. Send `visible` command → Verify Steam returns to ONLINE

### Test 4: Configuration
1. Set `presence.enabled: false` → Verify Steam stays ONLINE always
2. Set `inactivity_timeout: 5` → Verify away timeout is 5 minutes
3. Set `inactivity_timeout: 0` → Verify no auto-away

### Test 5: Edge Cases
1. Bridge reconnect → Verify returns to ONLINE
2. Multiple rapid presence changes → Verify final state is correct
3. gRPC service unavailable → Verify graceful error handling

---

## Build and Deployment

### 1. Regenerate Protobuf Code
```bash
./generate-protos.sh
```

### 2. Build
```bash
./build.sh
```

### 3. Test Configuration
Add to your `config.yaml`:
```yaml
presence:
  enabled: true
  inactivity_timeout: 15
```

### 4. Restart Bridge
The presence system will activate automatically on next connection.

---

## Future Enhancements

### Rich State Mapping (Future)
Allow mapping Matrix status messages to Steam's specialized states:
- Status message contains "trade" → LOOKING_TO_TRADE
- Status message contains "play" → LOOKING_TO_PLAY
- Status message contains "busy" → BUSY

This requires more investigation into Matrix status message formats.

### Presence Notifications (Future)
Optional bot command to subscribe to specific Steam users' presence:
```
subscribe-presence <steam-user-id>
```
Bridge sends Matrix message when that user's Steam status changes.

**Rationale for deferring:** Low value-add, requires additional storage and event handling infrastructure.

---

## Troubleshooting

### Presence not updating
1. Check `presence.enabled: true` in config
2. Verify gRPC connection is healthy
3. Check logs for gRPC errors
4. Ensure logged into Steam

### Always showing SNOOZE
1. Check if in manual invisible mode (use `visible` command)
2. Verify Matrix activity is being detected (check logs)
3. Increase `inactivity_timeout`

### Can't use commands
1. Verify presence tracking is enabled
2. Check bot DM room is properly configured
3. Ensure you're logged into Steam

---

## Implementation Checklist

- [ ] Update protobuf definitions
- [ ] Implement SteamPresenceService.cs
- [ ] Register service in Program.cs
- [ ] Regenerate protobuf code
- [ ] Implement presence.go
- [ ] Update client.go integration
- [ ] Add configuration structs
- [ ] Update example-config.yaml
- [ ] Update metadata.go
- [ ] Implement bot commands
- [ ] Test all scenarios
- [ ] Update main README.md with presence info
- [ ] Update ROADMAP.md to mark presence as complete
