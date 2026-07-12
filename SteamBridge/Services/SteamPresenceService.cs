using Grpc.Core;
using Microsoft.Extensions.Logging;
using SteamBridge.Proto;
using SteamKit2;
using System.Threading.Channels;

namespace SteamBridge.Services;

public class SteamPresenceService : Proto.SteamPresenceService.SteamPresenceServiceBase
{
    // Rich presence localization is always requested in English; no per-user language setting
    // exists anywhere else in this codebase today (ClientLanguage is likewise hardcoded to
    // "english" in SteamClientManager.LogOn).
    private const string RichPresenceLanguage = "english";

    private readonly ILogger<SteamPresenceService> _logger;
    private readonly SteamClientRegistry _registry;
    private readonly RichPresenceLocalizationService _richPresenceLocalization;

    public SteamPresenceService(
        ILogger<SteamPresenceService> logger,
        SteamClientRegistry registry,
        RichPresenceLocalizationService richPresenceLocalization)
    {
        _logger = logger;
        _registry = registry;
        _richPresenceLocalization = richPresenceLocalization;
    }

    public override Task<SetPersonaStateResponse> SetPersonaState(
        SetPersonaStateRequest request,
        ServerCallContext context)
    {
        var manager = _registry.Get(request.SteamId.ToString());
        if (manager == null)
        {
            _logger.LogWarning("No Steam session found for steam_id: {SteamId}", request.SteamId);
            return Task.FromResult(new SetPersonaStateResponse
            {
                Success = false,
                ErrorMessage = $"No Steam session found for steam_id {request.SteamId}"
            });
        }

        try
        {
            // Validate client is logged on
            if (!manager.IsLoggedOn)
            {
                _logger.LogWarning("Attempt to set persona state while not logged on");
                return Task.FromResult(new SetPersonaStateResponse
                {
                    Success = false,
                    ErrorMessage = "Not logged into Steam"
                });
            }

            // Validate PersonaState value
            if (!Enum.IsDefined(typeof(EPersonaState), (int)request.State))
            {
                _logger.LogWarning("Invalid PersonaState value: {State}", request.State);
                return Task.FromResult(new SetPersonaStateResponse
                {
                    Success = false,
                    ErrorMessage = $"Invalid PersonaState value: {request.State}"
                });
            }

            var steamState = (EPersonaState)request.State;

            _logger.LogInformation("Setting Steam PersonaState to: {State}", steamState);

            // Call SteamKit2 to set the persona state
            manager.SteamFriends.SetPersonaState(steamState);

            return Task.FromResult(new SetPersonaStateResponse
            {
                Success = true,
                ErrorMessage = string.Empty
            });
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error setting persona state to {State}", request.State);
            return Task.FromResult(new SetPersonaStateResponse
            {
                Success = false,
                ErrorMessage = $"Internal error: {ex.Message}"
            });
        }
    }

