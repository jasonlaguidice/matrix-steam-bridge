using Grpc.Core;
using Microsoft.Extensions.Logging;
using SteamBridge.Proto;
using SteamKit2;

namespace SteamBridge.Services;

public class SteamPresenceService : Proto.SteamPresenceService.SteamPresenceServiceBase
{
    private readonly ILogger<SteamPresenceService> _logger;
    private readonly SteamClientRegistry _registry;

    public SteamPresenceService(
        ILogger<SteamPresenceService> logger,
        SteamClientRegistry registry)
    {
        _logger = logger;
        _registry = registry;
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
}
