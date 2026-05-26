using Grpc.Core;
using SteamBridge.Proto;
using Microsoft.Extensions.Logging;
using SteamKit2;
using System.Threading.Channels;

namespace SteamBridge.Services;

public class SteamSessionService : Proto.SteamSessionService.SteamSessionServiceBase
{
    private readonly ILogger<SteamSessionService> _logger;
    private readonly SteamClientRegistry _registry;

    public SteamSessionService(
        ILogger<SteamSessionService> logger,
        SteamClientRegistry registry)
    {
        _logger = logger;
        _registry = registry;
    }

    public override async Task SubscribeToSessionEvents(
        SessionSubscriptionRequest request,
        IServerStreamWriter<Proto.SessionEvent> responseStream,
        ServerCallContext context)
    {
        _logger.LogInformation("New session event subscription started for steam_id: {SteamId}", request.SteamId);

        var manager = _registry.Get(request.SteamId.ToString());
        if (manager == null)
        {
            throw new RpcException(new Status(StatusCode.NotFound,
                $"No Steam session found for steam_id {request.SteamId}"));
        }

        var channel = Channel.CreateUnbounded<InternalSessionEvent>();

        void OnLoggedOff(object? sender, SteamUser.LoggedOffCallback callback)
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

            channel.Writer.TryWrite(new InternalSessionEvent
            {
                EventType = eventType,
                Reason = callback.Result.ToString(),
                Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds()
            });
        }

        void OnDisconnected(object? sender, SteamClient.DisconnectedCallback callback)
        {
            if (!callback.UserInitiated)
            {
                channel.Writer.TryWrite(new InternalSessionEvent
                {
                    EventType = SessionEventType.ConnectionLost,
                    Reason = "Connection lost unexpectedly",
                    Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds()
                });
            }
        }

        manager.LoggedOff += OnLoggedOff;
        manager.Disconnected += OnDisconnected;

        try
        {
            await foreach (var sessionEvent in channel.Reader.ReadAllAsync(context.CancellationToken))
            {
                var protoEvent = new Proto.SessionEvent
                {
                    EventType = MapToProtoEventType(sessionEvent.EventType),
                    Reason = sessionEvent.Reason,
                    Timestamp = sessionEvent.Timestamp
                };

                await responseStream.WriteAsync(protoEvent);

                _logger.LogDebug("Streamed session event: {EventType} - {Reason}",
                    sessionEvent.EventType, sessionEvent.Reason);
            }
        }
        catch (OperationCanceledException)
        {
            _logger.LogInformation("Session event subscription cancelled by client");
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error in session event subscription stream");
            throw new RpcException(new Status(StatusCode.Internal, $"Stream error: {ex.Message}"));
        }
        finally
        {
            manager.LoggedOff -= OnLoggedOff;
            manager.Disconnected -= OnDisconnected;
            channel.Writer.TryComplete();
            _logger.LogInformation("Session event subscription ended for steam_id: {SteamId}", request.SteamId);
        }
    }

    private static Proto.SessionEventType MapToProtoEventType(SessionEventType eventType)
    {
        return eventType switch
        {
            SessionEventType.LoggedOff => Proto.SessionEventType.LoggedOff,
            SessionEventType.ConnectionLost => Proto.SessionEventType.ConnectionLost,
            SessionEventType.SessionReplaced => Proto.SessionEventType.SessionReplaced,
            SessionEventType.TokenExpired => Proto.SessionEventType.TokenExpired,
            SessionEventType.AccountDisabled => Proto.SessionEventType.AccountDisabled,
            SessionEventType.Kicked => Proto.SessionEventType.Kicked,
            _ => Proto.SessionEventType.LoggedOff
        };
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
