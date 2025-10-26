using Grpc.Core;
using SteamBridge.Proto;
using Microsoft.Extensions.Logging;
using SteamKit2;
using SteamKit2.Internal;

namespace SteamBridge.Services;

public class SteamMessagingService : Proto.SteamMessagingService.SteamMessagingServiceBase
{
    private readonly ILogger<SteamMessagingService> _logger;
    private readonly SteamMessagingManager _messagingManager;
    private readonly SteamImageService _imageService;
    private readonly SteamUserInformationService _userInfoService;
    private readonly SteamClientManager _steamClientManager;

    public SteamMessagingService(
        ILogger<SteamMessagingService> logger,
        SteamMessagingManager messagingManager,
        SteamImageService imageService,
        SteamUserInformationService userInfoService,
        SteamClientManager steamClientManager)
    {
        _logger = logger;
        _messagingManager = messagingManager;
        _imageService = imageService;
        _userInfoService = userInfoService;
        _steamClientManager = steamClientManager;
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

    public override Task<UploadImageResponse> UploadImageToSteam(
        UploadImageRequest request, 
        ServerCallContext context)
    {
        _logger.LogWarning("Steam UGC upload request denied: {Filename}, {MimeType}, {Size} bytes. " +
                          "Steam blocks UGC uploads from third-party clients.", 
            request.Filename, request.MimeType, request.ImageData.Length);

        // Return consistent error explaining that Steam blocks UGC uploads from third-party clients
        return Task.FromResult(new UploadImageResponse
        {
            Success = false,
            ErrorMessage = "Steam UGC upload not supported: Steam blocks image uploads from third-party clients. " +
                          "Configure 'public_media' in bridge settings to enable Matrix→Steam image sharing via public URLs."
        });
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

    public override async Task<GetUserAvatarDataResponse> GetUserAvatarData(
        GetUserAvatarDataRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received avatar data request for Steam ID: {SteamId}", request.SteamId);

        try
        {
            // Get user info which includes avatar URL and hash
            var userInfo = await _userInfoService.GetUserInfoAsync(request.SteamId);
            if (userInfo == null)
            {
                return new GetUserAvatarDataResponse
                {
                    Success = false,
                    ErrorMessage = "User not found or unable to retrieve user information"
                };
            }

            // If no avatar URL is available, return empty response
            if (string.IsNullOrEmpty(userInfo.AvatarUrl))
            {
                _logger.LogDebug("No avatar URL available for Steam ID: {SteamId}", request.SteamId);
                return new GetUserAvatarDataResponse
                {
                    Success = true,
                    AvatarHash = userInfo.AvatarHash,
                    ImageData = Google.Protobuf.ByteString.Empty,
                    MimeType = string.Empty
                };
            }

            try
            {
                // Download the avatar image data
                var (imageData, mimeType) = await _imageService.DownloadImageAsync(userInfo.AvatarUrl);
                
                return new GetUserAvatarDataResponse
                {
                    Success = true,
                    AvatarHash = userInfo.AvatarHash,
                    AvatarUrl = userInfo.AvatarUrl,
                    ImageData = Google.Protobuf.ByteString.CopyFrom(imageData),
                    MimeType = mimeType
                };
            }
            catch (Exception downloadEx)
            {
                _logger.LogWarning(downloadEx, "Failed to download avatar image for Steam ID {SteamId} from URL: {AvatarUrl}", 
                    request.SteamId, userInfo.AvatarUrl);
                
                // Return success but with empty image data if download fails
                // This allows the system to still work with avatar URL for fallback
                return new GetUserAvatarDataResponse
                {
                    Success = true,
                    AvatarHash = userInfo.AvatarHash,
                    AvatarUrl = userInfo.AvatarUrl,
                    ImageData = Google.Protobuf.ByteString.Empty,
                    MimeType = string.Empty,
                    ErrorMessage = $"Avatar download failed: {downloadEx.Message}"
                };
            }
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing avatar data request for Steam ID: {SteamId}", request.SteamId);
            return new GetUserAvatarDataResponse
            {
                Success = false,
                ErrorMessage = $"Internal error: {ex.Message}"
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

    public override async Task<ChatMessageHistoryResponse> GetChatMessageHistory(
        ChatMessageHistoryRequest request,
        ServerCallContext context)
    {
        try
        {
            _logger.LogDebug("Getting chat message history for chat group {ChatGroupId}, chat {ChatId}",
                request.ChatGroupId, request.ChatId);

            var steamUnified = _steamClientManager.SteamUnifiedMessages;
            if (steamUnified == null)
            {
                _logger.LogError("SteamUnifiedMessages is not available");
                return new ChatMessageHistoryResponse
                {
                    Success = false,
                    ErrorMessage = "Steam client not connected"
                };
            }

            // Detect conversation type: DM (chat_group_id == 0) vs Group Chat (chat_group_id != 0)
            bool isDM = request.ChatGroupId == 0;

            if (isDM)
            {
                // Use FriendMessages API for 1-to-1 DM conversations
                return await GetFriendMessageHistory(request, steamUnified);
            }
            else
            {
                // Use ChatRoom API for group chats
                return await GetChatRoomMessageHistory(request, steamUnified);
            }
        }
        catch (TaskCanceledException ex)
        {
            _logger.LogError(ex, "Task was canceled while retrieving chat message history. CancellationToken.IsCancellationRequested: {IsCancelled}", context.CancellationToken.IsCancellationRequested);
            return new ChatMessageHistoryResponse
            {
                Success = false,
                ErrorMessage = $"Failed to retrieve message history: A task was canceled."
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error retrieving chat message history");
            return new ChatMessageHistoryResponse
            {
                Success = false,
                ErrorMessage = $"Failed to retrieve message history: {ex.Message}"
            };
        }
    }

    private async Task<ChatMessageHistoryResponse> GetFriendMessageHistory(
        ChatMessageHistoryRequest request,
        SteamUnifiedMessages steamUnified)
    {
        // Get our own Steam ID for the friend messages request
        var mySteamId = _steamClientManager.SteamClient.SteamID?.ConvertToUInt64() ?? 0;
        if (mySteamId == 0)
        {
            return new ChatMessageHistoryResponse
            {
                Success = false,
                ErrorMessage = "Cannot get own Steam ID"
            };
        }

        // Build FriendMessages.GetRecentMessages request
        var friendMsgRequest = new CFriendMessages_GetRecentMessages_Request
        {
            steamid1 = mySteamId,
            steamid2 = request.ChatId, // ChatId contains the friend's Steam ID for DMs
            count = Math.Min(request.MaxCount, 100)
        };

        // Set pagination cursor based on direction
        if (request.LastTime > 0 || request.LastOrdinal > 0)
        {
            if (request.Forward)
            {
                // Forward pagination: fetch messages AFTER the anchor point (for catchup)
                friendMsgRequest.rtime32_start_time = request.LastTime;
                friendMsgRequest.start_ordinal = request.LastOrdinal;
            }
            else
            {
                // Backward pagination: fetch messages BEFORE the anchor point (for history)
                friendMsgRequest.time_last = request.LastTime;
                friendMsgRequest.ordinal_last = request.LastOrdinal;
            }
        }

        _logger.LogDebug("Sending FriendMessages.GetRecentMessages: SteamId1={SteamId1}, SteamId2={SteamId2}, Count={Count}, TimeLast={TimeLast}",
            friendMsgRequest.steamid1, friendMsgRequest.steamid2, friendMsgRequest.count, friendMsgRequest.time_last);

        // Call Steam FriendMessages API using the registered service (enables proper callback handling)
        _logger.LogDebug("Starting Steam API call for FriendMessages.GetRecentMessages");
        var friendMessagesService = _steamClientManager.FriendMessagesService;
        var job = friendMessagesService.GetRecentMessages(friendMsgRequest);

        _logger.LogDebug("Awaiting Steam API response for FriendMessages.GetRecentMessages");
        var result = await job.ToTask();
        _logger.LogDebug("Received Steam API response for FriendMessages.GetRecentMessages");

        if (result == null || result.Result != EResult.OK)
        {
            _logger.LogWarning("Failed to get friend message history: result={Result}", result?.Result);
            return new ChatMessageHistoryResponse
            {
                Success = false,
                ErrorMessage = $"Steam API returned error: {result?.Result}"
            };
        }

        var response = result.Body;
        _logger.LogDebug("Received {MessageCount} friend messages from Steam API", response.messages?.Count ?? 0);

        var historyResponse = new ChatMessageHistoryResponse
        {
            Success = true,
            HasMore = response.more_available,
            Messages = { }
        };

        if (response.messages != null)
        {
            // Get the universe from our own Steam ID for constructing sender IDs
            var myUniverse = _steamClientManager.SteamClient.SteamID?.AccountUniverse ?? EUniverse.Public;

            foreach (var msg in response.messages)
            {
                // Convert 32-bit account ID to full 64-bit Steam ID using SteamKit's constructor
                // FriendMessages API returns account IDs only, following SteamKit's own pattern from Callbacks.cs
                var senderSteamId = new SteamID(msg.accountid, myUniverse, EAccountType.Individual);

                var historyMessage = new ChatHistoryMessage
                {
                    SenderSteamId = senderSteamId.ConvertToUInt64(),
                    Timestamp = msg.timestamp,
                    Ordinal = msg.ordinal,
                    MessageContent = msg.message ?? string.Empty,
                    MessageType = Proto.MessageType.ChatMessage // FriendMessages API doesn't include message type
                };

                // Parse image messages if present
                var (imageUrl, caption) = ParseImageMessage(historyMessage.MessageContent);
                if (!string.IsNullOrEmpty(imageUrl))
                {
                    historyMessage.ImageUrl = imageUrl;
                    historyMessage.MessageContent = caption;
                }

                historyResponse.Messages.Add(historyMessage);
            }

            // Set next cursor for pagination (use the oldest message for backward pagination)
            if (response.messages.Count > 0)
            {
                var lastMessage = response.messages.Last();
                historyResponse.NextTime = lastMessage.timestamp;
                historyResponse.NextOrdinal = lastMessage.ordinal;
            }
        }

        _logger.LogDebug("Returning {MessageCount} processed friend messages, HasMore={HasMore}",
            historyResponse.Messages.Count, historyResponse.HasMore);

        return historyResponse;
    }

    private async Task<ChatMessageHistoryResponse> GetChatRoomMessageHistory(
        ChatMessageHistoryRequest request,
        SteamUnifiedMessages steamUnified)
    {
        // Create the Steam API request for group chats
        var historyRequest = new CChatRoom_GetMessageHistory_Request
        {
            chat_group_id = request.ChatGroupId,
            chat_id = request.ChatId,
            max_count = Math.Min(request.MaxCount, 100) // Limit to reasonable batch size
        };

        // Set pagination cursor if provided
        if (request.LastTime > 0 && request.LastOrdinal > 0)
        {
            if (request.Forward)
            {
                historyRequest.start_time = request.LastTime;
                historyRequest.start_ordinal = request.LastOrdinal;
            }
            else
            {
                historyRequest.last_time = request.LastTime;
                historyRequest.last_ordinal = request.LastOrdinal;
            }
        }

        _logger.LogDebug("Sending ChatRoom.GetMessageHistory request: GroupId={GroupId}, ChatId={ChatId}, MaxCount={MaxCount}, Forward={Forward}",
            historyRequest.chat_group_id, historyRequest.chat_id, historyRequest.max_count, request.Forward);

        // Call Steam API
        _logger.LogDebug("Starting Steam API call for ChatRoom.GetMessageHistory");
        var job = steamUnified.SendMessage<CChatRoom_GetMessageHistory_Request, CChatRoom_GetMessageHistory_Response>(
            "ChatRoom.GetMessageHistory#1", historyRequest);

        _logger.LogDebug("Awaiting Steam API response for ChatRoom.GetMessageHistory");
        var result = await job.ToTask();
        _logger.LogDebug("Received Steam API response for ChatRoom.GetMessageHistory");

        if (result == null || result.Result != EResult.OK)
        {
            _logger.LogWarning("Failed to get chat room history: result={Result}", result?.Result);
            return new ChatMessageHistoryResponse
            {
                Success = false,
                ErrorMessage = $"Steam API returned error: {result?.Result}"
            };
        }

        var response = result.Body;
        _logger.LogDebug("Received {MessageCount} chat room messages from Steam API", response.messages?.Count ?? 0);

        var historyResponse = new ChatMessageHistoryResponse
        {
            Success = true,
            HasMore = response.more_available,
            Messages = { }
        };

        if (response.messages != null)
        {
            foreach (var msg in response.messages)
            {
                var historyMessage = new ChatHistoryMessage
                {
                    SenderSteamId = msg.sender,
                    Timestamp = msg.server_timestamp,
                    Ordinal = msg.ordinal,
                    MessageContent = msg.message ?? string.Empty,
                    MessageType = Proto.MessageType.ChatMessage // Default to chat message for now
                };

                // Parse image messages if present
                var (imageUrl, caption) = ParseImageMessage(historyMessage.MessageContent);
                if (!string.IsNullOrEmpty(imageUrl))
                {
                    historyMessage.ImageUrl = imageUrl;
                    historyMessage.MessageContent = caption;
                }

                historyResponse.Messages.Add(historyMessage);
            }

            // Set next cursor for pagination
            if (response.messages.Count > 0)
            {
                var lastMessage = response.messages.Last();
                historyResponse.NextTime = lastMessage.server_timestamp;
                historyResponse.NextOrdinal = lastMessage.ordinal;
            }
        }

        _logger.LogDebug("Returning {MessageCount} processed chat room messages, HasMore={HasMore}",
            historyResponse.Messages.Count, historyResponse.HasMore);

        return historyResponse;
    }

    private static Proto.MessageType ConvertSteamMessageType(uint steamMessageType)
    {
        // Steam message type constants from SteamKit2
        return steamMessageType switch
        {
            1 => Proto.MessageType.ChatMessage, // k_EChatEntryType_ChatMsg
            2 => Proto.MessageType.Typing,       // k_EChatEntryType_Typing
            3 => Proto.MessageType.Emote,        // k_EChatEntryType_Emote
            4 => Proto.MessageType.InviteGame,   // k_EChatEntryType_InviteGame
            _ => Proto.MessageType.ChatMessage   // Default to chat message
        };
    }
}