using System.Collections.Concurrent;
using Microsoft.Extensions.Logging;

namespace SteamBridge.Services;

/// <summary>
/// Manages one SteamClientManager per authenticated user.
/// Keys are either a login_session_key (during auth) or steam_id string (after auth).
/// </summary>
public class SteamClientRegistry : IDisposable
{
    private readonly ILogger<SteamClientRegistry> _logger;
    private readonly ILoggerFactory _loggerFactory;
    private readonly ConcurrentDictionary<string, SteamClientManager> _managers = new();

    public SteamClientRegistry(ILogger<SteamClientRegistry> logger, ILoggerFactory loggerFactory)
    {
        _logger = logger;
        _loggerFactory = loggerFactory;
    }

    /// <summary>
    /// Get or create a SteamClientManager for the given key.
    /// Used during auth phase with login_session_key, and after auth with steam_id.
    /// </summary>
    public SteamClientManager GetOrCreate(string key)
    {
        return _managers.GetOrAdd(key, k =>
        {
            _logger.LogInformation("Creating new SteamClientManager for key: {Key}", k);
            return new SteamClientManager(_loggerFactory.CreateLogger<SteamClientManager>(), _loggerFactory);
        });
    }

    /// <summary>
    /// Get an existing manager. Returns null if not found.
    /// </summary>
    public SteamClientManager? Get(string key)
    {
        _managers.TryGetValue(key, out var manager);
        return manager;
    }

    /// <summary>
    /// Re-key a manager from a temporary login_session_key to the permanent steam_id string.
    /// Called after successful authentication.
    /// </summary>
    public void TransitionKey(string tempKey, string permanentKey)
    {
        if (_managers.TryRemove(tempKey, out var manager))
        {
            _managers[permanentKey] = manager;
            _logger.LogInformation("Transitioned manager from temp key {TempKey} to steam_id {PermanentKey}", tempKey, permanentKey);
        }
        else
        {
            _logger.LogWarning("TransitionKey: temp key {TempKey} not found", tempKey);
        }
    }

    /// <summary>
    /// Remove and dispose a manager (on user logout).
    /// </summary>
    public void Remove(string key)
    {
        if (_managers.TryRemove(key, out var manager))
        {
            _logger.LogInformation("Removing SteamClientManager for key: {Key}", key);
            manager.Dispose();
        }
    }

    public void Dispose()
    {
        foreach (var manager in _managers.Values)
            manager.Dispose();
        _managers.Clear();
    }
}
