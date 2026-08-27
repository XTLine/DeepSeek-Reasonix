package agent

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"

	"reasonix/internal/provider"
)

func (s *Session) classifySnapshotWriteForCommit(path string, msgs []provider.Message, digest [sha256.Size]byte, version uint64, ownedRewrite bool, mode sessionSaveMode) (snapshotWriteDecision, error) {
	decision, err := s.classifySnapshotWrite(path, msgs, digest, version, ownedRewrite)
	if err != nil || decision.upToDate && mode != sessionSaveRewriteCompact && !decision.ledgerStale {
		return decision, err
	}
	// Invalidate before any transcript mutation or stale-ledger repair so an
	// interrupted commit cannot leave the previous listing self-certified.
	if err := invalidateSessionListingProjection(path); err != nil {
		return decision, fmt.Errorf("invalidate session listing projection: %w", err)
	}
	return decision, nil
}

func (s *Session) markPersistedWithListing(path string, digest [sha256.Size]byte, version uint64, revision int64, rewriteVersion int, msgs []provider.Message) {
	// Pair the persisted-message view with the baseline: writer-bound saves
	// use it to classify append shapes without reloading the transcript.
	s.setPersistedBaseline(path, digest, version, revision, true, true, rewriteVersion, msgs)
	persistSessionListingProjection(path, msgs, revision, digestString(digest))
}

// invalidateSessionListingProjection makes cached counts untrusted before a
// transcript commit begins. If the transcript lands but its revision ledger
// does not, readers must decode/repair instead of certifying the previous
// generation's preview from an internally consistent but stale sidecar.
func invalidateSessionListingProjection(path string) error {
	err := UpdateBranchMeta(path, false, func(meta *BranchMeta) error {
		// Preserve the old values for diagnostics and cheap retry repair; the
		// schema gate alone is enough to keep every current/legacy reader from
		// treating them as authoritative during the commit window.
		meta.SchemaVersion = 0
		return nil
	})
	if err == nil || sessionArtifactsHaveContent(path) {
		return err
	}
	// A brand-new session has no prior projection to reuse. Preserve transcript-
	// first durability when malformed metadata cannot be invalidated; the later
	// ledger update still reports that error after the bytes land.
	return nil
}

func persistSessionListingProjection(path string, msgs []provider.Message, revision int64, contentDigest string) {
	preview, turns := SessionPreviewFromMessages(msgs)
	if err := UpdateBranchMeta(path, false, func(meta *BranchMeta) error {
		// The transcript commit and this repairable projection are separate
		// critical sections. A newer writer may already have advanced the
		// sidecar, so only publish counts for the generation this save committed.
		contentDigest = strings.TrimSpace(contentDigest)
		if revision <= 0 || meta.Revision != revision || strings.TrimSpace(meta.ContentDigest) != contentDigest {
			return nil
		}
		meta.Preview = preview
		meta.Turns = turns
		meta.SchemaVersion = BranchMetaCountsVersion
		meta.ListingRevision = revision
		meta.ListingContentDigest = contentDigest
		return nil
	}); err != nil {
		// JSONL/event log already committed. Listing metadata is a repairable
		// projection and must never turn a successful transcript save into an
		// application error.
		slog.Warn("session: listing metadata update deferred", "path", path, "err", err)
	}
}
