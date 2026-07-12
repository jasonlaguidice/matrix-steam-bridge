using Microsoft.Extensions.Logging;
using SteamKit2;
using SteamKit2.Internal;

namespace SteamBridge.Services;

/// <summary>
/// Custom ClientMsgHandler that reads raw (unresolved) Steam Rich Presence key/value data
/// directly off the persona-state packets Steam already pushes automatically.
///
/// SteamKit2's high-level SteamFriends/PersonaStateCallback never surfaces this wire-level
/// data even though every CMsgClientPersonaState.Friend entry carries it (field "rich_presence"),
/// so this handler intercepts the same EMsg.ClientPersonaState packets SteamFriends itself
/// processes, using the documented multi-handler ClientMsgHandler extension point (see
/// ref/SteamKit/Samples/010_Extending/MyHandler.cs for the canonical pattern this follows -
/// SteamKit2 passes each incoming packet to every registered handler, so this doesn't
/// interfere with SteamFriends' own normal processing of the same packets).
///
/// This is a genuine push mechanism, not polling: Steam already sends these packets on its
/// own (observed roughly once a minute while a friend's rich presence is actively changing,
/// e.g. a game timer) as part of normal persona-state updates: no separate request is sent,
/// and no explicit request needs to be tracked/retried/re-issued.
///
/// An earlier version of this handler used the separate, explicit CMsgClientRichPresenceRequest
/// / CMsgClientRichPresenceInfo request-response pair (EMsg 7502/7503) instead. That mechanism
/// exists on the wire but proved unreliable in practice (repeatedly returned 0 tokens even for
/// a friend with confirmed-active rich presence) and required manually tracking when to
/// request/refresh data per friend. Reading rich_presence directly off the packets already
/// arriving via SteamFriends' own subscription is simpler and matches how the data actually
/// flows.
/// </summary>
public sealed class RichPresenceHandler : ClientMsgHandler
{
    private readonly ILogger<RichPresenceHandler> _logger;

    public RichPresenceHandler(ILogger<RichPresenceHandler> logger)
    {
        _logger = logger;
    }

    /// <summary>
    /// Raised when a persona-state packet carrying non-empty rich presence data arrives for a
    /// friend. Carries the friend's SteamID64 and the raw, unresolved key/value pairs Steam
    /// sent for them (e.g. "status" -&gt; "#DeadlockRP_HideoutSnack").
    /// </summary>
    public event EventHandler<RichPresenceReceivedEventArgs>? RichPresenceReceived;

    /// <inheritdoc/>
    public override void HandleMsg(IPacketMsg packetMsg)
    {
        if (packetMsg.MsgType != EMsg.ClientPersonaState)
        {
            return;
        }

        var state = new ClientMsgProtobuf<CMsgClientPersonaState>(packetMsg);

        foreach (var friend in state.Body.friends)
        {
            if (friend.rich_presence == null || friend.rich_presence.Count == 0)
            {
                continue;
            }

            var tokens = new Dictionary<string, string>();

            foreach (var kv in friend.rich_presence)
            {
                if (!string.IsNullOrEmpty(kv.key))
                {
                    tokens[kv.key] = kv.value ?? string.Empty;
                }
            }

            if (tokens.Count == 0)
            {
                continue;
            }

            // GameAppId/GameName come from this exact same friend entry, not a separate
            // SteamFriends cache lookup - observed empirically that friend.game_name is often
            // empty on packets that DO carry rich_presence (Steam apparently doesn't always
            // include the human-readable name on this type of push), and that a separate cache
            // lookup for the AppID via GetFriendGamePlayed() at event-handling time raced with
            // this packet's own cache write, reading 0 even when this exact packet's
            // game_played_app_id was already populated. Passing both directly from the packet
            // that produced the tokens avoids depending on cross-handler-processed cache state.
            RichPresenceReceived?.Invoke(this, new RichPresenceReceivedEventArgs(
                friend.friendid, friend.game_played_app_id, friend.game_name ?? string.Empty, tokens));
        }
    }
}

/// <summary>
/// Raw rich presence data for a single friend, as received from Steam. Values are unresolved
/// loc-file tokens (e.g. "#DeadlockRP_HideoutSnack") or, for some keys, already-literal text.
/// </summary>
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
