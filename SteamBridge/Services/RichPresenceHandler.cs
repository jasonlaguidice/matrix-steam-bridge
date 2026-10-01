using Microsoft.Extensions.Logging;
using SteamKit2;
using SteamKit2.Internal;

namespace SteamBridge.Services;


public sealed class RichPresenceHandler : ClientMsgHandler
{
    private readonly ILogger<RichPresenceHandler> _logger;


    private readonly Dictionary<ulong, Dictionary<string, string>> _richPresenceCache = new();
    private readonly Dictionary<ulong, uint> _lastKnownAppId = new();
    private readonly object _cacheLock = new();

    public RichPresenceHandler(ILogger<RichPresenceHandler> logger)
    {
        _logger = logger;
    }


    public event EventHandler<RichPresenceReceivedEventArgs>? RichPresenceReceived;

    /// <inheritdoc/>
    public override void HandleMsg(IPacketMsg packetMsg)
    {
        if (packetMsg.MsgType != EMsg.ClientPersonaState)
        {
            return;
        }

        var state = new ClientMsgProtobuf<CMsgClientPersonaState>(packetMsg);
        var flags = (EClientPersonaStateFlag)state.Body.status_flags;

        foreach (var friend in state.Body.friends)
        {
            var friendId = friend.friendid;
            Dictionary<string, string> snapshot;

            lock (_cacheLock)
            {
                // A friend's rich presence belongs to whatever game they're currently playing.
                // If this packet reports a different AppID (or that they've stopped playing)
                // than what we last saw for them, drop any cached tokens - they're leftovers
                // from the previous game/session.
                if ((flags & EClientPersonaStateFlag.GameDataBlob) == EClientPersonaStateFlag.GameDataBlob)
                {
                    var appId = friend.game_played_app_id;
                    if (!_lastKnownAppId.TryGetValue(friendId, out var lastAppId) || lastAppId != appId)
                    {
                        _richPresenceCache.Remove(friendId);
                    }
                    _lastKnownAppId[friendId] = appId;
                }

                if ((flags & EClientPersonaStateFlag.RichPresence) != EClientPersonaStateFlag.RichPresence ||
                    friend.rich_presence == null || friend.rich_presence.Count == 0)
                {
                    continue;
                }

                if (!_richPresenceCache.TryGetValue(friendId, out var cached))
                {
                    cached = new Dictionary<string, string>();
                    _richPresenceCache[friendId] = cached;
                }

                foreach (var kv in friend.rich_presence)
                {
                    if (string.IsNullOrEmpty(kv.key))
                    {
                        continue;
                    }

                    // Valve's documented SetRichPresence(key, value) convention treats an
                    // empty/null value as clearing that key - mirror that as a removal from the
                    // accumulated cache instead of storing an empty string.
                    if (string.IsNullOrEmpty(kv.value))
                    {
                        cached.Remove(kv.key);
                    }
                    else
                    {
                        cached[kv.key] = kv.value;
                    }
                }

                if (cached.Count == 0)
                {
                    continue;
                }

                snapshot = new Dictionary<string, string>(cached);
            }

            RichPresenceReceived?.Invoke(this, new RichPresenceReceivedEventArgs(
                friendId, friend.game_played_app_id, friend.game_name ?? string.Empty, snapshot));
        }
    }
}

public sealed class RichPresenceReceivedEventArgs : EventArgs
{
    public ulong SteamId { get; }
    public uint GameAppId { get; }
    public string GameName { get; }
    public IReadOnlyDictionary<string, string> Tokens { get; }

    public RichPresenceReceivedEventArgs(ulong steamId, uint gameAppId, string gameName, IReadOnlyDictionary<string, string> tokens)
    {
        SteamId = steamId;
        GameAppId = gameAppId;
        GameName = gameName;
        Tokens = tokens;
    }
}
