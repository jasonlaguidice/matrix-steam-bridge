// Image and video upload to Steam's chat web endpoints (beginfileupload → cloud PUT →
// commitfileupload), ported from the working reference implementation in
// cmd/steamchatprobe/main.go and the live-confirmed wire facts in
// STEAM_MEDIA_UPLOAD_PROBE.md §10. The server posts the resulting chat message
// itself on a successful commit; callers must not also SendMessage the image.
package connector

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

const (
	// chatUploadBaseURL is CHAT_BASE_URL from the app (with trailing slash).
	chatUploadBaseURL = "https://steam-chat.com/"
	// chatUploadLanguage is the `l` language parameter the app sends.
	chatUploadLanguage = "english"
	chatBeginPath      = "chat/beginfileupload/"
	chatCommitPath     = "chat/commitfileupload/"
	// maxUploadSize is the app's GetMaxFileSizeMB() = 30.
	maxUploadSize = 30 * 1024 * 1024
	// uploadHTTPTimeout caps each HTTP request of the dedicated client.
	uploadHTTPTimeout = 2 * time.Minute
	// uploadBodyLimit caps how much of a begin/commit response is read.
	uploadBodyLimit = 1 << 20
	// jpegExifScanLimit is where the app's StripExifMetadata stops walking
	// JPEG segments (128 KiB); APP1 segments past it are left untouched.
	jpegExifScanLimit = 128 * 1024
)

// beginRetryDelays backs off between begin retries; the live capture showed
// EResult 16 (Timeout) on roughly half of begin calls, with an immediate
// retry succeeding (PROBE.md §10). Three attempts total.
var beginRetryDelays = []time.Duration{500 * time.Millisecond, time.Second}

// uploadExtensionMimes is the app's SetImageFileToUpload allowlist with the
// MIME type each extension is uploaded as.
var uploadExtensionMimes = map[string]string{
	"jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif",
	"webp": "image/webp", "avif": "image/avif", "webm": "video/webm", "mpg": "video/mpeg",
	"mpeg": "video/mpeg", "mp4": "video/mp4", "ogv": "video/ogg",
}

// Upload stages, reported by UploadError.Stage.
const (
	StageAuth     = "auth"     // obtaining the web access token
	StageValidate = "validate" // local pre-checks failed
	StageBegin    = "begin"    // beginfileupload
	StagePut      = "put"      // cloud PUT
	StageCommit   = "commit"   // commitfileupload
)

// Steam EResults observed on the chat upload endpoints (PROBE.md §10).
const (
	eresultTimeout        = 16  // "batched request timeout", transient
	eresultNotLoggedOn    = 21  // stale/expired token
	eresultLimitedAccount = 112 // limited users cannot upload images
)

// UploadTarget is the chat an upload is committed into. Begin carries no
// recipient; only the commit form differs between a 1:1 chat and a group
// channel (CChatRoom.PopulateCommitFileUploadFormData in the app bundle).
type UploadTarget interface {
	// commitFields returns the recipient form fields, in the app's order.
	commitFields() []formField
	// logEvent adds the target's identifying fields to a log event.
	logEvent(e *zerolog.Event) *zerolog.Event
}

// DMUploadTarget addresses a 1:1 Steam chat.
type DMUploadTarget struct {
	FriendSteamID uint64
}

func (t DMUploadTarget) commitFields() []formField {
	return []formField{{"friend_steamid", strconv.FormatUint(t.FriendSteamID, 10)}}
}

func (t DMUploadTarget) logEvent(e *zerolog.Event) *zerolog.Event {
	return e.Uint64("friend_steamid", t.FriendSteamID)
}

// GroupUploadTarget addresses a Steam group chat channel.
type GroupUploadTarget struct {
	ChatGroupID uint64
	ChatID      uint64
}

func (t GroupUploadTarget) commitFields() []formField {
	return []formField{
		{"chat_group_id", strconv.FormatUint(t.ChatGroupID, 10)},
		{"chat_id", strconv.FormatUint(t.ChatID, 10)},
	}
}

