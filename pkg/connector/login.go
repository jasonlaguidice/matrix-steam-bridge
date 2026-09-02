package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

// Password Login Implementation

// Start implements bridgev2.LoginProcess for password login
func (slp *SteamLoginPassword) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "password",
		Instructions: "Enter your Steam username and password to login",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type:        "text",
					ID:          "username",
					Name:        "Steam Username",
					Description: "Your Steam account username",
				},
				{
					Type:        "password",
					ID:          "password",
					Name:        "Password",
					Description: "Your Steam account password",
				},
			},
		},
	}, nil
}

// Cancel implements bridgev2.LoginProcess for password login
func (slp *SteamLoginPassword) Cancel() {
	if slp.cancelFunc != nil {
		slp.cancelFunc()
	}
}

// SubmitUserInput implements bridgev2.LoginProcessUserInput for password login
func (slp *SteamLoginPassword) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	if slp.Main.authClient == nil {
		return nil, fmt.Errorf("Steam bridge service is not connected — check that the SteamBridge service is running and that steam_bridge_address is configured correctly")
	}

	username, hasUsername := input["username"]
	password, hasPassword := input["password"]
	guardCode := input["guard_code"] // Optional SteamGuard code
	emailCode := input["email_code"] // Optional email verification code

	// Create timeout context for login attempt
	loginCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	var resp *steamapi.LoginResponse
	var err error

	// Check if this is a 2FA continuation step (session ID exists, only codes provided)
	if slp.SessionID != "" && (!hasUsername && !hasPassword) && (guardCode != "" || emailCode != "") {
		// This is a 2FA continuation step - use the stored session
		req := &steamapi.ContinueAuthRequest{
			SessionId: slp.SessionID,
			GuardCode: guardCode,
			EmailCode: emailCode,
		}

		resp, err = slp.Main.authClient.ContinueAuthSession(loginCtx, req)
	} else {
		// This is an initial login step
		if !hasUsername || !hasPassword {
			return nil, fmt.Errorf("username and password are required")
		}

		slp.Username = username
		if slp.LoginSessionKey == "" {
			slp.LoginSessionKey = uuid.New().String()
		}

		req := &steamapi.CredentialsLoginRequest{
			Username:         username,
			Password:         password,
			GuardCode:        guardCode,
			EmailCode:        emailCode,
			RememberPassword: true,
			LoginSessionKey:  slp.LoginSessionKey,
		}

		resp, err = slp.Main.authClient.LoginWithCredentials(loginCtx, req)
	}
	if err != nil {
		// Check for context timeout
		if loginCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("Steam login timed out after 45 seconds")
		}
		return nil, fmt.Errorf("failed to authenticate with Steam: %w", err)
	}

	if !resp.Success {
		// Store session ID for continuation if provided
		if resp.SessionId != "" {
			slp.SessionID = resp.SessionId
		}

		// Handle specific authentication requirements
		if resp.RequiresGuard && guardCode == "" {
			return &bridgev2.LoginStep{
				Type:         bridgev2.LoginStepTypeUserInput,
				StepID:       "guard_code",
				Instructions: "Enter your SteamGuard code from the Steam mobile app or authenticator",
				UserInputParams: &bridgev2.LoginUserInputParams{
					Fields: []bridgev2.LoginInputDataField{
						{
							Type:        "text",
							ID:          "guard_code",
							Name:        "SteamGuard Code",
							Description: "6-digit code from your Steam mobile app",
						},
					},
				},
			}, nil
		}

		if resp.RequiresEmailVerification && emailCode == "" {
			return &bridgev2.LoginStep{
				Type:         bridgev2.LoginStepTypeUserInput,
				StepID:       "email_code",
				Instructions: "Enter the verification code sent to your registered email address",
				UserInputParams: &bridgev2.LoginUserInputParams{
					Fields: []bridgev2.LoginInputDataField{
						{
							Type:        "text",
							ID:          "email_code",
							Name:        "Email Verification Code",
							Description: "Code sent to your email address",
						},
					},
				},
			}, nil
		}

		// Handle incorrect codes
		if resp.RequiresGuard && guardCode != "" {
			return &bridgev2.LoginStep{
				Type:         bridgev2.LoginStepTypeUserInput,
				StepID:       "guard_code",
				Instructions: "Incorrect SteamGuard code. Please try again.",
				UserInputParams: &bridgev2.LoginUserInputParams{
					Fields: []bridgev2.LoginInputDataField{
						{
							Type:        "text",
							ID:          "guard_code",
							Name:        "SteamGuard Code",
							Description: "6-digit code from your Steam mobile app",
						},
					},
				},
			}, nil
		}

		if resp.RequiresEmailVerification && emailCode != "" {
			return &bridgev2.LoginStep{
				Type:         bridgev2.LoginStepTypeUserInput,
				StepID:       "email_code",
				Instructions: "Incorrect email verification code. Please try again.",
				UserInputParams: &bridgev2.LoginUserInputParams{
					Fields: []bridgev2.LoginInputDataField{
						{
							Type:        "text",
							ID:          "email_code",
							Name:        "Email Verification Code",
							Description: "Code sent to your email address",
						},
					},
				},
			}, nil
		}

		if resp.RequiresDeviceConfirmation {
			return &bridgev2.LoginStep{
				Type:         bridgev2.LoginStepTypeDisplayAndWait,
				StepID:       "device_confirmation",
				Instructions: "Open the Steam Mobile app and tap **Approve** on the Steam Guard prompt. Send `cancel` to cancel the login",
				DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{
					Type: bridgev2.LoginDisplayTypeNothing,
				},
			}, nil
		}

		return nil, fmt.Errorf("Steam authentication failed: %s", resp.ErrorMessage)
	}

	// Authentication successful, create user login
	return slp.finishLogin(ctx, resp)
}

