package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/filelock"
	"reasonix/internal/sessioncontent"
)

// SetTitle updates mutable session metadata through the canonical event log.
// A cold write acquires the ordinary writer lease, flushes the event, and then
// releases the exact Runtime; no title sidecar becomes a second source of truth.
func (s *Service) SetTitle(ctx context.Context, ref SessionRef, title string) error {
	if err := ref.validate(s.hostID); err != nil {
		return err
	}
	runtime, alreadyOpen := s.Runtime(ref)
	var session *Session
	var err error
	if alreadyOpen {
		session = runtime.Session()
	} else {
		session, err = s.persistence.Open(ref.SessionID, ReadWrite)
		if err != nil {
			return err
		}
		defer session.Close(context.Background())
	}
	payload, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return err
	}
	if _, err = session.AppendBatch(ctx, "session-title:"+randomID(), []Event{{Kind: "session/title", Payload: payload}}); err != nil {
		return err
	}
	_, err = session.Flush(ctx)
	return err
}

// Export writes a self-contained immutable copy of the session directory. It
// first establishes a durability checkpoint, then freezes the physical write
// boundary while copying, so the exported manifest and event prefix cannot
// describe different moments.
func (s *Session) Export(ctx context.Context, destination string) error {
	if s == nil {
		return os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.Flush(ctx); err != nil {
		return err
	}
	if s.binding == nil {
		return ErrReadOnly
	}
	source := s.dir()
	if source == "" {
		return os.ErrClosed
	}
	// The drain chain is the physical write boundary: holding it freezes the
	// bytes on disk. Accepted in-memory updates and Stop do not wait for the
	// export's disk I/O.
	return s.binding.freezePhysical(func() error { return exportDirectory(ctx, source, destination) })
}

func (p *FilesystemPersistence) exportCold(ctx context.Context, sessionID, destination string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	source, err := p.sessionDir(sessionID, true)
	if err != nil {
		return err
	}
	releaseDirectory, err := filelock.AcquireMode(ctx, directoryOwnershipPath(source), filelock.ModeShared)
	if err != nil {
		return err
	}
	defer releaseDirectory()
	if _, err := readManifest(filepath.Join(source, "manifest.json")); err != nil {
		return err
	}
	release, err := filelock.AcquireMode(ctx, filepath.Join(source, "writer.lock"), filelock.ModeShared)
	if err != nil {
		return fmt.Errorf("session: freeze cold export: %w", err)
	}
	defer release()
	return exportDirectory(ctx, source, destination)
}

func exportDirectory(ctx context.Context, source, destination string) error {
	source = filepath.Clean(source)
	destination = filepath.Clean(strings.TrimSpace(destination))
	if destination == "." || destination == source || strings.HasPrefix(destination+string(os.PathSeparator), source+string(os.PathSeparator)) {
		return fmt.Errorf("session: invalid export destination %q", destination)
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("%w: export destination", ErrSessionExists)
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".session-export-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(tmp)
		}
	}()
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == "." {
			return err
		}
		if relative == "writer.lock" || relative == "events.offset-index.json" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(tmp, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("session: export refuses symlink %s", relative)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("session: export refuses non-regular file %s", relative)
		}
		return copySessionFile(ctx, path, target, info.Mode().Perm())
	})
	if err != nil {
		return err
	}
	manifest, err := readManifest(filepath.Join(tmp, "manifest.json"))
	if err != nil {
		return fmt.Errorf("validate export manifest: %w", err)
	}
	if err := copyExportContentClosure(ctx, source, tmp, manifest); err != nil {
		return fmt.Errorf("export referenced content: %w", err)
	}
	manifest.ContentRoot = ".content-v1"
	if err := writeManifestFile(filepath.Join(tmp, "manifest.json"), manifest); err != nil {
		return err
	}
	if _, err := Replay(tmp, nil); err != nil {
		return fmt.Errorf("validate export events: %w", err)
	}
	if manifest.SessionID == "" {
		return fmt.Errorf("%w: export has empty session id", ErrDamagedStore)
	}
	if err := os.Rename(tmp, destination); err != nil {
		return err
	}
	published = true
	return nil
}

func copyExportContentClosure(ctx context.Context, source, target string, manifest Manifest) error {
	if manifest.Codec != Codec {
		return nil
	}
	log, err := os.Open(logPathForManifest(target, manifest))
	if err != nil {
		return err
	}
	defer log.Close()
	refs := map[string]sessioncontent.Ref{}
	if err := scanV4CommitFileRefs(ctx, log, 0, 1, nil, nil, func(_ int64, commit Commit) bool {
		for _, event := range commit.Events {
			if event.PayloadRef != nil {
				key := fmt.Sprintf("%s:%d:%s", event.PayloadRef.Digest, event.PayloadRef.Bytes, event.PayloadRef.IndexDigest)
				refs[key] = *event.PayloadRef
			}
		}
		return true
	}); err != nil {
		return err
	}
	sourceContent := contentStoreForSessionDir(source)
	targetContent := sessioncontent.New(filepath.Join(target, ".content-v1"))
	for _, ref := range refs {
		reader, err := sourceContent.Open(ctx, ref)
		if err != nil {
			return err
		}
		published, putErr := targetContent.Put(ctx, reader, sessioncontent.Metadata{MediaType: ref.MediaType, Name: ref.Name})
		closeErr := reader.Close()
		if putErr != nil || closeErr != nil {
			return errors.Join(putErr, closeErr)
		}
		if published.Digest != ref.Digest || published.Bytes != ref.Bytes || published.IndexDigest != ref.IndexDigest {
			return fmt.Errorf("%w: exported content identity changed", ErrDamagedStore)
		}
	}
	return nil
}

