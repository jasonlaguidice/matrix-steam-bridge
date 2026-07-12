using System.Collections.Concurrent;
using System.Linq;
using System.Text.RegularExpressions;
using Microsoft.Extensions.Logging;
using SteamKit2;
using SteamKit2.WebUI.Internal;

namespace SteamBridge.Services;

/// <summary>
/// Resolves raw Steam Rich Presence tokens (e.g. "#DeadlockRP_HideoutSnack") into
/// human-readable text using the public, ownership-independent
/// "Community.GetAppRichPresenceLocalization#1" SteamUnifiedMessages RPC.
///
/// Caches resolved token -&gt; text tables per (appid, language) since the localization table
/// for a given game rarely changes and is shared across every friend playing it.
/// </summary>
public class RichPresenceLocalizationService
{
    // {#TokenName} - a nested token reference inside an already-resolved template.
    private static readonly Regex NestedTokenPattern = new(@"\{#([A-Za-z0-9_]+)\}", RegexOptions.Compiled);

    // %VariableName% - a placeholder substituted from sibling raw rich-presence key/value pairs.
    private static readonly Regex VariablePattern = new(@"%([A-Za-z0-9_]+)%", RegexOptions.Compiled);

    private readonly ILogger<RichPresenceLocalizationService> _logger;

    // Cache of resolved token -> text tables, keyed by (appid, language). Token names are
    // matched case-insensitively, matching the real Steam client's own indexing behavior.
    private readonly ConcurrentDictionary<(uint AppId, string Language), Task<Dictionary<string, string>?>> _tokenMapCache = new();

    public RichPresenceLocalizationService(ILogger<RichPresenceLocalizationService> logger)
    {
        _logger = logger;
    }