// Wait implements bridgev2.LoginProcessDisplayAndWait for password login. It's only reached when
// SubmitUserInput returned a device_confirmation step, i.e. Steam wants the user to approve the
// login from the Steam Mobile app rather than type a code. Mirrors SteamLoginQR.Wait below, polling
// the same GetAuthStatus RPC by session ID until Steam reports a result.
func (slp *SteamLoginPassword) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timeout := time.After(5 * time.Minute)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, fmt.Errorf("Steam Guard mobile confirmation timed out after 5 minutes. Run `login` to try again")
		case <-ticker.C:
			resp, err := slp.Main.authClient.GetAuthStatus(ctx, &steamapi.AuthStatusRequest{
				SessionId: slp.SessionID,
			})
			if err != nil {
				slp.Main.br.Log.Err(err).Msg("Failed to check Steam Guard confirmation status")
				continue
			}

			switch resp.State {
			case steamapi.AuthStatusResponse_AUTHENTICATED:
				return finishAuthStatusLogin(ctx, slp.Main, slp.User, resp, "password", "fresh_login_device_confirmation")

			case steamapi.AuthStatusResponse_FAILED:
				return nil, fmt.Errorf("Steam login failed: %s", resp.ErrorMessage)

			case steamapi.AuthStatusResponse_EXPIRED:
				return nil, fmt.Errorf("Steam Guard confirmation expired. Run `login` to try again")

			case steamapi.AuthStatusResponse_PENDING:
				// Still waiting, continue polling
				continue
			}
		}
	}
}

