// AccessTokenProvider implementation backed by the C# service's
// MintAccessToken RPC, plus the per-login wiring of the web image upload
// flow. See STEAM_MEDIA_UPLOAD_PROBE.md §4: the access token stored in
// UserLoginMetadata can be months stale while the bridge looks healthy; only
// the C# service can mint a currently-valid web-capable token from the
// refresh token it holds (SteamAuthentication.GenerateAccessTokenForAppAsync
// with allowRenewal=false).
package connector

import (
	"context"
	"fmt"

	"go.shadowdrake.org/steam/pkg/steamapi"
)

// mintAccessTokenProvider implements AccessTokenProvider by minting a fresh
// web-capable access token through the C# service's MintAccessToken RPC.
// Tokens must never be logged or persisted; error text is sanitized by the
// upload flow before it reaches a log line or a user-facing notice.
type mintAccessTokenProvider struct {
	client *SteamClient
}

// AccessToken returns a freshly minted access token.
func (p *mintAccessTokenProvider) AccessToken(ctx context.Context) (string, error) {
	if p.client == nil || p.client.authClient == nil {
		return "", fmt.Errorf("Steam auth client not available")
	}
	steamID := p.client.steamID()
	if steamID == 0 {
		return "", fmt.Errorf("no Steam ID available for access token minting")
	}
	resp, err := p.client.authClient.MintAccessToken(ctx, &steamapi.MintAccessTokenRequest{SteamId: steamID})
	if err != nil {
		return "", fmt.Errorf("MintAccessToken RPC failed: %w", err)
	}
	if !resp.Success {
		return "", fmt.Errorf("MintAccessToken failed: %s", resp.ErrorMessage)
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("MintAccessToken returned an empty access token")
	}
	return resp.AccessToken, nil
}

// initUploadFlow wires the per-login web image uploader (tokens minted
// through the C# service's MintAccessToken RPC, cached with JWT `exp`
// awareness by webauth.go; the nil client lets the uploader build its
// dedicated 2-minute-timeout HTTP client) and the echo expectation registry
// its server-posted chat messages are correlated against. Idempotent; called
// once per SteamClient construction.
func (sc *SteamClient) initUploadFlow() {
	if sc.uploader != nil {
		return
	}
	sc.uploader = NewImageUploader(NewCachedAccessTokenProvider(&mintAccessTokenProvider{client: sc}, sc.br.Log), nil, sc.br.Log)
	sc.uploadExpectations = newUploadExpectations(sc.br.Log)
}