func copySessionFile(ctx context.Context, source, target string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, &contextReader{ctx: ctx, reader: in})
	syncErr := out.Sync()
	closeErr := out.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

// Delete holds directory ownership across rename. The inner writer lock must
// be closed before moving its directory on Windows; the outer lock keeps a
// competing opener from entering that interval.
func (p *FilesystemPersistence) Delete(ctx context.Context, sessionID string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	source, err := p.sessionDir(sessionID, true)
	if err != nil {
		return err
	}
	releaseDirectory, err := filelock.Acquire(ctx, directoryOwnershipPath(source))
	if err != nil {
		return err
	}
	defer releaseDirectory()
	if _, err := readManifest(filepath.Join(source, "manifest.json")); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
		}
		return err
	}
	release, err := filelock.Acquire(ctx, filepath.Join(source, "writer.lock"))
	if err != nil {
		return fmt.Errorf("session: delete ownership: %w", err)
	}
	release()
	trashRoot := filepath.Join(p.Root, ".trash")
	if err := os.MkdirAll(trashRoot, 0o700); err != nil {
		return err
	}
	tombstone := filepath.Join(trashRoot, sessionID+"-"+randomID())
	if err := os.Rename(source, tombstone); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(p.Root, ".query-cache", sessionID))
	return os.RemoveAll(tombstone)
}

func (s *Service) Export(ctx context.Context, ref SessionRef, destination string) error {
	if err := ref.validate(s.hostID); err != nil {
		return err
	}
	if runtime, ok := s.Runtime(ref); ok {
		return runtime.session.Export(ctx, destination)
	}
	filesystem, ok := s.persistence.(*FilesystemPersistence)
	if !ok {
		return errors.New("session: persistence does not support export")
	}
	return filesystem.exportCold(ctx, ref.SessionID, destination)
}

// Import validates and atomically adopts a self-contained exported directory.
// The archive's immutable identity is retained; importing over an existing
// identity is refused rather than merging two histories.
func (s *Service) Import(ctx context.Context, source string) (SessionRef, error) {
	filesystem, ok := s.persistence.(*FilesystemPersistence)
	if !ok {
		return SessionRef{}, errors.New("session: persistence does not support import")
	}
	id, err := filesystem.importDirectory(ctx, source)
	if err != nil {
		return SessionRef{}, err
	}
	return SessionRef{HostID: s.hostID, SessionID: id}, nil
}

func (p *FilesystemPersistence) importDirectory(ctx context.Context, source string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source = filepath.Clean(strings.TrimSpace(source))
	if source == "." {
		return "", errors.New("session: import source is required")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("session: import source must be a real directory")
	}
	manifest, err := readManifest(filepath.Join(source, "manifest.json"))
	if err != nil {
		return "", fmt.Errorf("validate import manifest: %w", err)
	}
	if err := validateSessionID(manifest.SessionID); err != nil {
		return "", err
	}
	if manifest.ContentRoot != ".content-v1" {
		return "", errors.New("session: import is not a self-contained export")
	}
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(p.Root, manifest.SessionID)
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("%w: %s", ErrSessionExists, manifest.SessionID)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	staging := filepath.Join(p.Root, "."+manifest.SessionID+".import-"+randomID())
	if err := exportDirectory(ctx, source, staging); err != nil {
		return "", err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if _, err := Replay(staging, nil); err != nil {
		return "", fmt.Errorf("validate imported events: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		return "", fmt.Errorf("publish imported session: %w", err)
	}
	published = true
	return manifest.SessionID, nil
}

func (s *Service) Delete(ctx context.Context, ref SessionRef) error {
	if err := ref.validate(s.hostID); err != nil {
		return err
	}
	if runtime, ok := s.Runtime(ref); ok {
		if err := s.Close(ctx, ref); err != nil {
			return err
		}
		// Exact-instance removal above completed before filesystem deletion;
		// delayed callbacks cannot remove a successor runtime.
		_ = runtime
	}
	filesystem, ok := s.persistence.(*FilesystemPersistence)
	if !ok {
		return errors.New("session: persistence does not support delete")
	}
	s.query.invalidateCatalog(ref.SessionID)
	return filesystem.Delete(ctx, ref.SessionID)
}
