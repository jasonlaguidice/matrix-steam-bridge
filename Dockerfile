# Multi-stage Dockerfile for Matrix Steam Bridge
# Supports both linux/amd64 and linux/arm64

# Stage 1: Build C# SteamBridge service
FROM mcr.microsoft.com/dotnet/sdk:10.0 AS dotnet-builder

# Install system protoc to avoid ARM64 protoc segfault
RUN apt-get update && apt-get install -y protobuf-compiler && rm -rf /var/lib/apt/lists/*

ARG TARGETARCH
WORKDIR /src
COPY SteamBridge/ ./SteamBridge/

# Build the C# gRPC service
WORKDIR /src/SteamBridge
RUN if [ "$TARGETARCH" = "arm64" ]; then \
        export Protobuf_ProtocFullPath=$(which protoc); \
        dotnet restore --runtime linux-musl-arm64; \
        dotnet publish -c Release --runtime linux-musl-arm64 --self-contained true -o /app; \
    else \
        dotnet restore --runtime linux-musl-x64; \
        dotnet publish -c Release --runtime linux-musl-x64 --self-contained true -o /app; \
    fi && \
    mv /app/SteamBridge /app/steamkit-service

# Stage 2: Build Go bridge
FROM golang:1.26.0-alpine AS go-builder

# Install build dependencies including gcc/g++ for CGO and olm for encryption
RUN apk add --no-cache git ca-certificates gcc g++ musl-dev sqlite-dev olm-dev

WORKDIR /src

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and .git directory for version information
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
COPY .git .git

# Build the Go bridge with proper version ldflags (matching build.sh)
RUN MAUTRIX_VERSION=$(grep 'maunium.net/go/mautrix ' go.mod | awk '{print $2}' | head -n1) && \
    GO_LDFLAGS="-s -w -X main.Tag=$(git describe --exact-match --tags 2>/dev/null) -X main.Commit=$(git rev-parse HEAD) -X 'main.BuildTime=$(date -Iseconds)' -X 'maunium.net/go/mautrix.GoModVersion=$MAUTRIX_VERSION'" && \
    go build -ldflags="$GO_LDFLAGS" -o steam ./cmd/steam

# Stage 3: Runtime image
FROM mcr.microsoft.com/dotnet/aspnet:10.0-alpine

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    su-exec \
    olm \
    && rm -rf /var/cache/apk/*

# Create bridge user
RUN adduser -D -s /bin/sh -u 1000 bridge

# Create application directories
WORKDIR /app
RUN mkdir -p /app/logs && \
    chown -R bridge:bridge /app

# Copy built applications
COPY --from=go-builder /src/steam /app/steam
COPY --from=dotnet-builder /app/ /app/

# Set proper permissions
RUN chmod +x /app/steam && \
    chown -R bridge:bridge /app


# Expose ports
EXPOSE 50051

# Set default config/registration file path
ENV CONFIG_FILE="/app/config/config.yaml"
ENV REGISTRATION_FILE="/app/data/registration.yaml"

# Create entrypoint script
COPY entrypoint.sh /app/entrypoint.sh

RUN chmod +x /app/entrypoint.sh && chown bridge:bridge /app/entrypoint.sh

# Switch to non-root user
USER bridge

VOLUME ["/app/logs"]

# Default working directory
WORKDIR /app

ENTRYPOINT ["/app/entrypoint.sh"]