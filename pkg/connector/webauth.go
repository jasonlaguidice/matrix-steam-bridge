// Web-cookie authentication for Steam's chat upload endpoints, per
// STEAM_MEDIA_UPLOAD_PROBE.md §10: the steamLoginSecure cookie
// (encodeURIComponent(steamid + "||" + access_token)) plus a client-generated
// 24-character base36 sessionid CSRF token, sent as a cookie AND a form
// field. Also defines the AccessTokenProvider interface the upload flow
// depends on and its JWT-exp-aware caching decorator.
package connector

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// sessionIDLength is the length of the client-generated sessionid CSRF token
// (the app's GenerateSessionID: 24 chars of RandomInt(0,35).toString(36)).
const sessionIDLength = 24

// tokenEarlyRefresh is how long before a cached token's `exp` a replacement is
// minted, so a token is never used within its final minute of validity.
const tokenEarlyRefresh = 60 * time.Second

// AccessTokenProvider supplies a currently-valid Steam web access token (the
// JWT accepted by steam-chat.com's upload endpoints). Implementations mint
// from the user's refresh token at send time; callers must never log or
// persist the token.
type AccessTokenProvider interface {
	AccessToken(ctx context.Context) (string, error)
}

// CachedAccessTokenProvider decorates an AccessTokenProvider with JWT-exp-aware
// caching: the wrapped provider is only called when the cached token is missing
// or within tokenEarlyRefresh of its parsed `exp` claim. Tokens whose `exp`
// cannot be parsed are never cached (single-use), so a provider that cannot
// guarantee expiry metadata degrades to minting per call.
type CachedAccessTokenProvider struct {
	inner AccessTokenProvider
	log   zerolog.Logger

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewCachedAccessTokenProvider wraps inner with exp-aware caching.
func NewCachedAccessTokenProvider(inner AccessTokenProvider, log zerolog.Logger) *CachedAccessTokenProvider {
	return &CachedAccessTokenProvider{inner: inner, log: log}
}

// AccessToken returns a cached token while it is safely valid and mints a
// fresh one otherwise.
func (p *CachedAccessTokenProvider) AccessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && time.Now().Before(p.expiresAt.Add(-tokenEarlyRefresh)) {
		return p.token, nil
	}
	tok, err := p.inner.AccessToken(ctx)
	if err != nil {
		return "", err
	}
	expiresAt, ok := jwtExpiresAt(tok)
	if ok {
		p.token = tok
		p.expiresAt = expiresAt
		p.log.Debug().Time("expires_at", expiresAt).Msg("Cached Steam web access token")
	} else {
		p.token = ""
		p.expiresAt = time.Time{}
		p.log.Debug().Msg("Steam web access token has no parseable exp claim; not caching")
	}
	return tok, nil
}

// jwtExpiresAt parses the `exp` claim (seconds since the Unix epoch) from a
// JWT's payload segment without verifying the signature; the token's validity
// is enforced by Steam itself, this only informs cache lifetime.
func jwtExpiresAt(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// steamWebCookie builds the Cookie header value for steam-chat.com upload
// requests: the app's SetNativeCookies sets `steamLoginSecure` to
// encodeURIComponent(steamid + "||" + access_token) and pairs it with a
// client-generated `sessionid` (the server only checks that the cookie and
// the form field match).
func steamWebCookie(steamID uint64, token, sessionID string) string {
	return "steamLoginSecure=" + encodeURIComponentExact(strconv.FormatUint(steamID, 10)+"||"+token) +
		"; sessionid=" + sessionID
}

// encodeURIComponentExact replicates JavaScript's encodeURIComponent byte for
// byte: every byte except A-Z a-z 0-9 and - _ . ~ ! ' ( ) * becomes "%XX"
// with uppercase hex digits. Go's url.QueryEscape differs ("+" for space, a
// different reserved set), and the server checks the exact cookie value, so
// the exact encoder is required.
func encodeURIComponentExact(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case strings.ContainsRune("-_.~!*'()", rune(c)):
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// randomSessionID generates the 24-character lowercase-base36 sessionid CSRF
// double-submit token the app generates client-side. crypto/rand.Read cannot
// fail on the systems this runs on (it crashes irrecoverably instead), so the
// error is ignored as in the probe tool that validated the wire protocol.
func randomSessionID() string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, sessionIDLength)
	_, _ = rand.Read(b)
	for i, v := range b {
		b[i] = digits[v%36]
	}
	return string(b)
}