    /// <summary>
    /// Resolves a friend's raw rich presence tokens into a single human-readable status string,
    /// following the same "status" key + "{#Token}"/"%Variable%" resolution rules as the real
    /// Steam client. Never throws; returns null if resolution isn't possible for any reason
    /// (unknown app, network failure, missing/garbled token) so callers can simply omit rich
    /// presence text rather than show something broken.
    /// </summary>
    public async Task<string?> ResolveAsync(
        SteamUnifiedMessages steamUnifiedMessages,
        uint appId,
        string language,
        IReadOnlyDictionary<string, string> rawTokens)
    {
        if (steamUnifiedMessages == null || rawTokens == null || rawTokens.Count == 0 || appId == 0)
        {
            return null;
        }

        try
        {
            var rawTokensCI = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
            foreach (var kv in rawTokens)
            {
                if (!string.IsNullOrEmpty(kv.Key))
                {
                    rawTokensCI[kv.Key] = kv.Value ?? string.Empty;
                }
            }

            // The display-worthy value lives under the "steam_display" key - this is Valve's
            // own documented key name for the primary rich presence display string (games call
            // SetRichPresence("steam_display", "#SomeToken")). Also check "status" as a
            // secondary fallback in case some games use that convention instead - not
            // documented by Valve, but cheap to also support.
            if ((!rawTokensCI.TryGetValue("steam_display", out var statusValue) || string.IsNullOrWhiteSpace(statusValue)) &&
                (!rawTokensCI.TryGetValue("status", out statusValue) || string.IsNullOrWhiteSpace(statusValue)))
            {
                _logger.LogDebug(
                    "No steam_display/status key in rich presence tokens for appid {AppId} - keys present: {Keys}",
                    appId, string.Join(", ", rawTokensCI.Keys));
                return null;
            }

            var tokenMap = await GetOrFetchTokenMapAsync(steamUnifiedMessages, appId, language).ConfigureAwait(false);
            if (tokenMap == null || tokenMap.Count == 0)
            {
                return null;
            }

            return ResolveToken(statusValue, tokenMap, rawTokensCI, _logger);
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "Unexpected error resolving rich presence text for appid {AppId}", appId);
            return null;
        }
    }

    private Task<Dictionary<string, string>?> GetOrFetchTokenMapAsync(
        SteamUnifiedMessages steamUnifiedMessages, uint appId, string language)
    {
        var key = (appId, language);
        return _tokenMapCache.GetOrAdd(key, k => FetchTokenMapAsync(steamUnifiedMessages, k.AppId, k.Language));
    }

    private async Task<Dictionary<string, string>?> FetchTokenMapAsync(
        SteamUnifiedMessages steamUnifiedMessages, uint appId, string language)
    {
        try
        {
            var community = steamUnifiedMessages.CreateService<Community>();
            var request = new CCommunity_GetAppRichPresenceLocalization_Request
            {
                appid = (int)appId,
                language = language
            };

            var job = community.GetAppRichPresenceLocalization(request);
            var result = await job.ToTask().ConfigureAwait(false);

            if (result == null || result.Result != EResult.OK)
            {
                _logger.LogWarning(
                    "Failed to fetch rich presence localization for appid {AppId} language {Language}: {Result}",
                    appId, language, result?.Result);
                _tokenMapCache.TryRemove((appId, language), out _);
                return null;
            }

            var tokenList = result.Body.token_lists
                .FirstOrDefault(tl => string.Equals(tl.language, language, StringComparison.OrdinalIgnoreCase));

            var tokenMap = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);

            if (tokenList == null)
            {
                _logger.LogInformation(
                    "No rich presence token list for appid {AppId} language {Language} (game may not support Enhanced Rich Presence)",
                    appId, language);
                // Cache the empty result - a game genuinely lacking Enhanced Rich Presence
                // localization is a stable fact, not a transient failure, so don't retry forever.
                return tokenMap;
            }

            foreach (var token in tokenList.tokens)
            {
                if (!string.IsNullOrEmpty(token.name))
                {
                    tokenMap[token.name] = token.value ?? string.Empty;
                }
            }

            return tokenMap;
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "Error fetching rich presence localization for appid {AppId} language {Language}", appId, language);
            _tokenMapCache.TryRemove((appId, language), out _);
            return null;
        }
    }

    /// <summary>
    /// Resolves a single raw token value (e.g. "#DeadlockRP_HideoutSnack" or
    /// "{#Status_%gamestatus%}: %SCORE%") against the app's token map and the friend's sibling
    /// raw key/value pairs. Implements one level of "{#Token}" re-resolution and "%Variable%"
    /// substitution, matching Valve's documented rich-presence template format.
    /// </summary>
    private static string? ResolveToken(
        string rawValue,
        Dictionary<string, string> tokenMap,
        Dictionary<string, string> rawTokensCI,
        ILogger logger)
    {
        if (string.IsNullOrWhiteSpace(rawValue))
        {
            return null;
        }

        string template;

        if (rawValue.StartsWith('#'))
        {
            // Observed empirically: some games' localization tables key their tokens WITH the
            // leading "#" included (e.g. "#OnMap"), not just the bare name ("OnMap") as Valve's
            // generic documentation example implied. Try the raw value as-is first, then fall
            // back to the stripped form, to handle both conventions.
            if (!tokenMap.TryGetValue(rawValue, out var resolved) &&
                !tokenMap.TryGetValue(rawValue[1..], out resolved))
            {
                // A token reference we can't resolve - don't show a raw "#Token" to the user.
                logger.LogDebug(
                    "Could not find rawValue='{RawValue}' (with or without leading #) in token map",
                    rawValue);
                return null;
            }
            template = resolved;
        }
        else
        {
            // Some rich presence values are already literal text rather than a token reference.
            template = rawValue;
        }

        // One level of nested {#Token} re-resolution. Same with/without-"#" lookup as above.
        template = NestedTokenPattern.Replace(template, m =>
        {
            var nestedKey = m.Groups[1].Value;
            if (tokenMap.TryGetValue("#" + nestedKey, out var nestedValue) || tokenMap.TryGetValue(nestedKey, out nestedValue))
            {
                return nestedValue;
            }
            return m.Value;
        });

        // %Variable% substitution from the friend's other raw rich-presence key/value pairs.
        template = VariablePattern.Replace(template, m =>
        {
            var varKey = m.Groups[1].Value;
            return rawTokensCI.TryGetValue(varKey, out var varValue) ? varValue : m.Value;
        });

        return string.IsNullOrWhiteSpace(template) ? null : template;
    }
}
