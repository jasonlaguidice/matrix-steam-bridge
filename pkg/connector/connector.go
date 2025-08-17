package connector

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.mau.fi/util/configupgrade"
	"go.mau.fi/util/ptr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	grpcstatus "google.golang.org/grpc/status"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

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

type Config struct {
	Path           string `yaml:"steam_bridge_path"`
	Address        string `yaml:"steam_bridge_address"`
	AutoStart      bool   `yaml:"steam_bridge_auto_start"`
	StartupTimeout int    `yaml:"steam_bridge_startup_timeout"`
}

//go:embed example-config.yaml
var ExampleConfig string

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

// startSteamBridgeService launches the SteamBridge service using the compiled executable
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
	cmd := exec.CommandContext(processCtx, "dotnet", "SteamBridge.dll")
	cmd.Dir = sc.Config.Path

	// Set environment variables including parent PID for monitoring
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PARENT_PID=%d", os.Getpid()),
		fmt.Sprintf("STEAM_BRIDGE_ADDRESS=%s", sc.Config.Address),
	)

	// Ensure logs directory exists
	if err := os.MkdirAll("./logs", 0755); err != nil {
		cancel()
		return fmt.Errorf("failed to create logs directory: %w", err)
	}

	// Create log file with current date and time in logs directory with SteamKit prefix
	logFileName := fmt.Sprintf("./logs/SteamKit_%s.log", time.Now().Format("2006-01-02_15-04-05"))
	logFile, err := os.Create(logFileName)
	if err != nil {
		cancel()
		return fmt.Errorf("failed to create log file %s: %w", logFileName, err)
	}

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Start the process
	if err := cmd.Start(); err != nil {
		logFile.Close()
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

	// SteamBridge logs are now written to ./logs directory with SteamKit prefix for better identification
	sc.log.Info().
		Int("pid", cmd.Process.Pid).
		Str("log_file", logFileName).
		Msg("SteamBridge compiled executable started successfully")

	// Start goroutine to monitor process
	go func() {
		defer logFile.Close()
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
		NetworkIcon:      "mxc://shadowdrake.org/EeNKAcrmByNubPwoyceQsBaN",
		NetworkID:        "steam",
		BeeperBridgeType: "go.shadowdrake.org/steam",
		DefaultPort:      50051,
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

type SteamLoginPassword struct {
	Main       *SteamConnector
	User       *bridgev2.User
	cancelFunc context.CancelFunc
	// Store session info for 2FA continuation
	SessionID string
	Username  string // Store username for UserLogin creation
}

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

// Implement the bridgev2.LoginProcess interface for password login
var _ bridgev2.LoginProcess = (*SteamLoginPassword)(nil)
var _ bridgev2.LoginProcess = (*SteamLoginQR)(nil)

// Helper methods for comprehensive status reporting

// buildBridgeState creates a properly configured BridgeState following Signal bridge patterns
func (sc *SteamClient) buildBridgeState(state status.BridgeStateEvent, message string, opts ...func(*status.BridgeState)) status.BridgeState {
	bridgeState := status.BridgeState{
		StateEvent: state,
		Message:    message,
	}

	// Apply optional modifications
	for _, opt := range opts {
		opt(&bridgeState)
	}

	// Add remote profile information if available
	if meta := sc.getUserMetadata(); meta != nil {
		bridgeState.RemoteID = string(meta.RemoteID)
		bridgeState.RemoteName = meta.PersonaName
		bridgeState.RemoteProfile = &status.RemoteProfile{
			Name: meta.PersonaName,
		}
	}

	return bridgeState
}

// getUserMetadata safely retrieves user metadata
func (sc *SteamClient) getUserMetadata() *UserLoginMetadata {
	if sc.UserLogin == nil || sc.UserLogin.Metadata == nil {
		return nil
	}

	if meta, ok := sc.UserLogin.Metadata.(*UserLoginMetadata); ok {
		return meta
	}

	return nil
}

// Bridge state option helpers
func withReason(reason string) func(*status.BridgeState) {
	return func(state *status.BridgeState) {
		state.Reason = reason
	}
}

func withUserAction(action status.BridgeStateUserAction) func(*status.BridgeState) {
	return func(state *status.BridgeState) {
		state.UserAction = action
	}
}

func withInfo(info map[string]interface{}) func(*status.BridgeState) {
	return func(state *status.BridgeState) {
		state.Info = info
	}
}

// debouncedDisconnectState sends a debounced transient disconnect state following Signal bridge patterns
func (sc *SteamClient) debouncedDisconnectState() {
	sc.disconnectDebounceMutex.Lock()
	defer sc.disconnectDebounceMutex.Unlock()

	// Stop existing timer if running
	if sc.disconnectDebounceTimer != nil {
		sc.disconnectDebounceTimer.Stop()
	}

	// Start new debounce timer
	sc.disconnectDebounceTimer = time.AfterFunc(disconnectDebounceDelay, func() {
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect, "Disconnected from Steam"))
	})
}

// cancelDisconnectDebounce cancels any pending disconnect debounce timer
func (sc *SteamClient) cancelDisconnectDebounce() {
	sc.disconnectDebounceMutex.Lock()
	defer sc.disconnectDebounceMutex.Unlock()

	if sc.disconnectDebounceTimer != nil {
		sc.disconnectDebounceTimer.Stop()
		sc.disconnectDebounceTimer = nil
	}
}

// startConnectionMonitoring starts monitoring connection health with gRPC health checks
func (sc *SteamClient) startConnectionMonitoring(ctx context.Context) {
	sc.connectionMutex.Lock()
	defer sc.connectionMutex.Unlock()

	// Cancel existing monitoring if running
	if sc.connectionCancel != nil {
		sc.connectionCancel()
	}

	sc.connectionCtx, sc.connectionCancel = context.WithCancel(ctx)

	go sc.connectionMonitorLoop()
}

// connectionMonitorLoop periodically checks connection health
func (sc *SteamClient) connectionMonitorLoop() {
	ticker := time.NewTicker(30 * time.Second) // Check every 30 seconds
	defer ticker.Stop()

	for {
		select {
		case <-sc.connectionCtx.Done():
			sc.br.Log.Debug().Msg("Connection monitoring stopped")
			return
		case <-ticker.C:
			if err := sc.checkConnectionHealth(); err != nil {
				sc.br.Log.Warn().Err(err).Msg("Connection health check failed")
				// Don't immediately report disconnection - let the message stream handle it
			} else {
				sc.br.Log.Trace().Msg("gRPC heartbeat to SteamBridge service successful")
			}
		}
	}
}

