using SteamKit2;
using Microsoft.Extensions.Logging;
using System.Collections.Concurrent;
using System.Threading.Channels;

namespace SteamBridge.Services;

public class SteamMessagingManager : IDisposable
{
    private readonly ILogger<SteamMessagingManager> _logger;
    private readonly SteamClientManager _steamClientManager;
    private readonly Channel<MessageEvent> _messageChannel;
    private readonly ChannelWriter<MessageEvent> _messageWriter;
    private readonly ChannelReader<MessageEvent> _messageReader;
    private readonly ConcurrentDictionary<string, bool> _activeSubscriptions;

    public SteamMessagingManager(
        ILogger<SteamMessagingManager> logger,
        SteamClientManager steamClientManager)
    {
        _logger = logger;
        _steamClientManager = steamClientManager;
        
        // Create unbounded channel for message events
        var channel = Channel.CreateUnbounded<MessageEvent>();
        _messageChannel = channel;
        _messageWriter = channel.Writer;
        _messageReader = channel.Reader;
        
        _activeSubscriptions = new ConcurrentDictionary<string, bool>();

        // Subscribe to Steam message events
        _steamClientManager.MessageReceived += OnMessageReceived;
        _steamClientManager.MessageEcho += OnMessageEcho;
        _steamClientManager.GroupMessageReceived += OnGroupMessageReceived;
        _steamClientManager.DirectMessageReceived += OnDirectMessageReceived;
        
        _logger.LogInformation("SteamMessagingManager initialized");
    }

