package connector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

// parseIdentifier parses various Steam identifier formats and returns the resolved SteamID64 and type
func parseIdentifier(identifier string) (steamID64 string, identifierType string, err error) {
	// Remove whitespace and normalize
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", "", fmt.Errorf("empty identifier")
	}

	// Check if direct SteamID64 (17 digits)
	if len(identifier) == 17 && isNumeric(identifier) {
		// Validate SteamID64 range (Steam IDs start from 76561197960265728)
		if steamID, err := strconv.ParseUint(identifier, 10, 64); err == nil {
			if steamID >= 76561197960265728 { // Minimum valid SteamID64
				return identifier, "steamid64", nil
			}
		}
	}

	// Parse Steam community URLs
	if strings.Contains(identifier, "steamcommunity.com") {
		return parseFromURL(identifier)
	}

	// Assume vanity username if it passes basic validation
	if isValidVanityUsername(identifier) {
		return identifier, "vanity", nil
	}

	return "", "", fmt.Errorf("invalid Steam identifier format: %s", identifier)
}

// parseFromURL extracts identifier from Steam community URLs
func parseFromURL(url string) (steamID64 string, identifierType string, err error) {
	// Handle various URL formats:
	// https://steamcommunity.com/id/username
	// https://steamcommunity.com/profiles/76561198123456789
	// steamcommunity.com/id/username
	// steamcommunity.com/profiles/76561198123456789

	// Remove protocol if present
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")

	// Extract path components
	if strings.HasPrefix(url, "steamcommunity.com/profiles/") {
		steamID := strings.TrimPrefix(url, "steamcommunity.com/profiles/")
		// Remove trailing slash and any query parameters
		if idx := strings.IndexAny(steamID, "/?"); idx != -1 {
			steamID = steamID[:idx]
		}

		// Validate it's a 17-digit SteamID64
		if len(steamID) == 17 && isNumeric(steamID) {
			if id, err := strconv.ParseUint(steamID, 10, 64); err == nil && id >= 76561197960265728 {
				return steamID, "steamid64", nil
			}
		}
		return "", "", fmt.Errorf("invalid SteamID64 in profile URL: %s", steamID)
	}

	if strings.HasPrefix(url, "steamcommunity.com/id/") {
		vanityURL := strings.TrimPrefix(url, "steamcommunity.com/id/")
		// Remove trailing slash and any query parameters
		if idx := strings.IndexAny(vanityURL, "/?"); idx != -1 {
			vanityURL = vanityURL[:idx]
		}

		if isValidVanityUsername(vanityURL) {
			return vanityURL, "vanity", nil
		}
		return "", "", fmt.Errorf("invalid vanity username in URL: %s", vanityURL)
	}

	return "", "", fmt.Errorf("unsupported Steam URL format: %s", url)
}

// isValidVanityUsername validates a Steam vanity username
func isValidVanityUsername(username string) bool {
	// Steam vanity URLs can contain letters, numbers, underscores, and hyphens
	// Must be 3-32 characters long
	if len(username) < 3 || len(username) > 32 {
		return false
	}

	// Check for invalid characters
	for _, char := range username {
		if !((char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '_' || char == '-') {
			return false
		}
	}

	// Must contain at least one letter (not just numbers/symbols)
	hasLetter := false
	for _, char := range username {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') {
			hasLetter = true
			break
		}
	}

	if !hasLetter {
		return false
	}

	// Check for reserved/invalid words
	lower := strings.ToLower(username)
	reserved := []string{
		"invalid", "admin", "administrator", "root", "steam", "valve",
		"support", "help", "moderator", "mod", "system", "api", "www",
		"ftp", "mail", "email", "test", "demo", "guest", "user", "null",
	}

	for _, word := range reserved {
		if lower == word {
			return false
		}
	}

	return true
}