// checkConnectionHealth performs a gRPC heartbeat to verify SteamBridge service is responsive
func (sc *SteamClient) checkConnectionHealth() error {
	if sc.userClient == nil {
		return fmt.Errorf("user client not initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use a non-destructive method to check if the gRPC connection is healthy
	// Get our own user info as a health check - this doesn't change any state
	meta := sc.getUserMetadata()
	if meta == nil {
		return fmt.Errorf("no user metadata for health check")
	}

	_, err := sc.userClient.GetUserInfo(ctx, &steamapi.UserInfoRequest{
		SteamId: meta.SteamID,
	})
	return err
}

// stopConnectionMonitoring stops connection monitoring
func (sc *SteamClient) stopConnectionMonitoring() {
	sc.connectionMutex.Lock()
	defer sc.connectionMutex.Unlock()

	if sc.connectionCancel != nil {
		sc.connectionCancel()
		sc.connectionCancel = nil
	}
}

var _ bridgev2.NetworkConnector = (*SteamConnector)(nil)

////////////////
// NETWORKAPI //
////////////////

var _ bridgev2.IdentifierResolvingNetworkAPI = (*SteamClient)(nil)
var _ bridgev2.UserSearchingNetworkAPI = (*SteamClient)(nil)
var _ bridgev2.BackfillingNetworkAPI = (*SteamClient)(nil)


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
}

// Constants for bridge state debouncing (following Signal bridge pattern)
const (
	disconnectDebounceDelay = 7 * time.Second // Debounce delay for transient disconnects
)







// Connect implements bridgev2.NetworkAPI.
func (sc *SteamClient) Connect(ctx context.Context) {
	// Set connection state to prevent duplicate connections
	sc.stateMutex.Lock()
	if sc.isConnecting || sc.isConnected {
		sc.stateMutex.Unlock()
		sc.br.Log.Debug().Msg("Already connecting or connected, skipping duplicate Connect() call")
		return
	}
	sc.isConnecting = true
	sc.stateMutex.Unlock()

	// Ensure we clean up connection state on exit
	defer func() {
		sc.stateMutex.Lock()
		sc.isConnecting = false
		sc.stateMutex.Unlock()
	}()

	sc.br.Log.Info().Msg("Connect() - Connecting to Steam")

	// Report service initialization
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting, "Initializing Steam services"))

	// Report connecting status
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting, "Connecting to Steam"))

	// Steam requires a persistent connection - setup that connection here
	// using gRPC API. This is missing from other attempts
	if sc.authClient == nil {
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials, "You're not logged into Steam",
			withUserAction(status.UserActionRelogin)))
		return
	}

	meta := sc.getUserMetadata()
	if meta == nil {
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials, "No user metadata found",
			withUserAction(status.UserActionRelogin)))
		return
	}

	// Attempt re-authentication with stored credentials
	if !meta.IsValid || time.Since(meta.LastValidated) > 5*time.Minute {
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting, "Re-authenticating with stored credentials"))

		// Check if we have stored credentials to attempt re-authentication
		if meta.AccessToken == "" || meta.RefreshToken == "" || (meta.AccountName == "" && meta.Username == "") {
			sc.br.Log.Warn().Msg("No stored credentials found for re-authentication")
			sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials,
				"No stored credentials - please log in",
				withUserAction(status.UserActionRelogin),
				withInfo(map[string]interface{}{
					"reason":       "missing_credentials",
					"session_type": meta.SessionType,
				})))
			return
		}

		// Attempt re-authentication using stored tokens
		// Use AccountName for authentication, fallback to Username for backward compatibility
		authUsername := meta.AccountName
		if authUsername == "" {
			authUsername = meta.Username
		}

		sc.br.Log.Info().Str("username", authUsername).Msg("Attempting re-authentication with stored tokens")

		reAuthReq := &steamapi.TokenReAuthRequest{
			AccessToken:  meta.AccessToken,
			RefreshToken: meta.RefreshToken,
			Username:     authUsername,
		}

		resp, err := sc.authClient.ReAuthenticateWithTokens(ctx, reAuthReq)
		if err != nil {
			sc.br.Log.Err(err).Msg("Failed to re-authenticate with stored tokens")
			sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateUnknownError,
				"Re-authentication failed - please check Steam service connection",
				withReason(err.Error()),
				withUserAction(status.UserActionRestart)))
			return
		}

		if !resp.Success || resp.State != steamapi.AuthStatusResponse_AUTHENTICATED {
			sc.br.Log.Warn().Str("auth_state", resp.State.String()).Str("error", resp.ErrorMessage).Msg("Token re-authentication failed")

			var userAction status.BridgeStateUserAction = status.UserActionRelogin
			var message string

			switch resp.State {
			case steamapi.AuthStatusResponse_EXPIRED:
				message = "Stored credentials expired - please log in again"
			case steamapi.AuthStatusResponse_FAILED:
				message = "Stored credentials invalid - please log in again"
			default:
				message = "Re-authentication failed - please log in again"
			}

			sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials, message,
				withUserAction(userAction),
				withInfo(map[string]interface{}{
					"auth_state":    resp.State.String(),
					"session_type":  meta.SessionType,
					"error_message": resp.ErrorMessage,
				})))
			return
		}

		// Re-authentication successful - update metadata
		sc.br.Log.Info().Msg("Successfully re-authenticated with stored tokens")

		// Update tokens if they were refreshed
		if resp.NewAccessToken != "" {
			meta.AccessToken = resp.NewAccessToken
		}
		if resp.NewRefreshToken != "" {
			meta.RefreshToken = resp.NewRefreshToken
		}

		// Update user info if provided
		if resp.UserInfo != nil {
			meta.PersonaName = resp.UserInfo.PersonaName
			meta.ProfileURL = resp.UserInfo.ProfileUrl
			meta.AvatarHash = resp.UserInfo.AvatarHash // Use hash instead of URL
		}

		meta.IsValid = true
		meta.LastValidated = time.Now()
		sc.UserLogin.Save(ctx)
	}

	// Cancel any pending disconnect debounce and report connected state
	sc.cancelDisconnectDebounce()

	// Mark as connected in state tracking
	sc.stateMutex.Lock()
	sc.isConnected = true
	sc.stateMutex.Unlock()

	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnected, "Connected to Steam"))

	// Start connection monitoring
	sc.startConnectionMonitoring(ctx)

	// Start gRPC message subscription for real-time messages
	go sc.startMessageSubscription(ctx)

	// Start session event subscription for logout notifications
	go sc.startSessionEventSubscription(ctx)
}

// startMessageSubscription starts a gRPC stream to receive real-time messages from Steam
// with robust reconnection logic and exponential backoff
func (sc *SteamClient) startMessageSubscription(ctx context.Context) {
	const (
		initialBackoff = 1 * time.Second
		maxBackoff     = 30 * time.Second
		backoffFactor  = 2.0
	)

	backoffDelay := initialBackoff

	for {
		select {
		case <-ctx.Done():
			sc.br.Log.Info().Msg("Message subscription context cancelled")
			return
		default:
			// Attempt to establish stream connection
			if err := sc.subscribeWithStream(ctx); err != nil {
				// Categorize error type
				if sc.isPermanentError(err) {
					sc.br.Log.Error().Err(err).Msg("Permanent error in message subscription, stopping")
					sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials,
						"Steam connection permanently failed",
						withReason(err.Error()),
						withUserAction(status.UserActionRelogin)))
					return
				}

				// Temporary error - update state and retry with backoff
				sc.br.Log.Warn().Err(err).
					Dur("retry_in", backoffDelay).
					Msg("Temporary error in message subscription, retrying")
				sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect,
					fmt.Sprintf("Steam connection lost, retrying in %v", backoffDelay),
					withReason(err.Error()),
					withInfo(map[string]interface{}{
						"retry_delay_seconds": backoffDelay.Seconds(),
						"error_type":          "temporary",
					})))

				// Wait for backoff delay or context cancellation
				select {
				case <-ctx.Done():
					sc.br.Log.Info().Msg("Message subscription context cancelled during backoff")
					return
				case <-time.After(backoffDelay):
					// Cancel any pending disconnect debounce and report reconnection attempt
					sc.cancelDisconnectDebounce()
					sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting, "Reconnecting to Steam"))

					// Increase backoff delay for next iteration, up to maximum
					backoffDelay = time.Duration(float64(backoffDelay) * backoffFactor)
					if backoffDelay > maxBackoff {
						backoffDelay = maxBackoff
					}
				}
			} else {
				// Successful connection - reset backoff delay
				backoffDelay = initialBackoff
			}
		}
	}
}

// subscribeWithStream handles a single stream connection lifecycle
func (sc *SteamClient) subscribeWithStream(ctx context.Context) error {
	// Report message stream initialization
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting, "Establishing Steam message stream"))

	stream, err := sc.msgClient.SubscribeToMessages(ctx, &steamapi.MessageSubscriptionRequest{})
	if err != nil {
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateUnknownError, "Failed to establish message stream",
			withReason(err.Error())))
		return fmt.Errorf("failed to start message subscription: %w", err)
	}

	sc.br.Log.Info().Msg("Started Steam message subscription")
	// Cancel any pending disconnect debounce and report streaming active
	sc.cancelDisconnectDebounce()
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnected, "Steam message streaming active"))

	for {
		select {
		case <-ctx.Done():
			sc.br.Log.Info().Msg("Stream context cancelled")
			sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting, "Message stream shutting down"))
			return ctx.Err()
		default:
			msgEvent, err := stream.Recv()
			if err != nil {
				sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect, "Steam message stream disconnected",
					withReason(err.Error())))
				return fmt.Errorf("error receiving message from stream: %w", err)
			}

			// Process incoming message
			sc.handleIncomingMessage(ctx, msgEvent)

			// Track message count for state reporting
			sc.messageCountMux.Lock()
			sc.messageCount++
			msgCount := sc.messageCount
			sc.messageCountMux.Unlock()

			// Periodically report active streaming status with message count
			if msgCount%100 == 0 {
				sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnected,
					fmt.Sprintf("Steam message streaming active (%d messages processed)", msgCount)))
			}
		}
	}
}

// isPermanentError determines if a gRPC error is permanent and should not be retried
func (sc *SteamClient) isPermanentError(err error) bool {
	if err == nil {
		return false
	}

	// Context cancellation is NOT permanent - it's expected behavior in bridgev2
	// Let bridgev2 handle connection lifecycle and retries
	if err == context.Canceled || err == context.DeadlineExceeded {
		return false
	}

	// Check gRPC status codes
	if grpcStatus, ok := grpcstatus.FromError(err); ok {
		switch grpcStatus.Code() {
		case codes.Canceled:
			return false // Context cancellation should be retryable
		case codes.InvalidArgument:
			return true
		case codes.NotFound:
			return true
		case codes.PermissionDenied:
			return true
		case codes.Unauthenticated:
			return true
		case codes.Unimplemented:
			return true
		default:
			// All other errors are considered temporary
			return false
		}
	}

	// Unknown errors are considered temporary to be safe
	return false
}

