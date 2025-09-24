package connector

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.mau.fi/util/configupgrade"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

//go:embed example-config.yaml
var ExampleConfig string

// Config contains the configuration for the Steam connector
type Config struct {
	Path           string        `yaml:"steam_bridge_path"`
	Address        string        `yaml:"steam_bridge_address"`
	AutoStart      bool          `yaml:"steam_bridge_auto_start"`
	StartupTimeout int           `yaml:"steam_bridge_startup_timeout"`
	
	// Reconnection configuration
	AutoReconnect      bool          `yaml:"auto_reconnect"`
	ReconnectDelay     time.Duration `yaml:"reconnect_delay"`
	MaxReconnectTries  int           `yaml:"max_reconnect_tries"`
}

// SteamConnector implements the NetworkConnector interface for Steam
type SteamConnector struct {
	br *bridgev2.Bridge

	Config Config

	// gRPC connection to Steam service
	steamConn *grpc.ClientConn
	// gRPC clients for Steam services
	authClient    steamapi.SteamAuthServiceClient
	userClient    steamapi.SteamUserServiceClient
	msgClient     steamapi.SteamMessagingServiceClient
	sessionClient steamapi.SteamSessionServiceClient
	healthClient  grpc_health_v1.HealthClient

	// Steam service process management
	steamProcess       *exec.Cmd
	steamProcessCancel context.CancelFunc
	processLockFile    string
	startTime          time.Time
	log                zerolog.Logger
}

// SteamClient implements the NetworkAPI for Steam
type SteamClient struct {
	UserLogin     *bridgev2.UserLogin
	authClient    steamapi.SteamAuthServiceClient
	userClient    steamapi.SteamUserServiceClient
	msgClient     steamapi.SteamMessagingServiceClient
	sessionClient steamapi.SteamSessionServiceClient

	br *bridgev2.Bridge

	// Bridge state debouncing (following Signal bridge pattern)
	disconnectDebounceTimer *time.Timer
	disconnectDebounceMutex sync.Mutex

	// Connection monitoring
	connectionCtx    context.Context
	connectionCancel context.CancelFunc
	connectionMutex  sync.Mutex

	// Connection state tracking
	isConnecting bool
	isConnected  bool
	stateMutex   sync.RWMutex

	// Message subscription tracking
	messageCount    uint64
	messageCountMux sync.RWMutex

	// Centralized reconnection management
	reconnectionMutex    sync.Mutex
	isReconnecting       bool
	reconnectionCancel   context.CancelFunc
	reconnectionAttempts int
	lastReconnectTime    time.Time
}

// SteamLoginPassword implements password-based login flow
type SteamLoginPassword struct {
	Main       *SteamConnector
	User       *bridgev2.User
	cancelFunc context.CancelFunc
	// Store session info for 2FA continuation
	SessionID string
	Username  string // Store username for UserLogin creation
}

// SteamLoginQR implements QR code-based login flow
type SteamLoginQR struct {
	Main *SteamConnector
	User *bridgev2.User

	// QR login state management
	cancelCtx  context.Context
	cancelFunc context.CancelFunc

	// Steam QR authentication data
	ChallengeID  string
	QRCodeData   string
	PollInterval time.Duration
	PollTimeout  time.Duration

	// Matrix UI management
	QRMessageID      id.EventID   // For redacting QR messages
	StatusMessageIDs []id.EventID // For redacting status update messages
}

// Interface compliance checks
var _ bridgev2.NetworkConnector = (*SteamConnector)(nil)
var _ bridgev2.LoginProcess = (*SteamLoginPassword)(nil)
var _ bridgev2.LoginProcess = (*SteamLoginQR)(nil)
var _ bridgev2.NetworkAPI = (*SteamClient)(nil)
var _ bridgev2.IdentifierResolvingNetworkAPI = (*SteamClient)(nil)
var _ bridgev2.UserSearchingNetworkAPI = (*SteamClient)(nil)
var _ bridgev2.BackfillingNetworkAPI = (*SteamClient)(nil)
var _ bridgev2.TypingHandlingNetworkAPI = (*SteamClient)(nil)

// upgradeConfig handles configuration upgrades
func upgradeConfig(helper configupgrade.Helper) {
	helper.Copy(configupgrade.Str, "steam_bridge_path")
	helper.Copy(configupgrade.Str, "steam_bridge_address")
	helper.Copy(configupgrade.Bool, "steam_bridge_auto_start")
	helper.Copy(configupgrade.Int, "steam_bridge_startup_timeout")
}

