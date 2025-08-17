using SteamKit2;
using Microsoft.Extensions.Logging;
using System.Text.Json;
using System.Linq;

namespace SteamBridge.Services;

public class SteamUserInformationService
{
    private readonly ILogger<SteamUserInformationService> _logger;
    private readonly SteamClientManager _steamClientManager;
    private readonly HttpClient _httpClient;

    // Steam Web API key - in production, this should be from configuration
    private const string STEAM_WEB_API_KEY = ""; // Leave empty for now - will use public endpoint

    public SteamUserInformationService(
        ILogger<SteamUserInformationService> logger,
        SteamClientManager steamClientManager,
        HttpClient httpClient)
    {
        _logger = logger;
        _steamClientManager = steamClientManager;
        _httpClient = httpClient;
    }

    public async Task<UserInfo?> GetUserInfoAsync(ulong? steamId = null)
    {
        if (!_steamClientManager.IsLoggedOn)
        {
            _logger.LogWarning("Cannot get user info - not logged on to Steam");
            return null;
        }

        var steamFriends = _steamClientManager.SteamFriends;
        var targetSteamId = steamId.HasValue 
            ? new SteamID(steamId.Value) 
            : _steamClientManager.SteamClient.SteamID;

        if (targetSteamId == null)
        {
            _logger.LogError("Invalid Steam ID");
            return null;
        }

        try
        {
            var avatarHash = GetAvatarHash(targetSteamId);
            var avatarHashString = string.Empty;
            
            if (avatarHash != null && avatarHash.Length > 0)
            {
                // Check if it's a zero-filled array (SHA-1 hash should be 20 bytes, all zeros means no avatar)
                bool isAllZeros = avatarHash.All(b => b == 0);
                if (!isAllZeros)
                {
                    avatarHashString = Convert.ToHexString(avatarHash).ToLowerInvariant();
                }
                else
                {
                    _logger.LogWarning("Avatar hash is all zeros for Steam ID {SteamId}, treating as no avatar", targetSteamId);
                }
            }
            
            _logger.LogDebug("Avatar info for SteamID {SteamId}: avatarHash is null={IsNull}, avatarHashString='{AvatarHashString}'", 
                targetSteamId, avatarHash == null, avatarHashString);
            
            var userInfo = new UserInfo
            {
                SteamId = targetSteamId.ConvertToUInt64(),
                PersonaName = steamFriends.GetFriendPersonaName(targetSteamId),
                AccountName = targetSteamId.Render(),
                Status = MapPersonaState(steamFriends.GetFriendPersonaState(targetSteamId)),
                CurrentGame = steamFriends.GetFriendGamePlayedName(targetSteamId) ?? string.Empty,
                ProfileUrl = $"https://steamcommunity.com/profiles/{targetSteamId.ConvertToUInt64()}",
                AvatarUrl = GetAvatarUrl(targetSteamId),
                AvatarHash = avatarHashString
            };

            _logger.LogDebug("Retrieved user info for SteamID: {SteamID}", targetSteamId);
            return userInfo;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error retrieving user info for SteamID: {SteamID}", targetSteamId);
            return null;
        }
    }

    public async Task<List<Friend>> GetFriendsListAsync()
    {
        if (!_steamClientManager.IsLoggedOn)
        {
            _logger.LogWarning("Cannot get friends list - not logged on to Steam");
            return new List<Friend>();
        }

        var steamFriends = _steamClientManager.SteamFriends;
        var friends = new List<Friend>();

        try
        {
            var friendCount = steamFriends.GetFriendCount();
            _logger.LogInformation("Processing {FriendCount} friends", friendCount);

            for (int i = 0; i < friendCount; i++)
            {
                var friendSteamId = steamFriends.GetFriendByIndex(i);
                if (friendSteamId == null) continue;

                var relationship = steamFriends.GetFriendRelationship(friendSteamId);
                
                var avatarHash = GetAvatarHash(friendSteamId);
                var avatarHashString = string.Empty;
                
                if (avatarHash != null && avatarHash.Length > 0)
                {
                    // Check if it's a zero-filled array (SHA-1 hash should be 20 bytes, all zeros means no avatar)
                    bool isAllZeros = avatarHash.All(b => b == 0);
                    if (!isAllZeros)
                    {
                        avatarHashString = Convert.ToHexString(avatarHash).ToLowerInvariant();
                    }
                    else
                    {
                        _logger.LogWarning("Avatar hash is all zeros for friend Steam ID {SteamId}, treating as no avatar", friendSteamId);
                    }
                }
                
                var friend = new Friend
                {
                    SteamId = friendSteamId.ConvertToUInt64(),
                    PersonaName = steamFriends.GetFriendPersonaName(friendSteamId),
                    Status = MapPersonaState(steamFriends.GetFriendPersonaState(friendSteamId)),
                    CurrentGame = steamFriends.GetFriendGamePlayedName(friendSteamId) ?? string.Empty,
                    Relationship = MapFriendRelationship(relationship),
                    AvatarUrl = GetAvatarUrl(friendSteamId),
                    AvatarHash = avatarHashString
                };

                friends.Add(friend);
            }

            _logger.LogInformation("Retrieved {FriendCount} friends", friends.Count);
            return friends;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error retrieving friends list");
            return new List<Friend>();
        }
    }

