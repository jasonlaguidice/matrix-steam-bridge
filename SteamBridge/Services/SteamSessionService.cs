using Grpc.Core;
using SteamBridge.Proto;
using Microsoft.Extensions.Logging;
using System.Collections.Concurrent;
using System.Threading.Channels;

namespace SteamBridge.Services;

public class SteamSessionService : Proto.SteamSessionService.SteamSessionServiceBase
{
    private readonly ILogger<SteamSessionService> _logger;
    private readonly SteamSessionManager _sessionManager;

    public SteamSessionService(
        ILogger<SteamSessionService> logger,
        SteamSessionManager sessionManager)
    {
        _logger = logger;
        _sessionManager = sessionManager;
    }

    public override async Task SubscribeToSessionEvents(
        SessionSubscriptionRequest request,
        IServerStreamWriter<Proto.SessionEvent> responseStream,
        ServerCallContext context)
    {
        _logger.LogInformation("New session event subscription started");

        try
        {
            var eventStream = _sessionManager.SubscribeToSessionEventsAsync(context.CancellationToken);
            
            await foreach (var sessionEvent in eventStream.WithCancellation(context.CancellationToken))
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
            _logger.LogInformation("Session event subscription ended");
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