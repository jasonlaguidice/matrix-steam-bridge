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
dotnet build --configuration Release
if [ $? -ne 0 ]; then
    echo "Failed to build SteamBridge service"
    exit 1
fi
cd ..

echo "Build completed successfully!"
echo "  - Go bridge: steam"
echo "  - C# service: SteamBridge/bin/Release/net8.0/SteamBridge.dll"