// isNumeric checks if a string contains only digits
func isNumeric(s string) bool {
	for _, char := range s {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// validateSteamID64 validates a SteamID64 format and range
func validateSteamID64(steamID string) bool {
	if len(steamID) != 17 || !isNumeric(steamID) {
		return false
	}

	id, err := strconv.ParseUint(steamID, 10, 64)
	return err == nil && id >= 76561197960265728 // Minimum valid SteamID64
}

// extractAvatarHash extracts a hash from Steam avatar URL for change detection
func extractAvatarHash(avatarURL string) string {
	// Steam avatar URLs typically look like:
	// https://avatars.steamstatic.com/b5bd56c1aa4644a474a2e4972be27ef9e82e517e_full.jpg
	// Extract the hash part for change detection
	if idx := strings.LastIndex(avatarURL, "/"); idx != -1 {
		filename := avatarURL[idx+1:]
		if dotIdx := strings.LastIndex(filename, "."); dotIdx != -1 {
			return filename[:dotIdx]
		}
		return filename
	}
	return avatarURL // Fallback to full URL if parsing fails
}

// downloadImageFromURL downloads an image from the given URL and returns the image data
func (sc *SteamClient) downloadImageFromURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %s: %w", url, err)
	}

	// Set user agent to identify as Steam bridge
	req.Header.Set("User-Agent", "Steam-Bridge/1.0")

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download image from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download image from %s: HTTP %d", url, resp.StatusCode)
	}

	// Limit response size to prevent abuse (10MB max)
	const maxImageSize = 10 * 1024 * 1024
	limitedReader := io.LimitReader(resp.Body, maxImageSize)

	imageData, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read image data from %s: %w", url, err)
	}

	return imageData, nil
}

// createAvatarDownloader creates a function that downloads avatar data for the given Steam ID
func (sc *SteamClient) createAvatarDownloader(steamID uint64) func(ctx context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		// Get the ghost metadata to access the avatar hash
		userID := makeUserID(steamID)
		ghost, err := sc.UserLogin.Bridge.GetGhostByID(ctx, userID)
		if err != nil {
			return nil, fmt.Errorf("failed to get ghost for Steam ID %d: %w", steamID, err)
		}

		ghostMeta, ok := ghost.Metadata.(*GhostMetadata)
		if !ok || ghostMeta == nil || ghostMeta.AvatarHash == "" {
			return nil, fmt.Errorf("no avatar hash available for Steam ID %d", steamID)
		}

		// Build Steam CDN URL from hash: https://avatars.steamstatic.com/{hash}_full.jpg
		avatarURL := fmt.Sprintf("https://avatars.steamstatic.com/%s_full.jpg", ghostMeta.AvatarHash)

		// Download the avatar from Steam CDN
		return sc.downloadImageFromURL(ctx, avatarURL)
	}
}

// resolveVanityURL resolves a Steam vanity URL to SteamID64 via gRPC
func (sc *SteamClient) resolveVanityURL(ctx context.Context, vanityURL string) (string, error) {
	if sc.userClient == nil {
		return "", fmt.Errorf("user client not initialized")
	}

	sc.br.Log.Debug().Str("vanity_url", vanityURL).Msg("Resolving vanity URL via gRPC")

	resp, err := sc.userClient.ResolveVanityURL(ctx, &steamapi.ResolveVanityURLRequest{
		VanityUrl: vanityURL,
	})
	if err != nil {
		return "", fmt.Errorf("gRPC call to ResolveVanityURL failed: %w", err)
	}

	if !resp.Success {
		// Handle specific error cases
		errorMsg := resp.ErrorMessage
		if errorMsg == "" {
			errorMsg = "unknown error"
		}

		// Check for common error patterns
		if strings.Contains(strings.ToLower(errorMsg), "not found") {
			return "", fmt.Errorf("Steam user with vanity URL '%s' not found", vanityURL)
		}
		if strings.Contains(strings.ToLower(errorMsg), "private") {
			return "", fmt.Errorf("Steam user '%s' profile is private", vanityURL)
		}

		return "", fmt.Errorf("failed to resolve vanity URL '%s': %s", vanityURL, errorMsg)
	}

	// Validate the returned SteamID64
	steamID := resp.SteamId
	if !validateSteamID64(steamID) {
		return "", fmt.Errorf("invalid SteamID64 returned from vanity URL resolution: %s", steamID)
	}

	sc.br.Log.Debug().
		Str("vanity_url", vanityURL).
		Str("resolved_steam_id", steamID).
		Msg("Successfully resolved vanity URL")

	return steamID, nil
}

// resolveToSteamID64 resolves a parsed identifier to a SteamID64 string
func (sc *SteamClient) resolveToSteamID64(ctx context.Context, identifier, identifierType string) (string, error) {
	switch identifierType {
	case "steamid64":
		// Already a SteamID64, just validate it
		if !validateSteamID64(identifier) {
			return "", fmt.Errorf("invalid SteamID64: %s", identifier)
		}
		return identifier, nil

	case "vanity":
		// Need to resolve vanity URL via gRPC
		return sc.resolveVanityURL(ctx, identifier)

	default:
		return "", fmt.Errorf("unsupported identifier type: %s", identifierType)
	}
}

