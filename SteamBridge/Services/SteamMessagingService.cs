using Grpc.Core;
using SteamBridge.Proto;
using Microsoft.Extensions.Logging;

namespace SteamBridge.Services;

public class SteamMessagingService : Proto.SteamMessagingService.SteamMessagingServiceBase
{
    private readonly ILogger<SteamMessagingService> _logger;
    private readonly SteamMessagingManager _messagingManager;

    public SteamMessagingService(
        ILogger<SteamMessagingService> logger,
        SteamMessagingManager messagingManager)
    {
        _logger = logger;
        _messagingManager = messagingManager;
    }

    public override async Task<SendMessageResponse> SendMessage(
        SendMessageRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received send message request to {SteamID}: {Message}", 
            request.TargetSteamId, request.Message);

        try
        {
            var messageType = MapFromProtoMessageType(request.MessageType);
            var result = await _messagingManager.SendMessageAsync(
                request.TargetSteamId, 
                request.Message, 
                messageType);

            return new SendMessageResponse
            {
                Success = result.Success,
                ErrorMessage = result.ErrorMessage ?? string.Empty,
                Timestamp = result.Timestamp
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing send message request");
            return new SendMessageResponse
            {
                Success = false,
                ErrorMessage = $"Internal error: {ex.Message}",
                Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds()
            };
        }
    }

    public override async Task SubscribeToMessages(
        MessageSubscriptionRequest request,
        IServerStreamWriter<Proto.MessageEvent> responseStream,
        ServerCallContext context)
    {
        _logger.LogInformation("New message subscription started");

        try
        {
            var messageStream = _messagingManager.SubscribeToMessagesAsync(context.CancellationToken);
            
            await foreach (var message in messageStream.WithCancellation(context.CancellationToken))
            {
                var protoMessage = new Proto.MessageEvent
                {
                    SenderSteamId = message.SenderSteamId,
                    TargetSteamId = message.TargetSteamId,
                    Message = message.Message,
                    MessageType = MapToProtoMessageType(message.MessageType),
                    Timestamp = message.Timestamp,
                    IsEcho = message.IsEcho
                };

                await responseStream.WriteAsync(protoMessage);
                
                _logger.LogDebug("Streamed message event from {SenderID} to {TargetID}", 
                    message.SenderSteamId, message.TargetSteamId);
            }
        }
        catch (OperationCanceledException)
        {
            _logger.LogInformation("Message subscription cancelled by client");
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error in message subscription stream");
            throw new RpcException(new Status(StatusCode.Internal, $"Stream error: {ex.Message}"));
        }
        finally
        {
            _logger.LogInformation("Message subscription ended");
        }
    }

    public override async Task<TypingNotificationResponse> SendTypingNotification(
        TypingNotificationRequest request, 
        ServerCallContext context)
    {
        _logger.LogDebug("Received typing notification request to {SteamID}: {IsTyping}", 
            request.TargetSteamId, request.IsTyping);

        try
        {
            var success = await _messagingManager.SendTypingNotificationAsync(
                request.TargetSteamId, 
                request.IsTyping);

            return new TypingNotificationResponse
            {
                Success = success
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing typing notification request");
            return new TypingNotificationResponse
            {
                Success = false
            };
        }
    }

    private static Services.MessageType MapFromProtoMessageType(Proto.MessageType messageType)
    {
        return messageType switch
        {
            Proto.MessageType.ChatMessage => Services.MessageType.ChatMessage,
            Proto.MessageType.Typing => Services.MessageType.Typing,
            Proto.MessageType.Emote => Services.MessageType.Emote,
            Proto.MessageType.InviteGame => Services.MessageType.InviteGame,
            _ => Services.MessageType.ChatMessage
        };
    }

    private static Proto.MessageType MapToProtoMessageType(Services.MessageType messageType)
    {
        return messageType switch
        {
            Services.MessageType.ChatMessage => Proto.MessageType.ChatMessage,
            Services.MessageType.Typing => Proto.MessageType.Typing,
            Services.MessageType.Emote => Proto.MessageType.Emote,
            Services.MessageType.InviteGame => Proto.MessageType.InviteGame,
            _ => Proto.MessageType.ChatMessage
        };
    }
}