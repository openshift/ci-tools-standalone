package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
	"github.com/slack-go/slack"
)

// workspaceTestWait records pacing without relying on wall-clock sleeps.
type workspaceTestWait struct {
	delays []time.Duration
	err    error
}

// workspaceReplyArchive combines in-memory Slack messages with the real paginated user resolver.
type workspaceReplyArchive struct {
	*demoArchive
	directory *slackArchive
}

// Post replaces the demo's wall clock with its fixed next timestamp for deterministic reply recovery.
func (a *workspaceReplyArchive) Post(ctx context.Context, channel, root, requestID string, message slack.Msg) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if channel != "CHY2E1BL4" || root != a.roots[0].Timestamp {
		return "", fmt.Errorf("unexpected test reply destination")
	}
	ts := protocolTimestamp(a.next)
	message.Timestamp, message.ThreadTimestamp = ts, root
	message.User, message.BotID, message.ClientMsgID = a.writer.UserID, a.writer.BotID, requestID
	a.threads[root] = append(a.threads[root], slack.Message{Msg: message})
	a.roots[0].ReplyCount++
	a.roots[0].LatestReply = ts
	return ts, nil
}

// ResolveThreadReference deliberately excludes demo aliases and cached participants.
func (a *workspaceReplyArchive) ResolveThreadReference(kind, name string) (string, bool) {
	return a.directory.ResolveThreadReference(kind, name)
}

// PrepareThreadReferences exercises the production directory-loading boundary without network access.
func (a *workspaceReplyArchive) PrepareThreadReferences(ctx context.Context) error {
	return a.directory.PrepareThreadReferences(ctx)
}

// TestWorkspaceUserReply proves uncached users become Slack references while submission fingerprints survive recovery.
func TestWorkspaceUserReply(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members string
		wantErr error
	}{
		{name: "uncached active workspace user", members: `[{"id":"U_NEW","name":"alice"}]`},
		{name: "deactivated user is rejected", members: `[{"id":"U_NEW","name":"alice","deleted":true}]`, wantErr: opsalerts.ErrValidation},
		{name: "ambiguous handle is rejected", members: `[{"id":"U_NEW","name":"alice"},{"id":"U_OTHER","name":"ALICE"}]`, wantErr: opsalerts.ErrValidation},
		{name: "unknown user is rejected", members: `[]`, wantErr: opsalerts.ErrValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			clock := &directoryClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
			cfg := opsalerts.Config{
				WorkspaceID: "T_DEMO", ChannelID: "CHY2E1BL4", WorkspaceURL: "https://demo.slack.com",
				Writer:  opsalerts.Identity{UserID: "U_DPTP", BotID: "B_DPTP"},
				Sources: []opsalerts.Source{{Name: "Prometheus", Kind: "alertmanager", UserIDs: []string{"U_PROM"}, BotIDs: []string{"B_PROM"}}},
			}
			fake := &slackHTTPFake{results: []slackHTTPResult{
				{body: `{"ok":true,"members":[{"id":"U_ONE","name":"first"}],"response_metadata":{"next_cursor":"next"}}`},
				{body: `{"ok":true,"members":` + tc.members + `}`},
			}}
			wait := &workspaceTestWait{}
			users := &workspaceUsers{client: fakeSlackClient(fake), lifetime: ctx, clock: clock, wait: wait.wait}
			directory := &slackDirectory{workspace: users}
			archive := &workspaceReplyArchive{
				demoArchive: &demoArchive{
					roots:   []slack.Message{demoMessage(clock.now, time.Minute, "U_PROM", "B_PROM", "TestAlert [ops_color=red ops_category=ci_platform]")},
					threads: map[string][]slack.Message{}, next: clock.now.Add(-time.Second), writer: cfg.Writer,
				},
				directory: &slackArchive{directory: directory},
			}
			classes, err := opsalerts.LoadClassification(demoClassification)
			if err != nil {
				t.Fatal(err)
			}
			service, err := opsalerts.NewService(cfg, classes, archive, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			req := opsalerts.ThreadReplyRequest{OccurrenceID: cfg.ChannelID + ":" + archive.roots[0].Timestamp, RequestID: "workspace-reply", Text: "Ask @alice"}
			view, err := service.ReplyToAlert(ctx, "operator@example.test", req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("unexpected reply result: got %v, want %v", err, tc.wantErr)
			}
			if len(fake.forms) != 2 || len(directory.cache.people) != 0 {
				t.Fatal("workspace resolution depended on participant profiles or repeated pagination")
			}
			if tc.wantErr != nil {
				if len(archive.threads) != 0 {
					t.Fatal("unresolved workspace user was posted")
				}
				return
			}
			if view.Confirmation == nil || len(view.Messages) != 1 || view.Messages[0].Text != "Ask <@U_NEW>" {
				t.Fatalf("uncached workspace user was not delivered as a mention: %#v", view)
			}
			stored := archive.threads[archive.roots[0].Timestamp][0]
			if !strings.HasPrefix(stored.Text, "Dashboard reply (on behalf of operator@example.test):\n\n") {
				t.Fatalf("operator attribution was lost: %q", stored.Text)
			}
			restarted, err := opsalerts.NewService(cfg, classes, archive, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			req.ReconcileOnly = true
			if _, err := restarted.ReplyToAlert(ctx, "operator@example.test", req); err != nil {
				t.Fatalf("original text fingerprint did not survive recovery: %v", err)
			}
			req.Text = "Ask <@U_NEW>"
			if _, err := restarted.ReplyToAlert(ctx, "operator@example.test", req); !errors.Is(err, opsalerts.ErrConflict) {
				t.Fatalf("rendered text replaced the original submission fingerprint: %v", err)
			}
			if len(fake.forms) != 2 || len(archive.threads[archive.roots[0].Timestamp]) != 1 {
				t.Fatal("recovery reloaded directory or posted a duplicate")
			}
		})
	}
}