// IsThisUser implements bridgev2.NetworkAPI.
func (sc *SteamClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	sc.br.Log.Info().Msg("IsThisUser() - Retrieving user info")

	// Return Steam ID as it will be unique for each user
	meta := sc.UserLogin.Metadata.(*UserLoginMetadata)
	if meta == nil {
		return false
	}

	return makeUserID(meta.SteamID) == userID
}

// GetChatInfo implements bridgev2.NetworkAPI.
func (sc *SteamClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	sc.br.Log.Info().Msg("GetChatInfo() - Retrieving chat info")

	// Below only handles DMs
	// TODO: Look up how multi-participant rooms are handled in Signal
	meta := sc.UserLogin.Metadata.(*UserLoginMetadata)
	if meta == nil {
		return nil, fmt.Errorf("no user metadata found")
	}

	return &bridgev2.ChatInfo{
		Members: &bridgev2.ChatMemberList{
			IsFull: true,
			Members: []bridgev2.ChatMember{
				{
					EventSender: bridgev2.EventSender{
						IsFromMe: true,
						Sender:   makeUserID(meta.SteamID),
					},
					Membership: event.MembershipJoin,
					PowerLevel: ptr.Ptr(50),
				},
				{
					EventSender: bridgev2.EventSender{
						Sender: networkid.UserID(portal.ID),
					},
					Membership: event.MembershipJoin,
					PowerLevel: ptr.Ptr(50),
				},
			},
		},
	}, nil
}

// GetUserInfo implements bridgev2.NetworkAPI.
func (sc *SteamClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	sc.br.Log.Info().Str("ghost_id", string(ghost.ID)).Msg("GetUserInfo() - Retrieving user info")

	// Parse Steam ID from ghost ID (now just numeric)
	steamID, err := strconv.ParseUint(string(ghost.ID), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Steam ID from ghost ID %s: %w", ghost.ID, err)
	}

	// Get or initialize ghost metadata
	ghostMeta := ghost.Metadata.(*GhostMetadata)
	if ghostMeta == nil {
		ghostMeta = &GhostMetadata{SteamID: steamID}
		ghost.Metadata = ghostMeta
	}

	// Check if we need to refresh profile data (cache for 1 hour)
	shouldRefreshProfile := ghostMeta.LastProfileUpdate.IsZero() ||
		time.Since(ghostMeta.LastProfileUpdate) > time.Hour ||
		ghostMeta.PersonaName == "" // Force refresh if missing essential data

	// Always fetch current user info for real-time data (presence, game status)
	resp, err := sc.userClient.GetUserInfo(ctx, &steamapi.UserInfoRequest{
		SteamId: steamID,
	})
	if err != nil {
		// On API error, fall back to cached data if available
		if ghostMeta.PersonaName != "" {
			sc.br.Log.Warn().Err(err).Msg("Failed to fetch fresh user info, using cached data")
			userInfo := &bridgev2.UserInfo{
				Identifiers: []string{fmt.Sprintf("%d", ghostMeta.SteamID)},
				Name:        ptr.Ptr(ghostMeta.PersonaName),
				IsBot:       ptr.Ptr(false),
			}

			// Add cached avatar if available
			if ghostMeta.AvatarHash != "" {
				userInfo.Avatar = &bridgev2.Avatar{
					ID:  networkid.AvatarID(ghostMeta.AvatarHash),
					Get: sc.createAvatarDownloader(steamID),
				}
			}

			return userInfo, nil
		}
		return nil, fmt.Errorf("failed to get user info for Steam ID %d: %w", steamID, err)
	}

	if resp.UserInfo == nil {
		return nil, fmt.Errorf("no user info returned for Steam ID %d", steamID)
	}

	userInfo := resp.UserInfo

	// Update cached profile data if needed
	if shouldRefreshProfile {
		ghostMeta.SteamID = userInfo.SteamId
		ghostMeta.AccountName = userInfo.AccountName
		ghostMeta.PersonaName = userInfo.PersonaName
		ghostMeta.ProfileURL = userInfo.ProfileUrl
		ghostMeta.LastProfileUpdate = time.Now()

		// Update relationship status if available
		// This would come from a friendship API call if implemented
		// ghostMeta.Relationship = resp.RelationshipStatus
	}

	// Check for avatar changes (hash comparison for efficiency)
	if userInfo.AvatarHash != "" {
		// Use the avatar hash from Steam API for change detection
		if userInfo.AvatarHash != ghostMeta.AvatarHash {
			ghostMeta.AvatarHash = userInfo.AvatarHash
			ghostMeta.LastAvatarUpdate = time.Now()
		}
	}

	// Create UserInfo with current data (NOT cached status/game data)
	userInfoResult := &bridgev2.UserInfo{
		Identifiers: []string{fmt.Sprintf("%d", userInfo.SteamId)},
		Name:        ptr.Ptr(userInfo.PersonaName),
		IsBot:       ptr.Ptr(false),
	}

	// Handle avatar if present
	if userInfo.AvatarHash != "" {
		userInfoResult.Avatar = &bridgev2.Avatar{
			ID:  networkid.AvatarID(userInfo.AvatarHash), // Use hash as stable ID
			Get: sc.createAvatarDownloader(userInfo.SteamId),
		}
	}

	// NOTE: We do NOT include Status or CurrentGame in UserInfo as these are
	// presence data that should be handled through presence events, not user profile

	return userInfoResult, nil
}