// finishLogin completes the login process by creating UserLogin and metadata
func (slp *SteamLoginPassword) finishLogin(ctx context.Context, resp *steamapi.LoginResponse) (*bridgev2.LoginStep, error) {
	// Create user login metadata
	userLoginID := makeUserLoginID(resp.UserInfo.SteamId)

	metadata := &UserLoginMetadata{
		SteamID:          resp.UserInfo.SteamId,
		Username:         resp.UserInfo.AccountName,
		PersonaName:      resp.UserInfo.PersonaName,
		ProfileURL:       resp.UserInfo.ProfileUrl,
		AvatarHash:       resp.UserInfo.AvatarHash,
		RemoteID:         userLoginID,
		AccessToken:      resp.AccessToken,
		RefreshToken:     resp.RefreshToken,
		SessionTimestamp: time.Now().Unix(),
		SessionType:      "password",
		IsValid:          true,
		RecentlyCreated:  time.Now(),
	}

	// Create user login in database
	userLogin, err := slp.User.NewLogin(ctx, &database.UserLogin{
		ID:         userLoginID,
		RemoteName: resp.UserInfo.PersonaName,
		RemoteProfile: status.RemoteProfile{
			Name: resp.UserInfo.PersonaName,
		},
		Metadata: metadata,
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			login.Client = &SteamClient{
				UserLogin:      login,
				connector:      slp.Main,
				authClient:     slp.Main.authClient,
				userClient:     slp.Main.userClient,
				msgClient:      slp.Main.msgClient,
				sessionClient:  slp.Main.sessionClient,
				presenceClient: slp.Main.presenceClient,
				groupClient:    slp.Main.groupClient,
				br:             slp.Main.br,
				typingCancels:  make(map[networkid.PortalID]context.CancelFunc),
				pendingInvites: make(map[networkid.MessageID]*pendingInvite),
				friendPresence: make(map[uint64]*friendPresenceInfo),
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create user login: %w", err)
	}

	// After fresh login, mark as connected since Steam session is already active
	// Do NOT call Connect() as that would re-authenticate and cause session replacement
	if steamClient, ok := userLogin.Client.(*SteamClient); ok {
		// Set connection state - Steam is already connected via the login process
		steamClient.stateMutex.Lock()
		steamClient.isConnected = true
		steamClient.isConnecting = false
		steamClient.stateMutex.Unlock()

		// Send connected bridge state
		steamClient.UserLogin.BridgeState.Send(steamClient.buildBridgeState(status.StateConnected,
			"Connected to Steam",
			withInfo(map[string]interface{}{
				"connection_type": "fresh_login",
			})))

		// Start connection monitoring and message subscriptions for fresh login
		ctx := context.Background()
		steamClient.startConnectionMonitoring(ctx)

		// Start gRPC message subscription for real-time messages
		go steamClient.startMessageSubscription(ctx)

		// Start session event subscription for logout notifications
		go steamClient.startSessionEventSubscription(ctx)

		// Initialize and start presence manager
		if steamClient.presenceManager == nil && steamClient.connector != nil {
			steamClient.presenceManager = NewPresenceManager(steamClient, &steamClient.connector.Config.Presence)
		}
		if steamClient.presenceManager != nil {
			steamClient.presenceManager.Start(ctx)
		}

		// Initialize presence topic stream (Steam friend presence -> DM room topic).
		// Presence topic loss is not fatal, so this runs independently and only logs on failure.
		go func() {
			if err := steamClient.initializePresenceStream(ctx); err != nil {
				steamClient.br.Log.Warn().Err(err).Msg("Failed to initialize presence topic stream")
			}
		}()

		// Start the periodic sweep for expiring live game-invite "Join Game" links (see
		// inviteexpiry.go). Tied to the same connection-scoped ctx as the other
		// background loops above.
		steamClient.startInviteExpirySweep(ctx)

		// Sync existing portals for backfill after fresh login
		go steamClient.syncExistingPortals(ctx)

		// Sync friends' profiles and seed already-open DM room topics with their current
		// game (Connect()'s reconnect path does this too, via syncFriendsOnStartup - a
		// fresh login completing via this path skipped it entirely otherwise, so a
		// friend already mid-game when the user logs in would show no topic until their
		// next live state change).
		go steamClient.syncFriendsOnStartup(ctx)

		// Sync Steam chat room groups (create Space + channel portals)
		go func() {
			if err := steamClient.syncGroups(context.Background()); err != nil {
				steamClient.br.Log.Warn().Err(err).Msg("Failed to sync Steam chat groups")
			}
		}()

		// Save the user login with updated metadata
		steamClient.UserLogin.Save(ctx)
	}

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "complete",
		Instructions: fmt.Sprintf("Successfully logged in as %s", resp.UserInfo.PersonaName),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: userLogin.ID,
			UserLogin:   userLogin,
		},
	}, nil
}

// QR Login Implementation