// wait supplies deterministic successful and interrupted rate-limit waits.
func (w *workspaceTestWait) wait(ctx context.Context, delay time.Duration) error {
	w.delays = append(w.delays, delay)
	if err := ctx.Err(); err != nil {
		return err
	}
	return w.err
}

// TestWorkspaceUserPages checks complete pagination, ambiguity, failure atomicity, and Retry-After.
func TestWorkspaceUserPages(t *testing.T) {
	for _, tc := range []struct {
		name      string
		results   []slackHTTPResult
		want      map[string]string
		cursors   []string
		delays    []time.Duration
		waitErr   error
		wantError string
	}{
		{
			name: "uncached user on later page",
			results: []slackHTTPResult{
				{body: `{"ok":true,"members":[{"id":"U_ONE","name":"first"}],"response_metadata":{"next_cursor":"next"}}`},
				{body: `{"ok":true,"members":[{"id":"U_NEW","name":"Alice.Smith"}]}`},
			},
			want: map[string]string{"first": "U_ONE", "alice.smith": "U_NEW"}, cursors: []string{"", "next"}, delays: []time.Duration{6 * time.Second},
		},
		{
			name: "empty intermediate page continues",
			results: []slackHTTPResult{
				{body: `{"ok":true,"members":[],"response_metadata":{"next_cursor":"next"}}`},
				{body: `{"ok":true,"members":[{"id":"W_NEW","name":"guest"}]}`},
			},
			want: map[string]string{"guest": "W_NEW"}, cursors: []string{"", "next"}, delays: []time.Duration{6 * time.Second},
		},
		{
			name:    "only active unique authoritative handles",
			results: []slackHTTPResult{{body: `{"ok":true,"members":[{"id":"U_ONE","name":"same"},{"id":"U_TWO","name":"SAME"},{"id":"U_ONE","name":"same"},{"id":"U_DEAD","name":"deleted","deleted":true},{"id":"U_BOT","name":"helper-bot","is_bot":true},{"id":"U_THREE","name":"handle","profile":{"display_name":"display"}},{"name":"no-id"},{"id":"U_NO_NAME"}]}`}},
			want:    map[string]string{"same": "", "helper-bot": "U_BOT", "handle": "U_THREE"}, cursors: []string{""},
		},
		{
			name: "later page failure discards partial index",
			results: []slackHTTPResult{
				{body: `{"ok":true,"members":[{"id":"U_ONE","name":"first"}],"response_metadata":{"next_cursor":"next"}}`},
				{body: `{"ok":false,"error":"missing_scope"}`},
			},
			wantError: "listing workspace users: missing_scope", cursors: []string{"", "next"}, delays: []time.Duration{6 * time.Second},
		},
		{
			name: "repeated cursor fails closed",
			results: []slackHTTPResult{
				{body: `{"ok":true,"response_metadata":{"next_cursor":"same"}}`},
				{body: `{"ok":true,"response_metadata":{"next_cursor":"same"}}`},
			},
			wantError: "workspace user directory repeated a cursor", cursors: []string{"", "same"}, delays: []time.Duration{6 * time.Second},
		},
		{
			name: "rate limited page resumes same cursor",
			results: []slackHTTPResult{
				{body: `{"ok":true,"response_metadata":{"next_cursor":"next"}}`},
				{status: http.StatusTooManyRequests},
				{body: `{"ok":true,"members":[{"id":"U_NEW","name":"new"}]}`},
			},
			want: map[string]string{"new": "U_NEW"}, cursors: []string{"", "next", "next"}, delays: []time.Duration{6 * time.Second, time.Hour},
		},
		{
			name:    "rate limit wait cancellation",
			results: []slackHTTPResult{{status: http.StatusTooManyRequests}},
			waitErr: context.Canceled, wantError: "context canceled", cursors: []string{""}, delays: []time.Duration{time.Hour},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: tc.results}
			wait := &workspaceTestWait{err: tc.waitErr}
			users := &workspaceUsers{client: fakeSlackClient(fake), wait: wait.wait}
			got, _, err := users.readPages(context.Background())
			message := ""
			if err != nil {
				message = err.Error()
			}
			if diff := cmp.Diff(tc.wantError, message); diff != "" {
				t.Errorf("unexpected error (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected handles (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.delays, wait.delays); diff != "" {
				t.Errorf("unexpected pacing (-want +got):\n%s", diff)
			}
			var cursors []string
			for i, form := range fake.forms {
				cursors = append(cursors, form.Get("cursor"))
				if fake.paths[i] != "/api/users.list" || form.Get("limit") != "200" {
					t.Errorf("unexpected users.list request: %s %v", fake.paths[i], form)
				}
			}
			if diff := cmp.Diff(tc.cursors, cursors); diff != "" {
				t.Errorf("unexpected cursors (-want +got):\n%s", diff)
			}
		})
	}
}

