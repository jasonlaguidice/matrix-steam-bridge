using SteamKit2;
using SteamKit2.Authentication;
using Microsoft.Extensions.Logging;
using System.Collections.Concurrent;
using System.Threading;

namespace SteamBridge.Services;

public class SteamAuthenticationService
{
    private readonly ILogger<SteamAuthenticationService> _logger;
    private readonly SteamClientManager _steamClientManager;
    private readonly ConcurrentDictionary<string, AuthSession> _activeAuthSessions;
    private readonly ConcurrentDictionary<string, QRAuthSessionInfo> _qrAuthSessions;
    private readonly ConcurrentDictionary<string, string> _sessionUsernames;
    
    public SteamAuthenticationService(
        ILogger<SteamAuthenticationService> logger,
        SteamClientManager steamClientManager)
    {
        _logger = logger;
        _steamClientManager = steamClientManager;
        _activeAuthSessions = new ConcurrentDictionary<string, AuthSession>();
        _qrAuthSessions = new ConcurrentDictionary<string, QRAuthSessionInfo>();
        _sessionUsernames = new ConcurrentDictionary<string, string>();
    }

    public async Task<CredentialsLoginResult> LoginWithCredentialsAsync(
        string username, 
        string password, 
        string? guardCode = null, 
        bool rememberPassword = false,
        string? emailCode = null)
    {
        try
        {
            _logger.LogInformation("Attempting credentials login for user: {Username}, HasGuardCode: {HasGuardCode}, HasEmailCode: {HasEmailCode}", username, !string.IsNullOrEmpty(guardCode), !string.IsNullOrEmpty(emailCode));

            // Ensure connected to Steam
            if (!_steamClientManager.IsConnected)
            {
                _logger.LogDebug("Steam client not connected, attempting to connect");
                var connected = await _steamClientManager.ConnectAsync();
                if (!connected)
                {
                    _logger.LogError("Failed to connect to Steam network");
                    return new CredentialsLoginResult
                    {
                        Success = false,
                        ErrorMessage = "Failed to connect to Steam network"
                    };
                }
                _logger.LogDebug("Successfully connected to Steam network");
            }

            // Begin authentication session
            var authSessionDetails = new AuthSessionDetails
            {
                Username = username,
                Password = password,
                Authenticator = new BridgeAuthenticator(guardCode, emailCode),
                PlatformType = (SteamKit2.Internal.EAuthTokenPlatformType)1, // SteamClient platform
                ClientOSType = EOSType.Win11,
                WebsiteID = "Unknown"
            };

            var authSession = await _steamClientManager.BeginAuthSessionViaCredentialsAsync(authSessionDetails);
            
            // Store the session for potential polling
            var sessionId = Guid.NewGuid().ToString();
            _activeAuthSessions[sessionId] = authSession;
            _sessionUsernames[sessionId] = username;
            
            bool shouldCleanupSession = true; // Flag to control session cleanup

            try
            {
                // Use SteamKit2's built-in polling mechanism without timeout
                // SteamKit2 handles its own timeouts and will properly throw AuthenticationException
                // when email verification or other 2FA is required
                
                try
                {
                    _logger.LogInformation("Starting authentication polling for user: {Username}", username);
                    
                    // PollingWaitForResultAsync handles all the polling logic internally
                    // Don't pass cancellation token - let SteamKit2 handle timeouts and 2FA detection
                    var pollResult = await authSession.PollingWaitForResultAsync();
                    
                    _logger.LogInformation("Authentication polling completed successfully for user: {Username}", username);

                    // Log on with the access token, including username for SteamKit2 compatibility
                    _steamClientManager.LogOn(pollResult.AccessToken, pollResult.RefreshToken, username);

                    // Wait for successful logon
                    var logonSuccess = await WaitForLogonAsync();
                    
                    _activeAuthSessions.TryRemove(sessionId, out _);
                    _sessionUsernames.TryRemove(sessionId, out _);

                    if (!logonSuccess)
                    {
                        return new CredentialsLoginResult
                        {
                            Success = false,
                            ErrorMessage = "Failed to log on to Steam"
                        };
                    }

                    _logger.LogInformation("Successfully authenticated user: {Username}", username);

                    return new CredentialsLoginResult
                    {
                        Success = true,
                        AccessToken = pollResult.AccessToken,
                        RefreshToken = pollResult.RefreshToken,
                        UserInfo = await GetCurrentUserInfoAsync()
                    };
                }
                catch (InvalidOperationException invalidOpEx) when (invalidOpEx.Message.Contains("email code"))
                {
                    _logger.LogInformation("Email verification required for user: {Username}, Exception: {Message}", username, invalidOpEx.Message);
                    shouldCleanupSession = false; // Preserve session for continuation
                    return new CredentialsLoginResult
                    {
                        Success = false,
                        RequiresEmailVerification = true,
                        ErrorMessage = "Email verification required",
                        SessionId = sessionId
                    };
                }
                catch (InvalidOperationException invalidOpEx) when (invalidOpEx.Message.Contains("device code"))
                {
                    _logger.LogInformation("SteamGuard verification required for user: {Username}, Exception: {Message}", username, invalidOpEx.Message);
                    shouldCleanupSession = false; // Preserve session for continuation
                    return new CredentialsLoginResult
                    {
                        Success = false,
                        RequiresGuard = true,
                        ErrorMessage = "SteamGuard authentication required",
                        SessionId = sessionId
                    };
                }
                catch (AuthenticationException authEx)
                {
                    _logger.LogWarning("Authentication exception for user {Username}: {Message}", username, authEx.Message);
                    
                    // Handle specific authentication errors based on EResult
                    switch (authEx.Result)
                    {
                        case EResult.AccountLoginDeniedNeedTwoFactor:
                        case EResult.TwoFactorCodeMismatch:
                            return new CredentialsLoginResult
                            {
                                Success = false,
                                RequiresGuard = true,
                                ErrorMessage = "SteamGuard authentication required",
                                SessionId = sessionId  // Return session ID for continuation
                            };
                            
                        case EResult.InvalidLoginAuthCode:
                            return new CredentialsLoginResult
                            {
                                Success = false,
                                RequiresEmailVerification = true,
                                ErrorMessage = "Email verification required",
                                SessionId = sessionId  // Return session ID for continuation
                            };
                            
                        case EResult.InvalidPassword:
                            return new CredentialsLoginResult
                            {
                                Success = false,
                                ErrorMessage = "Invalid username or password"
                            };
                            
                        case EResult.RateLimitExceeded:
                            return new CredentialsLoginResult
                            {
                                Success = false,
                                ErrorMessage = "Too many login attempts. Please try again later"
                            };
                            
                        default:
                            return new CredentialsLoginResult
                            {
                                Success = false,
                                ErrorMessage = $"Authentication failed: {authEx.Message}"
                            };
                    }
                }
            }
            finally
            {
                // Cleanup the session only if not needed for continuation
                if (shouldCleanupSession)
                {
                    _activeAuthSessions.TryRemove(sessionId, out _);
                    _sessionUsernames.TryRemove(sessionId, out _);
                }
            }
        }
        catch (InvalidOperationException invalidOpEx) when (invalidOpEx.Message.Contains("SteamGuard"))
        {
            _logger.LogDebug("SteamGuard required for user: {Username}", username);
            return new CredentialsLoginResult
            {
                Success = false,
                RequiresGuard = true,
                ErrorMessage = "SteamGuard authentication required"
            };
        }
        catch (InvalidOperationException invalidOpEx) when (invalidOpEx.Message.Contains("email"))
        {
            _logger.LogDebug("Email verification required for user: {Username}", username);
            return new CredentialsLoginResult
            {
                Success = false,
                RequiresEmailVerification = true,
                ErrorMessage = "Email verification required"
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error during credentials login for user: {Username}, Exception type: {ExceptionType}, Message: {Message}", username, ex.GetType().Name, ex.Message);
            return new CredentialsLoginResult
            {
                Success = false,
                ErrorMessage = $"Authentication failed: {ex.Message}"
            };
        }
    }

    public async Task<QRLoginResult> StartQRLoginAsync()
    {
        try
        {
            _logger.LogInformation("Starting QR code authentication");

            // Ensure connected to Steam
            if (!_steamClientManager.IsConnected)
            {
                var connected = await _steamClientManager.ConnectAsync();
                if (!connected)
                {
                    return new QRLoginResult
                    {
                        Success = false,
                        ErrorMessage = "Failed to connect to Steam network"
                    };
                }
            }

            // Begin QR authentication session
            var qrAuthSession = await _steamClientManager.BeginAuthSessionViaQRAsync();
            
            // Log URL details for debugging
            _logger.LogInformation("Steam QR Challenge URL received: {Length} characters", qrAuthSession.ChallengeURL.Length);
            _logger.LogDebug("Steam QR Challenge URL: {Url}", qrAuthSession.ChallengeURL);
            
            // Prepare fallback message in case the URL cannot be processed by the client
            string qrCodeFallback = $"Use this URL with your Steam mobile app:\n{qrAuthSession.ChallengeURL}";

            // Store session info
            var sessionId = Guid.NewGuid().ToString();
            _qrAuthSessions[sessionId] = new QRAuthSessionInfo
            {
                QrAuthSession = qrAuthSession,
                StartTime = DateTime.UtcNow
            };

            _logger.LogInformation("QR authentication session created with ID: {SessionId}", sessionId);

            return new QRLoginResult
            {
                Success = true,
                SessionId = sessionId,
                ChallengeUrl = qrAuthSession.ChallengeURL,
                QRCodeFallback = qrCodeFallback
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error starting QR login");
            return new QRLoginResult
            {
                Success = false,
                ErrorMessage = $"Failed to start QR login: {ex.Message}"
            };
        }
    }

    public async Task<AuthStatusResult> GetAuthStatusAsync(string sessionId)
    {
        if (!_qrAuthSessions.TryGetValue(sessionId, out var sessionInfo))
        {
            return new AuthStatusResult
            {
                State = AuthState.Failed,
                ErrorMessage = "Invalid session ID"
            };
        }

        // Check for timeout (5 minutes)
        if (DateTime.UtcNow - sessionInfo.StartTime > TimeSpan.FromMinutes(5))
        {
            _qrAuthSessions.TryRemove(sessionId, out _);
            return new AuthStatusResult
            {
                State = AuthState.Expired,
                ErrorMessage = "QR code expired"
            };
        }

        try
        {
            // Use non-blocking polling to check authentication status
            var pollResult = await sessionInfo.QrAuthSession.PollAuthSessionStatusAsync();
            
            if (pollResult == null)
            {
                return new AuthStatusResult
                {
                    State = AuthState.Pending
                };
            }

            // DEBUG: Log all available properties from pollResult
            _logger.LogInformation("QR Authentication pollResult received:");
            _logger.LogInformation("  AccessToken: {AccessToken}", string.IsNullOrEmpty(pollResult.AccessToken) ? "NULL/EMPTY" : $"[{pollResult.AccessToken.Length} chars]");
            _logger.LogInformation("  RefreshToken: {RefreshToken}", string.IsNullOrEmpty(pollResult.RefreshToken) ? "NULL/EMPTY" : $"[{pollResult.RefreshToken.Length} chars]");
            
            // Log pollResult type and available properties using reflection
            var pollResultType = pollResult.GetType();
            _logger.LogInformation("  pollResult Type: {TypeName}", pollResultType.FullName);
            
            var properties = pollResultType.GetProperties();
            foreach (var prop in properties)
            {
                try
                {
                    var value = prop.GetValue(pollResult);
                    _logger.LogInformation("  Property {PropertyName} ({PropertyType}): {Value}", 
                        prop.Name, prop.PropertyType.Name, value?.ToString() ?? "NULL");
                }
                catch (Exception ex)
                {
                    _logger.LogWarning("  Property {PropertyName}: Error getting value - {Error}", prop.Name, ex.Message);
                }
            }

            // Authentication successful, log on
            // Use AccountName from pollResult as the username (required by SteamKit2)
            _steamClientManager.LogOn(pollResult.AccessToken, pollResult.RefreshToken, pollResult.AccountName);

            // Wait for successful logon
            var logonSuccess = await WaitForLogonAsync();
            
            _qrAuthSessions.TryRemove(sessionId, out _);

            if (!logonSuccess)
            {
                return new AuthStatusResult
                {
                    State = AuthState.Failed,
                    ErrorMessage = "Failed to log on to Steam"
                };
            }

            _logger.LogInformation("QR authentication successful for session: {SessionId}", sessionId);

            // Get current user info but preserve the AccountName from QR authentication
            var userInfo = await GetCurrentUserInfoAsync();
            if (userInfo != null)
            {
                // Override AccountName with the real account name from QR authentication
                userInfo.AccountName = pollResult.AccountName;
            }

            return new AuthStatusResult
            {
                State = AuthState.Authenticated,
                AccessToken = pollResult.AccessToken,
                RefreshToken = pollResult.RefreshToken,
                UserInfo = userInfo
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error checking auth status for session: {SessionId}", sessionId);
            return new AuthStatusResult
            {
                State = AuthState.Failed,
                ErrorMessage = $"Authentication failed: {ex.Message}"
            };
        }
    }

    public async Task<TokenReAuthResult> ReAuthenticateWithTokensAsync(
        string accessToken, 
        string refreshToken, 
        string username)
    {
        try
        {
            _logger.LogInformation("Attempting token re-authentication for user: {Username}", username);

            // Validate input parameters
            if (string.IsNullOrEmpty(refreshToken))
            {
                return new TokenReAuthResult
                {
                    Success = false,
                    State = AuthState.Failed,
                    ErrorMessage = "Refresh token is required for re-authentication"
                };
            }

            if (string.IsNullOrEmpty(username))
            {
                return new TokenReAuthResult
                {
                    Success = false,
                    State = AuthState.Failed,
                    ErrorMessage = "Username is required for re-authentication"
                };
            }

            // Ensure connected to Steam
            if (!_steamClientManager.IsConnected)
            {
                _logger.LogDebug("Steam client not connected, attempting to connect");
                var connected = await _steamClientManager.ConnectAsync();
                if (!connected)
                {
                    _logger.LogError("Failed to connect to Steam network");
                    return new TokenReAuthResult
                    {
                        Success = false,
                        State = AuthState.Failed,
                        ErrorMessage = "Failed to connect to Steam network"
                    };
                }
                _logger.LogDebug("Successfully connected to Steam network");
            }

            // Attempt to log on using stored tokens
            // Per SteamKit2 pattern, use RefreshToken as AccessToken for LogOnDetails
            _logger.LogDebug("Attempting logon with stored tokens for user: {Username}", username);
            _steamClientManager.LogOn(accessToken, refreshToken, username);

            // Wait for logon result
            var logonSuccess = await WaitForLogonAsync();
            
            if (!logonSuccess)
            {
                _logger.LogWarning("Token re-authentication failed for user: {Username} - logon unsuccessful", username);
                return new TokenReAuthResult
                {
                    Success = false,
                    State = AuthState.Expired,
                    ErrorMessage = "Stored credentials are no longer valid"
                };
            }

            _logger.LogInformation("Successfully re-authenticated user: {Username} with stored tokens", username);

            return new TokenReAuthResult
            {
                Success = true,
                State = AuthState.Authenticated,
                UserInfo = await GetCurrentUserInfoAsync(),
                // Note: For now, we don't refresh tokens - that would require additional SteamKit2 integration
                NewAccessToken = accessToken,  // Return original tokens since we don't refresh yet
                NewRefreshToken = refreshToken
            };
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error during token re-authentication for user: {Username}", username);
            return new TokenReAuthResult
            {
                Success = false,
                State = AuthState.Failed,
                ErrorMessage = $"Re-authentication failed: {ex.Message}"
            };
        }
    }

    public Task<bool> LogoutAsync()
    {
        _logger.LogInformation("Logging out from Steam");
        
        if (_steamClientManager.IsLoggedOn)
        {
            _steamClientManager.SteamUser.LogOff();
        }
        
        // Clear any active sessions
        _activeAuthSessions.Clear();
        _qrAuthSessions.Clear();
        _sessionUsernames.Clear();
        
        return Task.FromResult(true);
    }

    private async Task<bool> WaitForLogonAsync()
    {
        var timeout = TimeSpan.FromSeconds(30);
        var startTime = DateTime.UtcNow;
        
        while (!_steamClientManager.IsLoggedOn && DateTime.UtcNow - startTime < timeout)
        {
            await Task.Delay(100);
        }
        
        return _steamClientManager.IsLoggedOn;
    }

    private async Task<UserInfo?> GetCurrentUserInfoAsync()
    {
        if (!_steamClientManager.IsLoggedOn)
            return null;

        var steamFriends = _steamClientManager.SteamFriends;
        var steamId = _steamClientManager.SteamClient.SteamID;

        if (steamId == null)
            return null;

        return new UserInfo
        {
            SteamId = steamId.ConvertToUInt64(),
            PersonaName = steamFriends.GetPersonaName(),
            AccountName = steamId.Render(),
            Status = MapPersonaState(steamFriends.GetPersonaState()),
            CurrentGame = steamFriends.GetFriendGamePlayedName(steamId) ?? string.Empty
        };
    }

    private static PersonaState MapPersonaState(EPersonaState state)
    {
        return state switch
        {
            EPersonaState.Offline => PersonaState.Offline,
            EPersonaState.Online => PersonaState.Online,
            EPersonaState.Busy => PersonaState.Busy,
            EPersonaState.Away => PersonaState.Away,
            EPersonaState.Snooze => PersonaState.Snooze,
            EPersonaState.LookingToTrade => PersonaState.LookingToTrade,
            EPersonaState.LookingToPlay => PersonaState.LookingToPlay,
            EPersonaState.Invisible => PersonaState.Invisible,
            _ => PersonaState.Offline
        };
    }

    private class ConsoleAuthenticator : IAuthenticator
    {
        private readonly string? _guardCode;

        public ConsoleAuthenticator(string? guardCode)
        {
            _guardCode = guardCode;
        }

        public Task<string> GetDeviceCodeAsync(bool previousCodeWasIncorrect)
        {
            if (!string.IsNullOrEmpty(_guardCode))
            {
                return Task.FromResult(_guardCode);
            }
            
            throw new InvalidOperationException("SteamGuard code required but not provided");
        }

        public Task<string> GetEmailCodeAsync(string email, bool previousCodeWasIncorrect)
        {
            if (!string.IsNullOrEmpty(_guardCode))
            {
                return Task.FromResult(_guardCode);
            }
            
            throw new InvalidOperationException("Email verification code required but not provided");
        }

        public Task<bool> AcceptDeviceConfirmationAsync()
        {
            return Task.FromResult(true); // Accept device confirmations from Steam mobile app
        }
    }

    public async Task<CredentialsLoginResult> ContinueAuthSessionAsync(
        string sessionId, 
        string? guardCode = null, 
        string? emailCode = null)
    {
        _logger.LogInformation("Continuing auth session: {SessionId}, HasGuardCode: {HasGuardCode}, HasEmailCode: {HasEmailCode}", 
            sessionId, !string.IsNullOrEmpty(guardCode), !string.IsNullOrEmpty(emailCode));

        try
        {
            // Retrieve the stored auth session
            if (!_activeAuthSessions.TryGetValue(sessionId, out var authSession))
            {
                _logger.LogWarning("Auth session not found: {SessionId}", sessionId);
                return new CredentialsLoginResult
                {
                    Success = false,
                    ErrorMessage = "Authentication session not found or expired"
                };
            }

            // Retrieve the stored username for this session
            if (!_sessionUsernames.TryGetValue(sessionId, out var username))
            {
                _logger.LogWarning("Username not found for session: {SessionId}", sessionId);
                username = ""; // Fallback to empty string if not found
            }

            // Update the existing authenticator with the provided codes
            // The authSession still references the original BridgeAuthenticator
            if (authSession.Authenticator is BridgeAuthenticator bridgeAuth)
            {
                if (!string.IsNullOrEmpty(guardCode))
                {
                    bridgeAuth.SetGuardCode(guardCode);
                    _logger.LogDebug("Updated authenticator with guard code for session: {SessionId}", sessionId);
                }
                
                if (!string.IsNullOrEmpty(emailCode))
                {
                    bridgeAuth.SetEmailCode(emailCode);
                    _logger.LogDebug("Updated authenticator with email code for session: {SessionId}", sessionId);
                }
            }
            else
            {
                _logger.LogWarning("Auth session authenticator is not a BridgeAuthenticator: {SessionId}", sessionId);
                return new CredentialsLoginResult
                {
                    Success = false,
                    ErrorMessage = "Invalid authenticator type for session continuation"
                };
            }
            
            try
            {
                // Use a reasonable timeout for continuation attempts
                var cancellationTokenSource = new CancellationTokenSource(TimeSpan.FromSeconds(15));
                
                _logger.LogDebug("Continuing authentication session: {SessionId}", sessionId);
                
                var pollResult = await authSession.PollingWaitForResultAsync(cancellationTokenSource.Token);
                
                _logger.LogDebug("Authentication session completed successfully: {SessionId}", sessionId);

                // Log on with the access token
                _steamClientManager.LogOn(pollResult.AccessToken, pollResult.RefreshToken, username);

                // Wait for successful logon
                var logonSuccess = await WaitForLogonAsync();
                
                // Clean up the session
                _activeAuthSessions.TryRemove(sessionId, out _);
                _sessionUsernames.TryRemove(sessionId, out _);

                if (!logonSuccess)
                {
                    return new CredentialsLoginResult
                    {
                        Success = false,
                        ErrorMessage = "Failed to log on to Steam"
                    };
                }

                _logger.LogInformation("Successfully continued authentication session: {SessionId}", sessionId);

                return new CredentialsLoginResult
                {
                    Success = true,
                    AccessToken = pollResult.AccessToken,
                    RefreshToken = pollResult.RefreshToken,
                    UserInfo = await GetCurrentUserInfoAsync()
                };
            }
            catch (AuthenticationException authEx)
            {
                _logger.LogWarning("Authentication exception for session {SessionId}: {Message}", sessionId, authEx.Message);
                
                // Handle specific authentication errors - return requirements for next step
                switch (authEx.Result)
                {
                    case EResult.AccountLoginDeniedNeedTwoFactor:
                    case EResult.TwoFactorCodeMismatch:
                        return new CredentialsLoginResult
                        {
                            Success = false,
                            RequiresGuard = true,
                            ErrorMessage = "Incorrect SteamGuard code. Please try again.",
                            SessionId = sessionId
                        };
                        
                    case EResult.InvalidLoginAuthCode:
                        return new CredentialsLoginResult
                        {
                            Success = false,
                            RequiresEmailVerification = true,
                            ErrorMessage = "Incorrect email verification code. Please try again.",
                            SessionId = sessionId
                        };
                        
                    default:
                        _activeAuthSessions.TryRemove(sessionId, out _);
                        _sessionUsernames.TryRemove(sessionId, out _);
                        return new CredentialsLoginResult
                        {
                            Success = false,
                            ErrorMessage = $"Authentication failed: {authEx.Result}"
                        };
                }
            }
            catch (OperationCanceledException)
            {
                _logger.LogWarning("Authentication continuation timed out for session: {SessionId}", sessionId);
                return new CredentialsLoginResult
                {
                    Success = false,
                    RequiresGuard = true,
                    RequiresEmailVerification = true,
                    ErrorMessage = "Authentication timed out - please try again",
                    SessionId = sessionId
                };
            }
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Unexpected error continuing authentication session: {SessionId}", sessionId);
            _activeAuthSessions.TryRemove(sessionId, out _);
            _sessionUsernames.TryRemove(sessionId, out _);
            return new CredentialsLoginResult
            {
                Success = false,
                ErrorMessage = $"Authentication failed: {ex.Message}"
            };
        }
    }

    private class QRAuthSessionInfo
    {
        public required QrAuthSession QrAuthSession { get; set; }
        public DateTime StartTime { get; set; }
    }
}

public class CredentialsLoginResult
{
    public bool Success { get; set; }
    public string? ErrorMessage { get; set; }
    public string? AccessToken { get; set; }
    public string? RefreshToken { get; set; }
    public UserInfo? UserInfo { get; set; }
    public bool RequiresGuard { get; set; }
    public bool RequiresEmailVerification { get; set; }
    public string? SessionId { get; set; }
}

public class QRLoginResult
{
    public bool Success { get; set; }
    public string? ErrorMessage { get; set; }
    public string? SessionId { get; set; }
    public string? ChallengeUrl { get; set; }
    public string? QRCodeFallback { get; set; }
}

public class AuthStatusResult
{
    public AuthState State { get; set; }
    public string? ErrorMessage { get; set; }
    public string? AccessToken { get; set; }
    public string? RefreshToken { get; set; }
    public UserInfo? UserInfo { get; set; }
}

public enum AuthState
{
    Pending,
    Authenticated,
    Failed,
    Expired
}

public enum PersonaState
{
    Offline,
    Online, 
    Busy,
    Away,
    Snooze,
    LookingToTrade,
    LookingToPlay,
    Invisible
}

public class TokenReAuthResult
{
    public bool Success { get; set; }
    public string? ErrorMessage { get; set; }
    public AuthState State { get; set; }
    public string? NewAccessToken { get; set; }
    public string? NewRefreshToken { get; set; }
    public UserInfo? UserInfo { get; set; }
}

public class UserInfo
{
    public ulong SteamId { get; set; }
    public string AccountName { get; set; } = string.Empty;
    public string PersonaName { get; set; } = string.Empty;
    public string ProfileUrl { get; set; } = string.Empty;
    public string AvatarUrl { get; set; } = string.Empty;
    public string AvatarHash { get; set; } = string.Empty;
    public PersonaState Status { get; set; }
    public string CurrentGame { get; set; } = string.Empty;
}

/// <summary>
/// Custom authenticator that can provide both SteamGuard codes and email verification codes
/// Supports dynamic code updates for session continuation
/// </summary>
public class BridgeAuthenticator : IAuthenticator
{
    private readonly object _lock = new object();
    private string? _guardCode;
    private string? _emailCode;
    private readonly TaskCompletionSource<string>? _guardCodeTcs;
    private readonly TaskCompletionSource<string>? _emailCodeTcs;
    private readonly bool _waitForCodes;
    
    public BridgeAuthenticator(string? guardCode = null, string? emailCode = null, bool waitForCodes = false)
    {
        lock (_lock)
        {
            _guardCode = guardCode;
            _emailCode = emailCode;
            _waitForCodes = waitForCodes;
            
            if (_waitForCodes)
            {
                _guardCodeTcs = new TaskCompletionSource<string>();
                _emailCodeTcs = new TaskCompletionSource<string>();
            }
        }
    }
    
    public void SetGuardCode(string guardCode)
    {
        lock (_lock)
        {
            _guardCode = guardCode;
            _guardCodeTcs?.TrySetResult(guardCode);
        }
    }
    
    public void SetEmailCode(string emailCode)
    {
        lock (_lock)
        {
            _emailCode = emailCode;
            _emailCodeTcs?.TrySetResult(emailCode);
        }
    }
    
    public async Task<string> GetDeviceCodeAsync(bool previousCodeWasIncorrect)
    {
        lock (_lock)
        {
            if (!string.IsNullOrEmpty(_guardCode))
            {
                return Task.FromResult(_guardCode).Result;
            }
            
            if (!_waitForCodes)
            {
                throw new InvalidOperationException("No device code was provided for authentication");
            }
        }
        
        // Wait for code to be provided
        if (_guardCodeTcs != null)
        {
            return await _guardCodeTcs.Task;
        }
        
        throw new InvalidOperationException("No device code was provided for authentication");
    }
    
    public async Task<string> GetEmailCodeAsync(string email, bool previousCodeWasIncorrect)
    {
        lock (_lock)
        {
            if (!string.IsNullOrEmpty(_emailCode))
            {
                return Task.FromResult(_emailCode).Result;
            }
            
            if (!_waitForCodes)
            {
                throw new InvalidOperationException("No email code was provided for authentication");
            }
        }
        
        // Wait for code to be provided
        if (_emailCodeTcs != null)
        {
            return await _emailCodeTcs.Task;
        }
        
        throw new InvalidOperationException("No email code was provided for authentication");
    }
    
    public Task<bool> AcceptDeviceConfirmationAsync()
    {
        return Task.FromResult(true);
    }
}