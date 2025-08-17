using Grpc.Core;
using SteamBridge.Proto;
using Microsoft.Extensions.Logging;

namespace SteamBridge.Services;

public class SteamAuthService : Proto.SteamAuthService.SteamAuthServiceBase
{
    private readonly ILogger<SteamAuthService> _logger;
    private readonly SteamAuthenticationService _authService;

    public SteamAuthService(
        ILogger<SteamAuthService> logger,
        SteamAuthenticationService authService)
    {
        _logger = logger;
        _authService = authService;
    }

    public override async Task<LoginResponse> LoginWithCredentials(
        CredentialsLoginRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received credentials login request for user: {Username}", request.Username);

        try
        {
            var result = await _authService.LoginWithCredentialsAsync(
                request.Username,
                request.Password,
                string.IsNullOrEmpty(request.GuardCode) ? null : request.GuardCode,
                request.RememberPassword,
                string.IsNullOrEmpty(request.EmailCode) ? null : request.EmailCode);

            var response = new LoginResponse
            {
                Success = result.Success,
                ErrorMessage = result.ErrorMessage ?? string.Empty,
                AccessToken = result.AccessToken ?? string.Empty,
                RefreshToken = result.RefreshToken ?? string.Empty,
                RequiresGuard = result.RequiresGuard,
                RequiresEmailVerification = result.RequiresEmailVerification,
                SessionId = result.SessionId ?? string.Empty
            };

            if (result.UserInfo != null)
            {
                response.UserInfo = MapToProtoUserInfo(result.UserInfo);
            }

            return response;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing credentials login request");
            return new LoginResponse
            {
                Success = false,
                ErrorMessage = $"Internal error: {ex.Message}"
            };
        }
    }

    public override async Task<QRLoginResponse> LoginWithQR(
        QRLoginRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received QR login request");

        try
        {
            var result = await _authService.StartQRLoginAsync();

            return new QRLoginResponse
            {
                ChallengeUrl = result.ChallengeUrl ?? string.Empty,
                QrCodeFallback = result.QRCodeFallback ?? string.Empty,
                SessionId = result.SessionId ?? string.Empty
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing QR login request");
            return new QRLoginResponse
            {
                ChallengeUrl = string.Empty,
                QrCodeFallback = $"Error: {ex.Message}",
                SessionId = string.Empty
            };
        }
    }

    public override async Task<AuthStatusResponse> GetAuthStatus(
        AuthStatusRequest request, 
        ServerCallContext context)
    {
        _logger.LogDebug("Checking auth status for session: {SessionId}", request.SessionId);

        try
        {
            var result = await _authService.GetAuthStatusAsync(request.SessionId);

            var response = new AuthStatusResponse
            {
                State = MapToProtoAuthState(result.State),
                ErrorMessage = result.ErrorMessage ?? string.Empty,
                AccessToken = result.AccessToken ?? string.Empty,
                RefreshToken = result.RefreshToken ?? string.Empty
            };

            if (result.UserInfo != null)
            {
                response.UserInfo = MapToProtoUserInfo(result.UserInfo);
            }

            return response;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error checking auth status");
            return new AuthStatusResponse
            {
                State = AuthStatusResponse.Types.AuthState.Failed,
                ErrorMessage = $"Internal error: {ex.Message}"
            };
        }
    }

    public override async Task<LoginResponse> ContinueAuthSession(
        ContinueAuthRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received auth session continuation request for session: {SessionId}", request.SessionId);

        try
        {
            var result = await _authService.ContinueAuthSessionAsync(
                request.SessionId,
                request.GuardCode,
                request.EmailCode);

            var response = new LoginResponse
            {
                Success = result.Success,
                ErrorMessage = result.ErrorMessage ?? string.Empty,
                AccessToken = result.AccessToken ?? string.Empty,
                RefreshToken = result.RefreshToken ?? string.Empty,
                RequiresGuard = result.RequiresGuard,
                RequiresEmailVerification = result.RequiresEmailVerification,
                SessionId = request.SessionId
            };

            if (result.UserInfo != null)
            {
                response.UserInfo = MapToProtoUserInfo(result.UserInfo);
            }

            return response;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing auth session continuation");
            return new LoginResponse
            {
                Success = false,
                ErrorMessage = $"Internal error: {ex.Message}",
                SessionId = request.SessionId
            };
        }
    }

    public override async Task<TokenReAuthResponse> ReAuthenticateWithTokens(
        TokenReAuthRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received token re-authentication request for user: {Username}", request.Username);

        try
        {
            var result = await _authService.ReAuthenticateWithTokensAsync(
                request.AccessToken,
                request.RefreshToken,
                request.Username);

            var response = new TokenReAuthResponse
            {
                Success = result.Success,
                ErrorMessage = result.ErrorMessage ?? string.Empty,
                State = MapToProtoAuthState(result.State),
                NewAccessToken = result.NewAccessToken ?? string.Empty,
                NewRefreshToken = result.NewRefreshToken ?? string.Empty
            };

            if (result.UserInfo != null)
            {
                response.UserInfo = MapToProtoUserInfo(result.UserInfo);
            }

            return response;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing token re-authentication request");
            return new TokenReAuthResponse
            {
                Success = false,
                ErrorMessage = $"Internal error: {ex.Message}",
                State = AuthStatusResponse.Types.AuthState.Failed
            };
        }
    }

    public override async Task<LogoutResponse> Logout(
        LogoutRequest request, 
        ServerCallContext context)
    {
        _logger.LogInformation("Received logout request");

        try
        {
            var result = await _authService.LogoutAsync();
            return new LogoutResponse
            {
                Success = result
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error processing logout request");
            return new LogoutResponse
            {
                Success = false
            };
        }
    }

    private static Proto.UserInfo MapToProtoUserInfo(Services.UserInfo userInfo)
    {
        return new Proto.UserInfo
        {
            SteamId = userInfo.SteamId,
            AccountName = userInfo.AccountName,
            PersonaName = userInfo.PersonaName,
            ProfileUrl = userInfo.ProfileUrl,
            AvatarUrl = userInfo.AvatarUrl,
            Status = MapToProtoPersonaState(userInfo.Status),
            CurrentGame = userInfo.CurrentGame
        };
    }

    private static Proto.PersonaState MapToProtoPersonaState(Services.PersonaState state)
    {
        return state switch
        {
            Services.PersonaState.Offline => Proto.PersonaState.Offline,
            Services.PersonaState.Online => Proto.PersonaState.Online,
            Services.PersonaState.Busy => Proto.PersonaState.Busy,
            Services.PersonaState.Away => Proto.PersonaState.Away,
            Services.PersonaState.Snooze => Proto.PersonaState.Snooze,
            Services.PersonaState.LookingToTrade => Proto.PersonaState.LookingToTrade,
            Services.PersonaState.LookingToPlay => Proto.PersonaState.LookingToPlay,
            Services.PersonaState.Invisible => Proto.PersonaState.Invisible,
            _ => Proto.PersonaState.Offline
        };
    }

    private static AuthStatusResponse.Types.AuthState MapToProtoAuthState(Services.AuthState state)
    {
        return state switch
        {
            Services.AuthState.Pending => AuthStatusResponse.Types.AuthState.Pending,
            Services.AuthState.Authenticated => AuthStatusResponse.Types.AuthState.Authenticated,
            Services.AuthState.Failed => AuthStatusResponse.Types.AuthState.Failed,
            Services.AuthState.Expired => AuthStatusResponse.Types.AuthState.Expired,
            _ => AuthStatusResponse.Types.AuthState.Failed
        };
    }
}