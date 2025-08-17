#!/bin/bash
set -e

echo "Generating protobuf files..."

# Change to project root
cd "$(dirname "$0")"

echo "Generating C# protobuf files..."
cd SteamBridge
# Clean old generated protobuf files only (not hand-written .cs files)
rm -f *Bridge.cs *BridgeGrpc.cs
rm -rf obj bin
cd ..

echo "Generating Go protobuf files..."
# Remove old files
rm -f pkg/steamapi/steam_bridge*.pb.go
rm -rf pkg/steamapi/Proto

# Generate new files
protoc --proto_path=SteamBridge \
       --go_out=pkg/steamapi \
       --go_opt=paths=source_relative \
       --go-grpc_out=pkg/steamapi \
       --go-grpc_opt=paths=source_relative \
       Proto/steam_bridge.proto

# Move files if they ended up in subdirectory
if [ -d "pkg/steamapi/Proto" ]; then
    mv pkg/steamapi/Proto/* pkg/steamapi/
    rmdir pkg/steamapi/Proto
fi

echo "✅ Protobuf files generated successfully"