func (t GroupUploadTarget) logEvent(e *zerolog.Event) *zerolog.Event {
	return e.Uint64("chat_group_id", t.ChatGroupID).Uint64("chat_id", t.ChatID)
}

// UploadRequest describes one image or video upload to a Steam chat.
type UploadRequest struct {
	SteamID  uint64       // 64-bit SteamID of the uploading account
	Target   UploadTarget // chat the server posts the resulting message into
	Data     []byte       // raw file bytes
	FileName string       // display filename; its extension drives validation
	MimeType string       // optional override; derived from the extension when empty
	Width    int          // optional pre-known pixel width (0 = parse from data)
	Height   int          // optional pre-known pixel height (0 = parse from data)
	Spoiler  bool
}

// UploadResult is the server-confirmed outcome of a successful upload.
type UploadResult struct {
	URL      string // permanent CDN URL (result.details.url); may be empty for videos
	FileSha  string // SHA-1 the server computed for the file (lowercase hex)
	FileSize int64  // size the server recorded
	Ugcid    string // server-assigned upload ID
}

// UploadError is a typed upload failure. Its Message never contains the
// access token or the signed cloud PUT URL (both are redacted).
type UploadError struct {
	Stage       string
	HTTPStatus  int // 0 when the failure is local or transport-level
	SteamResult int // Steam's `success` code; 0 when not from a Steam response
	Message     string
}