// Start implements bridgev2.LoginProcess.Start for QR login
func (slq *SteamLoginQR) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	if slq.Main.authClient == nil {
		return nil, fmt.Errorf("Steam bridge service is not connected — check that the SteamBridge service is running and that steam_bridge_address is configured correctly")
	}

	// Start QR authentication with SteamBridge service
	slq.LoginSessionKey = uuid.New().String()
	resp, err := slq.Main.authClient.LoginWithQR(ctx, &steamapi.QRLoginRequest{
		LoginSessionKey: slq.LoginSessionKey,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start QR login: %w", err)
	}

	slq.ChallengeID = resp.SessionId

	// Determine what to display - prioritize QR image, fallback to URL
	var qrData string
	var displayType bridgev2.LoginDisplayType = bridgev2.LoginDisplayTypeQR

	if resp.ChallengeUrl != "" {
		// Use raw Steam challenge URL - let the client handle QR generation
		qrData = resp.ChallengeUrl
	} else if resp.QrCodeFallback != "" {
		// Fallback to text display
		qrData = resp.QrCodeFallback
		displayType = bridgev2.LoginDisplayTypeCode
	} else {
		// Last resort - just the URL
		qrData = resp.ChallengeUrl
		displayType = bridgev2.LoginDisplayTypeCode
	}

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeDisplayAndWait,
		StepID:       "qr_display",
		Instructions: "Scan this QR code with your Steam mobile app:",
		DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{
			Type: displayType,
			Data: qrData,
		},
	}, nil
}

// Cancel implements bridgev2.LoginProcess for QR login
func (slq *SteamLoginQR) Cancel() {
	if slq.cancelFunc != nil {
		slq.cancelFunc()
	}
	// TODO: Clean up QR code messages and timers
}

// Wait implements bridgev2.LoginProcessDisplayAndWait for QR login
func (slq *SteamLoginQR) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	// Wait for QR authentication to complete using the actual polling mechanism
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timeout := time.After(5 * time.Minute)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, fmt.Errorf("QR authentication timed out")
		case <-ticker.C:
			// Check actual authentication status via gRPC
			resp, err := slq.Main.authClient.GetAuthStatus(ctx, &steamapi.AuthStatusRequest{
				SessionId: slq.ChallengeID,
			})

			if err != nil {
				slq.Main.br.Log.Err(err).Msg("Failed to check QR auth status")
				continue
			}

			switch resp.State {
			case steamapi.AuthStatusResponse_AUTHENTICATED:
				// Authentication successful - create user login with LoadUserLogin
				return finishAuthStatusLogin(ctx, slq.Main, slq.User, resp, "qr", "fresh_qr_login")

			case steamapi.AuthStatusResponse_FAILED:
				return nil, fmt.Errorf("QR authentication failed: %s", resp.ErrorMessage)

			case steamapi.AuthStatusResponse_EXPIRED:
				return nil, fmt.Errorf("QR code expired")

			case steamapi.AuthStatusResponse_PENDING:
				// Still waiting, continue polling
				continue
			}
		}
	}
}