// startSessionEventSubscription starts a gRPC stream to receive session events (logout notifications)
func (sc *SteamClient) startSessionEventSubscription(ctx context.Context) {
	sc.br.Log.Info().Msg("Starting Steam session event subscription")

	// Check if sessionClient is nil before using it
	if sc.sessionClient == nil {
		sc.br.Log.Error().Msg("Session client is nil, cannot start session event subscription")
		return
	}

	stream, err := sc.sessionClient.SubscribeToSessionEvents(ctx, &steamapi.SessionSubscriptionRequest{})
	if err != nil {
		sc.br.Log.Error().Err(err).Msg("Failed to start session event subscription")
		return
	}

	for {
		select {
		case <-ctx.Done():
			sc.br.Log.Info().Msg("Session event subscription context cancelled")
			return
		default:
			sessionEvent, err := stream.Recv()
			if err != nil {
				sc.br.Log.Error().Err(err).Msg("Error receiving session event from stream")
				return
			}

			sc.handleSessionEvent(ctx, sessionEvent)
		}
	}
}

// handleSessionEvent processes incoming session events and updates bridge state
func (sc *SteamClient) handleSessionEvent(ctx context.Context, event *steamapi.SessionEvent) {
	sc.br.Log.Info().
		Str("event_type", event.EventType.String()).
		Str("reason", event.Reason).
		Int64("timestamp", event.Timestamp).
		Msg("Received session event from Steam")

	// Update connection state
	sc.stateMutex.Lock()
	sc.isConnected = false
	sc.isConnecting = false
	sc.stateMutex.Unlock()

	// Stop connection monitoring since we're disconnected
	sc.stopConnectionMonitoring()

	// Send appropriate bridge state based on event type
	switch event.EventType {
	case steamapi.SessionEventType_SESSION_REPLACED:
		// Session replacement can happen during relogin or multiple connections
		// Treat as transient disconnect to allow reconnection instead of permanent failure
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect,
			"Steam session replaced by another login",
			withReason(event.Reason)))

	case steamapi.SessionEventType_TOKEN_EXPIRED:
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials,
			"Steam credentials expired",
			withReason(event.Reason),
			withUserAction(status.UserActionRelogin)))

	case steamapi.SessionEventType_ACCOUNT_DISABLED:
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateBadCredentials,
			"Steam account disabled",
			withReason(event.Reason),
			withUserAction(status.UserActionRelogin)))

	case steamapi.SessionEventType_CONNECTION_LOST:
		// For connection loss, use transient disconnect with potential for reconnection
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateTransientDisconnect,
			"Steam connection lost",
			withReason(event.Reason)))

		// TODO: Implement automatic reconnection logic if desired

	case steamapi.SessionEventType_LOGGED_OFF:
	default:
		sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateLoggedOut,
			"Logged out from Steam",
			withReason(event.Reason)))
	}

	// Invalidate user metadata if the session is permanently invalid
	// Note: SESSION_REPLACED is now treated as transient, so don't invalidate metadata
	if event.EventType == steamapi.SessionEventType_TOKEN_EXPIRED ||
		event.EventType == steamapi.SessionEventType_ACCOUNT_DISABLED {
		
		if meta := sc.getUserMetadata(); meta != nil {
			meta.IsValid = false
			sc.UserLogin.Save(ctx)
			sc.br.Log.Info().Msg("Invalidated user metadata due to session event")
		}
	}
}


// Disconnect implements bridgev2.NetworkAPI.
func (sc *SteamClient) Disconnect() {
	sc.br.Log.Info().Msg("Disconnect() - Disconnecting from Steam")

	// Update connection state
	sc.stateMutex.Lock()
	sc.isConnected = false
	sc.isConnecting = false
	sc.stateMutex.Unlock()

	// Stop connection monitoring
	sc.stopConnectionMonitoring()

	// Use debounced disconnect state following Signal bridge pattern
	sc.debouncedDisconnectState()

	// Note: gRPC connections are managed by the SteamConnector
	// Individual client disconnection doesn't close the shared connection
}

// IsLoggedIn implements bridgev2.NetworkAPI.
func (sc *SteamClient) IsLoggedIn() bool {
	sc.br.Log.Info().Msg("IsLoggedIn() - Checking if session is still valid")

	// Must confirm if network session is still valid
	// This could be as simple as checking a single variable
	meta := sc.UserLogin.Metadata.(*UserLoginMetadata)
	return meta != nil && meta.IsValid
}

// LogoutRemote implements bridgev2.NetworkAPI.
func (sc *SteamClient) LogoutRemote(ctx context.Context) {
	sc.br.Log.Info().Msg("Logging out from Steam network")

	// Stop connection monitoring
	sc.stopConnectionMonitoring()

	// Report service shutdown initiation
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateConnecting, "Shutting down Steam services"))

	// Report logout state
	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateLoggedOut, "Logging out from Steam"))

	// Invalidate credentials with remote network and logout
	if sc.authClient != nil {
		_, err := sc.authClient.Logout(ctx, &steamapi.LogoutRequest{})
		if err != nil {
			sc.br.Log.Err(err).Msg("Failed to logout from Steam")
			sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateUnknownError,
				"Failed to complete Steam logout",
				withReason(err.Error())))
			return
		}
	}

	// Mark metadata as invalid
	if meta := sc.getUserMetadata(); meta != nil {
		meta.IsValid = false
		meta.AccessToken = ""
		meta.RefreshToken = ""
		sc.UserLogin.Save(ctx)
	}

	sc.UserLogin.BridgeState.Send(sc.buildBridgeState(status.StateLoggedOut, "Successfully logged out from Steam"))
}

// GetCapabilities implements bridgev2.NetworkAPI.
func (sc *SteamClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	sc.br.Log.Info().Msg("Sending room capabilities")

	return &event.RoomFeatures{
		MaxTextLength: 4096,
	}
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


// Identifier parsing and validation for Steam user search

// parseIdentifier parses various Steam identifier formats and returns the resolved SteamID64 and type
func parseIdentifier(identifier string) (steamID64 string, identifierType string, err error) {
	// Remove whitespace and normalize
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", "", fmt.Errorf("empty identifier")
	}

	// Check if direct SteamID64 (17 digits)
	if len(identifier) == 17 && isNumeric(identifier) {
		// Validate SteamID64 range (Steam IDs start from 76561197960265728)
		if steamID, err := strconv.ParseUint(identifier, 10, 64); err == nil {
			if steamID >= 76561197960265728 { // Minimum valid SteamID64
				return identifier, "steamid64", nil
			}
		}
	}

	// Parse Steam community URLs
	if strings.Contains(identifier, "steamcommunity.com") {
		return parseFromURL(identifier)
	}

	// Assume vanity username if it passes basic validation
	if isValidVanityUsername(identifier) {
		return identifier, "vanity", nil
	}

	return "", "", fmt.Errorf("invalid Steam identifier format: %s", identifier)
}

// parseFromURL extracts identifier from Steam community URLs
func parseFromURL(url string) (steamID64 string, identifierType string, err error) {
	// Handle various URL formats:
	// https://steamcommunity.com/id/username
	// https://steamcommunity.com/profiles/76561198123456789
	// steamcommunity.com/id/username
	// steamcommunity.com/profiles/76561198123456789

	// Remove protocol if present
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")

	// Extract path components
	if strings.HasPrefix(url, "steamcommunity.com/profiles/") {
		steamID := strings.TrimPrefix(url, "steamcommunity.com/profiles/")
		// Remove trailing slash and any query parameters
		if idx := strings.IndexAny(steamID, "/?"); idx != -1 {
			steamID = steamID[:idx]
		}
		
		// Validate it's a 17-digit SteamID64
		if len(steamID) == 17 && isNumeric(steamID) {
			if id, err := strconv.ParseUint(steamID, 10, 64); err == nil && id >= 76561197960265728 {
				return steamID, "steamid64", nil
			}
		}
		return "", "", fmt.Errorf("invalid SteamID64 in profile URL: %s", steamID)
	}

	if strings.HasPrefix(url, "steamcommunity.com/id/") {
		vanityURL := strings.TrimPrefix(url, "steamcommunity.com/id/")
		// Remove trailing slash and any query parameters
		if idx := strings.IndexAny(vanityURL, "/?"); idx != -1 {
			vanityURL = vanityURL[:idx]
		}
		
		if isValidVanityUsername(vanityURL) {
			return vanityURL, "vanity", nil
		}
		return "", "", fmt.Errorf("invalid vanity username in URL: %s", vanityURL)
	}

	return "", "", fmt.Errorf("unsupported Steam URL format: %s", url)
}