    public async Task<PersonaState> GetUserStatusAsync(ulong steamId)
    {
        if (!_steamClientManager.IsLoggedOn)
        {
            _logger.LogWarning("Cannot get user status - not logged on to Steam");
            return PersonaState.Offline;
        }

        try
        {
            var targetSteamId = new SteamID(steamId);
            var steamFriends = _steamClientManager.SteamFriends;
            var status = steamFriends.GetFriendPersonaState(targetSteamId);
            
            return MapPersonaState(status);
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error retrieving user status for SteamID: {SteamID}", steamId);
            return PersonaState.Offline;
        }
    }

    public async Task<(bool Success, string? SteamId, string? ErrorMessage)> ResolveVanityUrlAsync(string vanityUrl)
    {
        _logger.LogInformation("Resolving vanity URL: {VanityUrl}", vanityUrl);

        try
        {
            // Use Steam Web API to resolve vanity URL to SteamID64
            // API endpoint: http://api.steampowered.com/ISteamUser/ResolveVanityURL/v0001/
            var apiUrl = $"http://api.steampowered.com/ISteamUser/ResolveVanityURL/v0001/?key={STEAM_WEB_API_KEY}&vanityurl={vanityUrl}";
            
            // If no API key is configured, try without key (limited functionality)
            if (string.IsNullOrEmpty(STEAM_WEB_API_KEY))
            {
                _logger.LogWarning("No Steam Web API key configured, attempting resolution without key");
                apiUrl = $"http://api.steampowered.com/ISteamUser/ResolveVanityURL/v0001/?vanityurl={vanityUrl}";
            }

            var response = await _httpClient.GetAsync(apiUrl);
            if (!response.IsSuccessStatusCode)
            {
                _logger.LogError("HTTP request failed with status: {StatusCode}", response.StatusCode);
                return (false, null, $"HTTP request failed: {response.StatusCode}");
            }

            var jsonContent = await response.Content.ReadAsStringAsync();
            _logger.LogDebug("Steam API response: {Response}", jsonContent);

            using var document = JsonDocument.Parse(jsonContent);
            var root = document.RootElement;

            if (!root.TryGetProperty("response", out var responseElement))
            {
                _logger.LogError("Invalid Steam API response format");
                return (false, null, "Invalid API response format");
            }

            if (!responseElement.TryGetProperty("success", out var successElement))
            {
                _logger.LogError("No success field in Steam API response");
                return (false, null, "Invalid API response - no success field");
            }

            var successCode = successElement.GetInt32();

            switch (successCode)
            {
                case 1: // Success
                    if (responseElement.TryGetProperty("steamid", out var steamIdElement))
                    {
                        var steamId = steamIdElement.GetString();
                        _logger.LogInformation("Successfully resolved vanity URL '{VanityUrl}' to SteamID: {SteamId}", 
                            vanityUrl, steamId);
                        return (true, steamId, null);
                    }
                    else
                    {
                        _logger.LogError("Success response but no SteamID found");
                        return (false, null, "Success response but no SteamID returned");
                    }

                case 42: // No match
                    _logger.LogInformation("Vanity URL '{VanityUrl}' not found", vanityUrl);
                    return (false, null, "Vanity URL not found");

                default:
                    _logger.LogWarning("Steam API returned unknown success code: {SuccessCode}", successCode);
                    return (false, null, $"Unknown success code: {successCode}");
            }
        }
        catch (HttpRequestException ex)
        {
            _logger.LogError(ex, "HTTP request exception while resolving vanity URL");
            return (false, null, $"Network error: {ex.Message}");
        }
        catch (JsonException ex)
        {
            _logger.LogError(ex, "JSON parsing error while processing Steam API response");
            return (false, null, $"JSON parsing error: {ex.Message}");
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Unexpected error while resolving vanity URL");
            return (false, null, $"Unexpected error: {ex.Message}");
        }
    }