    public async Task<SendMessageResult> SendMessageAsync(ulong targetSteamId, string message, MessageType messageType = MessageType.ChatMessage)
    {
        if (!_steamClientManager.IsLoggedOn)
        {
            return new SendMessageResult
            {
                Success = false,
                ErrorMessage = "Not logged on to Steam"
            };
        }

        try
        {
            var targetId = new SteamID(targetSteamId);
            var steamFriends = _steamClientManager.SteamFriends;
            var chatEntryType = MapToChatEntryType(messageType);

            _logger.LogInformation("Sending message to {SteamID}: {Message}", targetSteamId, message);

            steamFriends.SendChatMessage(targetId, chatEntryType, message);

            return new SendMessageResult
            {
                Success = true,
                Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds()
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error sending message to {SteamID}", targetSteamId);
            return new SendMessageResult
            {
                Success = false,
                ErrorMessage = $"Failed to send message: {ex.Message}"
            };
        }
    }

    public async Task<SendMessageResult> SendGroupMessageAsync(
        ulong chatGroupId, ulong chatId, string message)
    {
        if (!_steamClientManager.IsLoggedOn)
            return new SendMessageResult { Success = false, ErrorMessage = "Not logged on" };

        try
        {
            var request = new SteamKit2.Internal.CChatRoom_SendChatMessage_Request
            {
                chat_group_id = chatGroupId,
                chat_id = chatId,
                message = message,
            };
            var job = _steamClientManager.ChatRoomService.SendChatMessage(request);
            var result = await job.ToTask();

            if (result == null || result.Result != SteamKit2.EResult.OK)
                return new SendMessageResult { Success = false, ErrorMessage = $"Steam API: {result?.Result}" };

            return new SendMessageResult
            {
                Success = true,
                Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds(),
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error sending group message");
            return new SendMessageResult { Success = false, ErrorMessage = ex.Message };
        }
    }

    public async Task<bool> SendTypingNotificationAsync(ulong targetSteamId, bool isTyping)
    {
        if (!_steamClientManager.IsLoggedOn)
        {
            _logger.LogWarning("Cannot send typing notification - not logged on to Steam");
            return false;
        }

        try
        {
            var targetId = new SteamID(targetSteamId);
            var steamFriends = _steamClientManager.SteamFriends;
            
            if (isTyping)
            {
                steamFriends.SendChatMessage(targetId, EChatEntryType.Typing, string.Empty);
            }

            _logger.LogDebug("Sent typing notification to {SteamID}: {IsTyping}", targetSteamId, isTyping);
            return true;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error sending typing notification to {SteamID}", targetSteamId);
            return false;
        }
    }

    public IAsyncEnumerable<MessageEvent> SubscribeToMessagesAsync(CancellationToken cancellationToken = default)
    {
        var subscriptionId = Guid.NewGuid().ToString();
        _activeSubscriptions[subscriptionId] = true;
        
        _logger.LogInformation("New message subscription created: {SubscriptionId}", subscriptionId);

        return ReadMessagesAsync(subscriptionId, cancellationToken);
    }

    private async IAsyncEnumerable<MessageEvent> ReadMessagesAsync(
        string subscriptionId, 
        [System.Runtime.CompilerServices.EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        try
        {
            while (!cancellationToken.IsCancellationRequested && 
                   _activeSubscriptions.ContainsKey(subscriptionId))
            {
                MessageEvent message;
                try
                {
                    message = await _messageReader.ReadAsync(cancellationToken);
                }
                catch (OperationCanceledException)
                {
                    break;
                }
                catch (InvalidOperationException)
                {
                    // Channel was completed
                    break;
                }
                
                yield return message;
            }
        }
        finally
        {
            _activeSubscriptions.TryRemove(subscriptionId, out _);
            _logger.LogInformation("Message subscription ended: {SubscriptionId}", subscriptionId);
        }
    }

    private async void OnMessageReceived(object? sender, SteamFriends.FriendMsgCallback callback)
    {
        try
        {
            var messageEvent = new MessageEvent
            {
                SenderSteamId = callback.Sender.ConvertToUInt64(),
                TargetSteamId = _steamClientManager.SteamClient.SteamID?.ConvertToUInt64() ?? 0,
                Message = callback.Message.TrimEnd('\0'), // Remove null terminators
                MessageType = MapFromChatEntryType(callback.EntryType),
                Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds(),
                IsEcho = false
            };

            _logger.LogInformation("Received message from {SenderID}: {Message}", 
                callback.Sender, messageEvent.Message);

            await _messageWriter.WriteAsync(messageEvent);
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing received message");
        }
    }

    private async void OnGroupMessageReceived(object? sender, SteamKit2.Internal.CChatRoom_IncomingChatMessage_Notification notification)
    {
        try
        {
            // Skip server messages (system events, not user chat)
            if (notification.server_message != null) return;

            var msgNoBbCode = notification.message_no_bbcode?.TrimEnd('\0');
            var msgRaw = notification.message?.TrimEnd('\0');

            var messageEvent = new MessageEvent
            {
                SenderSteamId = notification.steamid_sender,
                TargetSteamId = 0, // Group messages have no single target
                Message = msgRaw ?? string.Empty,
                MessageType = MessageType.ChatMessage,
                Timestamp = notification.timestamp > 0 ? (long)notification.timestamp : DateTimeOffset.UtcNow.ToUnixTimeSeconds(),
                IsEcho = notification.steamid_sender == (_steamClientManager.SteamClient.SteamID?.ConvertToUInt64() ?? 0),
                ChatGroupId = notification.chat_group_id,
                ChatId = notification.chat_id,
                Ordinal = notification.ordinal,
            };

            _logger.LogDebug("Group message from {Sender} in group {GroupId} channel {ChatId}",
                notification.steamid_sender, notification.chat_group_id, notification.chat_id);

            await _messageWriter.WriteAsync(messageEvent);
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing group message notification");
        }
    }

    private async void OnDirectMessageReceived(object? sender, SteamKit2.Internal.CFriendMessages_IncomingMessage_Notification notification)
    {
        try
        {
            // Only handle actual chat messages; skip typing indicators etc.
            if (notification.chat_entry_type != 1) return;

            var msgRaw = notification.message?.TrimEnd('\0');
            var text = msgRaw ?? string.Empty;

            var botSteamId = _steamClientManager.SteamClient.SteamID?.ConvertToUInt64() ?? 0;

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

            var messageEvent = new MessageEvent
            {
                SenderSteamId = senderSteamId,
                TargetSteamId = targetSteamId,
                Message = text,
                MessageType = MessageType.ChatMessage,
                Timestamp = (long)notification.rtime32_server_timestamp,
                IsEcho = notification.local_echo,
                ChatGroupId = 0,
                ChatId = 0,
            };

            _logger.LogInformation("DM {Direction} {PeerId}: {Message}",
                notification.local_echo ? "to" : "from",
                notification.steamid_friend,
                text);

            await _messageWriter.WriteAsync(messageEvent);
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing direct message notification");
        }
    }

    private async void OnMessageEcho(object? sender, SteamFriends.FriendMsgEchoCallback callback)
    {
        try
        {
            var messageEvent = new MessageEvent
            {
                SenderSteamId = _steamClientManager.SteamClient.SteamID?.ConvertToUInt64() ?? 0,
                TargetSteamId = callback.Recipient.ConvertToUInt64(),
                Message = callback.Message.TrimEnd('\0'), // Remove null terminators  
                MessageType = MapFromChatEntryType(callback.EntryType),
                Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds(),
                IsEcho = true
            };

            _logger.LogDebug("Message echo to {RecipientID}: {Message}", 
                callback.Recipient, messageEvent.Message);

            await _messageWriter.WriteAsync(messageEvent);
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing message echo");
        }
    }

    private static EChatEntryType MapToChatEntryType(MessageType messageType)
    {
        return messageType switch
        {
            MessageType.ChatMessage => EChatEntryType.ChatMsg,
            MessageType.Typing => EChatEntryType.Typing,
            MessageType.Emote => EChatEntryType.ChatMsg, // Use ChatMsg for emotes as EChatEntryType.Emote may not exist
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

    public void Dispose()
    {
        _logger.LogInformation("Disposing SteamMessagingManager");
        
        _messageWriter.Complete();
        _activeSubscriptions.Clear();
        
        _steamClientManager.MessageReceived -= OnMessageReceived;
        _steamClientManager.MessageEcho -= OnMessageEcho;
        _steamClientManager.GroupMessageReceived -= OnGroupMessageReceived;
        _steamClientManager.DirectMessageReceived -= OnDirectMessageReceived;
        
        _logger.LogInformation("SteamMessagingManager disposed");
    }
}

public class SendMessageResult
{
    public bool Success { get; set; }
    public string? ErrorMessage { get; set; }
    public long Timestamp { get; set; }
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
    public ulong ChatGroupId { get; set; }  // 0 for DMs
    public ulong ChatId { get; set; }       // 0 for DMs
    public uint Ordinal { get; set; }       // Message ordinal for deduplication with backfill
}

public enum MessageType
{
    ChatMessage,
    Typing,
    Emote,
    InviteGame
}