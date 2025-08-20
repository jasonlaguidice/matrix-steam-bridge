# Multi-stage Dockerfile for Matrix Steam Bridge
# Supports both linux/amd64 and linux/arm64

# Stage 1: Build C# SteamBridge service  
FROM mcr.microsoft.com/dotnet/sdk:8.0 AS dotnet-builder

# Install system protoc to avoid ARM64 protoc segfault
RUN apt-get update && apt-get install -y protobuf-compiler && rm -rf /var/lib/apt/lists/*

ARG TARGETARCH
WORKDIR /src
COPY SteamBridge/ ./SteamBridge/

# Build the C# gRPC service
WORKDIR /src/SteamBridge
RUN if [ "$TARGETARCH" = "arm64" ]; then \
        export Protobuf_ProtocFullPath=$(which protoc); \
        dotnet restore --runtime linux-arm64; \
        dotnet publish -c Release --runtime linux-arm64 --self-contained false -o /app/steambridge; \
    else \
        dotnet restore --runtime linux-x64; \
        dotnet publish -c Release --runtime linux-x64 --self-contained false -o /app/steambridge; \
    fi

# Stage 2: Build Go bridge
FROM golang:1.24.5-alpine AS go-builder

# Install build dependencies including gcc/g++ for CGO and olm for encryption
RUN apk add --no-cache git ca-certificates gcc g++ musl-dev sqlite-dev olm-dev

WORKDIR /src

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/

# Build the Go bridge with CGO enabled for sqlite3
RUN CGO_ENABLED=1 GOOS=linux go build -a -ldflags="-s -w" -o steam ./cmd/steam

# Stage 3: Runtime image
FROM mcr.microsoft.com/dotnet/aspnet:8.0-alpine

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    su-exec \
    && rm -rf /var/cache/apk/*

# Create bridge user
RUN adduser -D -s /bin/sh -u 1000 bridge

# Create application directories
WORKDIR /app
RUN mkdir -p /app/steambridge /app/logs /app/data && \
    chown -R bridge:bridge /app

# Copy built applications
COPY --from=go-builder /src/steam /app/steam
COPY --from=dotnet-builder /app/steambridge/ /app/steambridge/

# Set proper permissions
RUN chmod +x /app/steam && \
    chown -R bridge:bridge /app

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
    CMD su-exec bridge /app/steam --health-check || exit 1

# Expose ports
EXPOSE 50051

# Create entrypoint script
RUN cat > /app/entrypoint.sh << 'EOF'
#!/bin/sh
set -e

# Check if running as root and switch to bridge user
if [ "$(id -u)" = "0" ]; then
    echo "Running as root, switching to bridge user"
    exec su-exec bridge "$0" "$@"
fi

# Ensure data directory exists and is writable
if [ ! -w /app/data ]; then
    echo "Error: /app/data is not writable by bridge user"
    exit 1
fi

# Check if config exists
if [ ! -f /app/data/config.yaml ]; then
    echo "No config file found at /app/data/config.yaml"
    echo "Please mount your config file to /app/data/config.yaml"
    echo "You can use the example config as a starting point:"
    echo "  docker run -v /path/to/config.yaml:/app/data/config.yaml ..."
    exit 1
fi

# Start the bridge
echo "Starting Matrix Steam Bridge..."
exec /app/steam -c /app/data/config.yaml "$@"
EOF

RUN chmod +x /app/entrypoint.sh && chown bridge:bridge /app/entrypoint.sh

# Switch to non-root user
USER bridge

# Set volumes
VOLUME ["/app/data", "/app/logs"]

# Default working directory for mounted configs
WORKDIR /app/data

ENTRYPOINT ["/app/entrypoint.sh"]