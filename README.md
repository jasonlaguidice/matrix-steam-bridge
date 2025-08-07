# Matrix Steam Bridge

A Matrix bridge for Steam Chat. Built using the [mautrix-go bridgev2](https://github.com/mautrix/go) framework.

## Architecture

The bridge consists of two main components:
- **Go Bridge Service**: Matrix integration using mautrix-go bridgev2 framework
- **C# gRPC Service**: Steam integration using SteamKit2 library

The Go service communicates with the C# SteamBridge service via gRPC to handle Steam authentication, messaging, and presence updates.

## Features

### ✅ Working Features

| Feature | Status | Description |
|---------|--------|-------------|
| **Steam Authentication** | ✅ Complete | Username/password with SteamGuard support |
| **QR Code Login** | ✅ Complete | Modern Steam mobile authentication flow |
| **Text-based Messaging** | ✅ Complete | Bidirectional message synchronization |
| **Contact Synchronization** | ✅ Complete | Automatic Steam friends list sync |
| **Typing Indicators** | ✅ Complete | Shows when users are typing |
| **Session Management** | ✅ Complete | Persistent Steam session handling |
| **Message Echo Handling** | ✅ Complete | Multi-client support (prevents duplicates) |
| **Presence Updates** | ✅ Complete | Steam online/offline status sync |

### ⏳ Pending Features

| Feature | Status | Priority | Description |
|---------|--------|----------|-------------|
| **File Attachments** | 🔄 Planned | High | Send/receive images and files |
| **Message Reactions** | 🔄 Planned | Medium | Steam emoticon support |
| **Steam Group Chats** | 🔄 Planned | Low | Multi-user Steam chat rooms |
| **Read Receipts** | 🔄 Planned | Low | Message read status |
| **Game Invites** | 🔄 Planned | Low | Handle Steam game invitations |

## Installation

### Prerequisites

- Go 1.24.5 or later
- .NET 8.0 SDK

### Building from Source

1. **Clone the repository**
   ```bash
   git clone https://github.com/jasonlaguidice/matrix-steam-bridge
   cd matrix-steam-bridge
   ```

2. **Build the bridge**
   ```bash
   cd SteamBridge
   ./build.sh
   ```

### Docker Installation

Docker images are available for multiple architectures:

```bash
# Pull the latest image
docker pull ghcr.io/jasonlaguidice/matrix-steam-bridge:latest

# Run with your config
docker run -v /path/to/your/config:/data ghcr.io/jasonlaguidice/matrix-steam-bridge:latest
```

Supported architectures: `linux/amd64`, `linux/arm64`

## Configuration (Generic)

1. **Generate an example configuration**
   ```bash
   ./steam -e
   ```

2. **Edit the configuration** to match your Matrix homeserver settings:
   - Set your homeserver address and domain
   - Configure database settings (SQLite or PostgreSQL)
   - Set bridge permissions for your users

3. **Generate the appservice registration**
   ```bash
   ./steam -g -c config.yaml -r registration.yaml
   ```

4. **Register the bridge** with your Matrix homeserver by adding the registration file to your homeserver configuration

5. **Start the bridge**
   ```bash
   ./steam -c config.yaml
   ```

## Usage

1. **Invite the bridge bot** to a new Matrix room - it will automatically mark the room as your management portam rool
2. **Login to Steam** using one of these methods:
   - `login qr` - QR code authentication (recommended)
   - `login password` - Username/password login
3. **Start chatting** - The bridge will automatically create portals for your Steam friends

## Development

### Project Structure

```
├── cmd/steam/           # Go bridge main entry point
├── pkg/
│   ├── connector/       # mautrix bridgev2 connector implementation  
│   └── steamapi/        # Generated gRPC client code
├── SteamBridge/         # C# gRPC service using SteamKit2
│   ├── Services/        # gRPC service implementations
│   ├── Proto/           # Protocol buffer definitions
│   └── Models/          # Data models
├── config.yaml          # Bridge configuration (create from example)
└── registration.yaml    # Matrix appservice registration
```

## Support

- **Matrix Room**: [#matrix-steam-bridge:shadowdrake.org](https://matrix.to/#/#matrix-steam-bridge:shadowdrake.org)
- **Issues**: [GitHub Issues](https://github.com/jasonlaguidice/matrix-steam-bridge/issues)
- **Contact**: [@jason:shadowdrake.org](https://matrix.to/#/@jason:shadowdrake.org)

## Acknowledgments

- Built with [mautrix-go](https://github.com/mautrix/go) bridgev2 framework
- Steam integration powered by [SteamKit2](https://github.com/SteamRE/SteamKit)
- Some code in this repository was generated using the assistance of AI