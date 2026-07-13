using System.Collections.Concurrent;
using System.Linq;
using Microsoft.Extensions.Logging;
using SteamKit2;

namespace SteamBridge.Services;

/// <summary>
/// Resolves a Steam AppID to its human-readable app name via PICS product info
/// (SteamApps.PICSGetProductInfo, "common"/"name"). Used as a fallback for friends whose
/// persona-state packets don't carry a game name at all (SteamFriends' friend.game_name
/// field and its cache, GetFriendGamePlayedName(), have both been observed empty for some
/// friends even while they have a real, active game and rich presence data) - the numeric
/// AppID still comes through reliably (game_played_app_id), so it can be resolved directly.
///
/// This is a distinct data source from RichPresenceLocalizationService: PICS appinfo's
/// "common" branch carries basic app metadata (name, type) for essentially any app without
/// an access token, but does not carry rich-presence token localization (confirmed during
/// this feature's Phase 0 spike) - that lives behind the separate
/// Community.GetAppRichPresenceLocalization SteamUnifiedMessages RPC instead.
///
/// Caches resolved names per appid since a game's display name is effectively static for
/// the life of the bridge process.
/// </summary>
public class SteamAppInfoService
{
    private readonly ILogger<SteamAppInfoService> _logger;
    private readonly ConcurrentDictionary<uint, Task<string?>> _appNameCache = new();

    public SteamAppInfoService(ILogger<SteamAppInfoService> logger)
    {
        _logger = logger;
    }

    public Task<string?> GetAppNameAsync(SteamApps? steamApps, uint appId)
    {
        if (steamApps == null || appId == 0)
        {
            return Task.FromResult<string?>(null);
        }

        return _appNameCache.GetOrAdd(appId, id => FetchAppNameAsync(steamApps, id));
    }

    private async Task<string?> FetchAppNameAsync(SteamApps steamApps, uint appId)
    {
        try
        {
            var request = new SteamApps.PICSRequest(appId);
            var resultSet = await steamApps.PICSGetProductInfo(app: request, package: null);

            var productInfo = resultSet.Results?.FirstOrDefault(r => r.Apps.ContainsKey(appId));
            if (productInfo == null || !productInfo.Apps.TryGetValue(appId, out var appInfo))
            {
                _logger.LogWarning("No PICS product info returned for appid {AppId}", appId);
                _appNameCache.TryRemove(appId, out _);
                return null;
            }

            var name = appInfo.KeyValues["common"]["name"].AsString();
            if (string.IsNullOrWhiteSpace(name))
            {
                _logger.LogWarning("PICS product info for appid {AppId} had no common/name field", appId);
                _appNameCache.TryRemove(appId, out _);
                return null;
            }

            return name;
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "Error fetching app name for appid {AppId}", appId);
            _appNameCache.TryRemove(appId, out _);
            return null;
        }
    }
}