// Init implements bridgev2.NetworkConnector.
func (sc *SteamConnector) Init(bridge *bridgev2.Bridge) {
	sc.br = bridge
	sc.log = log.With().Str("component", "steam_connector").Logger()
	sc.log.Info().Msg("Initializing Steam connector")
}

// Start implements bridgev2.NetworkConnector.
func (sc *SteamConnector) Start(ctx context.Context) error {
	sc.log.Info().Msg("Starting Steam connector")

	// Handle auto-start functionality
	if sc.Config.AutoStart {
		sc.log.Info().Msg("Auto-start is enabled, starting SteamBridge service")

		// Start the Steam Bridge service
		if err := sc.startSteamBridgeService(ctx); err != nil {
			return fmt.Errorf("failed to start SteamBridge service: %w", err)
		}

		// Wait for the service to be ready
		// Health check endpoint has been added to SteamBridge service at /health
		if err := sc.waitForServiceReady(ctx); err != nil {
			// If service failed to start, try to stop it to clean up
			if stopErr := sc.stopSteamBridgeService(); stopErr != nil {
				sc.log.Warn().Err(stopErr).Msg("Failed to stop SteamBridge service after startup failure")
			}
			return fmt.Errorf("SteamBridge service failed to become ready: %w", err)
		}
	} else {
		sc.log.Info().Msg("Auto-start is disabled, assuming SteamBridge service is running externally")
	}

	// Use configured address for gRPC connection
	address := sc.Config.Address
	if address == "" {
		address = "localhost:50051" // fallback to default
		sc.log.Warn().Msg("No steam_bridge_address configured, using default localhost:50051")
	}

	sc.log.Info().Str("address", address).Msg("Establishing gRPC connection to SteamBridge service")

	// Establish gRPC connection to SteamBridge service
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to SteamBridge service at %s: %w", address, err)
	}

	sc.steamConn = conn
	sc.authClient = steamapi.NewSteamAuthServiceClient(conn)
	sc.userClient = steamapi.NewSteamUserServiceClient(conn)
	sc.msgClient = steamapi.NewSteamMessagingServiceClient(conn)
	sc.sessionClient = steamapi.NewSteamSessionServiceClient(conn)
	sc.healthClient = grpc_health_v1.NewHealthClient(conn)

	sc.log.Info().Str("address", address).Msg("Successfully connected to SteamBridge service")
	return nil
}

// Stop implements graceful shutdown of the SteamConnector
func (sc *SteamConnector) Stop() error {
	sc.log.Info().Msg("Stopping Steam connector")

	// Close gRPC connection if it exists
	if sc.steamConn != nil {
		sc.log.Info().Msg("Closing gRPC connection to SteamBridge service")
		if err := sc.steamConn.Close(); err != nil {
			sc.log.Error().Err(err).Msg("Failed to close gRPC connection")
			// Don't return error here, continue with cleanup
		}
		sc.steamConn = nil
		sc.authClient = nil
		sc.userClient = nil
		sc.msgClient = nil
		sc.sessionClient = nil
		sc.healthClient = nil
	}

	// Stop the managed SteamBridge service if we started it
	if err := sc.stopSteamBridgeService(); err != nil {
		sc.log.Error().Err(err).Msg("Failed to stop SteamBridge service")
		return fmt.Errorf("failed to stop SteamBridge service: %w", err)
	}

	sc.log.Info().Msg("Steam connector stopped successfully")
	return nil
}

// GetCapabilities implements bridgev2.NetworkConnector.
func (sc *SteamConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		DisappearingMessages: false,
		AggressiveUpdateInfo: true,
	}
}

// GetBridgeInfoVersion implements bridgev2.NetworkConnector.
func (sc *SteamConnector) GetBridgeInfoVersion() (info int, capabilities int) {
	return 1, 1
}

// GetName implements bridgev2.NetworkConnector.
func (sc *SteamConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:      "Steam",
		NetworkURL:       "https://store.steampowered.com",
		NetworkIcon:      "mxc://shadowdrake.org/HqkqOxBknisPvpVfuAafUcrK",
		NetworkID:        "steam",
		BeeperBridgeType: "go.shadowdrake.org/steam",
		DefaultPort:      8080,
	}
}