// TestWorkspaceUserBounds rejects oversized or endlessly advancing directories without publishing partial users.
func TestWorkspaceUserBounds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pages     int
		members   int
		wantError string
	}{
		{name: "page budget", pages: 1000, members: 1, wantError: "workspace user directory exceeds page limit"},
		{name: "user budget", pages: 1, members: 200001, wantError: "workspace user directory exceeds safety limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members := strings.TrimSuffix(strings.Repeat(`{"id":"U_ONE","name":"one"},`, tc.members), ",")
			fake := &slackHTTPFake{}
			for page := range tc.pages {
				fake.results = append(fake.results, slackHTTPResult{body: fmt.Sprintf(`{"ok":true,"members":[%s],"response_metadata":{"next_cursor":"page-%d"}}`, members, page)})
			}
			wait := &workspaceTestWait{}
			users := &workspaceUsers{client: fakeSlackClient(fake), wait: wait.wait}
			handles, _, err := users.readPages(context.Background())
			if err == nil || err.Error() != tc.wantError {
				t.Fatalf("unexpected bound error: got %v, want %s", err, tc.wantError)
			}
			if handles != nil || len(fake.forms) != tc.pages {
				t.Fatal("exceeded the request budget or returned a partial directory")
			}
		})
	}
}

// TestWorkspaceUserCache checks cache hits/misses, TTL boundaries, and shared negative caching.
func TestWorkspaceUserCache(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure bool
		rate    bool
	}{
		{name: "positive directory TTL"},
		{name: "missing scope cooldown", failure: true},
		{name: "long Retry-After survives cancellation", failure: true, rate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := slackHTTPResult{body: `{"ok":true,"members":[{"id":"U_ONE","name":"old"}]}`}
			ttl := 24 * time.Hour
			if tc.failure {
				first = slackHTTPResult{body: `{"ok":false,"error":"missing_scope"}`}
				ttl = 15 * time.Minute
			}
			wait := &workspaceTestWait{}
			if tc.rate {
				first = slackHTTPResult{status: http.StatusTooManyRequests}
				wait.err = context.Canceled
				ttl = time.Hour
			}
			fake := &slackHTTPFake{results: []slackHTTPResult{first, {body: `{"ok":true,"members":[{"id":"U_NEW","name":"new"}]}`}}}
			clock := &directoryClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
			users := &workspaceUsers{client: fakeSlackClient(fake), clock: clock, lifetime: context.Background(), wait: wait.wait}
			err := users.prepare(context.Background())
			if (err != nil) != tc.failure {
				t.Fatalf("unexpected initial load result: %v", err)
			}
			clock.now = clock.now.Add(ttl - time.Nanosecond)
			for _, handle := range []string{"old", "unknown", "another"} {
				_, _ = users.resolve(handle)
				err = users.prepare(context.Background())
				if (err != nil) != tc.failure {
					t.Fatalf("unexpected cached load result: %v", err)
				}
			}
			if len(fake.forms) != 1 {
				t.Fatal("fresh directory or shared cooldown repeated users.list")
			}
			clock.now = clock.now.Add(time.Nanosecond)
			if _, found := users.resolve("old"); found {
				t.Fatal("expired directory resolved a stale user")
			}
			if err := users.prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			if id, found := users.resolve("NEW"); !found || id != "U_NEW" {
				t.Fatalf("refreshed directory did not resolve user: %q %v", id, found)
			}
			if _, found := users.resolve("old"); found || len(fake.forms) != 2 {
				t.Fatal("refresh retained stale handles or repeated the load")
			}
		})
	}
}

