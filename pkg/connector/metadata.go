package connector

import (
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

// PortalMetadata contains Steam-specific metadata for portals (DMs and group chats)
type PortalMetadata struct {
	// Nothing required
}

// GhostMetadata contains Steam user information for ghost users
// This stores persistent, network-specific data that is expensive to fetch
// and can be cached between bridge restarts.
type GhostMetadata struct {
	// Core identifiers (persistent, rarely change)
	SteamID uint64 `json:"steam_id,omitempty"`
	
	// Cached user profile data (for performance optimization)
	AccountName  string `json:"account_name,omitempty"`  // Steam login name (rarely changes)
	PersonaName  string `json:"persona_name,omitempty"`  // Display name (changes frequently)
	ProfileURL   string `json:"profile_url,omitempty"`   // Steam community profile URL
	AvatarHash   string `json:"avatar_hash,omitempty"`   // Avatar hash for change detection
	
	// Relationship context (affects portal behavior)
	Relationship string `json:"relationship,omitempty"` // friend, blocked, etc.
	
	// Cache invalidation tracking
	LastProfileUpdate time.Time `json:"last_profile_update"`
	LastAvatarUpdate  time.Time `json:"last_avatar_update"`
	
	// NOTE: Status and CurrentGame should NOT be stored here as they're
	// real-time presence data that should be fetched fresh via GetUserInfo()
}

// MessageMetadata contains Steam-specific message metadata that needs to persist
// in the database for proper message processing and bridge functionality.
// This should only include data that:
// 1. Affects message processing logic (replies, edits, reactions)
// 2. Provides Steam-specific features not available in Matrix
// 3. Helps prevent message loops or handle special cases
type MessageMetadata struct {
	// Core message context - affects bridge behavior
	SteamMessageType string `json:"steam_message_type,omitempty"` // CHAT_MESSAGE, TYPING, EMOTE, INVITE_GAME
	IsEcho           bool   `json:"is_echo,omitempty"`            // Echo from another client (prevents loops)
	
	// Game invite specific data (Steam-specific feature not available in Matrix)
	GameInviteID string `json:"game_invite_id,omitempty"` // Steam game invite identifier
	GameName     string `json:"game_name,omitempty"`      // Human-readable game name
	GameAppID    uint32 `json:"game_app_id,omitempty"`    // Steam App ID for the game
	
	// Content type flags for processing
	ContainsRichContent bool `json:"contains_rich_content,omitempty"` // Images, stickers, special formatting
	
	// Image data for Matrix→Steam messages
	ImageURL string `json:"image_url,omitempty"` // Steam UGC or data URL for images
	
	// Future extension points
	// EditTimestamp int64 `json:"edit_timestamp,omitempty"` // For when Steam adds message editing
}

// UserLoginMetadata contains Steam authentication and session data
type UserLoginMetadata struct {
	// Basic Steam user information
	SteamID     uint64                `json:"steam_id,omitempty"`
	Username    string                `json:"username,omitempty"`     // SteamID format for bridge identification
	AccountName string                `json:"account_name,omitempty"` // Steam account name for authentication
	PersonaName string                `json:"persona_name,omitempty"`
	ProfileURL  string                `json:"profile_url,omitempty"`
	AvatarHash  string                `json:"avatar_hash,omitempty"`
	RemoteID    networkid.UserLoginID `json:"remote_id,omitempty"`

	// Session persistence data (encrypted in database)
	AccessToken      string    `json:"access_token,omitempty"`      // Steam access token for API calls
	RefreshToken     string    `json:"refresh_token,omitempty"`     // Steam refresh token for renewal
	GuardData        string    `json:"guard_data,omitempty"`        // Steam Guard machine auth data
	SessionTimestamp int64     `json:"session_timestamp,omitempty"` // Unix timestamp of session creation
	ExpiresAt        time.Time `json:"expires_at"`                  // When session expires  
	LastValidated    time.Time `json:"last_validated"`              // Last successful validation

	// Session state tracking
	SessionType     string    `json:"session_type,omitempty"`      // "password", "qr", "refresh"
	IsValid         bool      `json:"is_valid,omitempty"`          // Cache validity state
	MachineAuthHash string    `json:"machine_auth_hash,omitempty"` // SHA-1 hash for machine authentication
	RecentlyCreated time.Time `json:"recently_created"`            // Track fresh sessions to avoid immediate validation
}