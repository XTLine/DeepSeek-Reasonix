package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// HostID returns the immutable host namespace used to validate SessionRef.
func (s *Service) HostID() string {
	if s == nil {
		return ""
	}
	return s.hostID
}

type prepareRuntime struct{ done chan struct{} }

// ClientBinding grants a caller access to one exact published runtime without
// granting authority to close the host-owned writer. Release only detaches this
// client; the host retires an idle runtime after its last binding disappears.
type ClientBinding struct {
	service *Service
	runtime *Runtime
	once    sync.Once
	err     error
}

func (b *ClientBinding) Runtime() *Runtime {
	if b == nil {
		return nil
	}
	return b.runtime
}

func (b *ClientBinding) Release(ctx context.Context) error {
	if b == nil || b.service == nil || b.runtime == nil {
		return nil
	}
	b.once.Do(func() { b.err = b.service.releaseBinding(ctx, b.runtime) })
	return b.err
}

// PreparedRuntime owns a write handle that has been fully opened but is not
// yet visible through the host registry. Callers may build projections, seed
// initial events, and flush before atomically publishing the exact instance.
// A candidate must be either published or discarded.
type PreparedRuntime struct {
	service  *Service
	runtime  *Runtime
	instance string

	mu        sync.Mutex
	published bool
	discarded bool
}

func (p *PreparedRuntime) Runtime() *Runtime {
	if p == nil {
		return nil
	}
	return p.runtime
}

func NewService(hostID string, persistence SessionPersistence) (*Service, error) {
	if hostID == "" || persistence == nil {
		return nil, errors.New("session: host id and persistence are required")
	}
	service := &Service{
		hostID: hostID, persistence: persistence,
		active: map[SessionRef]*Runtime{}, closed: map[SessionRef]error{}, preparing: map[SessionRef]*prepareRuntime{},
		bindings: map[*Runtime]int{}, retiring: map[*Runtime]chan struct{}{}, retireIdle: map[*Runtime]bool{},
	}
	service.query = newQuery(hostID, persistence, service)
	return service, nil
}

func (s *Service) Create(ctx context.Context, options CreateOptions) (*Runtime, error) {
	prepared, err := s.PrepareCreate(ctx, options)
	if err != nil {
		return nil, err
	}
	owner, err := s.Publish(prepared)
	if err != nil {
		_ = s.Discard(context.Background(), prepared)
		return nil, err
	}
	return owner.Runtime(), nil
}

// PrepareCreate reserves the immutable session identity and its writer lease
// without publishing an attachable runtime. This is the DSH prepare phase:
// host/controller state remains untouched until Publish succeeds.
func (s *Service) PrepareCreate(ctx context.Context, options CreateOptions) (*PreparedRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session, err := s.persistence.Create(options)
	if err != nil {
		return nil, err
	}
	session.externalizeDurableHistory()
	ref := SessionRef{HostID: s.hostID, SessionID: session.ID()}
	candidate := newRuntime(ref, session)
	candidate.owner = s
	return &PreparedRuntime{service: s, runtime: candidate, instance: randomID()}, nil
}

// Publish makes the exact prepared runtime visible and returns the host
// authority over that instance. It never replaces an existing instance with the
// same identity; the caller must resolve that ownership conflict explicitly.
//
// Only a RuntimeOwner can terminate a published runtime. Clients attach through
// ClientBinding and can only detach themselves.
func (s *Service) Publish(prepared *PreparedRuntime) (*RuntimeOwner, error) {
	if prepared == nil || prepared.service != s || prepared.runtime == nil {
		return nil, errors.New("session: invalid prepared runtime")
	}
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.discarded {
		return nil, errors.New("session: prepared runtime was discarded")
	}
	candidate := prepared.runtime
	if prepared.published {
		return &RuntimeOwner{service: s, runtime: candidate, instance: prepared.instance}, nil
	}
	ref := candidate.ref
	s.mu.Lock()
	if current := s.active[ref]; current != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrSessionExists, ref.SessionID)
	}
	delete(s.closed, ref)
	candidate.instance = prepared.instance
	s.active[ref] = candidate
	s.revision.Add(1)
	s.mu.Unlock()
	prepared.published = true
	return &RuntimeOwner{service: s, runtime: candidate, instance: prepared.instance}, nil
}

