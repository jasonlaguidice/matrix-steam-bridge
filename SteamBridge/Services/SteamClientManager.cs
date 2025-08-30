using SteamKit2;
using SteamKit2.Authentication;
using Microsoft.Extensions.Logging;
using System.Collections.Concurrent;

namespace SteamBridge.Services;

public class SteamClientManager : IDisposable
{
    private readonly ILogger<SteamClientManager> _logger;
    private readonly SteamClient _steamClient;
    private readonly CallbackManager _callbackManager;
    private readonly SteamUser _steamUser;
    private readonly SteamFriends _steamFriends;
    private readonly SteamUnifiedMessages _steamUnifiedMessages;
    
    private readonly CancellationTokenSource _cancellationTokenSource;
    private readonly Task _callbackTask;
    
    private bool _isConnected;
    private bool _isLoggedOn;
    private string? _currentAccessToken;
    private string? _currentRefreshToken;

    public event EventHandler<SteamUser.LoggedOnCallback>? LoggedOn;
    public event EventHandler<SteamUser.LoggedOffCallback>? LoggedOff;
    public event EventHandler<SteamClient.ConnectedCallback>? Connected;
    public event EventHandler<SteamClient.DisconnectedCallback>? Disconnected;
    public event EventHandler<SteamFriends.FriendsListCallback>? FriendsListReceived;
    public event EventHandler<SteamFriends.PersonaStateCallback>? PersonaStateChange;
    public event EventHandler<SteamFriends.FriendMsgCallback>? MessageReceived;
    public event EventHandler<SteamFriends.FriendMsgEchoCallback>? MessageEcho;

    public bool IsConnected => _isConnected;
    public bool IsLoggedOn => _isLoggedOn;
    public SteamUser SteamUser => _steamUser;
    public SteamFriends SteamFriends => _steamFriends;
    public SteamClient SteamClient => _steamClient;
    public SteamUnifiedMessages SteamUnifiedMessages => _steamUnifiedMessages;

    public SteamClientManager(ILogger<SteamClientManager> logger)
    {
        _logger = logger;
        _steamClient = new SteamClient();
        _callbackManager = new CallbackManager(_steamClient);
        
        _steamUser = _steamClient.GetHandler<SteamUser>()!;
        _steamFriends = _steamClient.GetHandler<SteamFriends>()!;
        _steamUnifiedMessages = _steamClient.GetHandler<SteamUnifiedMessages>()!;
        
        _cancellationTokenSource = new CancellationTokenSource();
        
        // Subscribe to callbacks
        _callbackManager.Subscribe<SteamClient.ConnectedCallback>(OnConnected);
        _callbackManager.Subscribe<SteamClient.DisconnectedCallback>(OnDisconnected);
        _callbackManager.Subscribe<SteamUser.LoggedOnCallback>(OnLoggedOn);
        _callbackManager.Subscribe<SteamUser.LoggedOffCallback>(OnLoggedOff);
        _callbackManager.Subscribe<SteamFriends.FriendsListCallback>(OnFriendsListReceived);
        _callbackManager.Subscribe<SteamFriends.PersonaStateCallback>(OnPersonaStateChange);
        _callbackManager.Subscribe<SteamFriends.FriendMsgCallback>(OnMessageReceived);
        _callbackManager.Subscribe<SteamFriends.FriendMsgEchoCallback>(OnMessageEcho);
        
        // Start callback processing task
        _callbackTask = Task.Run(ProcessCallbacks, _cancellationTokenSource.Token);
        
        _logger.LogInformation("SteamClientManager initialized");
    }

    public async Task<bool> ConnectAsync()
    {
        if (_isConnected)
        {
            _logger.LogWarning("Already connected to Steam");
            return true;
        }

        _logger.LogInformation("Connecting to Steam...");
        _steamClient.Connect();
        
        // Wait for connection with timeout
        var timeout = TimeSpan.FromSeconds(30);
        var startTime = DateTime.UtcNow;
        
        while (!_isConnected && DateTime.UtcNow - startTime < timeout)
        {
            await Task.Delay(100);
        }
        
        if (!_isConnected)
        {
            _logger.LogError("Failed to connect to Steam within timeout");
            return false;
        }
        
        _logger.LogInformation("Successfully connected to Steam");
        return true;
    }

