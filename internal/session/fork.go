package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/fileutil"
)

// Fork publishes an independent child session containing the exact durable
// prefix through a completed turn. The cut must also be a logical batch
// boundary, so a child can never inherit half of an atomic operation.
//
// Fork is a Session operation because the inherited prefix is business state:
// the physical layer only writes the child bytes.
func (s *Session) Fork(ctx context.Context, childDir, childID string, throughSequence uint64) (Manifest, error) {
	if s == nil {
		return Manifest{}, fmt.Errorf("session: nil parent session")
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if s.readOnly {
		return Manifest{}, ErrReadOnly
	}
	if _, err := s.Flush(ctx); err != nil {
		return Manifest{}, fmt.Errorf("flush parent before fork: %w", err)
	}
	var prefix []Commit
	var cursor uint64
	for {
		previous := cursor
		page, err := s.Read(ctx, cursor, 1000)
		if err != nil {
			return Manifest{}, err
		}
		stop := false
		for _, commit := range page.Commits {
			if commit.LastSequence() > throughSequence {
				stop = true
				break
			}
			prefix = append(prefix, commit)
		}
		if stop || !page.Truncated {
			break
		}
		if page.Next <= previous {
			return Manifest{}, fmt.Errorf("%w: fork cursor did not advance", ErrDamagedStore)
		}
		cursor = page.Next
	}
	s.mu.Lock()
	parentDir, parentID := s.dir(), s.id
	s.mu.Unlock()
	if throughSequence > 0 && (len(prefix) == 0 || prefix[len(prefix)-1].LastSequence() != throughSequence) {
		return Manifest{}, fmt.Errorf("session: fork cut %d is not an atomic batch boundary", throughSequence)
	}
	return writeForkChild(ctx, parentDir, parentID, prefix, childDir, childID, throughSequence)
}

// dir reports the physical directory backing this session, if any.
func (s *Session) dir() string {
	if s == nil || s.binding == nil {
		return ""
	}
	if store, ok := s.binding.handle.(*Store); ok {
		return store.Dir()
	}
	return ""
}

func writeForkChild(ctx context.Context, parentDir, parentID string, prefix []Commit, childDir, childID string, throughSequence uint64) (Manifest, error) {
	childDir = filepath.Clean(strings.TrimSpace(childDir))
	childID = strings.TrimSpace(childID)
	if childDir == "." || childID == "" {
		return Manifest{}, fmt.Errorf("session: child directory and id are required")
	}
	projection, err := Project(prefix)
	if err != nil {
		return Manifest{}, err
	}
	if projection.TurnID != "" || len(projection.Interactions) != 0 {
		return Manifest{}, fmt.Errorf("session: fork boundary retains active runtime authority")
	}

	// Inherited events retain their stable IDs and sequences, while physical
	// commit identity and operation id are rebound to the child. Parent
	// idempotency keys must never suppress a future child submission.
	inherited := make([]Commit, len(prefix))
	for i, original := range prefix {
		commit := cloneCommit(original)
		commit.Codec = Codec
		commit.ID = deterministicID("fork\x00" + childID + "\x00" + original.ID)
		commit.OperationID = "inherit:" + parentID + ":" + original.ID
		commit.WriterGeneration = 1
		hash, hashErr := hashOperation(childID, commit.TurnID, commit.Events)
		if hashErr != nil {
			return Manifest{}, hashErr
		}
		commit.OperationHash = hash
		inherited[i] = commit
	}
	var log bytes.Buffer
	if _, err := encodeV4Commits(ctx, &log, contentStoreForSessionDir(childDir), inherited); err != nil {
		return Manifest{}, err
	}
	digest := sha256.Sum256(log.Bytes())
	manifest := Manifest{
		SchemaVersion: SchemaVersion, Codec: Codec, StorageRevision: StorageRevision, ContentRoot: sharedContentRoot, SessionID: childID, CreatedAt: time.Now().UTC(),
		InheritedEvents: throughSequence,
		Source:          &Source{Path: parentDir, Size: int64(log.Len()), SHA256: hex.EncodeToString(digest[:]), Version: Codec},
	}
	if _, err := os.Stat(childDir); err == nil {
		return Manifest{}, fmt.Errorf("session: child session already exists")
	} else if !os.IsNotExist(err) {
		return Manifest{}, err
	}
	parent := filepath.Dir(childDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Manifest{}, err
	}
	tmp, err := os.MkdirTemp(parent, "."+childID+".fork-")
	if err != nil {
		return Manifest{}, err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := writeManifestFile(filepath.Join(tmp, "manifest.json"), manifest); err != nil {
		return Manifest{}, err
	}
	if err := fileutil.AtomicWriteFileStrict(filepath.Join(tmp, currentLogName), log.Bytes(), 0o600); err != nil {
		return Manifest{}, err
	}
	if err := copyOwnedSessionFiles(ctx, parentDir, tmp); err != nil {
		return Manifest{}, fmt.Errorf("copy fork attachments: %w", err)
	}
	if replayed, err := Replay(tmp, nil); err != nil || len(replayed) != len(inherited) {
		if err == nil {
			err = fmt.Errorf("copied %d of %d commits", len(replayed), len(inherited))
		}
		return Manifest{}, fmt.Errorf("validate fork: %w", err)
	}
	if err := os.Rename(tmp, childDir); err != nil {
		return Manifest{}, fmt.Errorf("publish child session: %w", err)
	}
	published = true
	return manifest, nil
}