// RuntimeOwner is the host-side authority over one exact published instance.
// Controller and client code must never hold one: they use ClientBinding so a
// failed attach can only undo its own bind, never dispose a shared runtime.
type RuntimeOwner struct {
	service  *Service
	runtime  *Runtime
	instance string
}

// Runtime exposes the owned instance for host preparation work.
func (o *RuntimeOwner) Runtime() *Runtime {
	if o == nil {
		return nil
	}
	return o.runtime
}

// Bind attaches a client to this instance without transferring ownership.
func (o *RuntimeOwner) Bind() (*ClientBinding, error) {
	if o == nil || o.service == nil || o.runtime == nil {
		return nil, ErrSessionNotRunning
	}
	return o.service.Bind(o.runtime)
}

// Close terminates this exact instance. It refuses while any client is still
// bound, and it can never affect a same-ID successor published later.
func (o *RuntimeOwner) Close(ctx context.Context) error {
	if o == nil || o.service == nil || o.runtime == nil {
		return ErrSessionNotRunning
	}
	return o.service.closeOwned(ctx, o.runtime, o.instance)
}

// Owner returns the host authority for the exact active instance. It fails for
// an unknown or superseded instance, so a delayed caller can never acquire
// authority over a same-ID successor. It is used by the flows that publish a
// brand-new identity in the same call; attaching to an existing session must go
// through Open and its ClientBinding instead.
func (s *Service) Owner(runtime *Runtime) (*RuntimeOwner, error) {
	if runtime == nil || runtime.owner != s {
		return nil, ErrSessionNotRunning
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if runtime.instance == "" || s.active[runtime.ref] != runtime {
		return nil, ErrSessionNotRunning
	}
	return &RuntimeOwner{service: s, runtime: runtime, instance: runtime.instance}, nil
}

// Discard closes an unpublished candidate and releases its writer lease.
// Published runtimes must be closed through Service.Close so exact-instance
// unregistering cannot be bypassed.
func (s *Service) Discard(ctx context.Context, prepared *PreparedRuntime) error {
	if prepared == nil || prepared.service != s || prepared.runtime == nil {
		return nil
	}
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.published {
		return errors.New("session: published runtime cannot be discarded")
	}
	if prepared.discarded {
		return prepared.runtime.close(ctx)
	}
	prepared.discarded = true
	return prepared.runtime.close(ctx)
}

// Open attaches a client to an existing session and returns a binding that can
// only detach this client. It never hands out authority to close a runtime that
// another client may already be using.
func (s *Service) Open(ctx context.Context, ref SessionRef) (*ClientBinding, error) {
	return s.openBinding(ctx, ref)
}

// openRuntime resolves the exact published instance. It is internal so only the
// service's own prepare/publish flows can reach a runtime without a grant.
func (s *Service) openRuntime(ctx context.Context, ref SessionRef) (*Runtime, error) {
	if err := ref.validate(s.hostID); err != nil {
		return nil, err
	}
	for {
		s.mu.Lock()
		if current := s.active[ref]; current != nil {
			s.mu.Unlock()
			return current, nil
		}
		if pending := s.preparing[ref]; pending != nil {
			done := pending.done
			s.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		pending := &prepareRuntime{done: make(chan struct{})}
		s.preparing[ref] = pending
		s.mu.Unlock()
		break
	}

	session, err := s.persistence.Open(ref.SessionID, ReadWrite)
	if err != nil {
		s.finishPrepare(ref)
		return nil, err
	}
	if _, _, recoverErr := session.RecoverInterrupted(ctx); recoverErr != nil {
		_ = session.Close(context.Background())
		s.finishPrepare(ref)
		return nil, fmt.Errorf("session: close interrupted runtime: %w", recoverErr)
	}
	session.externalizeDurableHistory()
	candidate := newRuntime(ref, session)
	candidate.owner = s
	s.mu.Lock()
	pending := s.preparing[ref]
	delete(s.preparing, ref)
	if current := s.active[ref]; current != nil {
		if pending != nil {
			close(pending.done)
		}
		s.mu.Unlock()
		_ = candidate.close(context.Background())
		return current, nil
	}
	delete(s.closed, ref)
	// Stamp the publish grant so Owner can hand the host authority over exactly
	// this instance and no same-ID successor.
	candidate.instance = randomID()
	s.active[ref] = candidate
	s.revision.Add(1)
	if pending != nil {
		close(pending.done)
	}
	s.mu.Unlock()
	return candidate, nil
}

func (s *Service) finishPrepare(ref SessionRef) {
	s.mu.Lock()
	if pending := s.preparing[ref]; pending != nil {
		delete(s.preparing, ref)
		close(pending.done)
	}
	s.mu.Unlock()
}

func (s *Service) Runtime(ref SessionRef) (*Runtime, bool) {
	if ref.validate(s.hostID) != nil {
		return nil, false
	}
	s.mu.Lock()
	runtime := s.active[ref]
	s.mu.Unlock()
	return runtime, runtime != nil
}

// Bind attaches a client to an exact published runtime. The returned binding
// can only release its own reference; it cannot dispose the shared runtime.
func (s *Service) Bind(runtime *Runtime) (*ClientBinding, error) {
	if runtime == nil || runtime.owner != s {
		return nil, ErrSessionNotRunning
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retiring[runtime] != nil {
		return nil, ErrRuntimeRetiring
	}
	if s.active[runtime.ref] != runtime {
		return nil, ErrSessionNotRunning
	}
	s.bindings[runtime]++
	delete(s.retireIdle, runtime)
	return &ClientBinding{service: s, runtime: runtime}, nil
}

// OpenBinding opens or reuses a runtime and attaches a client capability.
func (s *Service) openBinding(ctx context.Context, ref SessionRef) (*ClientBinding, error) {
	for {
		runtime, err := s.openRuntime(ctx, ref)
		if err != nil {
			return nil, err
		}
		binding, err := s.Bind(runtime)
		if err == nil {
			return binding, err
		}
		if errors.Is(err, ErrRuntimeRetiring) {
			s.mu.Lock()
			done := s.retiring[runtime]
			s.mu.Unlock()
			if done != nil {
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			continue
		}
		return nil, err
	}
}

func (s *Service) releaseBinding(ctx context.Context, runtime *Runtime) error {
	s.mu.Lock()
	count := s.bindings[runtime]
	if count <= 1 {
		delete(s.bindings, runtime)
		s.retireIdle[runtime] = true
	} else {
		s.bindings[runtime] = count - 1
	}
	s.mu.Unlock()
	if count <= 1 {
		return s.closeIfUnbound(ctx, runtime)
	}
	return nil
}

func (s *Service) closeIfUnbound(ctx context.Context, runtime *Runtime) error {
	if runtime == nil {
		return nil
	}
	s.mu.Lock()
	if s.active[runtime.ref] != runtime || s.bindings[runtime] != 0 || !s.retireIdle[runtime] {
		s.mu.Unlock()
		return nil
	}
	if runtime.current.Load() != nil {
		s.mu.Unlock()
		return nil
	}
	if done := s.retiring[runtime]; done != nil {
		s.mu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	s.retiring[runtime] = done
	s.mu.Unlock()

	err := runtime.close(ctx)
	s.mu.Lock()
	delete(s.retiring, runtime)
	if !errors.Is(err, ErrRuntimeBusy) && s.active[runtime.ref] == runtime && s.bindings[runtime] == 0 {
		delete(s.active, runtime.ref)
		delete(s.retireIdle, runtime)
		s.closed[runtime.ref] = err
		s.revision.Add(1)
	}
	close(done)
	s.mu.Unlock()
	return err
}