    private static PersonaState MapPersonaState(EPersonaState state)
    {
        return state switch
        {
            EPersonaState.Offline => PersonaState.Offline,
            EPersonaState.Online => PersonaState.Online,
            EPersonaState.Busy => PersonaState.Busy,
            EPersonaState.Away => PersonaState.Away,
            EPersonaState.Snooze => PersonaState.Snooze,
            EPersonaState.LookingToTrade => PersonaState.LookingToTrade,
            EPersonaState.LookingToPlay => PersonaState.LookingToPlay,
            EPersonaState.Invisible => PersonaState.Invisible,
            _ => PersonaState.Offline
        };
    }

    private static FriendRelationship MapFriendRelationship(EFriendRelationship relationship)
    {
        return relationship switch
        {
            EFriendRelationship.None => FriendRelationship.None,
            EFriendRelationship.Blocked => FriendRelationship.Blocked,
            EFriendRelationship.RequestRecipient => FriendRelationship.RequestRecipient,
            EFriendRelationship.Friend => FriendRelationship.Friend,
            EFriendRelationship.RequestInitiator => FriendRelationship.RequestInitiator,
            EFriendRelationship.Ignored => FriendRelationship.Ignored,
            EFriendRelationship.IgnoredFriend => FriendRelationship.IgnoredFriend,
            _ => FriendRelationship.None
        };
    }

    private string GetAvatarUrl(SteamID steamId)
    {
        try 
        {
            var steamFriends = _steamClientManager.SteamFriends;
            var avatarHash = steamFriends.GetFriendAvatar(steamId);
            
            if (avatarHash != null && avatarHash.Length > 0)
            {
                // Convert hash bytes to hex string
                var hashString = BitConverter.ToString(avatarHash).Replace("-", "").ToLowerInvariant();
                return $"https://avatars.steamstatic.com/{hashString}_full.jpg";
            }
            
            // Fallback to default avatar pattern if hash not available
            _logger.LogDebug("No avatar hash found for Steam ID {SteamId}, using fallback", steamId);
            var steamId64 = steamId.ConvertToUInt64();
            return $"https://avatars.steamstatic.com/{steamId64}.jpg";
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "Failed to get avatar hash for Steam ID {SteamId}, using fallback", steamId);
            var steamId64 = steamId.ConvertToUInt64();
            return $"https://avatars.steamstatic.com/{steamId64}.jpg";
        }
    }

    private byte[]? GetAvatarHash(SteamID steamId)
    {
        try 
        {
            var steamFriends = _steamClientManager.SteamFriends;
            var avatarBytes = steamFriends.GetFriendAvatar(steamId);
            
            if (avatarBytes == null)
            {
                _logger.LogWarning("GetFriendAvatar returned null for Steam ID {SteamId}", steamId);
                return null;
            }
            
            if (avatarBytes.Length == 0)
            {
                _logger.LogWarning("GetFriendAvatar returned empty array for Steam ID {SteamId}", steamId);
                return null;
            }
            
            _logger.LogInformation("GetFriendAvatar returned {Length} bytes for Steam ID {SteamId}: {AvatarHashHex}", 
                avatarBytes.Length, steamId, Convert.ToHexString(avatarBytes));
            return avatarBytes;
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "Failed to get avatar hash for Steam ID {SteamId}", steamId);
            return null;
        }
    }
}

public class Friend
{
    public ulong SteamId { get; set; }
    public string PersonaName { get; set; } = string.Empty;
    public string AvatarUrl { get; set; } = string.Empty;
    public string AvatarHash { get; set; } = string.Empty;
    public PersonaState Status { get; set; }
    public string CurrentGame { get; set; } = string.Empty;
    public FriendRelationship Relationship { get; set; }
}

public enum FriendRelationship
{
    None,
    Blocked,
    RequestRecipient,
    Friend,
    RequestInitiator,
    Ignored,
    IgnoredFriend
}