// prepareWorkspaceAsync exercises cancellation and coalescing without test closures.
func prepareWorkspaceAsync(ctx context.Context, users *workspaceUsers, done chan<- error) {
	done <- users.prepare(ctx)
}

// TestWorkspaceUserCoalescing checks that browser cancellation never aborts or duplicates a shared refresh.
func TestWorkspaceUserCoalescing(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cancelCaller bool
		cancelServer bool
	}{
		{name: "concurrent sends coalesce"},
		{name: "cancelled browser leaves load alive", cancelCaller: true},
		{name: "server shutdown cancels network load", cancelServer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lifetime, stop := context.WithCancel(context.Background())
			defer stop()
			release, entered := make(chan struct{}), make(chan struct{})
			fake := &slackHTTPFake{results: []slackHTTPResult{{body: `{"ok":true,"members":[{"id":"U_NEW","name":"new"}]}`}}}
			transport := &directoryHTTPFake{fake: fake, entered: entered, release: release}
			users := &workspaceUsers{client: slack.New("unit-test-placeholder", slack.OptionHTTPClient(transport)), lifetime: lifetime}
			caller, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 10)
			go prepareWorkspaceAsync(caller, users, done)
			<-entered
			if _, found := users.resolve("new"); found {
				t.Fatal("incomplete directory was published")
			}
			if tc.cancelCaller {
				cancel()
				err := <-done
				var lookupError *opsalerts.ThreadReferenceLookupError
				if !errors.As(err, &lookupError) || !errors.Is(err, opsalerts.ErrNotReady) {
					t.Fatalf("cancelled caller did not get a definite rejection: %v", err)
				}
			}
			for range 9 {
				observed := make(chan struct{})
				ctx := &directoryWaitContext{Context: context.Background(), observed: observed}
				go prepareWorkspaceAsync(ctx, users, done)
				<-observed
			}
			if transport.count() != 1 {
				t.Fatal("concurrent callers started multiple directory scans")
			}
			if tc.cancelServer {
				stop()
			}
			if !tc.cancelServer {
				close(release)
			}
			count := 10
			if tc.cancelCaller {
				count = 9
			}
			for range count {
				err := <-done
				if (err != nil) != tc.cancelServer {
					t.Errorf("unexpected coalesced load result: %v", err)
				}
			}
			if tc.cancelServer {
				close(release)
			}
			_, found := users.resolve("new")
			if found == tc.cancelServer || transport.count() != 1 {
				t.Fatal("shared result was lost or a cancelled server load was published")
			}
		})
	}
}