// ResolveIdentifier implements bridgev2.IdentifierResolvingNetworkAPI.
func (sc *SteamClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	sc.br.Log.Info().Str("identifier", identifier).Bool("create_chat", createChat).Msg("ResolveIdentifier() - Resolving Steam user identifier")

	// Parse and validate the identifier
	parsedID, identifierType, err := parseIdentifier(identifier)
	if err != nil {
		return nil, fmt.Errorf("failed to parse identifier '%s': %w", identifier, err)
	}

	// Resolve the identifier to a SteamID64
	steamID64, err := sc.resolveToSteamID64(ctx, parsedID, identifierType)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve identifier '%s': %w", identifier, err)
	}

	// Convert SteamID64 string to uint64 for network ID creation
	steamIDUint, err := strconv.ParseUint(steamID64, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse resolved SteamID64 '%s': %w", steamID64, err)
	}

	// Create network IDs
	userID := makeUserID(steamIDUint)
	portalID := networkid.PortalKey{
		ID:       makePortalID(steamIDUint),
		Receiver: sc.UserLogin.ID,
	}

	// Get or create ghost object
	ghost, err := sc.UserLogin.Bridge.GetGhostByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get ghost for user %s: %w", userID, err)
	}

	// Get or create portal object (only if createChat is true)
	var portal *bridgev2.Portal
	if createChat {
		portal, err = sc.UserLogin.Bridge.GetPortalByKey(ctx, portalID)
		if err != nil {
			return nil, fmt.Errorf("failed to get portal for key %v: %w", portalID, err)
		}
	}

	// Get user info for the ghost
	userInfo, err := sc.GetUserInfo(ctx, ghost)
	if err != nil {
		// If we can't get user info, the user might not exist or be private
		return nil, fmt.Errorf("failed to get user info for Steam user %s: %w", steamID64, err)
	}

	// Build the response
	response := &bridgev2.ResolveIdentifierResponse{
		Ghost:    ghost,
		UserID:   userID,
		UserInfo: userInfo,
	}

	// Add chat info if createChat is true and we have a portal
	if createChat && portal != nil {
		chatInfo, err := sc.GetChatInfo(ctx, portal)
		if err != nil {
			sc.br.Log.Warn().Err(err).Msg("Failed to get chat info, continuing without it")
			// Don't fail the whole request for chat info issues
			chatInfo = &bridgev2.ChatInfo{} // Provide empty chat info as fallback
		}

		response.Chat = &bridgev2.CreateChatResponse{
			Portal:     portal,
			PortalKey:  portalID,
			PortalInfo: chatInfo,
		}
	}

	sc.br.Log.Info().
		Str("resolved_steam_id", steamID64).
		Str("user_id", string(userID)).
		Bool("chat_created", response.Chat != nil).
		Msg("Successfully resolved Steam identifier")

	return response, nil
}

