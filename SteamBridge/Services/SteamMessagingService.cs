using Grpc.Core;
using SteamBridge.Proto;
using Microsoft.Extensions.Logging;
using System.Text.RegularExpressions;
using System.Threading.Channels;
using SteamKit2;
using SteamKit2.Internal;

namespace SteamBridge.Services;

public class SteamMessagingService : Proto.SteamMessagingService.SteamMessagingServiceBase
{
    private readonly ILogger<SteamMessagingService> _logger;
    private readonly SteamClientRegistry _registry;
    private readonly SteamImageService _imageService;
    private readonly SteamUserInformationService _userInfoService;
    private readonly SteamAppInfoService _appInfoService;

    public SteamMessagingService(
        ILogger<SteamMessagingService> logger,
        SteamClientRegistry registry,
        SteamImageService imageService,
        SteamUserInformationService userInfoService,
        SteamAppInfoService appInfoService)
    {
        _logger = logger;
        _registry = registry;
        _imageService = imageService;
        _userInfoService = userInfoService;
        _appInfoService = appInfoService;
    }

    public override async Task<SendMessageResponse> SendMessage(
        SendMessageRequest request,
        ServerCallContext context)
    {
        _logger.LogInformation("Received send message request to {SteamID}: {Message}",
            request.TargetSteamId, request.Message);

        var manager = _registry.Get(request.CallerSteamId.ToString())
            ?? throw new RpcException(new Status(StatusCode.NotFound,
                $"No Steam session for caller steam_id {request.CallerSteamId}"));

        try
        {
            if (request.ChatGroupId != 0)
            {
                var groupResult = await SendGroupMessageAsync(manager, request.ChatGroupId, request.ChatId, request.Message);
                return new SendMessageResponse
                {
                    Success = groupResult.Success,
                    ErrorMessage = groupResult.ErrorMessage ?? string.Empty,
                    Timestamp = groupResult.Timestamp,
                    Ordinal = groupResult.Ordinal,
                };
            }

            var messageType = MapFromProtoMessageType(request.MessageType);

            string messageToSend = request.Message;
            if (!string.IsNullOrEmpty(request.ImageUrl))
            {
                messageToSend = string.IsNullOrEmpty(request.Message)
                    ? $"[Image: {request.ImageUrl}]"
                    : $"[Image: {request.ImageUrl}] {request.Message}";
            }

            var result = await SendMessageAsync(manager, request.TargetSteamId, messageToSend, messageType);

            return new SendMessageResponse
            {
                Success = result.Success,
                ErrorMessage = result.ErrorMessage ?? string.Empty,
                Timestamp = result.Timestamp,
                Ordinal = result.Ordinal
            };
        }
        catch (RpcException)
        {
            throw;
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
        _logger.LogInformation("New message subscription started for steam_id: {SteamId}", request.SteamId);

        var manager = _registry.Get(request.SteamId.ToString())
            ?? throw new RpcException(new Status(StatusCode.NotFound,
                $"No Steam session for steam_id {request.SteamId}"));

        var channel = Channel.CreateUnbounded<MessageEvent>();

        async void OnMessageReceived(object? sender, SteamFriends.FriendMsgCallback callback)
        {
            try
            {
                var messageEvent = new MessageEvent
                {
                    SenderSteamId = callback.Sender.ConvertToUInt64(),
                    TargetSteamId = manager.SteamClient.SteamID?.ConvertToUInt64() ?? 0,
                    Message = callback.Message.TrimEnd('\0'),
                    MessageType = MapFromChatEntryType(callback.EntryType),
                    Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds(),
                    IsEcho = false
                };
                await channel.Writer.WriteAsync(messageEvent);
            }
            catch (Exception ex)
            {
                _logger.LogError(ex, "Error processing received message");
            }
        }

        async void OnMessageEcho(object? sender, SteamFriends.FriendMsgEchoCallback callback)
        {
            try
            {
                var messageEvent = new MessageEvent
                {
                    SenderSteamId = manager.SteamClient.SteamID?.ConvertToUInt64() ?? 0,
                    TargetSteamId = callback.Recipient.ConvertToUInt64(),
                    Message = callback.Message.TrimEnd('\0'),
                    MessageType = MapFromChatEntryType(callback.EntryType),
                    Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds(),
                    IsEcho = true
                };
                await channel.Writer.WriteAsync(messageEvent);
            }
            catch (Exception ex)
            {
                _logger.LogError(ex, "Error processing message echo");
            }
        }

        async void OnGroupMessageReceived(object? sender, CChatRoom_IncomingChatMessage_Notification notification)
        {
            try
            {
                if (notification.server_message != null) return;

                var msgRaw = notification.message?.TrimEnd('\0');

                var messageEvent = new MessageEvent
                {
                    SenderSteamId = notification.steamid_sender,
                    TargetSteamId = 0,
                    Message = msgRaw ?? string.Empty,
                    MessageType = MessageType.ChatMessage,
                    Timestamp = notification.timestamp > 0 ? (long)notification.timestamp : DateTimeOffset.UtcNow.ToUnixTimeSeconds(),
                    IsEcho = notification.steamid_sender == (manager.SteamClient.SteamID?.ConvertToUInt64() ?? 0),
                    ChatGroupId = notification.chat_group_id,
                    ChatId = notification.chat_id,
                    Ordinal = notification.ordinal,
                };

                await channel.Writer.WriteAsync(messageEvent);
            }
            catch (Exception ex)
            {
                _logger.LogError(ex, "Error processing group message notification");
            }
        }

        async void OnDirectMessageReceived(object? sender, CFriendMessages_IncomingMessage_Notification notification)
        {
            try
            {
                // chat_entry_type: 1 = ChatMsg, 2 = Typing, 3 = InviteGame
                if (notification.chat_entry_type != 1 && notification.chat_entry_type != 2 && notification.chat_entry_type != 3)
                {
                    _logger.LogDebug("Ignoring direct message notification with unhandled chat_entry_type={ChatEntryType} from {SteamId}",
                        notification.chat_entry_type, notification.steamid_friend);
                    return;
                }

                var botSteamId = manager.SteamClient.SteamID?.ConvertToUInt64() ?? 0;

                ulong senderSteamId;
                ulong targetSteamId;
                if (!notification.local_echo)
                {
                    senderSteamId = notification.steamid_friend;
                    targetSteamId = botSteamId;
                }
                else
                {
                    senderSteamId = botSteamId;
                    targetSteamId = notification.steamid_friend;
                }

                MessageEvent messageEvent;
                if (notification.chat_entry_type == 2)
                {
                    messageEvent = new MessageEvent
                    {
                        SenderSteamId = senderSteamId,
                        TargetSteamId = targetSteamId,
                        Message = string.Empty,
                        MessageType = MessageType.Typing,
                        Timestamp = (long)notification.rtime32_server_timestamp,
                        IsEcho = notification.local_echo,
                        ChatGroupId = 0,
                        ChatId = 0,
                        Ordinal = notification.ordinal,
                    };
                }
                else if (notification.chat_entry_type == 3)
                {
                    var msgRaw = notification.message?.TrimEnd('\0');
                    var msgNoBbCode = notification.message_no_bbcode?.TrimEnd('\0');
                    var parsed = ProcessMessageContent(msgRaw ?? string.Empty, msgNoBbCode ?? string.Empty);

                    messageEvent = new MessageEvent
                    {
                        SenderSteamId = senderSteamId,
                        TargetSteamId = targetSteamId,
                        Message = parsed.Text,
                        MessageType = MessageType.InviteGame,
                        Timestamp = (long)notification.rtime32_server_timestamp,
                        IsEcho = notification.local_echo,
                        ChatGroupId = 0,
                        ChatId = 0,
                        Ordinal = notification.ordinal,
                        InviteAppId = parsed.InviteAppId,
                        InviteLobbyId = parsed.InviteLobbyId,
                        InviteConnect = parsed.InviteConnect,
                    };
                }
                else
                {
                    var msgRaw = notification.message?.TrimEnd('\0');
                    var msgNoBbCode = notification.message_no_bbcode?.TrimEnd('\0');
                    var parsed = ProcessMessageContent(msgRaw ?? string.Empty, msgNoBbCode ?? string.Empty);

                    messageEvent = new MessageEvent
                    {
                        SenderSteamId = senderSteamId,
                        TargetSteamId = targetSteamId,
                        Message = parsed.Text,
                        MessageType = parsed.Type,
                        Timestamp = (long)notification.rtime32_server_timestamp,
                        IsEcho = notification.local_echo,
                        ChatGroupId = 0,
                        ChatId = 0,
                        Ordinal = notification.ordinal,
                        InviteAppId = parsed.InviteAppId,
                        InviteLobbyId = parsed.InviteLobbyId,
                        InviteConnect = parsed.InviteConnect,
                    };
                }

                await channel.Writer.WriteAsync(messageEvent);
            }
            catch (Exception ex)
            {
                _logger.LogError(ex, "Error processing direct message notification");
            }
        }

        manager.MessageReceived += OnMessageReceived;
        manager.MessageEcho += OnMessageEcho;
        manager.GroupMessageReceived += OnGroupMessageReceived;
        manager.DirectMessageReceived += OnDirectMessageReceived;

        try
        {
            await foreach (var message in channel.Reader.ReadAllAsync(context.CancellationToken))
            {
                var (imageUrl, caption) = ParseImageMessage(message.Message);

                var protoMessage = new Proto.MessageEvent
                {
                    SenderSteamId = message.SenderSteamId,
                    TargetSteamId = message.TargetSteamId,
                    Message = !string.IsNullOrEmpty(imageUrl) ? caption : message.Message,
                    MessageType = MapToProtoMessageType(message.MessageType),
                    Timestamp = message.Timestamp,
                    IsEcho = message.IsEcho,
                    ChatGroupId = message.ChatGroupId,
                    ChatId = message.ChatId,
                    Ordinal = message.Ordinal,
                    InviteAppId = message.InviteAppId,
                    InviteLobbyId = message.InviteLobbyId,
                    InviteConnect = message.InviteConnect,
                };

                if (!string.IsNullOrEmpty(imageUrl))
                    protoMessage.ImageUrl = imageUrl;

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
            manager.MessageReceived -= OnMessageReceived;
            manager.MessageEcho -= OnMessageEcho;
            manager.GroupMessageReceived -= OnGroupMessageReceived;
            manager.DirectMessageReceived -= OnDirectMessageReceived;
            channel.Writer.TryComplete();
            _logger.LogInformation("Message subscription ended for steam_id: {SteamId}", request.SteamId);
        }
    }

    public override async Task<TypingNotificationResponse> SendTypingNotification(
        TypingNotificationRequest request,
        ServerCallContext context)
    {
        _logger.LogDebug("Received typing notification request to {SteamID}: {IsTyping}",
            request.TargetSteamId, request.IsTyping);

        var manager = _registry.Get(request.CallerSteamId.ToString())
            ?? throw new RpcException(new Status(StatusCode.NotFound,
                $"No Steam session for caller steam_id {request.CallerSteamId}"));

        try
        {
            if (!manager.IsLoggedOn)
            {
                _logger.LogWarning("Cannot send typing notification - not logged on to Steam");
                return new TypingNotificationResponse { Success = false };
            }

            var targetId = new SteamID(request.TargetSteamId);
            if (request.IsTyping)
            {
                manager.SteamFriends.SendChatMessage(targetId, EChatEntryType.Typing, string.Empty);
            }

            return new TypingNotificationResponse { Success = true };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing typing notification request");
            return new TypingNotificationResponse { Success = false };
        }
    }

    public override Task<UploadImageResponse> UploadImageToSteam(
        UploadImageRequest request,
        ServerCallContext context)
    {
        _logger.LogWarning("Steam UGC upload request denied: {Filename}, {MimeType}, {Size} bytes. " +
                          "Steam blocks UGC uploads from third-party clients.",
            request.Filename, request.MimeType, request.ImageData.Length);

        return Task.FromResult(new UploadImageResponse
        {
            Success = false,
            ErrorMessage = "Steam UGC upload not supported: Steam blocks image uploads from third-party clients. " +
                          "Configure 'public_media' in bridge settings to enable Matrix→Steam image sharing via public URLs."
        });
    }

    public override async Task<DownloadMediaResponse> DownloadMediaFromSteam(
        DownloadMediaRequest request,
        ServerCallContext context)
    {
        _logger.LogInformation("Received media download request: {Url}", request.MediaUrl);

        try
        {
            var (imageData, mimeType) = await _imageService.DownloadImageAsync(request.MediaUrl);

            return new DownloadMediaResponse
            {
                Success = true,
                MediaData = Google.Protobuf.ByteString.CopyFrom(imageData),
                MimeType = mimeType
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error downloading media: {Url}", request.MediaUrl);
            return new DownloadMediaResponse
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
            var userInfo = await _userInfoService.GetUserInfoAsync(request.CallerSteamId, request.SteamId);
            if (userInfo == null)
            {
                return new GetUserAvatarDataResponse
                {
                    Success = false,
                    ErrorMessage = "User not found or unable to retrieve user information"
                };
            }

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

    public override async Task<ChatMessageHistoryResponse> GetChatMessageHistory(
        ChatMessageHistoryRequest request,
        ServerCallContext context)
    {
        var manager = _registry.Get(request.CallerSteamId.ToString())
            ?? throw new RpcException(new Status(StatusCode.NotFound,
                $"No Steam session for caller steam_id {request.CallerSteamId}"));

        try
        {
            _logger.LogDebug("Getting chat message history for chat group {ChatGroupId}, chat {ChatId}",
                request.ChatGroupId, request.ChatId);

            var steamUnified = manager.SteamUnifiedMessages;
            if (steamUnified == null)
            {
                _logger.LogError("SteamUnifiedMessages is not available");
                return new ChatMessageHistoryResponse
                {
                    Success = false,
                    ErrorMessage = "Steam client not connected"
                };
            }

            bool isDM = request.ChatGroupId == 0;

            if (isDM)
                return await GetFriendMessageHistory(request, manager, steamUnified);
            else
                return await GetChatRoomMessageHistory(request, manager, steamUnified);
        }
        catch (TaskCanceledException ex)
        {
            _logger.LogError(ex, "Task was canceled while retrieving chat message history.");
            return new ChatMessageHistoryResponse
            {
                Success = false,
                ErrorMessage = "Failed to retrieve message history: A task was canceled."
            };
        }
        catch (RpcException)
        {
            throw;
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

    public override async Task<GetAppInfoResponse> GetAppInfo(
        GetAppInfoRequest request,
        ServerCallContext context)
    {
        _logger.LogDebug("Getting app info for AppId {AppId}, caller {CallerSteamId}",
            request.AppId, request.CallerSteamId);

        var manager = _registry.Get(request.CallerSteamId.ToString())
            ?? throw new RpcException(new Status(StatusCode.NotFound,
                $"No Steam session for caller steam_id {request.CallerSteamId}"));

        var name = await _appInfoService.GetAppNameAsync(manager.SteamApps, (uint)request.AppId);

        _logger.LogDebug("App info lookup for AppId {AppId}: found={Found}, name={Name}",
            request.AppId, name != null, name);

        return new GetAppInfoResponse
        {
            Found = name != null,
            Name = name ?? string.Empty
        };
    }

    private async Task<SendMessageResult> SendMessageAsync(SteamClientManager manager, ulong targetSteamId, string message, MessageType messageType = MessageType.ChatMessage)
    {
        if (!manager.IsLoggedOn)
            return new SendMessageResult { Success = false, ErrorMessage = "Not logged on to Steam" };

        try
        {
            var chatEntryType = MapToChatEntryType(messageType);

            _logger.LogInformation("Sending message to {SteamID}: {Message}", targetSteamId, message);

            var request = new CFriendMessages_SendMessage_Request
            {
                steamid = targetSteamId,
                chat_entry_type = (int)chatEntryType,
                message = message,
            };
            var job = manager.FriendMessagesService.SendMessage(request);
            var result = await job.ToTask();

            if (result == null || result.Result != EResult.OK)
                return new SendMessageResult { Success = false, ErrorMessage = $"Steam API: {result?.Result}" };

            return new SendMessageResult { Success = true, Timestamp = result.Body.server_timestamp, Ordinal = result.Body.ordinal };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error sending message to {SteamID}", targetSteamId);
            return new SendMessageResult { Success = false, ErrorMessage = $"Failed to send message: {ex.Message}" };
        }
    }

    private async Task<SendMessageResult> SendGroupMessageAsync(SteamClientManager manager, ulong chatGroupId, ulong chatId, string message)
    {
        if (!manager.IsLoggedOn)
            return new SendMessageResult { Success = false, ErrorMessage = "Not logged on" };

        try
        {
            var request = new CChatRoom_SendChatMessage_Request
            {
                chat_group_id = chatGroupId,
                chat_id = chatId,
                message = message,
            };
            var job = manager.ChatRoomService.SendChatMessage(request);
            var result = await job.ToTask();

            if (result == null || result.Result != EResult.OK)
                return new SendMessageResult { Success = false, ErrorMessage = $"Steam API: {result?.Result}" };

            return new SendMessageResult { Success = true, Timestamp = result.Body.server_timestamp, Ordinal = result.Body.ordinal };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error sending group message");
            return new SendMessageResult { Success = false, ErrorMessage = ex.Message };
        }
    }

    private async Task<ChatMessageHistoryResponse> GetFriendMessageHistory(
        ChatMessageHistoryRequest request,
        SteamClientManager manager,
        SteamUnifiedMessages steamUnified)
    {
        var mySteamId = manager.SteamClient.SteamID?.ConvertToUInt64() ?? 0;
        if (mySteamId == 0)
        {
            return new ChatMessageHistoryResponse
            {
                Success = false,
                ErrorMessage = "Cannot get own Steam ID"
            };
        }

        var friendMsgRequest = new CFriendMessages_GetRecentMessages_Request
        {
            steamid1 = mySteamId,
            steamid2 = request.ChatId,
            count = Math.Min(request.MaxCount, 100),
            bbcode_format = true
        };

        if (request.LastTime > 0 || request.LastOrdinal > 0)
        {
            if (request.Forward)
            {
                friendMsgRequest.rtime32_start_time = request.LastTime;
                friendMsgRequest.start_ordinal = request.LastOrdinal;
            }
            else
            {
                friendMsgRequest.time_last = request.LastTime;
                friendMsgRequest.ordinal_last = request.LastOrdinal;
            }
        }

        _logger.LogDebug("Sending FriendMessages.GetRecentMessages: SteamId1={SteamId1}, SteamId2={SteamId2}, Count={Count}",
            friendMsgRequest.steamid1, friendMsgRequest.steamid2, friendMsgRequest.count);

        var friendMessagesService = manager.FriendMessagesService;
        var job = friendMessagesService.GetRecentMessages(friendMsgRequest);
        var result = await job.ToTask();

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
            var myUniverse = manager.SteamClient.SteamID?.AccountUniverse ?? EUniverse.Public;

            foreach (var msg in response.messages)
            {
                var senderSteamId = new SteamID(msg.accountid, myUniverse, EAccountType.Individual);
                var parsed = ProcessHistoryMessageContent(msg.message ?? string.Empty);

                var historyMessage = new ChatHistoryMessage
                {
                    SenderSteamId = senderSteamId.ConvertToUInt64(),
                    Timestamp = msg.timestamp,
                    Ordinal = msg.ordinal,
                    MessageContent = parsed.Content,
                    MessageType = parsed.Type,
                    InviteAppId = parsed.InviteAppId,
                    InviteLobbyId = parsed.InviteLobbyId,
                    InviteConnect = parsed.InviteConnect
                };

                if (parsed.Type == Proto.MessageType.ChatMessage)
                {
                    var (imageUrl, caption) = ParseImageMessage(historyMessage.MessageContent);
                    if (!string.IsNullOrEmpty(imageUrl))
                    {
                        historyMessage.ImageUrl = imageUrl;
                        historyMessage.MessageContent = caption;
                    }
                }

                historyResponse.Messages.Add(historyMessage);
            }

            if (response.messages.Count > 0)
            {
                var lastMessage = response.messages.Last();
                historyResponse.NextTime = lastMessage.timestamp;
                historyResponse.NextOrdinal = lastMessage.ordinal;
            }
        }

        return historyResponse;
    }

    private async Task<ChatMessageHistoryResponse> GetChatRoomMessageHistory(
        ChatMessageHistoryRequest request,
        SteamClientManager manager,
        SteamUnifiedMessages steamUnified)
    {
        var historyRequest = new CChatRoom_GetMessageHistory_Request
        {
            chat_group_id = request.ChatGroupId,
            chat_id = request.ChatId,
            max_count = Math.Min(request.MaxCount, 100)
        };

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

        _logger.LogDebug("Sending ChatRoom.GetMessageHistory request: GroupId={GroupId}, ChatId={ChatId}, MaxCount={MaxCount}",
            historyRequest.chat_group_id, historyRequest.chat_id, historyRequest.max_count);

        var job = steamUnified.SendMessage<CChatRoom_GetMessageHistory_Request, CChatRoom_GetMessageHistory_Response>(
            "ChatRoom.GetMessageHistory#1", historyRequest);
        var result = await job.ToTask();

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
            var myUniverse = manager.SteamClient.SteamID?.AccountUniverse ?? EUniverse.Public;

            foreach (var msg in response.messages)
            {
                var senderSteamId = new SteamID(msg.sender, myUniverse, EAccountType.Individual);

                var historyMessage = new ChatHistoryMessage
                {
                    SenderSteamId = senderSteamId.ConvertToUInt64(),
                    Timestamp = msg.server_timestamp,
                    Ordinal = msg.ordinal,
                    MessageContent = msg.message ?? string.Empty,
                    MessageType = Proto.MessageType.ChatMessage
                };

                var (imageUrl, caption) = ParseImageMessage(historyMessage.MessageContent);
                if (!string.IsNullOrEmpty(imageUrl))
                {
                    historyMessage.ImageUrl = imageUrl;
                    historyMessage.MessageContent = caption;
                }

                historyResponse.Messages.Add(historyMessage);
            }

            if (response.messages.Count > 0)
            {
                var lastMessage = response.messages.Last();
                historyResponse.NextTime = lastMessage.server_timestamp;
                historyResponse.NextOrdinal = lastMessage.ordinal;
            }
        }

        return historyResponse;
    }

    private static (string? imageUrl, string caption) ParseImageMessage(string message)
    {
        if (string.IsNullOrEmpty(message))
            return (null, message);

        const string imagePrefix = "[Image: ";
        const string imageSuffix = "]";

        if (!message.StartsWith(imagePrefix))
            return (null, message);

        var endIndex = message.IndexOf(imageSuffix, imagePrefix.Length);
        if (endIndex == -1)
            return (null, message);

        var imageUrl = message.Substring(imagePrefix.Length, endIndex - imagePrefix.Length);
        var captionStart = endIndex + imageSuffix.Length;
        var caption = captionStart < message.Length
            ? message.Substring(captionStart).TrimStart()
            : string.Empty;

        return (imageUrl, caption);
    }

    // Plain-text fallback description plus the parsed invite metadata for the live message
    // path (ProcessMessageContent). Go builds the richer invite rendering from
    // InviteAppId/InviteLobbyId/InviteConnect; Text/Type remain a sane fallback on their own.
    private readonly record struct ParsedMessageContent(string Text, MessageType Type, ulong InviteAppId, string InviteLobbyId, string InviteConnect);

    // Same shape as ParsedMessageContent but for the backfill/history path
    // (ProcessHistoryMessageContent), which returns Proto.MessageType directly.
    private readonly record struct ParsedHistoryContent(string Content, Proto.MessageType Type, ulong InviteAppId, string InviteLobbyId, string InviteConnect);

    // Extracts appid/lobbyid/connect attributes from an invite bbcode tag, e.g.
    // [gameinvite appid="1422450" connect="party_id:102060294209035138"][/gameinvite] or
    // [lobbyinvite appid=X lobbyid=Y]. Any attribute not present in the tag defaults to
    // 0 (appid) or empty (lobbyid/connect).
    private static (ulong appId, string lobbyId, string connect) ExtractInviteFields(string raw)
    {
        ulong appId = 0;
        var appIdMatch = Regex.Match(raw, @"appid=""?(\d+)""?", RegexOptions.IgnoreCase);
        if (appIdMatch.Success)
            ulong.TryParse(appIdMatch.Groups[1].Value, out appId);

        var lobbyIdMatch = Regex.Match(raw, @"lobbyid=""?(\d+)""?", RegexOptions.IgnoreCase);
        var lobbyId = lobbyIdMatch.Success ? lobbyIdMatch.Groups[1].Value : string.Empty;

        // connect's value is not purely numeric (e.g. "party_id:102060294209035138"), so
        // capture everything up to the closing quote/whitespace/bracket rather than \d+.
        var connectMatch = Regex.Match(raw, @"connect=""?([^""\s\]]+)""?", RegexOptions.IgnoreCase);
        var connect = connectMatch.Success ? connectMatch.Groups[1].Value : string.Empty;

        return (appId, lobbyId, connect);
    }

    private static ParsedMessageContent ProcessMessageContent(string raw, string noBbCode)
    {
        bool isInvite = raw.StartsWith("[lobbyinvite", StringComparison.OrdinalIgnoreCase)
                     || raw.StartsWith("[joingame", StringComparison.OrdinalIgnoreCase)
                     || raw.StartsWith("[gameinvite", StringComparison.OrdinalIgnoreCase);

        if (isInvite)
        {
            var (inviteAppId, inviteLobbyId, inviteConnect) = ExtractInviteFields(raw);

            if (!string.IsNullOrWhiteSpace(noBbCode))
                return new ParsedMessageContent(noBbCode, MessageType.InviteGame, inviteAppId, inviteLobbyId, inviteConnect);

            var text = inviteAppId != 0
                ? $"Invited you to play a game (App ID: {inviteAppId})"
                : "Invited you to play a game";
            return new ParsedMessageContent(text, MessageType.InviteGame, inviteAppId, inviteLobbyId, inviteConnect);
        }

        return new ParsedMessageContent(raw, MessageType.ChatMessage, 0, string.Empty, string.Empty);
    }

    private static ParsedHistoryContent ProcessHistoryMessageContent(string raw)
    {
        if (raw.StartsWith("[lobbyinvite", StringComparison.OrdinalIgnoreCase)
            || raw.StartsWith("[gameinvite", StringComparison.OrdinalIgnoreCase))
        {
            var (inviteAppId, inviteLobbyId, inviteConnect) = ExtractInviteFields(raw);
            var text = inviteAppId != 0
                ? $"Invited you to play a game (App ID: {inviteAppId})"
                : "Invited you to play a game";
            return new ParsedHistoryContent(text, Proto.MessageType.InviteGame, inviteAppId, inviteLobbyId, inviteConnect);
        }

        if (raw.StartsWith("[joingame", StringComparison.OrdinalIgnoreCase))
            return new ParsedHistoryContent("Invited you to join a game", Proto.MessageType.InviteGame, 0, string.Empty, string.Empty);

        return new ParsedHistoryContent(raw, Proto.MessageType.ChatMessage, 0, string.Empty, string.Empty);
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

    private static EChatEntryType MapToChatEntryType(MessageType messageType)
    {
        return messageType switch
        {
            MessageType.ChatMessage => EChatEntryType.ChatMsg,
            MessageType.Typing => EChatEntryType.Typing,
            MessageType.Emote => EChatEntryType.ChatMsg,
            MessageType.InviteGame => EChatEntryType.InviteGame,
            _ => EChatEntryType.ChatMsg
        };
    }

    private static MessageType MapFromChatEntryType(EChatEntryType entryType)
    {
        return entryType switch
        {
            EChatEntryType.ChatMsg => MessageType.ChatMessage,
            EChatEntryType.Typing => MessageType.Typing,
            EChatEntryType.InviteGame => MessageType.InviteGame,
            _ => MessageType.ChatMessage
        };
    }
}

public class SendMessageResult
{
    public bool Success { get; set; }
    public string? ErrorMessage { get; set; }
    public long Timestamp { get; set; }
    public uint Ordinal { get; set; }
}

public class MessageEvent
{
    public ulong SenderSteamId { get; set; }
    public ulong TargetSteamId { get; set; }
    public string Message { get; set; } = string.Empty;
    public MessageType MessageType { get; set; }
    public long Timestamp { get; set; }
    public bool IsEcho { get; set; }
    public string ImageUrl { get; set; } = string.Empty;
    public ulong ChatGroupId { get; set; }
    public ulong ChatId { get; set; }
    public uint Ordinal { get; set; }
    public ulong InviteAppId { get; set; }
    public string InviteLobbyId { get; set; } = string.Empty;
    public string InviteConnect { get; set; } = string.Empty;
}

public enum MessageType
{
    ChatMessage,
    Typing,
    Emote,
    InviteGame
}