// GetConfig implements bridgev2.NetworkConnector.
func (sc *SteamConnector) GetConfig() (example string, data any, upgrader configupgrade.Upgrader) {
	return ExampleConfig, &sc.Config, configupgrade.SimpleUpgrader(upgradeConfig)
}

// GetDBMetaTypes implements bridgev2.NetworkConnector.
func (sc *SteamConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Portal: func() any {
			return &PortalMetadata{}
		},
		Ghost: func() any {
			return &GhostMetadata{}
		},
		Message: func() any {
			return &MessageMetadata{}
		},
		UserLogin: func() any {
			return &UserLoginMetadata{}
		},
	}
}

// GetLoginFlows implements bridgev2.NetworkConnector.
func (sc *SteamConnector) GetLoginFlows() []bridgev2.LoginFlow {
	sc.br.Log.Info().Msg("GetLoginFlows() - Sending available login flows")

	return []bridgev2.LoginFlow{
		{
			Name:        "Username/Password",
			Description: "Log in with your Steam username and password",
			ID:          "password",
		},
		{
			Name:        "QR Code",
			Description: "Log in by scanning a QR code with your Steam mobile app",
			ID:          "qr",
		},
	}
}

// CreateLogin implements bridgev2.NetworkConnector.
func (sc *SteamConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	sc.br.Log.Info().Msg("CreateLogin() - Initiating login flow")

	switch flowID {
	case "password":
		return &SteamLoginPassword{
			Main:       sc,
			User:       user,
			cancelFunc: func() {}, // Password login uses per-request timeouts instead of persistent cancellation
		}, nil
	case "qr":
		ctx, cancel := context.WithCancel(context.Background())
		return &SteamLoginQR{
			Main:       sc,
			User:       user,
			cancelCtx:  ctx,
			cancelFunc: cancel,
		}, nil
	default:
		return nil, fmt.Errorf("unknown login flow ID: %s", flowID)
	}
}

// LoadUserLogin implements bridgev2.NetworkConnector.
func (sc *SteamConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	sc.br.Log.Info().Msg("LoadUserLogin - connecting to service")

	meta := login.Metadata.(*UserLoginMetadata)
	if meta == nil {
		return fmt.Errorf("no user login metadata found")
	}

	// Create Steam client with the loaded session data
	login.Client = &SteamClient{
		UserLogin:     login,
		authClient:    sc.authClient,
		userClient:    sc.userClient,
		msgClient:     sc.msgClient,
		sessionClient: sc.sessionClient,
		br:            sc.br,
	}

	// Auto-connect the login - mautrix-go bridgev2 doesn't automatically call Connect()
	// after login, so we need to do this ourselves, following the pattern used by
	// other mautrix bridges like Signal and WhatsApp
	if client, ok := login.Client.(*SteamClient); ok {
		// Check if already connecting/connected to prevent duplicate connections
		client.stateMutex.RLock()
		alreadyConnecting := client.isConnecting || client.isConnected
		client.stateMutex.RUnlock()

		if !alreadyConnecting {
			go client.Connect(ctx)
		} else {
			sc.br.Log.Debug().Msg("Skipping auto-connect - client already connecting/connected")
		}
	} else {
		sc.br.Log.Error().Msg("Failed to cast client to SteamClient for auto-connect")
	}

	return nil
}

// GetCapabilities implements bridgev2.NetworkAPI.
func (sc *SteamClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	sc.br.Log.Info().Msg("Sending room capabilities")

	return &event.RoomFeatures{
		MaxTextLength: 4096,
		File: event.FileFeatureMap{
			event.MsgImage: &event.FileFeatures{
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"image/jpeg": event.CapLevelFullySupported, // Steam uploads to steamusercontent.com
					"image/png":  event.CapLevelFullySupported, // Steam uploads to steamusercontent.com
					"image/gif":  event.CapLevelFullySupported, // Steam uploads to steamusercontent.com
					"image/webp": event.CapLevelFullySupported, // Steam uploads to steamusercontent.com
				},
				MaxSize: 50 * 1024 * 1024, // Steam's actual image upload limit
				Caption: event.CapLevelPartialSupport, // Captions not directly supported but can be split to discrete message
			},
		},
		TypingNotifications: true,
	}
}

// Steam service process management functions