// isValidVanityUsername validates a Steam vanity username
func isValidVanityUsername(username string) bool {
	// Steam vanity URLs can contain letters, numbers, underscores, and hyphens
	// Must be 3-32 characters long
	if len(username) < 3 || len(username) > 32 {
		return false
	}

	// Check for invalid characters
	for _, char := range username {
		if !((char >= 'a' && char <= 'z') || 
			 (char >= 'A' && char <= 'Z') || 
			 (char >= '0' && char <= '9') || 
			 char == '_' || char == '-') {
			return false
		}
	}

	// Must contain at least one letter (not just numbers/symbols)
	hasLetter := false
	for _, char := range username {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') {
			hasLetter = true
			break
		}
	}
	
	if !hasLetter {
		return false
	}

	// Check for reserved/invalid words
	lower := strings.ToLower(username)
	reserved := []string{
		"invalid", "admin", "administrator", "root", "steam", "valve", 
		"support", "help", "moderator", "mod", "system", "api", "www",
		"ftp", "mail", "email", "test", "demo", "guest", "user", "null",
	}
	
	for _, word := range reserved {
		if lower == word {
			return false
		}
	}

	return true
}

// isNumeric checks if a string contains only digits
func isNumeric(s string) bool {
	for _, char := range s {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// validateSteamID64 validates a SteamID64 format and range
func validateSteamID64(steamID string) bool {
	if len(steamID) != 17 || !isNumeric(steamID) {
		return false
	}
	
	id, err := strconv.ParseUint(steamID, 10, 64)
	return err == nil && id >= 76561197960265728 // Minimum valid SteamID64
}

// IsThisUser implements bridgev2.NetworkAPI.
func (sc *SteamClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	sc.br.Log.Info().Msg("IsThisUser() - Retrieving user info")

	// Return Steam ID as it will be unique for each user
	meta := sc.UserLogin.Metadata.(*UserLoginMetadata)
	if meta == nil {
		return false
	}

	return makeUserID(meta.SteamID) == userID
}

// GetChatInfo implements bridgev2.NetworkAPI.
func (sc *SteamClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	sc.br.Log.Info().Msg("GetChatInfo() - Retrieving chat info")

	// Below only handles DMs
	// TODO: Look up how multi-participant rooms are handled in Signal
	meta := sc.UserLogin.Metadata.(*UserLoginMetadata)
	if meta == nil {
		return nil, fmt.Errorf("no user metadata found")
	}

	return &bridgev2.ChatInfo{
		Members: &bridgev2.ChatMemberList{
			IsFull: true,
			Members: []bridgev2.ChatMember{
				{
					EventSender: bridgev2.EventSender{
						IsFromMe: true,
						Sender:   makeUserID(meta.SteamID),
					},
					Membership: event.MembershipJoin,
					PowerLevel: ptr.Ptr(50),
				},
				{
					EventSender: bridgev2.EventSender{
						Sender: networkid.UserID(portal.ID),
					},
					Membership: event.MembershipJoin,
					PowerLevel: ptr.Ptr(50),
				},
			},
		},
	}, nil
}

// GetUserInfo implements bridgev2.NetworkAPI.
func (sc *SteamClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	sc.br.Log.Info().Str("ghost_id", string(ghost.ID)).Msg("GetUserInfo() - Retrieving user info")

	// Parse Steam ID from ghost ID (now just numeric)
	steamID, err := strconv.ParseUint(string(ghost.ID), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Steam ID from ghost ID %s: %w", ghost.ID, err)
	}

	// Get or initialize ghost metadata
	ghostMeta := ghost.Metadata.(*GhostMetadata)
	if ghostMeta == nil {
		ghostMeta = &GhostMetadata{SteamID: steamID}
		ghost.Metadata = ghostMeta
	}

	// Check if we need to refresh profile data (cache for 1 hour)
	shouldRefreshProfile := ghostMeta.LastProfileUpdate.IsZero() ||
		time.Since(ghostMeta.LastProfileUpdate) > time.Hour ||
		ghostMeta.PersonaName == "" // Force refresh if missing essential data

	// Always fetch current user info for real-time data (presence, game status)
	resp, err := sc.userClient.GetUserInfo(ctx, &steamapi.UserInfoRequest{
		SteamId: steamID,
	})
	if err != nil {
		// On API error, fall back to cached data if available
		if ghostMeta.PersonaName != "" {
			sc.br.Log.Warn().Err(err).Msg("Failed to fetch fresh user info, using cached data")
			userInfo := &bridgev2.UserInfo{
				Identifiers: []string{fmt.Sprintf("%d", ghostMeta.SteamID)},
				Name:        ptr.Ptr(ghostMeta.PersonaName),
				IsBot:       ptr.Ptr(false),
			}
			
			// Add cached avatar if available
			if ghostMeta.AvatarHash != "" {
				userInfo.Avatar = &bridgev2.Avatar{
					ID: networkid.AvatarID(ghostMeta.AvatarHash),
					Get: sc.createAvatarDownloader(steamID),
				}
			}
			
			return userInfo, nil
		}
		return nil, fmt.Errorf("failed to get user info for Steam ID %d: %w", steamID, err)
	}

	if resp.UserInfo == nil {
		return nil, fmt.Errorf("no user info returned for Steam ID %d", steamID)
	}

	userInfo := resp.UserInfo

	// Update cached profile data if needed
	if shouldRefreshProfile {
		ghostMeta.SteamID = userInfo.SteamId
		ghostMeta.AccountName = userInfo.AccountName
		ghostMeta.PersonaName = userInfo.PersonaName
		ghostMeta.ProfileURL = userInfo.ProfileUrl
		ghostMeta.LastProfileUpdate = time.Now()

		// Update relationship status if available
		// This would come from a friendship API call if implemented
		// ghostMeta.Relationship = resp.RelationshipStatus
	}

	// Check for avatar changes (hash comparison for efficiency)
	if userInfo.AvatarHash != "" {
		// Use the avatar hash from Steam API for change detection
		if userInfo.AvatarHash != ghostMeta.AvatarHash {
			ghostMeta.AvatarHash = userInfo.AvatarHash
			ghostMeta.LastAvatarUpdate = time.Now()
		}
	}

	// Create UserInfo with current data (NOT cached status/game data)
	userInfoResult := &bridgev2.UserInfo{
		Identifiers: []string{fmt.Sprintf("%d", userInfo.SteamId)},
		Name:        ptr.Ptr(userInfo.PersonaName),
		IsBot:       ptr.Ptr(false),
	}

	// Handle avatar if present
	if userInfo.AvatarHash != "" {
		userInfoResult.Avatar = &bridgev2.Avatar{
			ID: networkid.AvatarID(userInfo.AvatarHash), // Use hash as stable ID
			Get: sc.createAvatarDownloader(userInfo.SteamId),
		}
	}

	// NOTE: We do NOT include Status or CurrentGame in UserInfo as these are
	// presence data that should be handled through presence events, not user profile

	return userInfoResult, nil
}

// createAvatarDownloader creates a function that downloads avatar data for the given Steam ID
func (sc *SteamClient) createAvatarDownloader(steamID uint64) func(ctx context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		// Get the ghost metadata to access the avatar hash
		userID := makeUserID(steamID)
		ghost, err := sc.UserLogin.Bridge.GetGhostByID(ctx, userID)
		if err != nil {
			return nil, fmt.Errorf("failed to get ghost for Steam ID %d: %w", steamID, err)
		}
		
		ghostMeta, ok := ghost.Metadata.(*GhostMetadata)
		if !ok || ghostMeta == nil || ghostMeta.AvatarHash == "" {
			return nil, fmt.Errorf("no avatar hash available for Steam ID %d", steamID)
		}
		
		// Build Steam CDN URL from hash: https://avatars.steamstatic.com/{hash}_full.jpg
		avatarURL := fmt.Sprintf("https://avatars.steamstatic.com/%s_full.jpg", ghostMeta.AvatarHash)
		
		// Download the avatar from Steam CDN
		return sc.downloadImageFromURL(ctx, avatarURL)
	}
}

// downloadImageFromURL downloads an image from the given URL and returns the image data
func (sc *SteamClient) downloadImageFromURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %s: %w", url, err)
	}
	
	// Set user agent to identify as Steam bridge
	req.Header.Set("User-Agent", "Steam-Bridge/1.0")
	
	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download image from %s: %w", url, err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download image from %s: HTTP %d", url, resp.StatusCode)
	}
	
	// Limit response size to prevent abuse (10MB max)
	const maxImageSize = 10 * 1024 * 1024
	limitedReader := io.LimitReader(resp.Body, maxImageSize)
	
	imageData, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read image data from %s: %w", url, err)
	}
	
	return imageData, nil
}

