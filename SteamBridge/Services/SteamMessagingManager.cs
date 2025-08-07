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
}

public enum MessageType
{
    ChatMessage,
    Typing,
    Emote,
    InviteGame
}