// startSteamBridgeService launches the SteamBridge service using the compiled executable
// bridgeLogWriter captures output from spawned processes and feeds it to the bridge logger
type bridgeLogWriter struct {
	logger zerolog.Logger
	source string
	level  zerolog.Level
}

func (w *bridgeLogWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimRight(string(p), "\r\n")
	if msg != "" {
		w.logger.WithLevel(w.level).
			Str("source", w.source).
			Msg(msg)
	}
	return len(p), nil
}

func (sc *SteamConnector) startSteamBridgeService(ctx context.Context) error {
	if sc.steamProcess != nil {
		return fmt.Errorf("SteamBridge service is already running")
	}

	if sc.Config.Path == "" {
		return fmt.Errorf("steam_bridge_path not configured")
	}

	sc.log.Info().Str("path", sc.Config.Path).Msg("Starting SteamBridge service")

	// Create context with cancellation for process management
	processCtx, cancel := context.WithCancel(ctx)
	sc.steamProcessCancel = cancel

	// Prepare the command to run the compiled executable
	// Determine platform-specific executable name
	execName := "steamkit-service"
	if runtime.GOOS == "windows" {
		execName += ".exe"
	}

	// Ensure we have an absolute path or explicit relative path
	var execPath string
	if filepath.IsAbs(sc.Config.Path) {
		execPath = filepath.Join(sc.Config.Path, execName)
	} else {
		// For relative paths, ensure we have an explicit "./" prefix
		execPath = filepath.Join(sc.Config.Path, execName)
		if !strings.HasPrefix(execPath, "./") && !strings.HasPrefix(execPath, "../") {
			execPath = "./" + execPath
		}
	}

	cmd := exec.CommandContext(processCtx, execPath)
	cmd.Dir = sc.Config.Path

	// Set environment variables including parent PID for monitoring
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PARENT_PID=%d", os.Getpid()),
		fmt.Sprintf("STEAM_BRIDGE_ADDRESS=%s", sc.Config.Address),
	)

	// Create bridge log writers for the steamkit service
	steamkitOut := &bridgeLogWriter{
		logger: sc.log,
		source: "steamkit",
		level:  zerolog.InfoLevel,
	}
	steamkitErr := &bridgeLogWriter{
		logger: sc.log,
		source: "steamkit",
		level:  zerolog.ErrorLevel,
	}

	cmd.Stdout = steamkitOut
	cmd.Stderr = steamkitErr

	// Start the process
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("failed to start SteamBridge service: %w", err)
	}

	sc.steamProcess = cmd
	sc.startTime = time.Now()
	sc.processLockFile = fmt.Sprintf("/tmp/steam_bridge_%d.pid", cmd.Process.Pid)

	// Create PID lock file
	pidFile, err := os.Create(sc.processLockFile)
	if err != nil {
		sc.log.Warn().Err(err).Msg("Failed to create PID lock file")
	} else {
		fmt.Fprintf(pidFile, "%d", cmd.Process.Pid)
		pidFile.Close()
	}

	sc.log.Info().
		Int("pid", cmd.Process.Pid).
		Msg("SteamBridge compiled executable started successfully with integrated logging")

	// Start goroutine to monitor process
	go func() {
		if err := cmd.Wait(); err != nil {
			if processCtx.Err() == nil {
				sc.log.Error().Err(err).Msg("SteamBridge service exited unexpectedly")
			} else {
				sc.log.Info().Msg("SteamBridge service shut down gracefully")
			}
		}
		// Cleanup PID file
		if sc.processLockFile != "" {
			os.Remove(sc.processLockFile)
		}
		sc.steamProcess = nil
		sc.steamProcessCancel = nil
	}()

	return nil
}

