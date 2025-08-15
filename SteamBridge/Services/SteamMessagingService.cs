using Grpc.Core;
using SteamBridge.Proto;
using Microsoft.Extensions.Logging;

namespace SteamBridge.Services;

public class SteamMessagingService : Proto.SteamMessagingService.SteamMessagingServiceBase
{
    private readonly ILogger<SteamMessagingService> _logger;
    private readonly SteamMessagingManager _messagingManager;
    private readonly SteamImageService _imageService;

    public SteamMessagingService(
        ILogger<SteamMessagingService> logger,
        SteamMessagingManager messagingManager,
        SteamImageService imageService)
    {
        _logger = logger;
        _messagingManager = messagingManager;
        _imageService = imageService;
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
            
            // If there's an image URL, format the message to include it
            string messageToSend = request.Message;
            if (!string.IsNullOrEmpty(request.ImageUrl))
            {
                // Format: [Image: URL] caption
                messageToSend = string.IsNullOrEmpty(request.Message) 
                    ? $"[Image: {request.ImageUrl}]"
                    : $"[Image: {request.ImageUrl}] {request.Message}";
            }
            
            var result = await _messagingManager.SendMessageAsync(
                request.TargetSteamId, 
                messageToSend, 
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
                // Parse image URL and caption from message
                var (imageUrl, caption) = ParseImageMessage(message.Message);
                
                var protoMessage = new Proto.MessageEvent
                {
                    SenderSteamId = message.SenderSteamId,
                    TargetSteamId = message.TargetSteamId,
                    Message = caption, // Use parsed caption instead of full message
                    MessageType = MapToProtoMessageType(message.MessageType),
                    Timestamp = message.Timestamp,
                    IsEcho = message.IsEcho
                };

                // Set image URL if present
                if (!string.IsNullOrEmpty(imageUrl))
                {
                    protoMessage.ImageUrl = imageUrl;
                }

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

    public override async Task<UploadImageResponse> UploadImageToSteam(
        UploadImageRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received image upload request: {Filename}, {MimeType}, {Size} bytes", 
            request.Filename, request.MimeType, request.ImageData.Length);

        try
        {
            // Validate the image
            if (!_imageService.ValidateImage(request.ImageData.ToByteArray(), request.MimeType))
            {
                return new UploadImageResponse
                {
                    Success = false,
                    ErrorMessage = "Invalid image format or size"
                };
            }

            // Upload the image to Steam
            var imageUrl = await _imageService.UploadImageAsync(
                request.ImageData.ToByteArray(), 
                request.MimeType, 
                request.Filename);

            return new UploadImageResponse
            {
                Success = true,
                ImageUrl = imageUrl
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error uploading image: {Filename}", request.Filename);
            return new UploadImageResponse
            {
                Success = false,
                ErrorMessage = $"Upload failed: {ex.Message}"
            };
        }
    }

    public override async Task<DownloadImageResponse> DownloadImageFromSteam(
        DownloadImageRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received image download request: {Url}", request.ImageUrl);

        try
        {
            var (imageData, mimeType) = await _imageService.DownloadImageAsync(request.ImageUrl);

            return new DownloadImageResponse
            {
                Success = true,
                ImageData = Google.Protobuf.ByteString.CopyFrom(imageData),
                MimeType = mimeType
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error downloading image: {Url}", request.ImageUrl);
            return new DownloadImageResponse
            {
                Success = false,
                ErrorMessage = $"Download failed: {ex.Message}"
            };
        }
    }

    /// <summary>
    /// Parses a message to extract image URL and caption.
    /// Expected format: "[Image: URL] caption" or "[Image: URL]"
    /// </summary>
    /// <param name="message">The message to parse</param>
    /// <returns>A tuple containing the image URL (if found) and the caption text</returns>
    private static (string? imageUrl, string caption) ParseImageMessage(string message)
    {
        if (string.IsNullOrEmpty(message))
        {
            return (null, message);
        }

        // Look for pattern: [Image: URL]
        const string imagePrefix = "[Image: ";
        const string imageSuffix = "]";

        if (!message.StartsWith(imagePrefix))
        {
            return (null, message); // No image URL found
        }

        var endIndex = message.IndexOf(imageSuffix, imagePrefix.Length);
        if (endIndex == -1)
        {
            return (null, message); // Malformed image marker
        }

        // Extract the URL
        var imageUrl = message.Substring(imagePrefix.Length, endIndex - imagePrefix.Length);
        
        // Extract the caption (everything after the closing bracket and optional space)
        var captionStart = endIndex + imageSuffix.Length;
        var caption = captionStart < message.Length 
            ? message.Substring(captionStart).TrimStart() 
            : string.Empty;

        return (imageUrl, caption);
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