// extractAvatarHash extracts a hash from Steam avatar URL for change detection
func extractAvatarHash(avatarURL string) string {
	// Steam avatar URLs typically look like:
	// https://avatars.steamstatic.com/b5bd56c1aa4644a474a2e4972be27ef9e82e517e_full.jpg
	// Extract the hash part for change detection
	if idx := strings.LastIndex(avatarURL, "/"); idx != -1 {
		filename := avatarURL[idx+1:]
		if dotIdx := strings.LastIndex(filename, "."); dotIdx != -1 {
			return filename[:dotIdx]
		}
		return filename
	}
	return avatarURL // Fallback to full URL if parsing fails
}

// HandleMatrixMessage implements bridgev2.NetworkAPI.
func (sc *SteamClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (message *bridgev2.MatrixMessageResponse, err error) {
	sc.br.Log.Info().Str("event_type", msg.Event.Type.String()).Msg("HandleMatrixMessage() - Processing Matrix message")

	// Parse target Steam ID from portal ID
	targetSteamID, err := parseSteamIDFromPortalID(msg.Portal.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to parse target Steam ID from portal %s: %w", msg.Portal.ID, err)
	}

	switch msg.Event.Type {
	case event.EventMessage:
		content := msg.Event.Content.AsMessage()
		if content != nil && content.MsgType == event.MsgImage {
			return sc.handleImageMessage(ctx, msg, targetSteamID)
		}
		return sc.handleTextMessage(ctx, msg, targetSteamID)
	case event.EventSticker:
		return sc.handleStickerMessage(ctx, msg, targetSteamID)
	default:
		sc.br.Log.Warn().Str("event_type", msg.Event.Type.String()).Msg("Unsupported message type")
		return nil, fmt.Errorf("unsupported message type: %v", msg.Event.Type)
	}
}

// handleTextMessage processes text messages and sends them to Steam
func (sc *SteamClient) handleTextMessage(ctx context.Context, msg *bridgev2.MatrixMessage, targetSteamID uint64) (*bridgev2.MatrixMessageResponse, error) {
	content := msg.Event.Content.AsMessage()
	if content == nil {
		return nil, fmt.Errorf("failed to parse message content")
	}

	// Extract text content
	var messageText string
	switch content.MsgType {
	case event.MsgText:
		messageText = content.Body
	case event.MsgEmote:
		// Steam emotes are handled differently - format as action
		messageText = content.Body
	case event.MsgNotice:
		messageText = content.Body
	default:
		return nil, fmt.Errorf("unsupported message type: %s", content.MsgType)
	}

	if messageText == "" {
		return nil, fmt.Errorf("empty message text")
	}

	// Determine Steam message type
	var steamMsgType steamapi.MessageType
	switch content.MsgType {
	case event.MsgEmote:
		steamMsgType = steamapi.MessageType_EMOTE
	default:
		steamMsgType = steamapi.MessageType_CHAT_MESSAGE
	}

	// Send message via gRPC
	resp, err := sc.msgClient.SendMessage(ctx, &steamapi.SendMessageRequest{
		TargetSteamId: targetSteamID,
		Message:       messageText,
		MessageType:   steamMsgType,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to send message to Steam: %w", err)
	}

	if !resp.Success {
		return nil, fmt.Errorf("steam message send failed: %s", resp.ErrorMessage)
	}

	// Create message metadata
	msgMeta := &MessageMetadata{
		SteamMessageType: steamMsgType.String(),
		IsEcho:           false, // This is our outgoing message
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        networkid.MessageID(fmt.Sprintf("%d:%d", targetSteamID, resp.Timestamp)),
			MXID:      msg.Event.ID,
			Timestamp: time.Unix(resp.Timestamp, 0),
			Metadata:  msgMeta,
		},
	}, nil
}

// handleStickerMessage processes sticker messages (Steam doesn't natively support stickers, so convert to text)
func (sc *SteamClient) handleStickerMessage(ctx context.Context, msg *bridgev2.MatrixMessage, targetSteamID uint64) (*bridgev2.MatrixMessageResponse, error) {
	content := msg.Event.Content.AsMessage()
	if content == nil {
		return nil, fmt.Errorf("failed to parse sticker content")
	}

	// Convert sticker to text message since Steam doesn't support stickers natively
	stickerText := fmt.Sprintf("[Sticker: %s]", content.Body)

	resp, err := sc.msgClient.SendMessage(ctx, &steamapi.SendMessageRequest{
		TargetSteamId: targetSteamID,
		Message:       stickerText,
		MessageType:   steamapi.MessageType_CHAT_MESSAGE,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to send sticker message to Steam: %w", err)
	}

	if !resp.Success {
		return nil, fmt.Errorf("steam sticker message send failed: %s", resp.ErrorMessage)
	}

	msgMeta := &MessageMetadata{
		SteamMessageType: "STICKER_AS_TEXT",
		IsEcho:           false,
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        networkid.MessageID(fmt.Sprintf("%d:%d", targetSteamID, resp.Timestamp)),
			MXID:      msg.Event.ID,
			Timestamp: time.Unix(resp.Timestamp, 0),
			Metadata:  msgMeta,
		},
	}, nil
}

// handleImageMessage processes image messages from Matrix and sends them to Steam
func (sc *SteamClient) handleImageMessage(ctx context.Context, msg *bridgev2.MatrixMessage, targetSteamID uint64) (*bridgev2.MatrixMessageResponse, error) {
	content := msg.Event.Content.AsMessage()
	if content == nil {
		return nil, fmt.Errorf("failed to parse image content")
	}

	sc.br.Log.Info().
		Str("image_url", string(content.URL)).
		Str("mime_type", content.Info.MimeType).
		Int("size", content.Info.Size).
		Str("caption", content.Body).
		Msg("Processing image message from Matrix")

	// Download image from Matrix
	imageData, err := sc.br.Bot.DownloadMedia(ctx, content.URL, content.File)
	if err != nil {
		return nil, fmt.Errorf("failed to download image from Matrix: %w", err)
	}

	// Upload image to Steam via gRPC
	uploadResp, err := sc.msgClient.UploadImageToSteam(ctx, &steamapi.UploadImageRequest{
		ImageData: imageData,
		MimeType:  content.Info.MimeType,
		Filename:  content.Body, // Use caption as filename, fallback to default if empty
	})
	if err != nil {
		return nil, fmt.Errorf("failed to upload image to Steam: %w", err)
	}

	if !uploadResp.Success {
		return nil, fmt.Errorf("steam image upload failed: %s", uploadResp.ErrorMessage)
	}

	// Send message with image URL
	resp, err := sc.msgClient.SendMessage(ctx, &steamapi.SendMessageRequest{
		TargetSteamId: targetSteamID,
		Message:       content.Body, // Caption
		MessageType:   steamapi.MessageType_CHAT_MESSAGE,
		ImageUrl:      &uploadResp.ImageUrl,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to send image message to Steam: %w", err)
	}

	if !resp.Success {
		return nil, fmt.Errorf("steam image message send failed: %s", resp.ErrorMessage)
	}

	msgMeta := &MessageMetadata{
		SteamMessageType: "IMAGE",
		IsEcho:           false,
		ImageURL:         uploadResp.ImageUrl,
	}

	sc.br.Log.Info().
		Str("steam_image_url", uploadResp.ImageUrl).
		Int64("timestamp", resp.Timestamp).
		Msg("Image message sent to Steam successfully")

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        networkid.MessageID(fmt.Sprintf("%d:%d", targetSteamID, resp.Timestamp)),
			MXID:      msg.Event.ID,
			Timestamp: time.Unix(resp.Timestamp, 0),
			Metadata:  msgMeta,
		},
	}, nil
}

