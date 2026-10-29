# syntax=docker/dockerfile:1

ARG BINARY_SOURCE=from-source

# Stage 1: Build the C# SteamKit/gRPC service (SteamBridge)
FROM mcr.microsoft.com/dotnet/sdk:10.0 AS dotnet-builder

RUN apt-get update && \
    apt-get install -y --no-install-recommends protobuf-compiler && \
    rm -rf /var/lib/apt/lists/*

ARG TARGETARCH
WORKDIR /src

COPY SteamBridge/SteamBridge.csproj ./SteamBridge/

RUN if [ "$TARGETARCH" = "arm64" ]; then \
        RID=linux-arm64; \
        export Protobuf_ProtocFullPath=/usr/bin/protoc; \
    else \
        RID=linux-x64; \
    fi && \
    dotnet restore ./SteamBridge/SteamBridge.csproj -r $RID

COPY SteamBridge/ ./SteamBridge/

RUN if [ "$TARGETARCH" = "arm64" ]; then \
        RID=linux-arm64; \
        export Protobuf_ProtocFullPath=/usr/bin/protoc; \
    else \
        RID=linux-x64; \
    fi && \
    dotnet publish ./SteamBridge/SteamBridge.csproj -c Release -r $RID --self-contained true --no-restore -o /app && \
    mv /app/SteamBridge /app/steamkit-service

# Stage 2: Build the Go bridge on Ubuntu 24.04
FROM ubuntu:24.04 AS go-builder

COPY --from=golang:1.25 /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOTOOLCHAIN=local

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        gcc \
        g++ \
        libc6-dev \
        libsqlite3-dev \
        libolm-dev \
        git \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and .git directory for version information
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
COPY .git .git

# Build the Go bridge with proper version ldflags
RUN MAUTRIX_VERSION=$(grep 'maunium.net/go/mautrix ' go.mod | awk '{print $2}' | head -n1) && \
    GO_LDFLAGS="-s -w -X main.Tag=$(git describe --exact-match --tags 2>/dev/null) -X main.Commit=$(git rev-parse HEAD) -X 'main.BuildTime=$(date -Iseconds)' -X 'maunium.net/go/mautrix.GoModVersion=$MAUTRIX_VERSION'" && \
    go build -ldflags="$GO_LDFLAGS" -o steam ./cmd/steam

# Stage 3: the two binary sources, selected by BINARY_SOURCE
FROM scratch AS from-source
COPY --from=go-builder /src/steam /steam
COPY --from=dotnet-builder /app/steamkit-service /steamkit-service

# Stage 4: prebuilt: take ./docker-bin/{steam,steamkit-service} from the build
# context
FROM scratch AS prebuilt
COPY docker-bin/steam docker-bin/steamkit-service /

# Stage 5: Copy from sources above
FROM ${BINARY_SOURCE} AS binaries

# Stage 6: runtime image: Ubuntu 24.04
FROM mcr.microsoft.com/dotnet/aspnet:10.0

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        tzdata \
        gosu \
        libolm3 \
        libsqlite3-0 && \
    rm -rf /var/lib/apt/lists/*

# Create bridge user
RUN userdel --remove ubuntu && \
    useradd --create-home --uid 1000 --shell /bin/sh bridge

# Create application directories
WORKDIR /app
RUN mkdir -p /app/logs && \
    chown -R bridge:bridge /app

COPY --from=binaries --chown=bridge:bridge --chmod=755 /steam /steamkit-service /app/

# Expose ports
EXPOSE 50051

# Set default config/registration file path
ENV CONFIG_FILE="/app/config/config.yaml"
ENV REGISTRATION_FILE="/app/data/registration.yaml"

# Entrypoint script (kept executable + owned by bridge)
COPY --chown=bridge:bridge --chmod=755 entrypoint.sh /app/entrypoint.sh

RUN chown -R bridge:bridge /app

USER bridge

VOLUME ["/app/logs"]

# Default working directory
WORKDIR /app

ENTRYPOINT ["/app/entrypoint.sh"]
