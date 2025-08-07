using Microsoft.Extensions.Logging;
using SteamKit2;
using System.Collections.Concurrent;
using System.Runtime.CompilerServices;
using System.Threading.Channels;

namespace SteamBridge.Services;

public class SteamSessionManager
{
    private readonly ILogger<SteamSessionManager> _logger;
    private readonly SteamClientManager _steamClientManager;
    private readonly ConcurrentDictionary<string, Channel<InternalSessionEvent>> _subscribers;

    public SteamSessionManager(
        ILogger<SteamSessionManager> logger,
        SteamClientManager steamClientManager)
    {
        _logger = logger;
        _steamClientManager = steamClientManager;
        _subscribers = new ConcurrentDictionary<string, Channel<InternalSessionEvent>>();

        // Subscribe to SteamKit events
        _steamClientManager.LoggedOff += OnLoggedOff;
        _steamClientManager.Disconnected += OnDisconnected;

        _logger.LogInformation("SteamSessionManager initialized");
    }

    public async IAsyncEnumerable<InternalSessionEvent> SubscribeToSessionEventsAsync(
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        var subscriptionId = Guid.NewGuid().ToString();
        var channel = Channel.CreateUnbounded<InternalSessionEvent>();
        
        _subscribers.TryAdd(subscriptionId, channel);
        _logger.LogInformation("New session event subscription created: {SubscriptionId}", subscriptionId);

        try
        {
            await foreach (var sessionEvent in channel.Reader.ReadAllAsync(cancellationToken))
            {
                yield return sessionEvent;
            }
        }
        finally
        {
            _subscribers.TryRemove(subscriptionId, out _);
            _logger.LogInformation("Session event subscription removed: {SubscriptionId}", subscriptionId);
        }
    }

    private void OnLoggedOff(object? sender, SteamUser.LoggedOffCallback callback)
    {
        var eventType = callback.Result switch
        {
            EResult.LogonSessionReplaced => SessionEventType.SessionReplaced,
            EResult.AccountDisabled => SessionEventType.AccountDisabled,
            EResult.AccountLoginDeniedNeedTwoFactor => SessionEventType.TokenExpired,
            EResult.InvalidPassword => SessionEventType.TokenExpired,
            EResult.LoggedInElsewhere => SessionEventType.SessionReplaced,
            _ => SessionEventType.LoggedOff
        };

        var sessionEvent = new InternalSessionEvent
        {
            EventType = eventType,
            Reason = callback.Result.ToString(),
            Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds()
        };

        BroadcastEvent(sessionEvent);
    }

    private void OnDisconnected(object? sender, SteamClient.DisconnectedCallback callback)
    {
        // Only broadcast if this wasn't a user-initiated disconnection
        if (!callback.UserInitiated)
        {
            var sessionEvent = new InternalSessionEvent
            {
                EventType = SessionEventType.ConnectionLost,
                Reason = "Connection lost unexpectedly",
                Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds()
            };

            BroadcastEvent(sessionEvent);
        }
    }

    private void BroadcastEvent(InternalSessionEvent sessionEvent)
    {
        _logger.LogInformation("Broadcasting session event: {EventType} - {Reason}", 
            sessionEvent.EventType, sessionEvent.Reason);

        foreach (var subscriber in _subscribers.Values)
        {
            try
            {
                subscriber.Writer.TryWrite(sessionEvent);
            }
            catch (Exception ex)
            {
                _logger.LogWarning(ex, "Failed to send session event to subscriber");
            }
        }
    }
}

public class InternalSessionEvent
{
    public SessionEventType EventType { get; set; }
    public string Reason { get; set; } = string.Empty;
    public long Timestamp { get; set; }
}

public enum SessionEventType
{
    LoggedOff,
    ConnectionLost,
    SessionReplaced,
    TokenExpired,
    AccountDisabled,
    Kicked
}