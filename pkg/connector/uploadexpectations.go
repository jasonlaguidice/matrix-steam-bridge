// In-memory registry correlating web-uploaded images with the Steam echo
// messages the server posts on a successful commit. The upload/commit goes
// over the web (steam-chat.com), not the bridge's CM session, so Steam posts
// the resulting chat message server-side and it reaches this session as a
// local echo (IsEcho=true) that would otherwise render the image a second
// time in Matrix. The echo's [img src=...] URL carries the uppercase SHA-1 of
// the uploaded bytes, so the echo can be matched to its Matrix event without
// any server round trip; expectations are registered BEFORE the upload starts
// because the echo can overtake the commit response. Losing an entry across a
// restart is acceptable graceful degradation: the echo then flows through the
// normal conversion path.
package connector

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// uploadExpectationTTL is how long an expectation stays live. It must
// comfortably exceed the bridgev2 NoEchoTimeout (uploadNoEchoTimeout): the
// visible "may not have been delivered" failure fires first, and an echo that
// still arrives afterwards must be matched so bridgev2 re-keys the row and
// replaces the failure with the success status. Past the TTL the echo is
// considered lost and the Matrix message keeps its synthetic
// upload:{sha1}:{eventID}:out identity (see uploadRowStore).
const uploadExpectationTTL = 10 * time.Minute

// uploadNoEchoTimeout is how long after the upload handler returns bridgev2
// waits for the Steam echo before marking the Matrix event as failed.
const uploadNoEchoTimeout = 30 * time.Second

// uploadOutgoingTimeouts is the bridgev2 pending-message timeout config for
// web uploads. NoAckTimeout stays zero (disabled): the handler is synchronous,
// so bridgev2 measures the ack from when the handler returns, and a NoAck
// timer measured from the row timestamp would misfire on slow uploads.
func uploadOutgoingTimeouts() *bridgev2.OutgoingTimeoutConfig {
	return &bridgev2.OutgoingTimeoutConfig{
		CheckInterval: 5 * time.Second,
		NoEchoTimeout: uploadNoEchoTimeout,
		NoEchoMessage: "Steam accepted the upload but the message did not appear in the chat within 30s; it may still have been delivered",
	}
}

type uploadExpectationKey struct {
	portal networkid.PortalID
	sha1   string // uppercase SHA-1 hex of the uploaded bytes
}

type uploadExpectation struct {
	txnID        networkid.TransactionID // Matrix event ID the upload belongs to
	registeredAt time.Time
}

type uploadExpectations struct {
	log     zerolog.Logger
	mu      sync.Mutex
	entries map[uploadExpectationKey]uploadExpectation
}

func newUploadExpectations(log zerolog.Logger) *uploadExpectations {
	return &uploadExpectations{
		log:     log,
		entries: make(map[uploadExpectationKey]uploadExpectation),
	}
}

// register records that an upload of bytes with SHA-1 sha1Upper into portal is
// starting for the Matrix event txnID. Expired entries are pruned first; a
// pruned entry means the echo never arrived within the TTL, so the upload's
// database row keeps its synthetic identity.
func (r *uploadExpectations) register(portal networkid.PortalID, sha1Upper string, txnID networkid.TransactionID) {
	key := uploadExpectationKey{portal: portal, sha1: sha1Upper}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	r.entries[key] = uploadExpectation{txnID: txnID, registeredAt: now}
}

// consume returns and removes the expectation registered for
// portal+sha1Upper, or reports false when no live expectation matches.
func (r *uploadExpectations) consume(portal networkid.PortalID, sha1Upper string) (networkid.TransactionID, bool) {
	key := uploadExpectationKey{portal: portal, sha1: sha1Upper}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now())
	entry, ok := r.entries[key]
	if !ok {
		return "", false
	}
	delete(r.entries, key)
	return entry.txnID, true
}

// pruneLocked drops entries past the TTL.
func (r *uploadExpectations) pruneLocked(now time.Time) {
	for key, entry := range r.entries {
		if now.Sub(entry.registeredAt) <= uploadExpectationTTL {
			continue
		}
		delete(r.entries, key)
		r.log.Warn().
			Str("sha1", key.sha1).
			Str("portal", string(key.portal)).
			Str("txn_id", string(entry.txnID)).
			Msg("Steam echo for image upload never arrived within TTL; dropping expectation")
	}
}

// syntheticUploadMessageID is the identity of a committed upload's database
// row until (and unless) its Steam echo re-keys it to the real message ID.
func syntheticUploadMessageID(sha1Upper string, eventID id.EventID) networkid.MessageID {
	return networkid.MessageID("upload:" + strings.ToLower(sha1Upper) + ":" + string(eventID) + ":out")
}

// uploadRowStore persists a committed upload's synthetic row. The row exists
// from commit time so that a Steam echo that never arrives cannot make a later
// backfill import the image a second time (uploadRowStore.isBridgedUpload);
// when the echo does arrive, release removes it just before bridgev2 inserts
// the row re-keyed to the real Steam message ID.
type uploadRowStore struct {
	messages *database.MessageQuery
}

func newUploadRowStore(messages *database.MessageQuery) uploadRowStore {
	return uploadRowStore{messages: messages}
}

// persist inserts a copy of the pending upload row (the pending original stays
// untouched for bridgev2's own insert on echo).
func (s uploadRowStore) persist(ctx context.Context, pending *database.Message) error {
	row := *pending
	row.RowID = 0
	return s.messages.Insert(ctx, &row)
}

// release deletes the synthetic row for mxid, if any, so the echo's row can be
// inserted under the same MXID.
func (s uploadRowStore) release(ctx context.Context, mxid id.EventID, realID networkid.MessageID) error {
	existing, err := s.messages.GetPartByMXID(ctx, mxid)
	if err != nil || existing == nil || existing.ID == realID || !strings.HasPrefix(string(existing.ID), "upload:") {
		return err
	}
	return s.messages.Delete(ctx, existing.RowID)
}

// isBridgedUpload reports whether a synthetic upload row in portal carries the
// UGC hash of steamMsgContent and was created within the expectation TTL of
// sentAt, i.e. the Steam message is the echo of a Matrix upload the bridge
// already delivered.
func (s uploadRowStore) isBridgedUpload(ctx context.Context, portal networkid.PortalKey, steamMsgContent string, sentAt time.Time) (bool, error) {
	hash, _ := ugcMediaHashFromMessage(steamMsgContent)
	if hash == "" {
		return false, nil
	}
	rows, err := s.messages.GetMessagesBetweenTimeQuery(ctx, portal, sentAt.Add(-uploadExpectationTTL), sentAt.Add(uploadExpectationTTL))
	if err != nil {
		return false, err
	}
	prefix := "upload:" + strings.ToLower(hash) + ":"
	for _, row := range rows {
		if strings.HasPrefix(string(row.ID), prefix) {
			return true, nil
		}
	}
	return false, nil
}