    public void Disconnect()
    {
        if (_isConnected)
        {
            _logger.LogInformation("Disconnecting from Steam...");
            _steamClient.Disconnect();
        }
    }

    public async Task<AuthSession> BeginAuthSessionViaCredentialsAsync(AuthSessionDetails details)
    {
        if (!_isConnected)
        {
            throw new InvalidOperationException("Must be connected to Steam before authenticating");
        }

        _logger.LogInformation("Beginning authentication session for user: {Username}", details.Username);
        return await _steamClient.Authentication.BeginAuthSessionViaCredentialsAsync(details);
    }

    public async Task<QrAuthSession> BeginAuthSessionViaQRAsync()
    {
        if (!_isConnected)
        {
            throw new InvalidOperationException("Must be connected to Steam before authenticating");
        }

        _logger.LogInformation("Beginning QR authentication session");
        
        var authSessionDetails = new AuthSessionDetails
        {
            PlatformType = (SteamKit2.Internal.EAuthTokenPlatformType)1, // SteamClient platform
            ClientOSType = EOSType.Win11,
            WebsiteID = "Client" // Masquerade as official Steam desktop client
        };
        
        return await _steamClient.Authentication.BeginAuthSessionViaQRAsync(authSessionDetails);
    }

    public void LogOn(string accessToken, string? refreshToken = null, string? username = null)
    {
        if (!_isConnected)
        {
            throw new InvalidOperationException("Must be connected to Steam before logging on");
        }

        // DEBUG: Log all input parameters
        _logger.LogInformation("LogOn called with parameters:");
        _logger.LogInformation("  AccessToken: {AccessToken}", string.IsNullOrEmpty(accessToken) ? "NULL/EMPTY" : $"[{accessToken.Length} chars]");
        _logger.LogInformation("  RefreshToken: {RefreshToken}", string.IsNullOrEmpty(refreshToken) ? "NULL/EMPTY" : $"[{refreshToken?.Length} chars]");
        _logger.LogInformation("  Username: {Username}", string.IsNullOrEmpty(username) ? "NULL/EMPTY" : $"[{username}]");

        _logger.LogInformation("Logging on to Steam with access token");
        _currentAccessToken = accessToken;
        _currentRefreshToken = refreshToken;
        
        // DEBUG: Log the LogOnDetails before calling SteamKit2
        var logOnDetails = new SteamUser.LogOnDetails
        {
            Username = username, // Username is required even with access token
            AccessToken = refreshToken ?? accessToken, // Use RefreshToken as AccessToken per SteamKit2 sample
            
            // Enhanced client masquerading to appear as official Steam client
            ClientOSType = EOSType.Win11, // Modern Windows client
            MachineName = Environment.MachineName, // Remove "(SteamKit2)" suffix
            UIMode = SteamKit2.EUIMode.ClientUI, // Desktop Steam client UI mode
            ChatMode = SteamUser.ChatMode.Default, // Keep default chat mode for compatibility with FriendMsgCallback
            ClientLanguage = "english",
            AccountInstance = SteamID.DesktopInstance, // PC Steam instance
            
            // Generate consistent LoginID for this session
            LoginID = GenerateStableLoginID()
        };
        
        _logger.LogInformation("Creating LogOnDetails with client masquerading:");
        _logger.LogInformation("  LogOnDetails.Username: {Username}", string.IsNullOrEmpty(logOnDetails.Username) ? "NULL/EMPTY" : $"[{logOnDetails.Username}]");
        _logger.LogInformation("  LogOnDetails.AccessToken: {AccessToken}", string.IsNullOrEmpty(logOnDetails.AccessToken) ? "NULL/EMPTY" : $"[{logOnDetails.AccessToken.Length} chars]");
        _logger.LogInformation("  ClientOSType: {OSType}, MachineName: {MachineName}", logOnDetails.ClientOSType, logOnDetails.MachineName);
        _logger.LogInformation("  UIMode: {UIMode}, ChatMode: {ChatMode}, LoginID: {LoginID}", logOnDetails.UIMode, logOnDetails.ChatMode, logOnDetails.LoginID);
        
        _steamUser.LogOn(logOnDetails);
    }