func (e *UploadError) Error() string {
	var b strings.Builder
	b.WriteString("steam chat upload failed at " + e.Stage)
	if e.HTTPStatus != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.HTTPStatus)
	}
	if e.SteamResult != 0 {
		fmt.Fprintf(&b, " (success %d)", e.SteamResult)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// IsLimitedAccount reports the EResult 112 policy rejection (limited users
// cannot upload images); the caller must not retry it.
func (e *UploadError) IsLimitedAccount() bool {
	return e.SteamResult == eresultLimitedAccount
}

// IsNotLoggedOn reports EResult 21 (stale/expired token); the caller must not
// retry it.
func (e *UploadError) IsNotLoggedOn() bool {
	return e.SteamResult == eresultNotLoggedOn
}

// IsTransient reports EResult 16 (server timeout); begin is retried internally
// before this is ever returned.
func (e *UploadError) IsTransient() bool {
	return e.SteamResult == eresultTimeout
}

// ImageUploader implements Steam's chat web image-upload flow over plain HTTP,
// authenticating with web cookies built from a token supplied by tokens
// (webauth.go). It is independent of the CM connection.
type ImageUploader struct {
	baseURL  string
	language string
	client   *http.Client
	tokens   AccessTokenProvider
	log      zerolog.Logger
}

// NewImageUploader creates an uploader; client may be nil for a dedicated
// client with a sane timeout.
func NewImageUploader(tokens AccessTokenProvider, client *http.Client, log zerolog.Logger) *ImageUploader {
	if client == nil {
		client = &http.Client{Timeout: uploadHTTPTimeout}
	}
	return &ImageUploader{
		baseURL:  chatUploadBaseURL,
		language: chatUploadLanguage,
		client:   client,
		tokens:   tokens,
		log:      log,
	}
}

// Upload runs the validated, EXIF-stripped begin → PUT → commit flow and
// returns the server-confirmed CDN URL. On a cloud PUT failure it still
// commits success=0 so the server abandons the upload, then returns the PUT
// error (the app's own flow).
func (u *ImageUploader) Upload(ctx context.Context, req UploadRequest) (*UploadResult, error) {
	if req.Target == nil {
		return nil, &UploadError{Stage: StageValidate, Message: "no upload target chat specified"}
	}
	mime, err := validateUpload(req)
	if err != nil {
		return nil, err
	}
	payload, shaUpper := prepareUploadPayload(req.Data)
	width, height := uploadDimensions(payload, mime, req.Width, req.Height)
	token, err := u.tokens.AccessToken(ctx)
	if err != nil {
		return nil, &UploadError{Stage: StageAuth, Message: sanitizeErrorText("failed to obtain Steam web access token: " + err.Error())}
	}
	sessionID := randomSessionID()
	s := &uploadState{
		req:       req,
		mime:      mime,
		upName:    uploadFileName(req.FileName),
		shaHex:    strings.ToLower(shaUpper),
		data:      payload,
		width:     width,
		height:    height,
		sessionID: sessionID,
		cookie:    steamWebCookie(req.SteamID, token, sessionID),
	}
	req.Target.logEvent(u.log.Debug().
		Str("file_name", s.upName).
		Int("file_size", len(s.data)).
		Str("mime", s.mime).
		Int("width", s.width).
		Int("height", s.height)).
		Msg("Uploading media to Steam chat")

	begin, err := u.beginFileUpload(ctx, s)
	if err != nil {
		return nil, err
	}
	u.log.Debug().
		Str("ugcid", begin.Result.Ugcid).
		Str("host", begin.Result.UrlHost).
		Int("headers", len(begin.Result.RequestHeaders)).
		Bool("use_https", bool(begin.Result.UseHTTPS)).
		Msg("Steam beginfileupload accepted")

	putErr := u.putToCloud(ctx, s, begin)
	if putErr != nil {
		// The app still commits success=0 so the server abandons the upload
		// (no message is posted); surface the PUT failure.
		if _, commitErr := u.commitFileUpload(ctx, s, begin, false); commitErr != nil {
			u.log.Warn().Err(commitErr).Msg("Steam commitfileupload abandon call failed after cloud PUT failure")
		}
		return nil, putErr
	}
	commit, err := u.commitFileUpload(ctx, s, begin, true)
	if err != nil {
		return nil, err
	}
	u.log.Debug().Str("url", commit.Result.Details.URL).Msg("Steam chat media upload committed")
	return &UploadResult{
		URL:      commit.Result.Details.URL,
		FileSha:  commit.Result.Details.FileSha,
		FileSize: commit.Result.Details.FileSize,
		Ugcid:    begin.Result.Ugcid,
	}, nil
}

// uploadState bundles the per-upload values that stay fixed from begin through
// commit (PROBE.md §10: file_name and file_sha must be byte-identical between
// the two calls, and the sessionid must match its cookie).
type uploadState struct {
	req       UploadRequest
	mime      string
	upName    string
	shaHex    string
	data      []byte
	width     int
	height    int
	sessionID string
	cookie    string
}

// validateUpload enforces the app's client-side pre-checks (extension
// allowlist and 30 MB cap, plus a non-empty payload) and resolves the MIME
// type from the extension when the request does not supply one.
func validateUpload(req UploadRequest) (string, error) {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(req.FileName)), ".")
	mime, allowed := uploadExtensionMimes[ext]
	if !allowed {
		return "", &UploadError{Stage: StageValidate, Message: fmt.Sprintf("file extension %q is not supported by Steam chat uploads", ext)}
	}
	if len(req.Data) == 0 {
		return "", &UploadError{Stage: StageValidate, Message: "file is empty"}
	}
	if len(req.Data) > maxUploadSize {
		return "", &UploadError{Stage: StageValidate, Message: fmt.Sprintf("file is %d bytes; Steam chat uploads accept at most %d bytes", len(req.Data), maxUploadSize)}
	}
	if req.MimeType != "" {
		return req.MimeType, nil
	}
	return mime, nil
}

