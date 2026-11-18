package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
)

// workspaceUserLoad shares one bounded refresh across callers without tying it to a browser's lifetime.
type workspaceUserLoad struct {
	done chan struct{}
	err  error
}

// workspaceUserState publishes only complete indexes, so later pages can never change a chosen target.
type workspaceUserState struct {
	handles map[string]string
	until   time.Time
	retry   time.Time
	load    *workspaceUserLoad
}

// workspaceUsers keeps handle resolution independent of the small participant-profile cache.
// Loading is on demand, paced to 10 pages/minute and bounded to 200,000 users and two hours.
// Complete indexes (including misses) live for 24 hours to avoid scans for each new name.
type workspaceUsers struct {
	client   *slack.Client
	lifetime context.Context
	clock    opsalerts.Clock
	wait     func(context.Context, time.Duration) error
	mu       sync.RWMutex
	state    workspaceUserState
}

// now allows deterministic expiry and cooldown tests without wall-clock sleeps.
func (w *workspaceUsers) now() time.Time {
	if w.clock != nil {
		return w.clock.Now()
	}
	return time.Now()
}

// resolve uses only a complete, fresh directory; missing or ambiguous handles never fall back to profiles.
func (w *workspaceUsers) resolve(name string) (string, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if !w.now().Before(w.state.until) {
		return "", false
	}
	id := w.state.handles[strings.ToLower(name)]
	return id, id != ""
}

// prepare waits at most two seconds, then rejects this send definitively while the shared load continues.
// No caller posts after this method returns an error; retrying is always an explicit new send.
func (w *workspaceUsers) prepare(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return &opsalerts.ThreadReferenceLookupError{}
	}
	w.mu.Lock()
	now := w.now()
	if now.Before(w.state.until) {
		w.mu.Unlock()
		return nil
	}
	if now.Before(w.state.retry) {
		w.mu.Unlock()
		return &opsalerts.ThreadReferenceLookupError{}
	}
	load := w.state.load
	if load == nil {
		load = &workspaceUserLoad{done: make(chan struct{})}
		w.state.load = load
		go w.refresh(load)
	}
	w.mu.Unlock()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return &opsalerts.ThreadReferenceLookupError{}
	case <-timer.C:
		return &opsalerts.ThreadReferenceLookupError{}
	case <-load.done:
		if load.err != nil {
			return &opsalerts.ThreadReferenceLookupError{}
		}
		return nil
	}
}

// refresh owns all pagination and publishes the final index atomically, never a partially checked handle.
func (w *workspaceUsers) refresh(load *workspaceUserLoad) {
	ctx, cancel := context.WithTimeout(w.lifetime, 2*time.Hour)
	defer cancel()
	handles, retry, err := w.readPages(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err == nil {
		w.state.handles = handles
		w.state.until = w.now().Add(24 * time.Hour)
		w.state.retry = time.Time{}
	}
	if err != nil {
		w.state.retry = w.now().Add(15 * time.Minute)
		if retry.After(w.state.retry) {
			w.state.retry = retry
		}
	}
	load.err = err
	w.state.load = nil
	close(load.done)
}

// readPages respects cursor progression and Retry-After without the SDK's unbounded users.list retry loop.
func (w *workspaceUsers) readPages(ctx context.Context) (map[string]string, time.Time, error) {
	page := w.client.GetUsersPaginated(slack.GetUsersOptionLimit(200))
	cursors := sets.New[string]()
	handles := map[string]string{}
	users := 0
	retry := time.Time{}
	for count := 0; count < 1000; count++ {
		if err := ctx.Err(); err != nil {
			return nil, retry, err
		}
		next, err := page.Next(ctx)
		var limited *slack.RateLimitedError
		for errors.As(err, &limited) {
			delay := max(limited.RetryAfter, 6*time.Second)
			retry = w.now().Add(delay)
			if err := w.pause(ctx, delay); err != nil {
				return nil, retry, err
			}
			next, err = page.Next(ctx)
		}
		if err != nil {
			return nil, retry, fmt.Errorf("listing workspace users: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, retry, err
		}
		users += len(next.Users)
		if users > 200000 {
			return nil, retry, fmt.Errorf("workspace user directory exceeds safety limit")
		}
		for _, user := range next.Users {
			addWorkspaceHandle(handles, user)
		}
		if next.Cursor == "" {
			return handles, retry, nil
		}
		if cursors.Has(next.Cursor) {
			return nil, retry, fmt.Errorf("workspace user directory repeated a cursor")
		}
		cursors.Insert(next.Cursor)
		page = next
		if err := w.pause(ctx, 6*time.Second); err != nil {
			return nil, retry, err
		}
	}
	return nil, retry, fmt.Errorf("workspace user directory exceeds page limit")
}

// pause permits deterministic pacing/backoff tests while production waits remain cancellable.
func (w *workspaceUsers) pause(ctx context.Context, delay time.Duration) error {
	if w.wait != nil {
		return w.wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// addWorkspaceHandle ignores deactivated identities and marks conflicting active handles unresolvable.
func addWorkspaceHandle(handles map[string]string, user slack.User) {
	if user.Deleted || user.Name == "" || user.ID == "" {
		return
	}
	name := strings.ToLower(user.Name)
	id := user.ID
	if previous, exists := handles[name]; exists && previous != id {
		id = ""
	}
	handles[name] = id
}

// PrepareThreadReferences keeps workspace loading out of the service's serialized posting section.
func (a *slackArchive) PrepareThreadReferences(ctx context.Context) error {
	if a.directory == nil || a.directory.workspace == nil {
		return nil
	}
	return a.directory.workspace.prepare(ctx)
}