// SearchUsers implements bridgev2.UserSearchingNetworkAPI.
func (sc *SteamClient) SearchUsers(ctx context.Context, query string) ([]*bridgev2.ResolveIdentifierResponse, error) {
	sc.br.Log.Info().Str("query", query).Msg("SearchUsers() - Searching for Steam users in friends list")

	// Validate search query
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty search query")
	}

	if len(query) < 2 {
		return nil, fmt.Errorf("search query must be at least 2 characters long")
	}

	if len(query) > 100 {
		return nil, fmt.Errorf("search query too long (max 100 characters)")
	}

	// Use the gRPC UserService to get friends list and filter by query
	if sc.userClient == nil {
		return nil, fmt.Errorf("user client not initialized")
	}

	sc.br.Log.Debug().Str("query", query).Msg("Fetching friends list for user search")

	// Get the user's friends list
	resp, err := sc.userClient.GetFriendsList(ctx, &steamapi.FriendsListRequest{})
	if err != nil {
		return nil, fmt.Errorf("gRPC call to GetFriendsList failed: %w", err)
	}

	if resp.Friends == nil {
		sc.br.Log.Info().Msg("No friends found")
		return []*bridgev2.ResolveIdentifierResponse{}, nil
	}

	// Filter friends based on query (case-insensitive partial matching)
	queryLower := strings.ToLower(query)
	var matchingFriends []*steamapi.Friend

	for _, friend := range resp.Friends {
		// Check if the query matches the persona name
		if strings.Contains(strings.ToLower(friend.PersonaName), queryLower) {
			matchingFriends = append(matchingFriends, friend)
		}
		// Also check if query matches SteamID64 string representation
		steamIDStr := fmt.Sprintf("%d", friend.SteamId)
		if strings.Contains(steamIDStr, query) {
			matchingFriends = append(matchingFriends, friend)
		}
	}

	// Limit results to reasonable number for UI
	maxResults := 10
	if len(matchingFriends) > maxResults {
		matchingFriends = matchingFriends[:maxResults]
	}

	// Convert matching friends to ResolveIdentifierResponse format
	results := make([]*bridgev2.ResolveIdentifierResponse, 0, len(matchingFriends))

	for _, friend := range matchingFriends {
		// Convert to our network IDs
		steamIDUint := friend.SteamId
		userID := makeUserID(steamIDUint)

		// Get or create ghost object
		ghost, err := sc.UserLogin.Bridge.GetGhostByID(ctx, userID)
		if err != nil {
			sc.br.Log.Warn().Err(err).Uint64("steam_id", steamIDUint).Msg("Failed to get ghost for search result, skipping")
			continue
		}

		// Create user info from friend data
		userInfo := &bridgev2.UserInfo{
			Identifiers: []string{fmt.Sprintf("%d", steamIDUint)},
			Name:        &friend.PersonaName,
			IsBot:       ptr.Ptr(false),
		}

		// Handle avatar if present
		if friend.AvatarHash != "" {
			userInfo.Avatar = &bridgev2.Avatar{
				ID:  networkid.AvatarID(friend.AvatarHash), // Use hash as stable ID
				Get: sc.createAvatarDownloader(friend.SteamId),
			}
		}

		// Update ghost metadata with friend data
		if ghostMeta, ok := ghost.Metadata.(*GhostMetadata); ok && ghostMeta != nil {
			ghostMeta.SteamID = steamIDUint
			ghostMeta.PersonaName = friend.PersonaName
			ghostMeta.AvatarHash = friend.AvatarHash // Use hash from API
			ghostMeta.LastProfileUpdate = time.Now()
			// Store relationship info if available
			ghostMeta.Relationship = friend.Relationship.String()
		} else {
			// Initialize metadata if it doesn't exist
			ghost.Metadata = &GhostMetadata{
				SteamID:           steamIDUint,
				PersonaName:       friend.PersonaName,
				AvatarHash:        friend.AvatarHash, // Use hash from API
				LastProfileUpdate: time.Now(),
				Relationship:      friend.Relationship.String(),
			}
		}

		results = append(results, &bridgev2.ResolveIdentifierResponse{
			Ghost:    ghost,
			UserID:   userID,
			UserInfo: userInfo,
			// Note: We don't create chats in search results, only when explicitly requested
			Chat: nil,
		})
	}

	sc.br.Log.Info().
		Str("query", query).
		Int("total_friends", len(resp.Friends)).
		Int("matching_results", len(results)).
		Msg("Friends list search completed successfully")

	return results, nil
}