// stripJPEGExif removes JPEG APP1 (EXIF/XMP) segments before SOS, mirroring
// the app's StripExifMetadata; non-JPEG data passes through unchanged.
func stripJPEGExif(data []byte) []byte {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return data
	}
	out := make([]byte, 0, len(data))
	out = append(out, data[:2]...)
	off := 2
	for off < jpegExifScanLimit && off < len(data) {
		if data[off] != 0xFF {
			break // malformed: keep the remainder verbatim
		}
		m := off + 1
		for m < len(data) && data[m] == 0xFF {
			m++
		}
		if m >= len(data) {
			break
		}
		marker := data[m]
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0xDA || marker == 0xD9 {
			break // SOS/EOI/standalone markers: no more APP1 ahead
		}
		if m+3 > len(data) {
			break
		}
		segLen := int(data[m+1])<<8 | int(data[m+2])
		if segLen < 2 {
			break
		}
		segEnd := m + 1 + segLen
		if segEnd > len(data) {
			break
		}
		if marker != 0xE1 {
			out = append(out, data[off:segEnd]...)
		}
		off = segEnd
	}
	out = append(out, data[off:]...)
	return out
}

// prepareUploadPayload returns the exact bytes an upload PUTs (JPEG APP1
// EXIF segments stripped, mirroring the app's StripExifMetadata) and the
// uppercase SHA-1 hex of those bytes — the same hash Steam echoes into the
// CDN URL of the chat message it posts after commit. Upload and the echo
// expectation registration must both derive their hashes from this function;
// hashing the unstripped input diverges from the CDN URL for any JPEG with
// EXIF.
func prepareUploadPayload(data []byte) (payload []byte, sha1Upper string) {
	payload = stripJPEGExif(data)
	return payload, strings.ToUpper(sha1Hex(payload))
}

// uploadDimensions prefers the request's pixel size, else parses the image
// header (JPEG/PNG/GIF/WebP, as the app measures after its own image pass).
// Non-image files (videos) always report 0×0, as the app does.
func uploadDimensions(data []byte, mime string, width, height int) (int, int) {
	if !strings.HasPrefix(mime, "image/") {
		return 0, 0
	}
	if width > 0 && height > 0 {
		return width, height
	}
	w, h := imageDims(data)
	return w, h
}

func imageDims(d []byte) (int, int) {
	if w, h, ok := dimsJPEG(d); ok {
		return w, h
	}
	if w, h, ok := dimsPNG(d); ok {
		return w, h
	}
	if w, h, ok := dimsGIF(d); ok {
		return w, h
	}
	if w, h, ok := dimsWebP(d); ok {
		return w, h
	}
	return 0, 0
}

func dimsJPEG(d []byte) (int, int, bool) {
	if len(d) < 4 || d[0] != 0xFF || d[1] != 0xD8 {
		return 0, 0, false
	}
	i := 2
	for i < len(d) {
		if d[i] != 0xFF {
			i++
			continue
		}
		if i+1 >= len(d) {
			return 0, 0, false
		}
		m := d[i+1]
		if m == 0xFF {
			i++
			continue
		}
		i += 2
		if m == 0xDA || m == 0xD9 {
			return 0, 0, false
		}
		if m == 0x01 || (m >= 0xD0 && m <= 0xD7) {
			continue
		}
		if i+2 > len(d) {
			return 0, 0, false
		}
		segLen := int(d[i])<<8 | int(d[i+1])
		if segLen < 2 {
			return 0, 0, false
		}
		if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
			if i+6 >= len(d) {
				return 0, 0, false
			}
			h := int(d[i+3])<<8 | int(d[i+4])
			w := int(d[i+5])<<8 | int(d[i+6])
			return w, h, true
		}
		i += segLen
	}
	return 0, 0, false
}

func dimsPNG(d []byte) (int, int, bool) {
	if len(d) < 24 || string(d[0:4]) != "\x89PNG" || string(d[12:16]) != "IHDR" {
		return 0, 0, false
	}
	w := int(uint32(d[16])<<24 | uint32(d[17])<<16 | uint32(d[18])<<8 | uint32(d[19]))
	h := int(uint32(d[20])<<24 | uint32(d[21])<<16 | uint32(d[22])<<8 | uint32(d[23]))
	return w, h, true
}

func dimsGIF(d []byte) (int, int, bool) {
	if len(d) < 10 || string(d[0:3]) != "GIF" {
		return 0, 0, false
	}
	return int(d[6]) | int(d[7])<<8, int(d[8]) | int(d[9])<<8, true
}

