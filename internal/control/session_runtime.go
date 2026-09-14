package control

import (
	"context"
	"errors"

	"reasonix/internal/session"
)

func (c *Controller) beginSessionRuntimeActivity(ctx context.Context, name string) (context.Context, *session.Activity, error) {
	_, runtime, exclusive := c.v3Binding()
	if !exclusive || runtime == nil {
		return ctx, nil, nil
	}
	runtimeCtx, activity, err := runtime.BeginOwnedActivity(ctx, name)
	if err != nil {
		return ctx, nil, err
	}
	c.v3ActivityMu.Lock()
	c.v3Activity = activity
	c.v3ActivityMu.Unlock()
	return runtimeCtx, activity, nil
}

func (c *Controller) finishSessionRuntimeActivity(activity *session.Activity) {
	if activity == nil {
		return
	}
	c.v3ActivityMu.Lock()
	if c.v3Activity == activity {
		c.v3Activity = nil
	}
	c.v3ActivityMu.Unlock()
	activity.Finish(nil)
}

func (c *Controller) appendSessionBatch(ctx context.Context, store *session.Session, batch session.Batch) (session.Commit, error) {
	if store == nil {
		return session.Commit{}, session.ErrSessionNotRunning
	}
	c.v3ActivityMu.Lock()
	activity := c.v3Activity
	c.v3ActivityMu.Unlock()
	if activity != nil {
		commit, err := activity.Append(ctx, batch)
		if !errors.Is(err, session.ErrStaleActivity) {
			return commit, err
		}
		_, runtime, exclusive := c.v3Binding()
		if exclusive && runtime != nil && runtime.Session() == store {
			return runtime.RecordRecovery(ctx, batch)
		}
		return session.Commit{}, err
	}
	return store.Append(ctx, batch)
}
