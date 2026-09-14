package session

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"reasonix/internal/provider"
)

// Query is the read model shared by desktop, CLI, Serve, ACP and bots. It uses
// an attached runtime when one exists and otherwise opens only a read handle;
// querying cold history never constructs an Agent or acquires writer ownership.
type Query struct {
	hostID      string
	persistence SessionPersistence
	service     *Service
	rebuildMu   sync.Mutex
	rebuilding  map[string]struct{}
	generation  map[string]uint64
	rebuildCtx  context.Context
	rebuildStop context.CancelFunc
	rebuildSlot chan struct{}
}

func (s *Service) Query() *Query {
	if s == nil {
		return nil
	}
	return s.query
}

func newQuery(hostID string, persistence SessionPersistence, service *Service) *Query {
	rebuildCtx, rebuildStop := context.WithCancel(context.Background())
	query := &Query{
		hostID: hostID, persistence: persistence, service: service,
		rebuilding: map[string]struct{}{}, generation: map[string]uint64{}, rebuildCtx: rebuildCtx,
		rebuildStop: rebuildStop, rebuildSlot: make(chan struct{}, 2),
	}
	return query
}

// Close stops catalog work owned by this query. Individual List callers do not
// own shared rebuilds, so cancelling one request never cancels work another
// caller may use; the host query lifetime is the cancellation boundary.
func (q *Query) Close() {
	if q != nil && q.rebuildStop != nil {
		q.rebuildStop()
	}
}

func (q *Query) Snapshot(ctx context.Context, ref SessionRef) (Snapshot, error) {
	if q == nil || q.persistence == nil {
		return Snapshot{}, fmt.Errorf("session: nil session query")
	}
	if err := ref.validate(q.hostID); err != nil {
		return Snapshot{}, err
	}
	if q.service != nil {
		if runtime, ok := q.service.Runtime(ref); ok {
			return runtime.Session().Snapshot(), nil
		}
	}
	handle, err := q.persistence.Open(ref.SessionID, ReadOnly)
	if err != nil {
		return Snapshot{}, err
	}
	defer handle.Close(context.WithoutCancel(ctx))
	projection := Projection{}
	var cursor uint64
	for {
		page, readErr := handle.Read(ctx, cursor, 1000)
		if readErr != nil {
			return Snapshot{}, readErr
		}
		for _, commit := range page.Commits {
			if err := applyProjectionCommit(&projection, commit); err != nil {
				return Snapshot{}, err
			}
		}
		if !page.Truncated {
			break
		}
		if page.Next <= cursor {
			return Snapshot{}, fmt.Errorf("%w: cold history cursor did not advance", ErrDamagedStore)
		}
		cursor = page.Next
	}
	sequence := projection.CommittedSequence
	return Snapshot{EventSequence: sequence, DurableSequence: sequence, PersistenceStatus: PersistenceReady, Projection: projection}, nil
}

func (q *Query) History(ctx context.Context, ref SessionRef) ([]provider.Message, error) {
	snapshot, err := q.Snapshot(ctx, ref)
	if err != nil {
		return nil, err
	}
	return append([]provider.Message(nil), snapshot.Projection.Messages...), nil
}

func (q *Query) List(ctx context.Context, cursor string, limit int) (SessionPage, error) {
	if q == nil || q.persistence == nil {
		return SessionPage{}, fmt.Errorf("session: nil session query")
	}
	page, err := q.persistence.List(ctx, cursor, limit)
	if err != nil {
		return SessionPage{}, err
	}
	for i := range page.Sessions {
		info := &page.Sessions[i]
		info.Ref = SessionRef{HostID: q.hostID, SessionID: info.SessionID}
		if info.Error != "" {
			continue
		}
		if q.service != nil {
			if runtime, ok := q.service.Runtime(info.Ref); ok {
				metadata := runtime.Session().CatalogMetadata()
				applyCatalogMetadata(info, metadata)
				continue
			}
		}
		if info.Codec == Codec && info.MetadataStatus != MetadataReady {
			q.scheduleMetadataRebuild(info.SessionID)
		}
	}
	return page, nil
}

func applyCatalogMetadata(info *SessionInfo, metadata catalogMetadata) {
	info.Title, info.ModelRef, info.ModelIdentity = metadata.Title, metadata.ModelRef, metadata.ModelIdentity
	info.Turns, info.Preview, info.MetadataStatus = metadata.Turns, metadata.Preview, MetadataReady
}

func (q *Query) scheduleMetadataRebuild(sessionID string) {
	if _, ok := q.persistence.(*FilesystemPersistence); !ok {
		return
	}
	q.rebuildMu.Lock()
	if _, exists := q.rebuilding[sessionID]; exists {
		q.rebuildMu.Unlock()
		return
	}
	q.rebuilding[sessionID] = struct{}{}
	generation := q.generation[sessionID]
	select {
	case q.rebuildSlot <- struct{}{}:
	case <-q.rebuildCtx.Done():
		delete(q.rebuilding, sessionID)
		q.rebuildMu.Unlock()
		return
	default:
		delete(q.rebuilding, sessionID)
		q.rebuildMu.Unlock()
		return
	}
	q.rebuildMu.Unlock()
	go q.rebuildCatalogMetadata(sessionID, generation)
}

// invalidateCatalog fences every metadata task scheduled for an older session
// incarnation. Service calls it before deleting the directory, so a delayed
// task cannot publish a cache entry that makes the deleted session reappear.
func (q *Query) invalidateCatalog(sessionID string) {
	if q == nil {
		return
	}
	q.rebuildMu.Lock()
	q.generation[sessionID]++
	q.rebuildMu.Unlock()
}

func (q *Query) rebuildCatalogMetadata(sessionID string, generation uint64) {
	defer func() { <-q.rebuildSlot }()
	defer func() {
		q.rebuildMu.Lock()
		delete(q.rebuilding, sessionID)
		q.rebuildMu.Unlock()
	}()
	filesystem, ok := q.persistence.(*FilesystemPersistence)
	if !ok {
		return
	}
	handle, err := q.persistence.Open(sessionID, ReadOnly)
	if err != nil {
		return
	}
	defer handle.Close(context.WithoutCancel(q.rebuildCtx))
	sessionDir := filepath.Join(filesystem.Root, sessionID)
	cacheDir := filepath.Join(filesystem.Root, ".query-cache", filepath.Base(sessionID))
	manifest, err := readManifest(filepath.Join(sessionDir, "manifest.json"))
	if err != nil {
		return
	}
	metadata, err := reduceCatalogMetadata(q.rebuildCtx, handle, manifest)
	if err != nil {
		return
	}
	q.rebuildMu.Lock()
	defer q.rebuildMu.Unlock()
	if q.rebuildCtx.Err() != nil || q.generation[sessionID] != generation {
		return
	}
	// Hold the generation boundary through publication. Deletion invalidates
	// before it moves the directory, so it either wins first or waits until this
	// exact-incarnation cache is completely written and then removes it.
	_ = writeCatalogMetadataForSession(cacheDir, sessionDir, metadata)
}