func dimsWebP(d []byte) (int, int, bool) {
	if len(d) < 20 || string(d[0:4]) != "RIFF" || string(d[8:12]) != "WEBP" {
		return 0, 0, false
	}
	off := 12
	for off+7 <= len(d) {
		chunk := string(d[off : off+4])
		size := int(d[off+4]) | int(d[off+5])<<8 | int(d[off+6])<<16
		payload := off + 7
		switch chunk {
		case "VP8X":
			if payload+10 <= len(d) {
				w := 1 + (int(d[payload+4]) | int(d[payload+5])<<8 | int(d[payload+6])<<16)
				h := 1 + (int(d[payload+7]) | int(d[payload+8])<<8 | int(d[payload+9])<<16)
				return w, h, true
			}
		case "VP8L":
			if payload+5 <= len(d) && d[payload] == 0x2F {
				u := uint32(d[payload+1]) | uint32(d[payload+2])<<8 | uint32(d[payload+3])<<16 | uint32(d[payload+4])<<24
				return int(u&0x3FFF) + 1, int((u>>14)&0x3FFF) + 1, true
			}
		case "VP8 ":
			if payload+9 <= len(d) && d[payload+3] == 0x9D && d[payload+4] == 0x01 {
				w := int(d[payload+5]) | int(d[payload+6])<<8
				h := int(d[payload+7]) | int(d[payload+8])<<8
				return w & 0x3FFF, h & 0x3FFF, true
			}
		}
		off = payload + size + (size & 1)
	}
	return 0, 0, false
}

// uploadFileName prefixes the display name with a JavaScript-style
// performance.now() float millisecond timestamp. The name is generated once
// per upload and reused for begin and commit; the server requires the exact
// same name at both (mismatch → commit fails with success 9).
func uploadFileName(original string) string {
	ms := float64(time.Now().UnixNano()) / 1e6
	return strconv.FormatFloat(ms, 'f', -1, 64) + "_" + original
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// beginFileUpload calls chat/beginfileupload. EResult 16 (Timeout) failures
// are retried with short backoff; 21/112 and other failures are never retried.
func (u *ImageUploader) beginFileUpload(ctx context.Context, s *uploadState) (*beginUploadResponse, error) {
	fields := []formField{
		{"sessionid", s.sessionID},
		{"l", u.language},
		{"file_size", strconv.Itoa(len(s.data))},
		{"file_name", s.upName},
		{"file_sha", s.shaHex},
		{"file_image_width", strconv.Itoa(s.width)},
		{"file_image_height", strconv.Itoa(s.height)},
		{"file_type", s.mime},
	}
	for attempt := 0; ; attempt++ {
		status, body, err := u.postChatForm(ctx, chatBeginPath+"?l="+u.language, s.cookie, fields)
		if err != nil {
			return nil, &UploadError{Stage: StageBegin, Message: sanitizeErrorText(err.Error())}
		}
		parsed, err := decodeBeginResponse(body)
		if err != nil {
			return nil, &UploadError{Stage: StageBegin, HTTPStatus: status, Message: "beginfileupload response is not JSON"}
		}
		if parsed.Success == eresultTimeout && attempt < len(beginRetryDelays) {
			u.log.Debug().Int("attempt", attempt+1).Msg("Steam beginfileupload returned success 16 (Timeout); retrying")
			if !sleepCtx(ctx, beginRetryDelays[attempt]) {
				return nil, &UploadError{Stage: StageBegin, Message: "context cancelled during beginfileupload retry backoff"}
			}
			continue
		}
		if parsed.Success == 1 && status/100 == 2 && parsed.Result.Ugcid != "" && parsed.Result.UrlHost != "" && parsed.Result.UrlPath != "" {
			return parsed, nil
		}
		return nil, &UploadError{
			Stage:       StageBegin,
			HTTPStatus:  status,
			SteamResult: parsed.Success,
			Message:     sanitizeErrorText(parsed.Message),
		}
	}
}

// putToCloud PUTs the raw file bytes to the cloud upload URL from the begin
// result, applying every request_headers entry except Host and Content-Length
// (which the HTTP stack sets itself). Only 200/201 are accepted (PROBE.md
// §10: GCS answers 200, Azure 201).
func (u *ImageUploader) putToCloud(ctx context.Context, s *uploadState, begin *beginUploadResponse) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadPutTarget(begin), bytes.NewReader(s.data))
	if err != nil {
		return &UploadError{Stage: StagePut, Message: sanitizeErrorText(err.Error())}
	}
	for _, h := range begin.Result.RequestHeaders {
		switch strings.ToLower(h.Name) {
		case "", "host", "content-length":
			continue
		}
		// Direct map assignment preserves the exact header-name case the
		// cloud host requires (x-ms-*, x-goog-*).
		req.Header[h.Name] = []string{h.Value}
	}
	res, err := u.client.Do(req)
	if err != nil {
		return &UploadError{Stage: StagePut, Message: sanitizeErrorText(err.Error())}
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64*1024))
	if res.StatusCode != 200 && res.StatusCode != 201 {
		return &UploadError{
			Stage:      StagePut,
			HTTPStatus: res.StatusCode,
			Message:    fmt.Sprintf("cloud upload rejected with HTTP %d", res.StatusCode),
		}
	}
	u.log.Debug().Int("status", res.StatusCode).Msg("Cloud PUT accepted")
	return nil
}