    private async Task ProcessCallbacks()
    {
        try
        {
            while (!_cancellationTokenSource.Token.IsCancellationRequested)
            {
                _callbackManager.RunWaitCallbacks(TimeSpan.FromMilliseconds(100));
                await Task.Delay(10, _cancellationTokenSource.Token);
            }
        }
        catch (OperationCanceledException)
        {
            _logger.LogInformation("Callback processing stopped");
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Error in callback processing");
        }
    }

    private void OnConnected(SteamClient.ConnectedCallback callback)
    {
        _isConnected = true;
        _logger.LogInformation("Connected to Steam");
        Connected?.Invoke(this, callback);
    }

    private void OnDisconnected(SteamClient.DisconnectedCallback callback)
    {
        _isConnected = false;
        _isLoggedOn = false;
        _logger.LogInformation("Disconnected from Steam. User initiated: {UserInitiated}", callback.UserInitiated);
        Disconnected?.Invoke(this, callback);
    }

    private void OnLoggedOn(SteamUser.LoggedOnCallback callback)
    {
        if (callback.Result == EResult.OK)
        {
            _isLoggedOn = true;
            _logger.LogInformation("Successfully logged on to Steam. SteamID: {SteamID}", callback.ClientSteamID);
            
            // Set persona state to online
            _steamFriends.SetPersonaState(EPersonaState.Online);
        }
        else
        {
            _logger.LogError("Failed to log on to Steam: {Result}", callback.Result);
        }
        
        LoggedOn?.Invoke(this, callback);
    }

    private void OnLoggedOff(SteamUser.LoggedOffCallback callback)
    {
        _isLoggedOn = false;
        _logger.LogInformation("Logged off from Steam: {Result}", callback.Result);
        LoggedOff?.Invoke(this, callback);
    }

    private void OnFriendsListReceived(SteamFriends.FriendsListCallback callback)
    {
        _logger.LogInformation("Friends list received");
        FriendsListReceived?.Invoke(this, callback);
    }

    private void OnPersonaStateChange(SteamFriends.PersonaStateCallback callback)
    {
        _logger.LogDebug("Persona state change for {SteamID}: {State}", callback.FriendID, callback.State);
        PersonaStateChange?.Invoke(this, callback);
    }

    private void OnMessageReceived(SteamFriends.FriendMsgCallback callback)
    {
        _logger.LogInformation("Message received from {SteamID}: {Message}", callback.Sender, callback.Message);
        MessageReceived?.Invoke(this, callback);
    }

    private void OnMessageEcho(SteamFriends.FriendMsgEchoCallback callback)
    {
        _logger.LogDebug("Message echo from {SteamID}: {Message}", callback.Recipient, callback.Message);
        MessageEcho?.Invoke(this, callback);
    }

    public void Dispose()
    {
        _logger.LogInformation("Disposing SteamClientManager");
        
        _cancellationTokenSource.Cancel();
        
        if (_isLoggedOn)
        {
            _steamUser.LogOff();
        }
        
        if (_isConnected)
        {
            _steamClient.Disconnect();
        }
        
        try
        {
            _callbackTask.Wait(TimeSpan.FromSeconds(5));
        }
        catch (AggregateException ex) when (ex.InnerExceptions.All(e => e is OperationCanceledException))
        {
            // Expected when cancelling
        }
        
        _cancellationTokenSource.Dispose();
        _logger.LogInformation("SteamClientManager disposed");
    }
    
    /// <summary>
    /// Generates a stable LoginID based on machine characteristics.
    /// This makes multiple sessions from the same machine appear consistent to Steam.
    /// </summary>
    private uint GenerateStableLoginID()
    {
        // Generate a stable LoginID based on machine name and process ID
        // This ensures consistent identification while allowing multiple bridge instances
        var machineHash = Environment.MachineName.GetHashCode();
        var processHash = Environment.ProcessId.GetHashCode();
        
        // Combine hashes to create a stable but unique LoginID
        var combinedHash = (uint)(machineHash ^ (processHash << 16));
        
        _logger.LogDebug("Generated stable LoginID: {LoginID} (Machine: {Machine}, PID: {PID})", 
            combinedHash, Environment.MachineName, Environment.ProcessId);
        
        return combinedHash;
    }
}