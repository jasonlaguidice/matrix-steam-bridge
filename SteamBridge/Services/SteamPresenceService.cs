using Grpc.Core;
using Microsoft.Extensions.Logging;
using SteamBridge.Proto;
using SteamKit2;

namespace SteamBridge.Services;

public class SteamPresenceService : Proto.SteamPresenceService.SteamPresenceServiceBase
{
    private readonly ILogger<SteamPresenceService> _logger;
    private readonly SteamClientManager _steamClientManager;

    public SteamPresenceService(
        ILogger<SteamPresenceService> logger,
        SteamClientManager steamClientManager)
    {
        _logger = logger;
        _steamClientManager = steamClientManager;
    }

    public override Task<SetPersonaStateResponse> SetPersonaState(
        SetPersonaStateRequest request,
        ServerCallContext context)
    {
        try
        {
            // Validate client is logged on
            if (!_steamClientManager.IsLoggedOn)
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
            _steamClientManager.SteamFriends.SetPersonaState(steamState);

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