// uploadPutTarget builds the cloud PUT URL from the begin result. The signed
// upload credential rides in url_path's query string, so this URL must never
// be logged or embedded in errors (redactQueryString strips it).
func uploadPutTarget(begin *beginUploadResponse) string {
	scheme := "http"
	if begin.Result.UseHTTPS {
		scheme = "https"
	}
	host := begin.Result.UrlHost
	if !strings.Contains(host, "://") {
		host = scheme + "://" + host
	}
	return host + begin.Result.UrlPath
}

// commitFileUpload commits (or, with success=false, abandons) the upload. The
// server posts the resulting chat message itself when the commit succeeds;
// friend_steamid is only sent here, never at begin.
func (u *ImageUploader) commitFileUpload(ctx context.Context, s *uploadState, begin *beginUploadResponse, success bool) (*commitUploadResponse, error) {
	fields := []formField{
		{"sessionid", s.sessionID},
		{"l", u.language},
		{"file_name", s.upName},
		{"file_sha", s.shaHex},
		{"success", boolToDigit(success)},
		{"ugcid", begin.Result.Ugcid},
		{"file_type", s.mime},
		{"file_image_width", strconv.Itoa(s.width)},
		{"file_image_height", strconv.Itoa(s.height)},
		{"timestamp", strings.TrimSpace(string(begin.Timestamp))},
		{"hmac", begin.Hmac},
	}
	fields = append(fields, s.req.Target.commitFields()...)
	fields = append(fields, formField{"spoiler", boolToDigit(s.req.Spoiler)})
	status, body, err := u.postChatForm(ctx, chatCommitPath, s.cookie, fields)
	if err != nil {
		return nil, &UploadError{Stage: StageCommit, Message: sanitizeErrorText(err.Error())}
	}
	parsed, err := decodeCommitResponse(body)
	if err != nil {
		return nil, &UploadError{Stage: StageCommit, HTTPStatus: status, Message: "commitfileupload response is not JSON"}
	}
	if parsed.Success != 1 || status/100 != 2 {
		return nil, &UploadError{
			Stage:       StageCommit,
			HTTPStatus:  status,
			SteamResult: parsed.Success,
			Message:     sanitizeErrorText(parsed.Message),
		}
	}
	if success && parsed.Result.Details.URL == "" {
		// Confirmed for images (PROBE.md §10); videos may be processed
		// asynchronously, so a successful commit without a URL is not a failure.
		u.log.Warn().Str("mime", s.mime).Msg("Steam commitfileupload succeeded without result.details.url")
	}
	return parsed, nil
}