// finishAuthStatusLogin completes a login from a resolved AuthStatusResponse. It's shared by the QR
// flow and the password flow's Steam Guard mobile confirmation wait, since both resolve the same way:
// polling GetAuthStatus until it reports AUTHENTICATED.
func finishAuthStatusLogin(ctx context.Context, main *SteamConnector, user *bridgev2.User, resp *steamapi.AuthStatusResponse, sessionType string, connectionType string) (*bridgev2.LoginStep, error) {
	// Create user login metadata
	userLoginID := makeUserLoginID(resp.UserInfo.SteamId)

	metadata := &UserLoginMetadata{
		SteamID:          resp.UserInfo.SteamId,
		Username:         string(makeUserLoginID(resp.UserInfo.SteamId)), // Use SteamID format for bridge identification
		AccountName:      resp.UserInfo.AccountName,                      // Real Steam account name for authentication
		PersonaName:      resp.UserInfo.PersonaName,
		ProfileURL:       resp.UserInfo.ProfileUrl,
		AvatarHash:       resp.UserInfo.AvatarHash,
		RemoteID:         userLoginID,
		AccessToken:      resp.AccessToken,
		RefreshToken:     resp.RefreshToken,
		SessionTimestamp: time.Now().Unix(),
		SessionType:      sessionType,
		IsValid:          true,
		RecentlyCreated:  time.Now(),
	}

	// Create user login in database WITH LoadUserLogin function
	userLogin, err := user.NewLogin(ctx, &database.UserLogin{
		ID:         userLoginID,
		RemoteName: resp.UserInfo.PersonaName,
		RemoteProfile: status.RemoteProfile{
			Name: resp.UserInfo.PersonaName,
		},
		Metadata: metadata,
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			login.Client = &SteamClient{
				UserLogin:      login,
				connector:      main,
				authClient:     main.authClient,
				userClient:     main.userClient,
				msgClient:      main.msgClient,
				sessionClient:  main.sessionClient,
				presenceClient: main.presenceClient,
				groupClient:    main.groupClient,
				br:             main.br,
				typingCancels:  make(map[networkid.PortalID]context.CancelFunc),
				pendingInvites: make(map[networkid.MessageID]*pendingInvite),
				friendPresence: make(map[uint64]*friendPresenceInfo),
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create user login: %w", err)
	}

	// After a fresh login, mark as connected since Steam session is already active
	// Do NOT call Connect() as that would re-authenticate and cause session replacement
	if steamClient, ok := userLogin.Client.(*SteamClient); ok {
		// Set connection state - Steam is already connected via the login process
		steamClient.stateMutex.Lock()
		steamClient.isConnected = true
		steamClient.isConnecting = false
		steamClient.stateMutex.Unlock()

		// Send connected bridge state
		steamClient.UserLogin.BridgeState.Send(steamClient.buildBridgeState(status.StateConnected,
			"Connected to Steam",
			withInfo(map[string]interface{}{
				"connection_type": connectionType,
			})))

		// Start connection monitoring and message subscriptions for the fresh login
		ctx := context.Background()
		steamClient.startConnectionMonitoring(ctx)

		// Start gRPC message subscription for real-time messages
		go steamClient.startMessageSubscription(ctx)

		// Start session event subscription for logout notifications
		go steamClient.startSessionEventSubscription(ctx)

		// Initialize and start presence manager
		if steamClient.presenceManager == nil && steamClient.connector != nil {
			steamClient.presenceManager = NewPresenceManager(steamClient, &steamClient.connector.Config.Presence)
		}
		if steamClient.presenceManager != nil {
			steamClient.presenceManager.Start(ctx)
		}

		// Initialize presence topic stream (Steam friend presence -> DM room topic).
		// Presence topic loss is not fatal, so this runs independently and only logs on failure.
		go func() {
			if err := steamClient.initializePresenceStream(ctx); err != nil {
				steamClient.br.Log.Warn().Err(err).Msg("Failed to initialize presence topic stream")
			}
		}()

		// Start the periodic sweep for expiring live game-invite "Join Game" links (see
		// inviteexpiry.go). Tied to the same connection-scoped ctx as the other
		// background loops above.
		steamClient.startInviteExpirySweep(ctx)

		// Sync existing portals for backfill after fresh login
		go steamClient.syncExistingPortals(ctx)

		// Sync friends' profiles and seed already-open DM room topics with their current
		// game (Connect()'s reconnect path does this too, via syncFriendsOnStartup - a
		// fresh login completing via this path skipped it entirely otherwise, so a
		// friend already mid-game when the user logs in would show no topic until their
		// next live state change).
		go steamClient.syncFriendsOnStartup(ctx)

		// Sync Steam chat room groups (create Space + channel portals)
		go func() {
			if err := steamClient.syncGroups(context.Background()); err != nil {
				steamClient.br.Log.Warn().Err(err).Msg("Failed to sync Steam chat groups")
			}
		}()

		// Save the user login with updated metadata
		steamClient.UserLogin.Save(ctx)
	}

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "complete",
		Instructions: fmt.Sprintf("Successfully logged in as %s", resp.UserInfo.PersonaName),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: userLogin.ID,
			UserLogin:   userLogin,
		},
	}, nil
}

// Implement required interfaces
var _ bridgev2.LoginProcessUserInput = (*SteamLoginPassword)(nil)
var _ bridgev2.LoginProcessDisplayAndWait = (*SteamLoginPassword)(nil)
var _ bridgev2.LoginProcessDisplayAndWait = (*SteamLoginQR)(nil)