// handleIncomingMessage processes incoming messages from Steam and sends them to Matrix
func (sc *SteamClient) handleIncomingMessage(_ context.Context, msgEvent *steamapi.MessageEvent) {
	sc.br.Log.Info().
		Uint64("sender_steam_id", msgEvent.SenderSteamId).
		Str("message_type", msgEvent.MessageType.String()).
		Int64("timestamp", msgEvent.Timestamp).
		Msg("Received message from Steam")

	// Detect echo messages from other Steam clients
	if msgEvent.IsEcho {
		sc.br.Log.Debug().
			Uint64("sender_steam_id", msgEvent.SenderSteamId).
			Msg("Processing echo message from other Steam client")
		// Continue processing instead of skipping
	}

	// Generate message ID
	var msgID string
	switch msgEvent.MessageType {
	case steamapi.MessageType_INVITE_GAME:
		msgID = fmt.Sprintf("%d:%d:invite", msgEvent.SenderSteamId, msgEvent.Timestamp)
	default:
		msgID = fmt.Sprintf("%d:%d", msgEvent.SenderSteamId, msgEvent.Timestamp)
	}

	// Get current user's Steam ID to determine if this is a DM
	meta := sc.UserLogin.Metadata.(*UserLoginMetadata)
	if meta == nil {
		sc.br.Log.Error().Msg("No user metadata found for handling incoming message")
		return
	}

	// Determine portal ID - for DMs, use the other user's Steam ID
	var portalID networkid.PortalID
	if msgEvent.TargetSteamId == meta.SteamID {
		// This is a message sent to us, portal is the sender
		portalID = makePortalID(msgEvent.SenderSteamId)
	} else {
		// This is a message we sent from another client, portal is the target
		portalID = makePortalID(msgEvent.TargetSteamId)
	}

	// Create portal key
	portalKey := networkid.PortalKey{
		ID:       portalID,
		Receiver: sc.UserLogin.ID,
	}

	// Determine sender ID
	senderID := makeUserID(msgEvent.SenderSteamId)

	// Create appropriate EventSender based on whether this is an echo message
	var eventSender bridgev2.EventSender
	if msgEvent.IsEcho {
		// Echo message from our other Steam clients - show as "from me"
		eventSender = bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: sc.UserLogin.ID,
			Sender:      senderID,
		}
	} else {
		// Regular incoming message from another user
		eventSender = bridgev2.EventSender{
			Sender: senderID,
		}
	}

	// Message metadata will be created in the conversion function

	// Create appropriate remote event based on message type
	switch msgEvent.MessageType {
	case steamapi.MessageType_CHAT_MESSAGE:
		remoteMsg := &simplevent.Message[*steamapi.MessageEvent]{
			EventMeta: simplevent.EventMeta{
				Type: bridgev2.RemoteEventMessage,
				LogContext: func(c zerolog.Context) zerolog.Context {
					return c.Uint64("sender_steam_id", msgEvent.SenderSteamId).
						Int64("timestamp", msgEvent.Timestamp)
				},
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       eventSender,
				Timestamp:    time.Unix(msgEvent.Timestamp, 0),
			},
			Data:               msgEvent,
			ID:                 networkid.MessageID(msgID),
			ConvertMessageFunc: sc.convertSteamMessage,
		}
		sc.br.QueueRemoteEvent(sc.UserLogin, remoteMsg)

	case steamapi.MessageType_EMOTE:
		remoteMsg := &simplevent.Message[*steamapi.MessageEvent]{
			EventMeta: simplevent.EventMeta{
				Type: bridgev2.RemoteEventMessage,
				LogContext: func(c zerolog.Context) zerolog.Context {
					return c.Uint64("sender_steam_id", msgEvent.SenderSteamId).
						Int64("timestamp", msgEvent.Timestamp)
				},
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       eventSender,
				Timestamp:    time.Unix(msgEvent.Timestamp, 0),
			},
			Data:               msgEvent,
			ID:                 networkid.MessageID(msgID),
			ConvertMessageFunc: sc.convertSteamMessage,
		}
		sc.br.QueueRemoteEvent(sc.UserLogin, remoteMsg)

	case steamapi.MessageType_TYPING:
		// Handle typing notifications - create typing event
		typingEvent := &simplevent.Typing{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventTyping,
				PortalKey: portalKey,
				Sender:    eventSender,
				Timestamp: time.Unix(msgEvent.Timestamp, 0),
			},
			Timeout: 5 * time.Second,
		}
		sc.br.QueueRemoteEvent(sc.UserLogin, typingEvent)

	case steamapi.MessageType_INVITE_GAME:
		// Handle game invites as special messages
		remoteMsg := &simplevent.Message[*steamapi.MessageEvent]{
			EventMeta: simplevent.EventMeta{
				Type: bridgev2.RemoteEventMessage,
				LogContext: func(c zerolog.Context) zerolog.Context {
					return c.Uint64("sender_steam_id", msgEvent.SenderSteamId).
						Int64("timestamp", msgEvent.Timestamp).
						Str("message_type", "game_invite")
				},
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       eventSender,
				Timestamp:    time.Unix(msgEvent.Timestamp, 0),
			},
			Data:               msgEvent,
			ID:                 networkid.MessageID(msgID),
			ConvertMessageFunc: sc.convertSteamMessage,
		}
		sc.br.QueueRemoteEvent(sc.UserLogin, remoteMsg)

	default:
		sc.br.Log.Warn().Str("message_type", msgEvent.MessageType.String()).Msg("Unsupported message type")
	}
}

// convertSteamMessage converts a Steam message event to a Matrix message
func (sc *SteamClient) convertSteamMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *steamapi.MessageEvent) (*bridgev2.ConvertedMessage, error) {
	var content *event.MessageEventContent

	switch data.MessageType {
	case steamapi.MessageType_CHAT_MESSAGE:
		// Check if this message contains an image URL
		if data.ImageUrl != nil && *data.ImageUrl != "" {
			return sc.convertImageMessage(ctx, portal, intent, data)
		}
		
		content = &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    data.Message,
		}
	case steamapi.MessageType_EMOTE:
		content = &event.MessageEventContent{
			MsgType: event.MsgEmote,
			Body:    data.Message,
		}
	case steamapi.MessageType_INVITE_GAME:
		content = &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    fmt.Sprintf("🎮 Game Invite: %s", data.Message),
		}
	default:
		return nil, fmt.Errorf("unsupported message type: %s", data.MessageType.String())
	}

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

// convertImageMessage converts a Steam image message to a Matrix image message
func (sc *SteamClient) convertImageMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *steamapi.MessageEvent) (*bridgev2.ConvertedMessage, error) {
	if data.ImageUrl == nil || *data.ImageUrl == "" {
		return nil, fmt.Errorf("no image URL provided in message")
	}

	imageURL := *data.ImageUrl
	sc.br.Log.Info().
		Str("image_url", imageURL).
		Str("caption", data.Message).
		Msg("Converting Steam image message to Matrix")

	// Download image from Steam
	downloadResp, err := sc.msgClient.DownloadImageFromSteam(ctx, &steamapi.DownloadImageRequest{
		ImageUrl: imageURL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download image from Steam: %w", err)
	}

	if !downloadResp.Success {
		return nil, fmt.Errorf("steam image download failed: %s", downloadResp.ErrorMessage)
	}

	// Extract filename from URL or use default
	filename := extractFilenameFromURL(imageURL)
	if filename == "" {
		filename = "image"
	}

	// Add file extension based on MIME type
	if ext := getFileExtensionFromMimeType(downloadResp.MimeType); ext != "" {
		filename += "." + ext
	}

	// Upload image to Matrix
	mxcURL, encryptedFile, err := intent.UploadMedia(ctx, portal.MXID, downloadResp.ImageData, filename, downloadResp.MimeType)
	if err != nil {
		return nil, fmt.Errorf("failed to upload image to Matrix: %w", err)
	}

	// Create Matrix image message content
	content := &event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    data.Message, // Use Steam message as caption
		URL:     mxcURL,
		File:    encryptedFile,
		Info: &event.FileInfo{
			MimeType: downloadResp.MimeType,
			Size:     len(downloadResp.ImageData),
		},
	}

	// If caption is empty, use filename as body
	if content.Body == "" {
		content.Body = filename
	}

	sc.br.Log.Info().
		Str("matrix_mxc_url", string(mxcURL)).
		Str("filename", filename).
		Int("size", len(downloadResp.ImageData)).
		Msg("Image converted and uploaded to Matrix successfully")

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

// extractFilenameFromURL extracts a filename from a URL path
func extractFilenameFromURL(imageURL string) string {
	// Simple extraction - get the last path component
	if idx := strings.LastIndex(imageURL, "/"); idx != -1 && idx < len(imageURL)-1 {
		return imageURL[idx+1:]
	}
	return ""
}

// getFileExtensionFromMimeType returns the appropriate file extension for a MIME type
func getFileExtensionFromMimeType(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return ""
	}
}