    public override async Task SubscribeToPresence(
        PresenceSubscriptionRequest request,
        IServerStreamWriter<Proto.PresenceEvent> responseStream,
        ServerCallContext context)
    {
        var richPresenceEnabled = request.RichPresenceEnabled;

        _logger.LogInformation(
            "New presence subscription started for steam_id: {SteamId}, rich_presence_enabled={RichPresenceEnabled}",
            request.SteamId, richPresenceEnabled);

        var manager = _registry.Get(request.SteamId.ToString())
            ?? throw new RpcException(new Status(StatusCode.NotFound,
                $"No Steam session for steam_id {request.SteamId}"));

        var channel = Channel.CreateUnbounded<Proto.PresenceEvent>();

        async void OnPersonaStateChange(object? sender, SteamFriends.PersonaStateCallback callback)
        {
            try
            {
                var steamFriends = manager.SteamFriends;
                var friendId = callback.FriendID;

                var cachedGameName = steamFriends.GetFriendGamePlayedName(friendId) ?? string.Empty;
                var cachedGameId = steamFriends.GetFriendGamePlayed(friendId);

                var presenceEvent = new Proto.PresenceEvent
                {
                    SteamId = friendId.ConvertToUInt64(),
                    Status = MapPersonaState(callback.State),
                    CurrentGame = cachedGameName,
                    GameAppId = cachedGameId.AppID,
                    Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds()
                };

                // Phase 2/3 fields are only ever set when the caller opted in - when disabled,
                // this callback is otherwise byte-for-byte identical to Phase 1 behavior.
                if (richPresenceEnabled)
                {
                    presenceEvent.HasRichPresence =
                        (callback.StateFlags & EPersonaStateFlag.HasRichPresence) == EPersonaStateFlag.HasRichPresence;
                }

                await channel.Writer.WriteAsync(presenceEvent);
            }
            catch (Exception ex)
            {
                _logger.LogError(ex, "Error processing persona state change for presence subscription");
            }
        }

        async void OnRichPresenceReceived(object? sender, RichPresenceReceivedEventArgs args)
        {
            try
            {
                var steamFriends = manager.SteamFriends;
                var friendId = new SteamID(args.SteamId);

                // Prefer AppId/GameName straight from the packet that produced these tokens
                // (see RichPresenceHandler's doc comment) over a separate SteamFriends cache
                // lookup, which has been observed to race with this same packet's own cache
                // write and read stale/zero values. Fall back to the cache only if this
                // specific packet's own fields happen to be empty, in case an earlier packet
                // already populated better data.
                var currentGame = !string.IsNullOrEmpty(args.GameName)
                    ? args.GameName
                    : steamFriends.GetFriendGamePlayedName(friendId) ?? string.Empty;
                var gameAppId = args.GameAppId != 0
                    ? args.GameAppId
                    : steamFriends.GetFriendGamePlayed(friendId).AppID;

                var presenceEvent = new Proto.PresenceEvent
                {
                    SteamId = args.SteamId,
                    Status = MapPersonaState(steamFriends.GetFriendPersonaState(friendId)),
                    CurrentGame = currentGame,
                    GameAppId = gameAppId,
                    Timestamp = DateTimeOffset.UtcNow.ToUnixTimeSeconds(),
                    HasRichPresence = true
                };

                foreach (var kv in args.Tokens)
                {
                    presenceEvent.RichPresenceTokens[kv.Key] = kv.Value;
                }

                var statusText = await _richPresenceLocalization.ResolveAsync(
                    manager.SteamUnifiedMessages, presenceEvent.GameAppId, RichPresenceLanguage, args.Tokens);
                presenceEvent.RichPresenceStatusText = statusText ?? string.Empty;

                await channel.Writer.WriteAsync(presenceEvent);
            }
            catch (Exception ex)
            {
                _logger.LogError(ex, "Error processing rich presence data for presence subscription");
            }
        }

        manager.PersonaStateChange += OnPersonaStateChange;

        if (richPresenceEnabled)
        {
            manager.RichPresenceReceived += OnRichPresenceReceived;
        }

        try
        {
            await foreach (var presenceEvent in channel.Reader.ReadAllAsync(context.CancellationToken))
            {
                await responseStream.WriteAsync(presenceEvent);

                _logger.LogDebug("Streamed presence event for steam_id {SteamId}: status={Status}, game={Game}",
                    presenceEvent.SteamId, presenceEvent.Status, presenceEvent.CurrentGame);
            }
        }
        catch (OperationCanceledException)
        {
            _logger.LogInformation("Presence subscription cancelled by client");
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error in presence subscription stream");
            throw new RpcException(new Status(StatusCode.Internal, $"Stream error: {ex.Message}"));
        }
        finally
        {
            manager.PersonaStateChange -= OnPersonaStateChange;

            if (richPresenceEnabled)
            {
                manager.RichPresenceReceived -= OnRichPresenceReceived;
            }

            channel.Writer.TryComplete();
            _logger.LogInformation("Presence subscription ended for steam_id: {SteamId}", request.SteamId);
        }
    }

    private static Proto.PersonaState MapPersonaState(EPersonaState state)
    {
        return state switch
        {
            EPersonaState.Offline => Proto.PersonaState.Offline,
            EPersonaState.Online => Proto.PersonaState.Online,
            EPersonaState.Busy => Proto.PersonaState.Busy,
            EPersonaState.Away => Proto.PersonaState.Away,
            EPersonaState.Snooze => Proto.PersonaState.Snooze,
            EPersonaState.LookingToTrade => Proto.PersonaState.LookingToTrade,
            EPersonaState.LookingToPlay => Proto.PersonaState.LookingToPlay,
            EPersonaState.Invisible => Proto.PersonaState.Invisible,
            _ => Proto.PersonaState.Offline
        };
    }
}
