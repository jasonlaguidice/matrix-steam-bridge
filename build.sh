#!/bin/sh
MAUTRIX_VERSION=$(cat go.mod | grep 'maunium.net/go/mautrix ' | awk '{ print $2 }' | head -n1)
GO_LDFLAGS="-s -w -X main.Tag=$(git describe --exact-match --tags 2>/dev/null) -X main.Commit=$(git rev-parse HEAD) -X 'main.BuildTime=`date -Iseconds`' -X 'maunium.net/go/mautrix.GoModVersion=$MAUTRIX_VERSION'"

echo "Building Go bridge..."
go build -ldflags="$GO_LDFLAGS" ./cmd/steam "$@"
if [ $? -ne 0 ]; then
    echo "Failed to build Go bridge"
    exit 1
fi

echo "Building SteamBridge C# service..."
cd SteamBridge

# Detect platform and set appropriate runtime and executable name
case "$(uname -s)" in
    MINGW*|CYGWIN*|MSYS*)
        RUNTIME="win-x64"
        EXEC_NAME="steamkit-service.exe"
        SOURCE_BINARY="SteamBridge.exe"
        ;;
    Darwin*)
        case "$(uname -m)" in
            arm64)
                RUNTIME="osx-arm64"
                ;;
            *)
                RUNTIME="osx-x64"
                ;;
        esac
        EXEC_NAME="steamkit-service"
        SOURCE_BINARY="SteamBridge"
        ;;
    *)
        case "$(uname -m)" in
            aarch64|arm64)
                RUNTIME="linux-arm64"
                ;;
            *)
                RUNTIME="linux-x64"
                ;;
        esac
        EXEC_NAME="steamkit-service"
        SOURCE_BINARY="SteamBridge"
        ;;
esac

echo "Building for runtime: $RUNTIME"
dotnet publish --configuration Release --self-contained true --runtime "$RUNTIME"
if [ $? -ne 0 ]; then
    echo "Failed to build SteamBridge service"
    exit 1
fi
cd ..

echo "Moving SteamBridge binary to root level..."
mv "SteamBridge/bin/Release/net8.0/$RUNTIME/publish/$SOURCE_BINARY" "./$EXEC_NAME"

echo "Build completed successfully!"
echo "  - Go bridge: steam"
echo "  - C# service: $EXEC_NAME"