// healthCheckSteamBridge performs gRPC health check using the health service
func (sc *SteamConnector) healthCheckSteamBridge(ctx context.Context) error {
	if sc.Config.Address == "" {
		return fmt.Errorf("steam_bridge_address not configured")
	}

	// If we don't have a health client yet, establish connection
	if sc.healthClient == nil {
		address := sc.Config.Address
		if address == "" {
			address = "localhost:50051"
		}

		sc.log.Debug().Str("address", address).Msg("Establishing gRPC connection for health check")

		conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return fmt.Errorf("failed to connect for health check: %w", err)
		}

		sc.healthClient = grpc_health_v1.NewHealthClient(conn)
		sc.steamConn = conn // Store connection for later use
	}

	sc.log.Debug().Msg("Performing gRPC health check")

	// Create context with timeout for the health check
	healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Perform health check using the standard gRPC health service
	resp, err := sc.healthClient.Check(healthCtx, &grpc_health_v1.HealthCheckRequest{
		Service: "", // Empty string checks overall server health
	})
	if err != nil {
		sc.log.Error().Err(err).Msg("gRPC health check request failed")
		return fmt.Errorf("gRPC health check request failed: %w", err)
	}

	sc.log.Debug().
		Str("status", resp.Status.String()).
		Msg("gRPC health check response received")

	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		sc.log.Warn().
			Str("expected", grpc_health_v1.HealthCheckResponse_SERVING.String()).
			Str("actual", resp.Status.String()).
			Msg("gRPC health check failed: unexpected status")
		return fmt.Errorf("gRPC health check failed: service status is %s (expected SERVING)", resp.Status.String())
	}

	sc.log.Debug().Msg("gRPC health check passed")
	return nil
}

// waitForServiceReady waits for the SteamBridge service to be ready with timeout
func (sc *SteamConnector) waitForServiceReady(ctx context.Context) error {
	timeout := time.Duration(sc.Config.StartupTimeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second // Default timeout
	}

	sc.log.Info().Dur("timeout", timeout).Msg("Waiting for SteamBridge service to be ready")

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	startTime := time.Now()

	for {
		select {
		case <-timeoutCtx.Done():
			elapsed := time.Since(startTime)
			return fmt.Errorf("timeout waiting for SteamBridge service after %v", elapsed)
		case <-ticker.C:
			if err := sc.healthCheckSteamBridge(timeoutCtx); err == nil {
				elapsed := time.Since(startTime)
				sc.log.Info().Dur("elapsed", elapsed).Msg("SteamBridge service is ready")
				return nil
			}
			// Continue waiting on health check failures
		}
	}
}

// stopSteamBridgeService performs graceful shutdown of the SteamBridge service
func (sc *SteamConnector) stopSteamBridgeService() error {
	if sc.steamProcess == nil {
		sc.log.Debug().Msg("No SteamBridge process to stop")
		return nil
	}

	sc.log.Info().Msg("Stopping SteamBridge service")

	// Cancel the process context to signal shutdown
	if sc.steamProcessCancel != nil {
		sc.steamProcessCancel()
	}

	// Give the process time to shut down gracefully
	gracefulTimeout := 10 * time.Second
	done := make(chan error, 1)

	go func() {
		done <- sc.steamProcess.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			sc.log.Warn().Err(err).Msg("SteamBridge process exited with error")
		} else {
			sc.log.Info().Msg("SteamBridge service stopped gracefully")
		}
	case <-time.After(gracefulTimeout):
		sc.log.Warn().Msg("Graceful shutdown timeout, forcing kill")

		// Force kill the process
		if err := sc.steamProcess.Process.Signal(syscall.SIGKILL); err != nil {
			sc.log.Error().Err(err).Msg("Failed to force kill SteamBridge process")
			return fmt.Errorf("failed to force kill process: %w", err)
		}

		// Wait for the kill to complete
		<-done
		sc.log.Info().Msg("SteamBridge service force killed")
	}

	// Cleanup
	sc.steamProcess = nil
	sc.steamProcessCancel = nil

	if sc.processLockFile != "" {
		if err := os.Remove(sc.processLockFile); err != nil {
			sc.log.Warn().Err(err).Str("file", sc.processLockFile).Msg("Failed to remove PID lock file")
		}
		sc.processLockFile = ""
	}

	return nil
}

// Steam ID conversion utilities
func makeUserID(steamID uint64) networkid.UserID {
	return networkid.UserID(fmt.Sprintf("%d", steamID))
}

func makePortalID(steamID uint64) networkid.PortalID {
	return networkid.PortalID(fmt.Sprintf("%d", steamID))
}

func makeUserLoginID(steamID uint64) networkid.UserLoginID {
	return networkid.UserLoginID(fmt.Sprintf("%d", steamID))
}

// Parse Steam ID from PortalID
func parseSteamIDFromPortalID(portalID networkid.PortalID) (uint64, error) {
	// Portal IDs are now just the numeric Steam ID without prefix
	return strconv.ParseUint(string(portalID), 10, 64)
}