// formField is one multipart form value; the field order is significant to the
// server (the app builds FormData in a fixed order).
type formField struct {
	key   string
	value string
}

// postChatForm POSTs the multipart form the app sends (fetch with
// credentials:'include', no custom headers) and returns the HTTP status and
// body; callers parse the body regardless of status because Steam's error
// responses are JSON too (PROBE.md §10).
func (u *ImageUploader) postChatForm(ctx context.Context, path, cookie string, fields []formField) (int, []byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := w.WriteField(f.key, f.value); err != nil {
			return 0, nil, fmt.Errorf("multipart field %q: %w", f.key, err)
		}
	}
	if err := w.Close(); err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.baseURL+path, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+w.Boundary())
	req.Header.Set("Cookie", cookie)
	res, err := u.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, uploadBodyLimit))
	if err != nil {
		return res.StatusCode, nil, err
	}
	return res.StatusCode, body, nil
}

// beginUploadHeader is one name/value entry of the begin result's
// request_headers, to be applied verbatim to the cloud PUT.
type beginUploadHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type beginUploadResult struct {
	Ugcid          string              `json:"ugcid"`
	UrlHost        string              `json:"url_host"`
	UrlPath        string              `json:"url_path"`
	UseHTTPS       flexBool            `json:"use_https"`
	RequestHeaders []beginUploadHeader `json:"request_headers"`
}

type beginUploadResponse struct {
	Success   int               `json:"success"`
	Result    beginUploadResult `json:"result"`
	Hmac      string            `json:"hmac"`
	Timestamp json.RawMessage   `json:"timestamp"`
	Message   string            `json:"message"`
}

func decodeBeginResponse(body []byte) (*beginUploadResponse, error) {
	var parsed beginUploadResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	return &parsed, nil
}

type commitUploadDetails struct {
	URL      string `json:"url"`
	FileSha  string `json:"file_sha"`
	FileSize int64  `json:"file_size"`
}

type commitUploadResult struct {
	Details commitUploadDetails `json:"details"`
}

type commitUploadResponse struct {
	Success int                `json:"success"`
	Message string             `json:"message"`
	Result  commitUploadResult `json:"result"`
}

func decodeCommitResponse(body []byte) (*commitUploadResponse, error) {
	var parsed commitUploadResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	return &parsed, nil
}

// flexBool decodes a JSON bool that Steam may send as true/false or 1/0
// (PROBE.md §10: use_https arrives as the number 1).
type flexBool bool

func (f *flexBool) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true", "1":
		*f = true
	case "false", "0", "null":
		*f = false
	default:
		return fmt.Errorf("invalid bool value %s", data)
	}
	return nil
}

// boolToDigit renders b as the "1"/"0" form field value the app sends.
func boolToDigit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// sleepCtx sleeps for d or until ctx is done, reporting which happened.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

var jwtTokenPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)

// redactJWTs replaces any JWT-looking access token in s with a placeholder,
// so foreign error text (e.g. a token provider's error) can never carry one.
func redactJWTs(s string) string {
	return jwtTokenPattern.ReplaceAllString(s, "<redacted>")
}

// redactQueryString strips the query string from any URL-looking text so the
// signed cloud PUT URL (whose query carries the upload credential) never
// leaks through error messages or logs.
func redactQueryString(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		q := strings.Index(s[i:], "?")
		if q < 0 {
			out.WriteString(s[i:])
			break
		}
		q += i
		end := q + 1
		for end < len(s) && s[end] != ' ' && s[end] != '\n' && s[end] != '\r' && s[end] != '\t' {
			end++
		}
		if strings.Contains(s[q+1:end], "=") {
			out.WriteString(s[i:q])
			out.WriteString("?<redacted>")
		} else {
			out.WriteString(s[i:end])
		}
		i = end
	}
	return out.String()
}

// sanitizeErrorText makes foreign error text safe to embed in an
// UploadError/log line: JWT-looking tokens and URL query strings are redacted.
func sanitizeErrorText(s string) string {
	return redactQueryString(redactJWTs(s))
}