// ResolveIdentifier implements bridgev2.IdentifierResolvingNetworkAPI.
func (sc *SteamClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	sc.br.Log.Info().Str("identifier", identifier).Bool("create_chat", createChat).Msg("ResolveIdentifier() - Resolving Steam user identifier")

	// Parse and validate the identifier
	parsedID, identifierType, err := parseIdentifier(identifier)
	if err != nil {
		return nil, fmt.Errorf("failed to parse identifier '%s': %w", identifier, err)
	}

	// Resolve the identifier to a SteamID64
	steamID64, err := sc.resolveToSteamID64(ctx, parsedID, identifierType)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve identifier '%s': %w", identifier, err)
	}

	// Convert SteamID64 string to uint64 for network ID creation
	steamIDUint, err := strconv.ParseUint(steamID64, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse resolved SteamID64 '%s': %w", steamID64, err)
	}

	// Create network IDs
	userID := makeUserID(steamIDUint)
	portalID := networkid.PortalKey{
		ID:       makePortalID(steamIDUint),
		Receiver: sc.UserLogin.ID,
	}

	// Get or create ghost object
	ghost, err := sc.UserLogin.Bridge.GetGhostByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get ghost for user %s: %w", userID, err)
	}

	// Get or create portal object (only if createChat is true)
	var portal *bridgev2.Portal
	if createChat {
		portal, err = sc.UserLogin.Bridge.GetPortalByKey(ctx, portalID)
		if err != nil {
			return nil, fmt.Errorf("failed to get portal for key %v: %w", portalID, err)
		}
	}

	// Get user info for the ghost
	userInfo, err := sc.GetUserInfo(ctx, ghost)
	if err != nil {
		// If we can't get user info, the user might not exist or be private
		return nil, fmt.Errorf("failed to get user info for Steam user %s: %w", steamID64, err)
	}

	// Build the response
	response := &bridgev2.ResolveIdentifierResponse{
		Ghost:    ghost,
		UserID:   userID,
		UserInfo: userInfo,
	}

	// Add chat info if createChat is true and we have a portal
	if createChat && portal != nil {
		chatInfo, err := sc.GetChatInfo(ctx, portal)
		if err != nil {
			sc.br.Log.Warn().Err(err).Msg("Failed to get chat info, continuing without it")
			// Don't fail the whole request for chat info issues
			chatInfo = &bridgev2.ChatInfo{} // Provide empty chat info as fallback
		}

		response.Chat = &bridgev2.CreateChatResponse{
			Portal:     portal,
			PortalKey:  portalID,
			PortalInfo: chatInfo,
		}
	}

	sc.br.Log.Info().
		Str("resolved_steam_id", steamID64).
		Str("user_id", string(userID)).
		Bool("chat_created", response.Chat != nil).
		Msg("Successfully resolved Steam identifier")

	return response, nil
}

// resolveToSteamID64 resolves a parsed identifier to a SteamID64 string
func (sc *SteamClient) resolveToSteamID64(ctx context.Context, identifier, identifierType string) (string, error) {
	switch identifierType {
	case "steamid64":
		// Already a SteamID64, just validate it
		if !validateSteamID64(identifier) {
			return "", fmt.Errorf("invalid SteamID64: %s", identifier)
		}
		return identifier, nil

	case "vanity":
		// Need to resolve vanity URL via gRPC
		return sc.resolveVanityURL(ctx, identifier)

	default:
		return "", fmt.Errorf("unsupported identifier type: %s", identifierType)
	}
}

// resolveVanityURL resolves a Steam vanity URL to SteamID64 via gRPC
func (sc *SteamClient) resolveVanityURL(ctx context.Context, vanityURL string) (string, error) {
	if sc.userClient == nil {
		return "", fmt.Errorf("user client not initialized")
	}

	sc.br.Log.Debug().Str("vanity_url", vanityURL).Msg("Resolving vanity URL via gRPC")

	resp, err := sc.userClient.ResolveVanityURL(ctx, &steamapi.ResolveVanityURLRequest{
		VanityUrl: vanityURL,
	})
	if err != nil {
		return "", fmt.Errorf("gRPC call to ResolveVanityURL failed: %w", err)
	}

	if !resp.Success {
		// Handle specific error cases
		errorMsg := resp.ErrorMessage
		if errorMsg == "" {
			errorMsg = "unknown error"
		}

		// Check for common error patterns
		if strings.Contains(strings.ToLower(errorMsg), "not found") {
			return "", fmt.Errorf("Steam user with vanity URL '%s' not found", vanityURL)
		}
		if strings.Contains(strings.ToLower(errorMsg), "private") {
			return "", fmt.Errorf("Steam user '%s' profile is private", vanityURL)
		}

		return "", fmt.Errorf("failed to resolve vanity URL '%s': %s", vanityURL, errorMsg)
	}

	// Validate the returned SteamID64
	steamID := resp.SteamId
	if !validateSteamID64(steamID) {
		return "", fmt.Errorf("invalid SteamID64 returned from vanity URL resolution: %s", steamID)
	}

	sc.br.Log.Debug().
		Str("vanity_url", vanityURL).
		Str("resolved_steam_id", steamID).
		Msg("Successfully resolved vanity URL")

	return steamID, nil
}

// SearchUsers implements bridgev2.UserSearchingNetworkAPI.
func (sc *SteamClient) SearchUsers(ctx context.Context, query string) ([]*bridgev2.ResolveIdentifierResponse, error) {
	sc.br.Log.Info().Str("query", query).Msg("SearchUsers() - Searching for Steam users in friends list")

	// Validate search query
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty search query")
	}

	if len(query) < 2 {
		return nil, fmt.Errorf("search query must be at least 2 characters long")
	}

	if len(query) > 100 {
		return nil, fmt.Errorf("search query too long (max 100 characters)")
	}

	// Use the gRPC UserService to get friends list and filter by query
	if sc.userClient == nil {
		return nil, fmt.Errorf("user client not initialized")
	}

	sc.br.Log.Debug().Str("query", query).Msg("Fetching friends list for user search")

	// Get the user's friends list
	resp, err := sc.userClient.GetFriendsList(ctx, &steamapi.FriendsListRequest{})
	if err != nil {
		return nil, fmt.Errorf("gRPC call to GetFriendsList failed: %w", err)
	}

	if resp.Friends == nil {
		sc.br.Log.Info().Msg("No friends found")
		return []*bridgev2.ResolveIdentifierResponse{}, nil
	}

	// Filter friends based on query (case-insensitive partial matching)
	queryLower := strings.ToLower(query)
	var matchingFriends []*steamapi.Friend
	
	for _, friend := range resp.Friends {
		// Check if the query matches the persona name
		if strings.Contains(strings.ToLower(friend.PersonaName), queryLower) {
			matchingFriends = append(matchingFriends, friend)
		}
		// Also check if query matches SteamID64 string representation
		steamIDStr := fmt.Sprintf("%d", friend.SteamId)
		if strings.Contains(steamIDStr, query) {
			matchingFriends = append(matchingFriends, friend)
		}
	}

	// Limit results to reasonable number for UI
	maxResults := 10
	if len(matchingFriends) > maxResults {
		matchingFriends = matchingFriends[:maxResults]
	}

	// Convert matching friends to ResolveIdentifierResponse format
	results := make([]*bridgev2.ResolveIdentifierResponse, 0, len(matchingFriends))

	for _, friend := range matchingFriends {
		// Convert to our network IDs
		steamIDUint := friend.SteamId
		userID := makeUserID(steamIDUint)

		// Get or create ghost object
		ghost, err := sc.UserLogin.Bridge.GetGhostByID(ctx, userID)
		if err != nil {
			sc.br.Log.Warn().Err(err).Uint64("steam_id", steamIDUint).Msg("Failed to get ghost for search result, skipping")
			continue
		}

		// Create user info from friend data
		userInfo := &bridgev2.UserInfo{
			Identifiers: []string{fmt.Sprintf("%d", steamIDUint)},
			Name:        &friend.PersonaName,
			IsBot:       ptr.Ptr(false),
		}

		// Handle avatar if present
		if friend.AvatarHash != "" {
			userInfo.Avatar = &bridgev2.Avatar{
				ID: networkid.AvatarID(friend.AvatarHash), // Use hash as stable ID
				Get: sc.createAvatarDownloader(friend.SteamId),
			}
		}

		// Update ghost metadata with friend data
		if ghostMeta, ok := ghost.Metadata.(*GhostMetadata); ok && ghostMeta != nil {
			ghostMeta.SteamID = steamIDUint
			ghostMeta.PersonaName = friend.PersonaName
			ghostMeta.AvatarHash = friend.AvatarHash // Use hash from API
			ghostMeta.LastProfileUpdate = time.Now()
			// Store relationship info if available
			ghostMeta.Relationship = friend.Relationship.String()
		} else {
			// Initialize metadata if it doesn't exist
			ghost.Metadata = &GhostMetadata{
				SteamID:             steamIDUint,
				PersonaName:         friend.PersonaName,
				AvatarHash:          friend.AvatarHash, // Use hash from API
				LastProfileUpdate:   time.Now(),
				Relationship:        friend.Relationship.String(),
			}
		}

		results = append(results, &bridgev2.ResolveIdentifierResponse{
			Ghost:    ghost,
			UserID:   userID,
			UserInfo: userInfo,
			// Note: We don't create chats in search results, only when explicitly requested
			Chat: nil,
		})
	}

	sc.br.Log.Info().
		Str("query", query).
		Int("total_friends", len(resp.Friends)).
		Int("matching_results", len(results)).
		Msg("Friends list search completed successfully")

	return results, nil
}

