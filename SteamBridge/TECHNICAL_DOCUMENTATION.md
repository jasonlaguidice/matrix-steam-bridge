# Steam Bridge gRPC API - Technical Documentation

## Table of Contents
1. [Architecture Overview](#architecture-overview)
2. [Core Services](#core-services)
3. [gRPC API Reference](#grpc-api-reference)
4. [Data Models](#data-models)
5. [Error Handling](#error-handling)
6. [Integration Examples](#integration-examples)
7. [Advanced Usage](#advanced-usage)

## Architecture Overview

The Steam Bridge service is built using a layered architecture:

```
┌─────────────────────────────────────┐
│           gRPC Services             │
│  (SteamAuthService, SteamUserService, │
│    SteamMessagingService)           │
├─────────────────────────────────────┤
│         Business Logic              │
│  (Authentication, User Info,        │
│    Messaging Managers)             │
├─────────────────────────────────────┤
│        SteamClientManager          │
│     (SteamKit2 Integration)        │
├─────────────────────────────────────┤
│          SteamKit2 SDK             │
│      (Steam Network Protocol)      │
└─────────────────────────────────────┘
```

### Key Components

- **SteamClientManager**: Core Steam connection and callback management
- **Authentication Service**: Handles login flows and session management
- **User Information Service**: Manages user data and friends lists
- **Messaging Manager**: Handles message sending/receiving and streaming
- **gRPC Services**: Protocol Buffer-based API endpoints

---

## Core Services

### SteamClientManager

Central manager for SteamKit2 connection and callback handling.

#### Key Methods

##### `ConnectAsync() -> Task<bool>`
Establishes connection to Steam network.

**Usage**: Initialize Steam connection before authentication.

**Example**:
```csharp
var clientManager = serviceProvider.GetService<SteamClientManager>();
bool connected = await clientManager.ConnectAsync();
if (connected) {
    Console.WriteLine("Connected to Steam network");
}
```

##### `BeginAuthSessionViaCredentialsAsync(AuthSessionDetails) -> Task<AuthSession>`
Initiates password-based authentication with Steam.

**Parameters**:
- `AuthSessionDetails`: Username, password, authenticator, platform info

**Example**:
```csharp
var authDetails = new AuthSessionDetails {
    Username = "myusername",
    Password = "mypassword",
    Authenticator = new ConsoleAuthenticator(guardCode),
    PlatformType = (SteamKit2.Internal.EAuthTokenPlatformType)1,
    ClientOSType = EOSType.Win11,
    WebsiteID = "Unknown"
};

var authSession = await clientManager.BeginAuthSessionViaCredentialsAsync(authDetails);
```

##### `BeginAuthSessionViaQRAsync() -> Task<QrAuthSession>`
Initiates QR code authentication flow.

**Returns**: QR authentication session with challenge URL

**Example**:
```csharp
var qrSession = await clientManager.BeginAuthSessionViaQRAsync();
Console.WriteLine($"Scan QR code: {qrSession.ChallengeURL}");
```

##### `LogOn(string accessToken, string? refreshToken = null)`
Logs into Steam using authentication tokens.

**Parameters**:
- `accessToken`: OAuth access token from authentication
- `refreshToken`: Optional refresh token for session renewal

**Example**:
```csharp
clientManager.LogOn(pollResult.AccessToken, pollResult.RefreshToken);
```

---

## gRPC API Reference

### SteamAuthService

Authentication service providing Steam login capabilities.

#### LoginWithCredentials

**Endpoint**: `steambridge.SteamAuthService/LoginWithCredentials`
**Type**: Unary RPC
**Purpose**: Authenticate using Steam username/password with SteamGuard support

**Request Message**: `CredentialsLoginRequest`
```protobuf
message CredentialsLoginRequest {
  string username = 1;
  string password = 2;
  string guard_code = 3; // Optional SteamGuard code
  bool remember_password = 4;
}
```

**Response Message**: `LoginResponse`
```protobuf
message LoginResponse {
  bool success = 1;
  string error_message = 2;
  string access_token = 3;
  string refresh_token = 4;
  UserInfo user_info = 5;
  bool requires_guard = 6;
  bool requires_email_verification = 7;
}
```

**Example Usage**:
```csharp
// C# Client Example
var request = new CredentialsLoginRequest {
    Username = "mysteamusername",
    Password = "mypassword",
    GuardCode = "ABC123", // From Steam Mobile App
    RememberPassword = true
};

var response = await authClient.LoginWithCredentialsAsync(request);
if (response.Success) {
    Console.WriteLine($"Logged in as: {response.UserInfo.PersonaName}");
    Console.WriteLine($"Access Token: {response.AccessToken}");
} else {
    Console.WriteLine($"Login failed: {response.ErrorMessage}");
}
```

**Go Client Example**:
```go
import (
    "context"
    pb "path/to/generated/protobuf"
)

req := &pb.CredentialsLoginRequest{
    Username: "mysteamusername",
    Password: "mypassword",
    GuardCode: "ABC123",
    RememberPassword: true,
}

resp, err := client.LoginWithCredentials(context.Background(), req)
if err != nil {
    log.Fatalf("Login failed: %v", err)
}

if resp.Success {
    fmt.Printf("Logged in as: %s\n", resp.UserInfo.PersonaName)
} else {
    fmt.Printf("Login failed: %s\n", resp.ErrorMessage)
}
```

#### LoginWithQR

**Endpoint**: `steambridge.SteamAuthService/LoginWithQR`
**Type**: Unary RPC
**Purpose**: Initiate QR code authentication flow

**Request Message**: `QRLoginRequest` (empty)
**Response Message**: `QRLoginResponse`
```protobuf
message QRLoginResponse {
  string challenge_url = 1;
  string qr_code_ascii = 2; // ASCII art representation
  string session_id = 3; // For polling status
}
```

**Example Usage**:
```csharp
var qrResponse = await authClient.LoginWithQRAsync(new QRLoginRequest());
Console.WriteLine("Scan this QR code with Steam Mobile App:");
Console.WriteLine(qrResponse.QrCodeAscii);

// Poll for authentication status
while (true) {
    var statusResp = await authClient.GetAuthStatusAsync(new AuthStatusRequest {
        SessionId = qrResponse.SessionId
    });
    
    if (statusResp.State == AuthStatusResponse.Types.AuthState.Authenticated) {
        Console.WriteLine($"QR Login successful! Welcome {statusResp.UserInfo.PersonaName}");
        break;
    } else if (statusResp.State == AuthStatusResponse.Types.AuthState.Failed) {
        Console.WriteLine($"QR Login failed: {statusResp.ErrorMessage}");
        break;
    }
    
    await Task.Delay(2000); // Poll every 2 seconds
}
```

#### GetAuthStatus

**Endpoint**: `steambridge.SteamAuthService/GetAuthStatus`
**Type**: Unary RPC
**Purpose**: Poll QR authentication status

**Request Message**: `AuthStatusRequest`
```protobuf
message AuthStatusRequest {
  string session_id = 1; // From QR login response
}
```

**Response Message**: `AuthStatusResponse`
```protobuf
message AuthStatusResponse {
  enum AuthState {
    PENDING = 0;
    AUTHENTICATED = 1;
    FAILED = 2;
    EXPIRED = 3;
  }
  AuthState state = 1;
  string error_message = 2;
  string access_token = 3;
  string refresh_token = 4;
  UserInfo user_info = 5;
}
```

#### Logout

**Endpoint**: `steambridge.SteamAuthService/Logout`
**Type**: Unary RPC
**Purpose**: Log out from Steam and clear session

**Request Message**: `LogoutRequest` (empty)
**Response Message**: `LogoutResponse`
```protobuf
message LogoutResponse {
  bool success = 1;
}
```

---

### SteamUserService

User information and friends management service.

#### GetUserInfo

**Endpoint**: `steambridge.SteamUserService/GetUserInfo`
**Type**: Unary RPC
**Purpose**: Retrieve user profile information

**Request Message**: `UserInfoRequest`
```protobuf
message UserInfoRequest {
  uint64 steam_id = 1; // Optional, defaults to current user
}
```

**Response Message**: `UserInfoResponse`
```protobuf
message UserInfoResponse {
  UserInfo user_info = 1;
}
```

**Example Usage**:
```csharp
// Get current user info
var userResp = await userClient.GetUserInfoAsync(new UserInfoRequest());
Console.WriteLine($"Current user: {userResp.UserInfo.PersonaName}");
Console.WriteLine($"Steam ID: {userResp.UserInfo.SteamId}");
Console.WriteLine($"Status: {userResp.UserInfo.Status}");

// Get specific user info
var friendResp = await userClient.GetUserInfoAsync(new UserInfoRequest {
    SteamId = 76561198000000000 // Friend's Steam ID
});
Console.WriteLine($"Friend: {friendResp.UserInfo.PersonaName}");
```

#### GetFriendsList

**Endpoint**: `steambridge.SteamUserService/GetFriendsList`
**Type**: Unary RPC
**Purpose**: Retrieve complete friends list with status information

**Request Message**: `FriendsListRequest` (empty)
**Response Message**: `FriendsListResponse`
```protobuf
message FriendsListResponse {
  repeated Friend friends = 1;
}
```

**Example Usage**:
```csharp
var friendsResp = await userClient.GetFriendsListAsync(new FriendsListRequest());
Console.WriteLine($"You have {friendsResp.Friends.Count} friends:");

foreach (var friend in friendsResp.Friends) {
    Console.WriteLine($"- {friend.PersonaName} ({friend.Status})");
    if (!string.IsNullOrEmpty(friend.CurrentGame)) {
        Console.WriteLine($"  Playing: {friend.CurrentGame}");
    }
}
```

#### GetUserStatus

**Endpoint**: `steambridge.SteamUserService/GetUserStatus`
**Type**: Unary RPC
**Purpose**: Get real-time status of specific user

**Request Message**: `UserStatusRequest`
```protobuf
message UserStatusRequest {
  uint64 steam_id = 1;
}
```

**Response Message**: `UserStatusResponse`
```protobuf
message UserStatusResponse {
  PersonaState status = 1;
  string current_game = 2;
  int64 last_online = 3; // Unix timestamp
}
```

---

### SteamMessagingService

Messaging service providing send/receive capabilities and real-time streaming.

#### SendMessage

**Endpoint**: `steambridge.SteamMessagingService/SendMessage`
**Type**: Unary RPC
**Purpose**: Send a message to another Steam user

**Request Message**: `SendMessageRequest`
```protobuf
message SendMessageRequest {
  uint64 target_steam_id = 1;
  string message = 2;
  MessageType message_type = 3;
}
```

**Response Message**: `SendMessageResponse`
```protobuf
message SendMessageResponse {
  bool success = 1;
  string error_message = 2;
  int64 timestamp = 3;
}
```

**Example Usage**:
```csharp
var sendResp = await messagingClient.SendMessageAsync(new SendMessageRequest {
    TargetSteamId = 76561198000000000,
    Message = "Hello from Steam Bridge!",
    MessageType = MessageType.ChatMessage
});

if (sendResp.Success) {
    Console.WriteLine($"Message sent at {DateTimeOffset.FromUnixTimeSeconds(sendResp.Timestamp)}");
} else {
    Console.WriteLine($"Failed to send message: {sendResp.ErrorMessage}");
}
```

#### SubscribeToMessages

**Endpoint**: `steambridge.SteamMessagingService/SubscribeToMessages`
**Type**: Server Streaming RPC
**Purpose**: Subscribe to real-time message events

**Request Message**: `MessageSubscriptionRequest` (empty)
**Stream Response**: `MessageEvent`
```protobuf
message MessageEvent {
  uint64 sender_steam_id = 1;
  uint64 target_steam_id = 2;
  string message = 3;
  MessageType message_type = 4;
  int64 timestamp = 5;
  bool is_echo = 6; // True if this is an echo of our own message
}
```

**Example Usage**:
```csharp
// C# Client with async streaming
using var call = messagingClient.SubscribeToMessages(new MessageSubscriptionRequest());

await foreach (var messageEvent in call.ResponseStream.ReadAllAsync()) {
    if (messageEvent.IsEcho) {
        Console.WriteLine($"[ECHO] You -> {messageEvent.TargetSteamId}: {messageEvent.Message}");
    } else {
        Console.WriteLine($"[MSG] {messageEvent.SenderSteamId} -> You: {messageEvent.Message}");
    }
    
    // Handle different message types
    switch (messageEvent.MessageType) {
        case MessageType.ChatMessage:
            // Process regular chat message
            break;
        case MessageType.Typing:
            Console.WriteLine($"{messageEvent.SenderSteamId} is typing...");
            break;
        case MessageType.InviteGame:
            Console.WriteLine($"Game invitation from {messageEvent.SenderSteamId}");
            break;
    }
}
```

**Go Client Example**:
```go
stream, err := client.SubscribeToMessages(context.Background(), &pb.MessageSubscriptionRequest{})
if err != nil {
    log.Fatalf("Failed to subscribe: %v", err)
}

for {
    msg, err := stream.Recv()
    if err == io.EOF {
        break
    }
    if err != nil {
        log.Fatalf("Stream error: %v", err)
    }
    
    if msg.IsEcho {
        fmt.Printf("[ECHO] You -> %d: %s\n", msg.TargetSteamId, msg.Message)
    } else {
        fmt.Printf("[MSG] %d -> You: %s\n", msg.SenderSteamId, msg.Message)
    }
}
```

#### SendTypingNotification

**Endpoint**: `steambridge.SteamMessagingService/SendTypingNotification`
**Type**: Unary RPC
**Purpose**: Send typing indicator to another user

**Request Message**: `TypingNotificationRequest`
```protobuf
message TypingNotificationRequest {
  uint64 target_steam_id = 1;
  bool is_typing = 2;
}
```

**Response Message**: `TypingNotificationResponse`
```protobuf
message TypingNotificationResponse {
  bool success = 1;
}
```

**Example Usage**:
```csharp
// Start typing
await messagingClient.SendTypingNotificationAsync(new TypingNotificationRequest {
    TargetSteamId = friendSteamId,
    IsTyping = true
});

// User is composing message...
await Task.Delay(3000);

// Stop typing and send message
await messagingClient.SendTypingNotificationAsync(new TypingNotificationRequest {
    TargetSteamId = friendSteamId,
    IsTyping = false
});

await messagingClient.SendMessageAsync(new SendMessageRequest {
    TargetSteamId = friendSteamId,
    Message = "Hello there!",
    MessageType = MessageType.ChatMessage
});
```

---

## Data Models

### UserInfo
Complete user profile information.

```protobuf
message UserInfo {
  uint64 steam_id = 1;           // Unique Steam identifier
  string account_name = 2;       // Steam account login name
  string persona_name = 3;       // Display name
  string profile_url = 4;        // Steam profile URL
  string avatar_url = 5;         // Profile picture URL
  PersonaState status = 6;       // Current online status
  string current_game = 7;       // Currently played game
}
```

### Friend
Friend list entry with relationship information.

```protobuf
message Friend {
  uint64 steam_id = 1;
  string persona_name = 2;
  string avatar_url = 3;
  PersonaState status = 4;
  string current_game = 5;
  FriendRelationship relationship = 6;
}
```

### Enumerations

#### PersonaState
```protobuf
enum PersonaState {
  OFFLINE = 0;
  ONLINE = 1;
  BUSY = 2;
  AWAY = 3;
  SNOOZE = 4;
  LOOKING_TO_TRADE = 5;
  LOOKING_TO_PLAY = 6;
  INVISIBLE = 7;
}
```

#### FriendRelationship
```protobuf
enum FriendRelationship {
  NONE = 0;
  BLOCKED = 1;
  REQUEST_RECIPIENT = 2;  // They sent you a friend request
  FRIEND = 3;             // Mutual friends
  REQUEST_INITIATOR = 4;  // You sent them a friend request
  IGNORED = 5;
  IGNORED_FRIEND = 6;
}
```

#### MessageType
```protobuf
enum MessageType {
  CHAT_MESSAGE = 0;   // Regular text message
  TYPING = 1;         // Typing indicator
  EMOTE = 2;          // Emote/action message
  INVITE_GAME = 3;    // Game invitation
}
```

---

## Error Handling

### gRPC Status Codes

The service maps Steam errors to standard gRPC status codes:

| Steam Error | gRPC Status | Description |
|-------------|-------------|-------------|
| `EResult.OK` | `OK` | Operation successful |
| `EResult.InvalidPassword` | `UNAUTHENTICATED` | Invalid credentials |
| `EResult.AccountLogonDenied` | `PERMISSION_DENIED` | Account suspended/banned |
| `EResult.TwoFactorCodeMismatch` | `FAILED_PRECONDITION` | Invalid SteamGuard code |
| `EResult.Timeout` | `DEADLINE_EXCEEDED` | Request timeout |
| `EResult.ServiceUnavailable` | `UNAVAILABLE` | Steam servers down |
| Connection errors | `UNAVAILABLE` | Network connectivity issues |
| Internal errors | `INTERNAL` | Service-side errors |

### Error Response Patterns

**Authentication Errors**:
```csharp
try {
    var response = await authClient.LoginWithCredentialsAsync(request);
    if (!response.Success) {
        switch (response.ErrorMessage) {
            case "SteamGuard code required but not provided":
                // Prompt user for SteamGuard code
                break;
            case "Authentication timed out":
                // Retry or inform user
                break;
            default:
                Console.WriteLine($"Login failed: {response.ErrorMessage}");
                break;
        }
    }
} catch (RpcException ex) {
    switch (ex.StatusCode) {
        case StatusCode.Unauthenticated:
            Console.WriteLine("Invalid credentials provided");
            break;
        case StatusCode.Unavailable:
            Console.WriteLine("Steam service unavailable, please try again later");
            break;
        default:
            Console.WriteLine($"gRPC error: {ex.Status}");
            break;
    }
}
```

**Connection Error Handling**:
```csharp
// Implement retry logic with exponential backoff
var retryAttempts = 0;
const int maxRetries = 3;
TimeSpan delay = TimeSpan.FromSeconds(1);

while (retryAttempts < maxRetries) {
    try {
        var response = await userClient.GetUserInfoAsync(request);
        // Process successful response
        break;
    } catch (RpcException ex) when (ex.StatusCode == StatusCode.Unavailable) {
        retryAttempts++;
        if (retryAttempts >= maxRetries) {
            throw; // Re-throw after max retries
        }
        
        Console.WriteLine($"Connection failed, retrying in {delay.TotalSeconds}s (attempt {retryAttempts}/{maxRetries})");
        await Task.Delay(delay);
        delay = TimeSpan.FromMilliseconds(delay.TotalMilliseconds * 2); // Exponential backoff
    }
}
```

---

## Integration Examples

### Complete Authentication Flow

```csharp
public class SteamAuthenticationExample {
    private readonly SteamAuthService.SteamAuthServiceClient _authClient;
    
    public async Task<string> AuthenticateWithPasswordAsync(string username, string password) {
        try {
            // Step 1: Attempt initial login
            var request = new CredentialsLoginRequest {
                Username = username,
                Password = password,
                RememberPassword = true
            };
            
            var response = await _authClient.LoginWithCredentialsAsync(request);
            
            if (response.Success) {
                Console.WriteLine($"Login successful! Welcome {response.UserInfo.PersonaName}");
                return response.AccessToken;
            }
            
            // Step 2: Handle SteamGuard requirement
            if (response.RequiresGuard) {
                Console.Write("Enter SteamGuard code: ");
                var guardCode = Console.ReadLine();
                
                request.GuardCode = guardCode;
                response = await _authClient.LoginWithCredentialsAsync(request);
                
                if (response.Success) {
                    Console.WriteLine($"Login successful with SteamGuard! Welcome {response.UserInfo.PersonaName}");
                    return response.AccessToken;
                } else {
                    throw new Exception($"SteamGuard authentication failed: {response.ErrorMessage}");
                }
            }
            
            throw new Exception($"Authentication failed: {response.ErrorMessage}");
            
        } catch (RpcException ex) {
            throw new Exception($"gRPC error during authentication: {ex.Status}", ex);
        }
    }
    
    public async Task<string> AuthenticateWithQRAsync() {
        try {
            // Step 1: Start QR authentication
            var qrResponse = await _authClient.LoginWithQRAsync(new QRLoginRequest());
            
            Console.WriteLine("Please scan this QR code with Steam Mobile App:");
            Console.WriteLine(qrResponse.QrCodeAscii);
            Console.WriteLine($"Session ID: {qrResponse.SessionId}");
            
            // Step 2: Poll for authentication status
            var timeout = DateTime.UtcNow.AddMinutes(5);
            
            while (DateTime.UtcNow < timeout) {
                var statusResponse = await _authClient.GetAuthStatusAsync(new AuthStatusRequest {
                    SessionId = qrResponse.SessionId
                });
                
                switch (statusResponse.State) {
                    case AuthStatusResponse.Types.AuthState.Authenticated:
                        Console.WriteLine($"QR authentication successful! Welcome {statusResponse.UserInfo.PersonaName}");
                        return statusResponse.AccessToken;
                        
                    case AuthStatusResponse.Types.AuthState.Failed:
                        throw new Exception($"QR authentication failed: {statusResponse.ErrorMessage}");
                        
                    case AuthStatusResponse.Types.AuthState.Expired:
                        throw new Exception("QR code expired, please try again");
                        
                    case AuthStatusResponse.Types.AuthState.Pending:
                        Console.WriteLine("Waiting for QR code scan...");
                        await Task.Delay(2000);
                        break;
                }
            }
            
            throw new Exception("QR authentication timed out");
            
        } catch (RpcException ex) {
            throw new Exception($"gRPC error during QR authentication: {ex.Status}", ex);
        }
    }
}
```

### Matrix Bridge Integration Pattern

```go
package steambridge

import (
    "context"
    "io"
    "log"
    "time"
    
    "google.golang.org/grpc"
    pb "path/to/steam/bridge/proto"
)

type SteamBridge struct {
    authClient    pb.SteamAuthServiceClient
    userClient    pb.SteamUserServiceClient
    msgClient     pb.SteamMessagingServiceClient
    accessToken   string
    userInfo      *pb.UserInfo
}

func NewSteamBridge(conn *grpc.ClientConn) *SteamBridge {
    return &SteamBridge{
        authClient: pb.NewSteamAuthServiceClient(conn),
        userClient: pb.NewSteamUserServiceClient(conn),
        msgClient:  pb.NewSteamMessagingServiceClient(conn),
    }
}

func (sb *SteamBridge) Login(username, password string) error {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    
    resp, err := sb.authClient.LoginWithCredentials(ctx, &pb.CredentialsLoginRequest{
        Username:         username,
        Password:         password,
        RememberPassword: true,
    })
    if err != nil {
        return fmt.Errorf("login failed: %w", err)
    }
    
    if !resp.Success {
        return fmt.Errorf("authentication failed: %s", resp.ErrorMessage)
    }
    
    sb.accessToken = resp.AccessToken
    sb.userInfo = resp.UserInfo
    
    log.Printf("Logged in as %s (Steam ID: %d)", resp.UserInfo.PersonaName, resp.UserInfo.SteamId)
    return nil
}

func (sb *SteamBridge) GetFriends() ([]*pb.Friend, error) {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    
    resp, err := sb.userClient.GetFriendsList(ctx, &pb.FriendsListRequest{})
    if err != nil {
        return nil, fmt.Errorf("failed to get friends: %w", err)
    }
    
    return resp.Friends, nil
}

func (sb *SteamBridge) SendMessage(targetSteamID uint64, message string) error {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    
    resp, err := sb.msgClient.SendMessage(ctx, &pb.SendMessageRequest{
        TargetSteamId: targetSteamID,
        Message:       message,
        MessageType:   pb.MessageType_CHAT_MESSAGE,
    })
    if err != nil {
        return fmt.Errorf("failed to send message: %w", err)
    }
    
    if !resp.Success {
        return fmt.Errorf("message send failed: %s", resp.ErrorMessage)
    }
    
    return nil
}

func (sb *SteamBridge) SubscribeToMessages(ctx context.Context, msgChan chan<- *pb.MessageEvent) error {
    stream, err := sb.msgClient.SubscribeToMessages(ctx, &pb.MessageSubscriptionRequest{})
    if err != nil {
        return fmt.Errorf("failed to subscribe to messages: %w", err)
    }
    
    go func() {
        defer close(msgChan)
        
        for {
            msg, err := stream.Recv()
            if err == io.EOF {
                log.Println("Message stream ended")
                return
            }
            if err != nil {
                log.Printf("Message stream error: %v", err)
                return
            }
            
            select {
            case msgChan <- msg:
            case <-ctx.Done():
                return
            }
        }
    }()
    
    return nil
}

// Usage example for Matrix bridge
func (sb *SteamBridge) RunMessageBridge(ctx context.Context) error {
    msgChan := make(chan *pb.MessageEvent, 100)
    
    if err := sb.SubscribeToMessages(ctx, msgChan); err != nil {
        return err
    }
    
    for {
        select {
        case msg := <-msgChan:
            if msg == nil {
                return nil // Channel closed
            }
            
            // Convert Steam message to Matrix event
            if !msg.IsEcho { // Only process incoming messages
                matrixEvent := convertSteamToMatrix(msg)
                // Send to Matrix room...
                log.Printf("Bridging message from Steam user %d to Matrix", msg.SenderSteamId)
            }
            
        case <-ctx.Done():
            return ctx.Err()
        }
    }
}
```

---

## Advanced Usage

### Connection Management and Reconnection

```csharp
public class ResilientSteamClient {
    private readonly GrpcChannel _channel;
    private readonly SteamAuthService.SteamAuthServiceClient _authClient;
    private string _accessToken;
    private string _refreshToken;
    
    public async Task<bool> EnsureConnectedAsync() {
        try {
            // Test connection with a lightweight call
            var healthCheck = await _authClient.GetAuthStatusAsync(
                new AuthStatusRequest { SessionId = "health-check" },
                deadline: DateTime.UtcNow.AddSeconds(5)
            );
            return true;
        } catch (RpcException ex) when (ex.StatusCode == StatusCode.Unavailable) {
            // Connection lost, attempt reconnection
            return await ReconnectAsync();
        }
    }
    
    private async Task<bool> ReconnectAsync() {
        var retryCount = 0;
        const int maxRetries = 5;
        
        while (retryCount < maxRetries) {
            try {
                var delay = TimeSpan.FromSeconds(Math.Pow(2, retryCount)); // Exponential backoff
                await Task.Delay(delay);
                
                // Attempt to refresh session if we have tokens
                if (!string.IsNullOrEmpty(_refreshToken)) {
                    // Implementation would depend on Steam's token refresh mechanism
                    // This is a simplified example
                    var refreshResult = await RefreshSessionAsync(_refreshToken);
                    if (refreshResult.Success) {
                        _accessToken = refreshResult.AccessToken;
                        return true;
                    }
                }
                
                // If refresh fails, fall back to full re-authentication
                // This would typically involve prompting user or using stored credentials
                return false;
                
            } catch (Exception ex) {
                Console.WriteLine($"Reconnection attempt {retryCount + 1} failed: {ex.Message}");
                retryCount++;
            }
        }
        
        return false;
    }
}
```

### Message Queue Management

```csharp
public class MessageQueueManager {
    private readonly Queue<PendingMessage> _messageQueue = new();
    private readonly SemaphoreSlim _queueSemaphore = new(1, 1);
    private readonly Timer _retryTimer;
    
    public async Task QueueMessageAsync(ulong targetSteamId, string message) {
        await _queueSemaphore.WaitAsync();
        try {
            _messageQueue.Enqueue(new PendingMessage {
                TargetSteamId = targetSteamId,
                Message = message,
                Timestamp = DateTimeOffset.UtcNow,
                RetryCount = 0
            });
        } finally {
            _queueSemaphore.Release();
        }
        
        // Trigger immediate processing
        _ = Task.Run(ProcessQueueAsync);
    }
    
    private async Task ProcessQueueAsync() {
        await _queueSemaphore.WaitAsync();
        try {
            while (_messageQueue.Count > 0) {
                var message = _messageQueue.Dequeue();
                
                try {
                    var result = await _messagingClient.SendMessageAsync(new SendMessageRequest {
                        TargetSteamId = message.TargetSteamId,
                        Message = message.Message,
                        MessageType = MessageType.ChatMessage
                    });
                    
                    if (result.Success) {
                        Console.WriteLine($"Message sent to {message.TargetSteamId}: {message.Message}");
                    } else {
                        await HandleFailedMessage(message, result.ErrorMessage);
                    }
                } catch (RpcException ex) {
                    await HandleFailedMessage(message, ex.Status.Detail);
                }
            }
        } finally {
            _queueSemaphore.Release();
        }
    }
    
    private async Task HandleFailedMessage(PendingMessage message, string error) {
        message.RetryCount++;
        
        if (message.RetryCount < 3) {
            // Re-queue with exponential backoff
            await Task.Delay(TimeSpan.FromSeconds(Math.Pow(2, message.RetryCount)));
            _messageQueue.Enqueue(message);
            Console.WriteLine($"Retrying message to {message.TargetSteamId} (attempt {message.RetryCount})");
        } else {
            Console.WriteLine($"Failed to send message to {message.TargetSteamId} after 3 attempts: {error}");
            // Could implement dead letter queue or user notification here
        }
    }
    
    private class PendingMessage {
        public ulong TargetSteamId { get; set; }
        public string Message { get; set; }
        public DateTimeOffset Timestamp { get; set; }
        public int RetryCount { get; set; }
    }
}
```

### Performance Monitoring

```csharp
public class SteamBridgeMetrics {
    private readonly Counter _messagesReceived;
    private readonly Counter _messagesSent;
    private readonly Histogram _authenticationLatency;
    private readonly Gauge _activeConnections;
    
    public SteamBridgeMetrics() {
        var factory = Metrics.NewCollectorRegistry();
        
        _messagesReceived = factory.CreateCounter(
            "steam_messages_received_total",
            "Total number of messages received from Steam"
        );
        
        _messagesSent = factory.CreateCounter(
            "steam_messages_sent_total", 
            "Total number of messages sent to Steam"
        );
        
        _authenticationLatency = factory.CreateHistogram(
            "steam_authentication_duration_seconds",
            "Time taken for Steam authentication"
        );
        
        _activeConnections = factory.CreateGauge(
            "steam_active_connections",
            "Number of active Steam connections"
        );
    }
    
    public IDisposable MeasureAuthenticationTime() {
        return _authenticationLatency.NewTimer();
    }
    
    public void RecordMessageReceived() => _messagesReceived.Inc();
    public void RecordMessageSent() => _messagesSent.Inc();
    public void SetActiveConnections(int count) => _activeConnections.Set(count);
}
```

---

## Configuration and Deployment

### Service Configuration

```json
// appsettings.json
{
  "Logging": {
    "LogLevel": {
      "Default": "Information",
      "SteamBridge": "Debug",
      "Microsoft.AspNetCore": "Warning"
    }
  },
  "Kestrel": {
    "Endpoints": {
      "Http": {
        "Url": "http://localhost:50051"
      },
      "Https": {
        "Url": "https://localhost:5001"
      }
    }
  },
  "SteamBridge": {
    "ConnectionTimeout": "00:00:30",
    "AuthenticationTimeout": "00:02:00",
    "MessageQueueSize": 1000,
    "EnableMetrics": true
  }
}
```

### Docker Deployment

```dockerfile
# Dockerfile
FROM mcr.microsoft.com/dotnet/aspnet:8.0 AS base
WORKDIR /app
EXPOSE 5000

FROM mcr.microsoft.com/dotnet/sdk:8.0 AS build
WORKDIR /src
COPY ["SteamBridge.csproj", "."]
RUN dotnet restore "SteamBridge.csproj"
COPY . .
WORKDIR "/src"
RUN dotnet build "SteamBridge.csproj" -c Release -o /app/build

FROM build AS publish
RUN dotnet publish "SteamBridge.csproj" -c Release -o /app/publish

FROM base AS final
WORKDIR /app
COPY --from=publish /app/publish .
ENTRYPOINT ["dotnet", "SteamBridge.dll"]
```

```yaml
# docker-compose.yml
version: '3.8'
services:
  steam-bridge:
    build: .
    ports:
      - "50051:50051"
    environment:
      - ASPNETCORE_ENVIRONMENT=Production
      - ASPNETCORE_URLS=http://+:50051
    volumes:
      - ./logs:/app/logs
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:50051/health"]
      interval: 30s
      timeout: 10s
      retries: 3
```

This technical documentation provides comprehensive coverage of the Steam Bridge gRPC API service, including detailed function references, usage examples, and integration patterns for building Matrix bridges and other Steam-integrated applications.