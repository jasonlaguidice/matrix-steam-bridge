# Steam Bridge gRPC API Service

A comprehensive gRPC API service for Steam integration using SteamKit2, designed specifically for bridging Steam functionality with Matrix chat clients.

## Overview

This service provides three main gRPC services:
- **SteamAuthService**: Authentication with Steam (password + SteamGuard, QR code)
- **SteamUserService**: User information, friends list, and status management  
- **SteamMessagingService**: Bidirectional messaging and real-time message streaming

## Features

### Authentication
- ✅ Password-based login with SteamGuard support
- ✅ QR code authentication flow
- ✅ Session management and token handling
- ✅ Automatic reconnection and error handling

### User Management
- ✅ Steam ID, profile name, and display name extraction
- ✅ Friends list synchronization
- ✅ User status tracking (online/offline/away/etc.)
- ✅ Avatar URL generation

### Messaging
- ✅ Bidirectional private messaging
- ✅ Real-time message reception using SteamKit callbacks
- ✅ Message streaming via gRPC server streaming
- ✅ Typing indicators
- ✅ Message echo handling for multi-client support

## Quick Start

### Prerequisites
- .NET 8.0 Runtime
- Steam account credentials

### Running the Service

```bash
dotnet run
```

The service will start on `http://localhost:50051` and expose gRPC endpoints.

### Testing Connectivity

```bash
curl http://localhost:50051
# Returns: "Steam Bridge gRPC Service is running. Use a gRPC client to connect."
```

## API Documentation

### SteamAuthService

#### LoginWithCredentials
Authenticate using Steam username/password with optional SteamGuard code.

```protobuf
rpc LoginWithCredentials(CredentialsLoginRequest) returns (LoginResponse);
```

#### LoginWithQR  
Start QR code authentication flow.

```protobuf
rpc LoginWithQR(QRLoginRequest) returns (QRLoginResponse);
```

#### GetAuthStatus
Poll for QR authentication status.

```protobuf
rpc GetAuthStatus(AuthStatusRequest) returns (AuthStatusResponse);
```

### SteamUserService

#### GetUserInfo
Get user profile information.

```protobuf
rpc GetUserInfo(UserInfoRequest) returns (UserInfoResponse);
```

#### GetFriendsList
Retrieve complete friends list.

```protobuf
rpc GetFriendsList(FriendsListRequest) returns (FriendsListResponse);
```

### SteamMessagingService

#### SendMessage
Send a message to another Steam user.

```protobuf
rpc SendMessage(SendMessageRequest) returns (SendMessageResponse);
```

#### SubscribeToMessages
Subscribe to real-time message events (server streaming).

```protobuf
rpc SubscribeToMessages(MessageSubscriptionRequest) returns (stream MessageEvent);
```

## Architecture

### Core Components

- **SteamClientManager**: Manages SteamKit2 connection and callback processing
- **SteamAuthenticationService**: Handles all authentication flows  
- **SteamUserInformationService**: User data retrieval and management
- **SteamMessagingManager**: Message handling and streaming

### Protocol Buffers

All API definitions are in `Proto/steam_bridge.proto` with generated C# classes for type safety.

### Error Handling

- Comprehensive error mapping from Steam EResult codes to gRPC status codes
- Automatic reconnection with exponential backoff
- Graceful handling of Steam rate limits

## Integration

This service is designed to be consumed by a Go-based Matrix bridge using the mautrix-go bridgev2 framework. The gRPC interface provides a clean separation between Steam protocol handling (C#) and Matrix bridge logic (Go).

## Development Status

**Current Status**: Core functionality complete and tested
**Next Steps**: Session persistence, parent process monitoring, production deployment features

### Completed Features ✅
- SteamKit2 integration with modern authentication API
- Complete gRPC service implementation  
- Password and QR code authentication
- Friends list and user information retrieval
- Bidirectional messaging with real-time streaming
- Comprehensive error handling and logging

### Pending Features ⏳
- Session persistence and token storage
- Parent process monitoring for auto-termination
- Health check endpoints
- Metrics and monitoring integration

## Configuration

The service uses standard ASP.NET Core configuration. Future versions will support:
- Custom port configuration
- Steam API rate limiting settings
- Logging level configuration
- Session storage options

## Logging

Comprehensive structured logging using Microsoft.Extensions.Logging:
- Connection status and authentication events
- Message send/receive activity  
- Error conditions and recovery attempts
- Performance metrics and diagnostics

---

Built with SteamKit2 3.3.0, gRPC for .NET, and ASP.NET Core 8.0