// FetchMessages implements bridgev2.BackfillingNetworkAPI
func (sc *SteamClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	sc.br.Log.Debug().
		Str("portal_id", string(params.Portal.PortalKey.ID)).
		Str("receiver", string(params.Portal.PortalKey.Receiver)).
		Bool("forward", params.Forward).
		Int("count", params.Count).
		Msg("Fetching message history")

	if sc.msgClient == nil {
		return nil, fmt.Errorf("messaging client not initialized")
	}

	// Extract chat IDs from portal key
	chatGroupID, chatID, err := sc.extractChatIDs(params.Portal.PortalKey)
	if err != nil {
		return nil, fmt.Errorf("failed to extract chat IDs: %w", err)
	}

	// Prepare pagination cursor
	var lastTime, lastOrdinal uint32
	if params.Cursor != "" {
		cursor := parsePaginationCursor(params.Cursor)
		lastTime = cursor.Time
		lastOrdinal = cursor.Ordinal
	} else if params.AnchorMessage != nil {
		// Use anchor message timestamp if no cursor provided
		lastTime = uint32(params.AnchorMessage.Timestamp.Unix())
		// Extract ordinal from message metadata if available
		if msgMeta, ok := params.AnchorMessage.Metadata.(*MessageMetadata); ok && msgMeta != nil {
			lastOrdinal = msgMeta.Ordinal
		}
	}

	// Call gRPC service
	historyResp, err := sc.msgClient.GetChatMessageHistory(ctx, &steamapi.ChatMessageHistoryRequest{
		ChatGroupId: chatGroupID,
		ChatId:      chatID,
		LastTime:    lastTime,
		LastOrdinal: lastOrdinal,
		MaxCount:    uint32(params.Count),
		Forward:     params.Forward,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get chat history: %w", err)
	}

	if !historyResp.Success {
		return nil, fmt.Errorf("steam API error: %s", historyResp.ErrorMessage)
	}

	// Convert Steam messages to BackfillMessages
	messages := make([]*bridgev2.BackfillMessage, 0, len(historyResp.Messages))
	for _, steamMsg := range historyResp.Messages {
		backfillMsg, err := sc.convertSteamMessageToBackfill(ctx, steamMsg, params.Portal)
		if err != nil {
			sc.br.Log.Warn().Err(err).
				Uint64("sender", steamMsg.SenderSteamId).
				Uint32("timestamp", steamMsg.Timestamp).
				Msg("Failed to convert Steam message, skipping")
			continue
		}
		messages = append(messages, backfillMsg)
	}

	// Generate next cursor
	var nextCursor networkid.PaginationCursor
	if historyResp.HasMore && historyResp.NextTime > 0 {
		nextCursor = createPaginationCursor(historyResp.NextTime, historyResp.NextOrdinal)
	}

	response := &bridgev2.FetchMessagesResponse{
		Messages: messages,
		Cursor:   nextCursor,
		HasMore:  historyResp.HasMore,
		Forward:  params.Forward,
	}

	sc.br.Log.Debug().
		Int("message_count", len(messages)).
		Bool("has_more", historyResp.HasMore).
		Msg("Message history fetched successfully")

	return response, nil
}

// Chat ID mapping utilities

// PaginationCursor represents a cursor for message pagination
type PaginationCursor struct {
	Time    uint32 `json:"time"`
	Ordinal uint32 `json:"ordinal"`
}

// extractChatIDs extracts Steam chat group ID and chat ID from portal key
func (sc *SteamClient) extractChatIDs(portalKey networkid.PortalKey) (chatGroupID, chatID uint64, err error) {
	// For Steam, the portal ID typically contains the Steam ID
	// In 1-to-1 chats, the chat_group_id is 0 and chat_id is the Steam ID
	// In group chats, both would be set appropriately
	
	// Parse the portal ID as a Steam ID
	steamIDStr := string(portalKey.ID)
	steamID, parseErr := strconv.ParseUint(steamIDStr, 10, 64)
	if parseErr != nil {
		return 0, 0, fmt.Errorf("failed to parse portal ID as Steam ID: %w", parseErr)
	}
	
	// For now, assume all chats are 1-to-1 (chat_group_id = 0, chat_id = steam_id)
	// TODO: Add support for group chats with proper chat_group_id extraction
	return 0, steamID, nil
}

// parsePaginationCursor parses a pagination cursor string
func parsePaginationCursor(cursor networkid.PaginationCursor) PaginationCursor {
	// Parse JSON cursor format
	var parsed PaginationCursor
	if err := json.Unmarshal([]byte(cursor), &parsed); err != nil {
		// If parsing fails, return empty cursor
		return PaginationCursor{}
	}
	return parsed
}

// createPaginationCursor creates a pagination cursor string
func createPaginationCursor(time, ordinal uint32) networkid.PaginationCursor {
	cursor := PaginationCursor{
		Time:    time,
		Ordinal: ordinal,
	}
	data, _ := json.Marshal(cursor) // Ignore error, return empty string if marshal fails
	return networkid.PaginationCursor(data)
}

// convertSteamMessageToBackfill converts a Steam API message to bridgev2 BackfillMessage
func (sc *SteamClient) convertSteamMessageToBackfill(ctx context.Context, steamMsg *steamapi.ChatHistoryMessage, portal *bridgev2.Portal) (*bridgev2.BackfillMessage, error) {
	// Create sender information
	senderID := makeUserID(steamMsg.SenderSteamId)
	sender := bridgev2.EventSender{
		IsFromMe: steamMsg.SenderSteamId == sc.getUserID(),
		Sender:   senderID,
	}

	// Convert timestamp
	timestamp := time.Unix(int64(steamMsg.Timestamp), 0)

	// Convert message content
	var convertedMsg *bridgev2.ConvertedMessage
	var err error

	if steamMsg.ImageUrl != nil && *steamMsg.ImageUrl != "" {
		// Handle image message
		convertedMsg, err = sc.convertImageMessageFromHistory(ctx, steamMsg.MessageContent, *steamMsg.ImageUrl, portal)
	} else {
		// Handle text message
		convertedMsg, err = sc.convertTextMessageFromHistory(ctx, steamMsg.MessageContent, portal)
	}
	
	if err != nil {
		return nil, fmt.Errorf("failed to convert message content: %w", err)
	}

	// Create message metadata to store ordinal for pagination
	messageMetadata := &MessageMetadata{
		SteamID:   steamMsg.SenderSteamId,
		Ordinal:   steamMsg.Ordinal,
		Timestamp: timestamp,
	}

	backfillMsg := &bridgev2.BackfillMessage{
		ConvertedMessage: convertedMsg,
		Sender:           sender,
		ID:               networkid.MessageID(fmt.Sprintf("%d_%d", steamMsg.Timestamp, steamMsg.Ordinal)),
		Timestamp:        timestamp,
		StreamOrder:     int64(steamMsg.Ordinal), // Use ordinal for ordering
		Reactions:       []*bridgev2.BackfillReaction{}, // TODO: Add reaction support
	}
	
	// Store metadata in the first part if available
	if len(backfillMsg.Parts) > 0 {
		backfillMsg.Parts[0].DBMetadata = messageMetadata
	}

	return backfillMsg, nil
}

// getUserID gets the current user's Steam ID
func (sc *SteamClient) getUserID() uint64 {
	if metadata := sc.getUserMetadata(); metadata != nil {
		return metadata.SteamID
	}
	return 0
}

// convertTextMessageFromHistory converts a text message from history to bridgev2 format
func (sc *SteamClient) convertTextMessageFromHistory(ctx context.Context, content string, portal *bridgev2.Portal) (*bridgev2.ConvertedMessage, error) {
	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{
			{
				Type: event.EventMessage,
				Content: &event.MessageEventContent{
					MsgType: event.MsgText,
					Body:    content,
				},
				ID: networkid.PartID("text"),
			},
		},
	}, nil
}

// convertImageMessageFromHistory converts an image message from history to bridgev2 format
func (sc *SteamClient) convertImageMessageFromHistory(ctx context.Context, caption, imageURL string, portal *bridgev2.Portal) (*bridgev2.ConvertedMessage, error) {
	parts := []*bridgev2.ConvertedMessagePart{}
	
	// Add image part
	imageContent := &event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    "Image",
		URL:     id.ContentURIString(imageURL), // This will need proper handling
	}
	
	parts = append(parts, &bridgev2.ConvertedMessagePart{
		Type:    event.EventMessage,
		Content: imageContent,
		ID:      networkid.PartID("image"),
	})
	
	// Add caption if present
	if caption != "" {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    caption,
			},
			ID: networkid.PartID("caption"),
		})
	}
	
	return &bridgev2.ConvertedMessage{
		Parts: parts,
	}, nil
}

var _ bridgev2.NetworkAPI = (*SteamClient)(nil)
