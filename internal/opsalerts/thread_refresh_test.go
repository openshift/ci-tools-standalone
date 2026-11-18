package opsalerts

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/slack-go/slack"
)

// refreshOutcome captures one asynchronous refresh result for channel-controlled tests.
type refreshOutcome struct {
	thread AlertThread
	err    error
}

// callRefresh runs one refresh and reports its outcome without capturing mutable state.
func callRefresh(ctx context.Context, s *Service, occurrenceID string, out chan<- refreshOutcome) {
	thread, err := s.RefreshAlertThread(ctx, occurrenceID)
	out <- refreshOutcome{thread: thread, err: err}
}

// gatedArchive pauses archive reads so concurrent callers serialize behind one in-flight read.
// It never touches the shared clock, whose plain reads by other callers are unsynchronized.
type gatedArchive struct {
	*memoryArchive
	entered chan struct{}
	release chan struct{}
}

// Replies implements Archive, blocking each read until the test releases it.
func (a *gatedArchive) Replies(ctx context.Context, channel, root string) ([]slack.Message, error) {
	a.entered <- struct{}{}
	<-a.release
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.replyCalls++
	return append([]slack.Message(nil), a.replies[root]...), a.repliesError
}

// threadReplyTexts flattens one page for compact assertions.
func threadReplyTexts(view AlertThread) []string {
	texts := make([]string, 0, len(view.Messages))
	for _, message := range view.Messages {
		texts = append(texts, message.Text)
	}
	return texts
}

// TestRefreshAlertThreadTargetedRead verifies refreshes bypass unchanged parent metadata.
func TestRefreshAlertThreadTargetedRead(t *testing.T) {
	cases := []struct {
		name        string
		mutation    string
		wantTexts   []string
		wantTotal   int
		replyEdited bool
		rootEdited  bool
	}{
		{name: "new reply with unchanged parent metadata", mutation: "reply", wantTexts: []string{"original reply", "late reply"}, wantTotal: 2},
		{name: "edited reply observed", mutation: "edit", wantTexts: []string{"edited reply"}, wantTotal: 1, replyEdited: true},
		{name: "edited root observed through replies", mutation: "root", wantTexts: []string{"original reply"}, wantTotal: 1, rootEdited: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := fixture(t)
			root := archive.roots[0].Timestamp
			id := "C_TEST:" + root
			replyTS := fmt.Sprintf("%d.000100", clock.now.Unix())
			archive.replies[root] = []slack.Message{{Msg: slack.Msg{Timestamp: replyTS, ThreadTimestamp: root, User: "U_HUMAN", Text: "original reply"}}}
			archive.roots[0].ReplyCount = 1
			archive.roots[0].LatestReply = replyTS
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			switch tc.mutation {
			case "reply":
				// Slack holds a new reply while the parent history metadata stays stale.
				archive.replies[root] = append(archive.replies[root], slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000200", clock.now.Unix()), ThreadTimestamp: root, User: "U_HUMAN", Text: "late reply"}})
			case "edit":
				stored := &archive.replies[root][0]
				stored.Text = "edited reply"
				stored.Edited = &slack.Edited{Timestamp: fmt.Sprintf("%d.000150", clock.now.Unix())}
			case "root":
				edited := slack.Message{Msg: slack.Msg{Timestamp: root, ThreadTimestamp: root, BotID: "B_ALERTS", Text: "Alert: etcdDown", ReplyCount: 1, LatestReply: replyTS, Edited: &slack.Edited{Timestamp: fmt.Sprintf("%d.000150", clock.now.Unix())}}}
				archive.replies[root] = append(archive.replies[root], edited)
			}
			if tc.mutation == "reply" {
				// A complete sync alone still misses the reply: that delay is the bug being fixed.
				clock.now = clock.now.Add(time.Second)
				if err := service.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				stale, err := service.AlertThread(id, "")
				if err != nil {
					t.Fatal(err)
				}
				if slices.Contains(threadReplyTexts(stale), "late reply") {
					t.Fatal("cache-only sync observed the reply without a targeted read")
				}
			}
			// The full-sync read must age out before a targeted read is due.
			clock.now = clock.now.Add(6 * time.Second)
			calls := archive.replyCalls
			view, err := service.RefreshAlertThread(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if archive.replyCalls != calls+1 {
				t.Fatalf("refresh reads %d want %d", archive.replyCalls-calls, 1)
			}
			if diff := cmp.Diff(tc.wantTexts, threadReplyTexts(view)); diff != "" {
				t.Errorf("unexpected replies (-want +got):\n%s", diff)
			}
			if view.TotalReplies != tc.wantTotal {
				t.Errorf("total replies %d want %d", view.TotalReplies, tc.wantTotal)
			}
			if !view.FetchedAt.Equal(clock.now) {
				t.Errorf("fetched at %s want %s", view.FetchedAt, clock.now)
			}
			if view.Root.Edited != tc.rootEdited {
				t.Errorf("root edited %v want %v", view.Root.Edited, tc.rootEdited)
			}
			if len(view.Messages) > 0 && view.Messages[len(view.Messages)-1].Edited != tc.replyEdited {
				t.Errorf("reply edited %v want %v", view.Messages[len(view.Messages)-1].Edited, tc.replyEdited)
			}
			cached, err := service.AlertThread(id, "")
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.wantTexts, threadReplyTexts(cached)); diff != "" {
				t.Errorf("unexpected cached replies (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRefreshAlertThreadNoTargetIO covers guards that must reject before any Slack read.
func TestRefreshAlertThreadNoTargetIO(t *testing.T) {
	cases := []struct {
		name     string
		mutation string
		wantErr  error
	}{
		{name: "missing occurrence", mutation: "missing", wantErr: ErrNotFound},
		{name: "expired occurrence", mutation: "expired", wantErr: ErrNotFound},
		{name: "coverage not ready", mutation: "unsynced", wantErr: ErrNotReady},
		{name: "retained thread missing", mutation: "threadless", wantErr: ErrNotReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := fixture(t)
			root := archive.roots[0].Timestamp
			target := "C_TEST:" + root
			switch tc.mutation {
			case "missing":
				if err := service.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				target = "C_TEST:1799999999.000001"
			case "expired":
				if err := service.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				clock.now = clock.now.Add(49 * time.Hour)
			case "threadless":
				if err := service.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				cache := service.currentCache()
				cache.Threads = maps.Clone(cache.Threads)
				delete(cache.Threads, root)
				service.publish(cache)
			}
			calls := archive.replyCalls
			_, err := service.RefreshAlertThread(ctx, target)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v want %v", err, tc.wantErr)
			}
			if archive.replyCalls != calls {
				t.Fatal("rejected refresh performed Slack I/O")
			}
		})
	}
}

// TestAlertThreadFetchedAtAccuracy keeps per-root fetch time independent of full coverage.
func TestAlertThreadFetchedAtAccuracy(t *testing.T) {
	for _, tc := range []struct{ name string }{{name: "one alert thread"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := fixture(t)
			root := archive.roots[0].Timestamp
			id := "C_TEST:" + root
			synced := clock.now
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			view, err := service.AlertThread(id, "")
			if err != nil {
				t.Fatal(err)
			}
			if !view.FetchedAt.Equal(synced) {
				t.Fatalf("initial fetch %s want %s", view.FetchedAt, synced)
			}
			// Unchanged parent metadata keeps the old time while coverage advances.
			clock.now = clock.now.Add(time.Second)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if !service.Snapshot().Status.LastSync.Equal(clock.now) {
				t.Fatal("full sync did not advance coverage")
			}
			view, err = service.AlertThread(id, "")
			if err != nil {
				t.Fatal(err)
			}
			if !view.FetchedAt.Equal(synced) {
				t.Fatalf("unchanged root fetch %s want %s", view.FetchedAt, synced)
			}
			// Changed parent metadata makes the next sync actually read replies again.
			lateTS := fmt.Sprintf("%d.000200", clock.now.Unix())
			archive.replies[root] = append(archive.replies[root], slack.Message{Msg: slack.Msg{Timestamp: lateTS, ThreadTimestamp: root, User: "U_HUMAN", Text: "late reply"}})
			archive.roots[0].ReplyCount++
			archive.roots[0].LatestReply = lateTS
			clock.now = clock.now.Add(time.Second)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			view, err = service.AlertThread(id, "")
			if err != nil {
				t.Fatal(err)
			}
			if !view.FetchedAt.Equal(clock.now) || !slices.Contains(threadReplyTexts(view), "late reply") {
				t.Fatalf("changed root not reread: %#v", view)
			}
			read := clock.now
			// A failed targeted refresh keeps the last complete content and timestamp.
			archive.repliesError = errors.New("unavailable")
			clock.now = clock.now.Add(6 * time.Second)
			if _, err := service.RefreshAlertThread(ctx, id); !errors.Is(err, ErrNotReady) {
				t.Fatalf("read failure %v", err)
			}
			view, err = service.AlertThread(id, "")
			if err != nil {
				t.Fatal(err)
			}
			if !view.FetchedAt.Equal(read) {
				t.Fatalf("failed refresh moved fetch time to %s want %s", view.FetchedAt, read)
			}
			// A successful refresh stamps the actual targeted read time.
			archive.repliesError = nil
			clock.now = clock.now.Add(11 * time.Second)
			view, err = service.RefreshAlertThread(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !view.FetchedAt.Equal(clock.now) {
				t.Fatalf("refresh fetch %s want %s", view.FetchedAt, clock.now)
			}
		})
	}
}

// TestRefreshAlertThreadCoalescing shares one read across recent and concurrent callers.
func TestRefreshAlertThreadCoalescing(t *testing.T) {
	for _, tc := range []struct{ name string }{{name: "cooldown, budget and freshness"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := fixture(t)
			root := archive.roots[0].Timestamp
			id := "C_TEST:" + root
			other := "C_TEST:" + archive.roots[1].Timestamp
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			archive.replies[root] = append(archive.replies[root], slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000200", clock.now.Unix()), ThreadTimestamp: root, User: "U_HUMAN", Text: "late reply"}})
			// The full-sync read must age out before a targeted read is due.
			clock.now = clock.now.Add(6 * time.Second)
			calls := archive.replyCalls
			view, err := service.RefreshAlertThread(ctx, id)
			if err != nil || !slices.Contains(threadReplyTexts(view), "late reply") {
				t.Fatalf("initial refresh %#v err %v", view, err)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("initial refresh did not read exactly once")
			}
			fetched := view.FetchedAt
			// Several sequential viewers inside the reuse window all get the
			// latest cached page without another Slack read.
			for range 3 {
				clock.now = clock.now.Add(time.Second)
				view, err = service.RefreshAlertThread(ctx, id)
				if err != nil || !slices.Contains(threadReplyTexts(view), "late reply") {
					t.Fatalf("recent viewer %#v err %v", view, err)
				}
				if !view.FetchedAt.Equal(fetched) {
					t.Fatalf("recent viewer reread: fetched %s want %s", view.FetchedAt, fetched)
				}
				if archive.replyCalls != calls+1 {
					t.Fatal("recent viewer read Slack")
				}
			}
			// A stale root is throttled by the shared global budget.
			var refreshError *ThreadRefreshError
			_, err = service.RefreshAlertThread(ctx, other)
			if !errors.As(err, &refreshError) || !errors.Is(err, ErrNotReady) || refreshError.RetryAfter != 2*time.Second {
				t.Fatalf("cross-target budget error %#v", err)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("budgeted refresh read Slack")
			}
			// A fresh root bypasses the global cooldown because reuse needs no I/O.
			view, err = service.RefreshAlertThread(ctx, id)
			if err != nil || !slices.Contains(threadReplyTexts(view), "late reply") || !view.FetchedAt.Equal(fetched) {
				t.Fatalf("fresh-cache bypass %#v err %v", view, err)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("fresh-cache bypass read Slack")
			}
			// Once the reuse window closes the next viewer reads again.
			clock.now = clock.now.Add(3 * time.Second)
			view, err = service.RefreshAlertThread(ctx, id)
			if err != nil || !slices.Contains(threadReplyTexts(view), "late reply") {
				t.Fatalf("post-window refresh %#v err %v", view, err)
			}
			if archive.replyCalls != calls+2 || !view.FetchedAt.Equal(clock.now) {
				t.Fatalf("post-window refresh reads %d fetched %s", archive.replyCalls-calls, view.FetchedAt)
			}
		})
	}
}

// TestRefreshAlertThreadConcurrentCallers serializes viewers behind one Slack read.
func TestRefreshAlertThreadConcurrentCallers(t *testing.T) {
	for _, tc := range []struct{ name string }{{name: "two simultaneous viewers"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := fixture(t)
			root := archive.roots[0].Timestamp
			id := "C_TEST:" + root
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			archive.replies[root] = append(archive.replies[root], slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000200", clock.now.Unix()), ThreadTimestamp: root, User: "U_HUMAN", Text: "late reply"}})
			// A stale baseline forces the first viewer to perform a real read.
			clock.now = clock.now.Add(6 * time.Second)
			calls := archive.replyCalls
			gated := &gatedArchive{memoryArchive: archive, entered: make(chan struct{}), release: make(chan struct{})}
			service.archive = gated
			results := make(chan refreshOutcome, 2)
			// Holding the token queues both viewers before either can read.
			service.serial <- struct{}{}
			go callRefresh(ctx, service, id, results)
			go callRefresh(ctx, service, id, results)
			<-service.serial
			<-gated.entered
			// An unrelated canceled waiter returns promptly without disturbing
			// the in-flight live read.
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			_, err := service.RefreshAlertThread(canceled, id)
			if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrNotReady) {
				t.Fatalf("canceled waiter error %v", err)
			}
			if archive.replyCalls != calls {
				t.Fatal("canceled waiter performed Slack I/O")
			}
			gated.release <- struct{}{}
			for range 2 {
				outcome := <-results
				if outcome.err != nil {
					t.Fatalf("coalesced viewer error %v", outcome.err)
				}
				if !slices.Contains(threadReplyTexts(outcome.thread), "late reply") {
					t.Fatalf("coalesced viewer missed the reply: %#v", outcome.thread)
				}
			}
			if archive.replyCalls != calls+1 {
				t.Fatalf("concurrent viewers caused %d reads", archive.replyCalls-calls)
			}
			cached, err := service.AlertThread(id, "")
			if err != nil || !slices.Contains(threadReplyTexts(cached), "late reply") {
				t.Fatalf("published cache %#v err %v", cached, err)
			}
		})
	}
}

// TestThreadPostReadbackKeepsReadTime ensures one confirmed post never claims
// a complete-thread read, so a refresh after posting still observes external
// replies that arrived since the last full read.
func TestThreadPostReadbackKeepsReadTime(t *testing.T) {
	cases := []struct {
		name   string
		linked bool
	}{
		{name: "plain reply on alert thread", linked: true},
		{name: "standalone conversation post"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			root := archive.roots[0].Timestamp
			id := "C_TEST:" + root
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			read := service.currentCache().Threads[root].FetchedAt
			// Age the complete read out of the reuse window.
			clock.now = clock.now.Add(6 * time.Second)
			if !tc.linked {
				conversation, err := service.Converse(ctx, "operator", ConversationRequest{RequestID: "post-readback", Text: "Standalone question?"})
				if err != nil {
					t.Fatal(err)
				}
				created := strings.TrimPrefix(conversation.ID, "C_TEST:")
				if fetched := service.currentCache().Threads[created].FetchedAt; !fetched.IsZero() {
					t.Fatalf("standalone post claimed a complete read at %s", fetched)
				}
				return
			}
			// Slack receives an external reply without any parent metadata change.
			archive.replies[root] = append(archive.replies[root], slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000300", clock.now.Unix()), ThreadTimestamp: root, User: "U_HUMAN", Text: "external reply"}})
			if _, err := service.ReplyToAlert(ctx, "operator", ThreadReplyRequest{OccurrenceID: id, RequestID: "post-readback", Text: "dashboard post"}); err != nil {
				t.Fatal(err)
			}
			if fetched := service.currentCache().Threads[root].FetchedAt; !fetched.Equal(read) {
				t.Fatalf("post advanced fetch time to %s want %s", fetched, read)
			}
			cached, err := service.AlertThread(id, "")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(threadReplyTexts(cached), "dashboard post") || slices.Contains(threadReplyTexts(cached), "external reply") {
				t.Fatalf("unexpected cached page after post: %v", threadReplyTexts(cached))
			}
			// The immediate refresh must perform a real read, not reuse the
			// single-message confirmation as a fresh complete thread.
			calls := archive.replyCalls
			view, err := service.RefreshAlertThread(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("recent post suppressed the targeted read")
			}
			if !slices.Contains(threadReplyTexts(view), "external reply") || !slices.Contains(threadReplyTexts(view), "dashboard post") {
				t.Fatalf("refresh missed replies: %v", threadReplyTexts(view))
			}
			if !view.FetchedAt.Equal(clock.now) {
				t.Fatalf("refresh fetch %s want %s", view.FetchedAt, clock.now)
			}
		})
	}
}

// TestRefreshAlertThreadFailures memoizes read errors without losing prior content.
func TestRefreshAlertThreadFailures(t *testing.T) {
	cases := []struct {
		name             string
		readError        error
		cancelDuringRead bool
		cooldown         time.Duration
	}{
		{name: "ordinary failure", readError: errors.New("unavailable"), cooldown: threadReadFailureCooldown},
		{name: "rate limited", readError: &slack.RateLimitedError{RetryAfter: 30 * time.Second}, cooldown: 30 * time.Second},
		// Production shape: a GET deadline interrupts a long Retry-After wait.
		{name: "rate limit during canceled request", readError: errors.Join(context.DeadlineExceeded, &slack.RateLimitedError{RetryAfter: 30 * time.Second}), cancelDuringRead: true, cooldown: 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			root := archive.roots[0].Timestamp
			id := "C_TEST:" + root
			synced := clock.now
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			archive.replies[root] = append(archive.replies[root], slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000200", clock.now.Unix()), ThreadTimestamp: root, User: "U_HUMAN", Text: "late reply"}})
			archive.repliesError = tc.readError
			requestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if tc.cancelDuringRead {
				archive.cancelOnReply = cancel
			}
			// The full-sync read must age out before a targeted read is due.
			clock.now = clock.now.Add(6 * time.Second)
			calls := archive.replyCalls
			_, err := service.RefreshAlertThread(requestCtx, id)
			if !errors.Is(err, ErrNotReady) || !errors.Is(err, tc.readError) || !strings.Contains(err.Error(), "refreshing alert thread") {
				t.Fatalf("read failure %v", err)
			}
			// The first failure already carries the typed retry hint.
			var refreshError *ThreadRefreshError
			if !errors.As(err, &refreshError) || refreshError.RetryAfter != tc.cooldown {
				t.Fatalf("first failure retry hint %#v want %s", err, tc.cooldown)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("failed refresh did not attempt exactly one read")
			}
			archive.cancelOnReply = nil
			view, viewErr := service.AlertThread(id, "")
			if viewErr != nil || len(view.Messages) != 0 || !view.FetchedAt.Equal(synced) {
				t.Fatalf("failed refresh changed retained content: %#v err %v", view, viewErr)
			}
			// The memoized cooldown throttles repeats without more Slack reads.
			clock.now = clock.now.Add(time.Second)
			_, err = service.RefreshAlertThread(ctx, id)
			if !errors.As(err, &refreshError) || !errors.Is(err, ErrNotReady) || refreshError.RetryAfter != tc.cooldown-1*time.Second {
				t.Fatalf("cooldown error %#v want retry after %s", err, tc.cooldown-1*time.Second)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("cooldown did not stop repeated reads")
			}
			// A confirmed post does not clear or satisfy the failure cooldown.
			// The chat archive ticks the clock one millisecond per post, so the
			// write starts just before a second boundary to keep exact timings.
			clock.now = clock.now.Add(time.Second - time.Millisecond)
			if _, err := service.ReplyToAlert(ctx, "operator", ThreadReplyRequest{OccurrenceID: id, RequestID: "after-failure", Text: "written after failure"}); err != nil {
				t.Fatal(err)
			}
			clock.now = clock.now.Add(time.Second)
			_, err = service.RefreshAlertThread(ctx, id)
			if !errors.As(err, &refreshError) || refreshError.RetryAfter != tc.cooldown-3*time.Second {
				t.Fatalf("fresh write bypassed failure cooldown: %#v", err)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("failure cooldown let a read through")
			}
			// Once the cooldown passes the refresh recovers the observed replies.
			archive.repliesError = nil
			clock.now = clock.now.Add(tc.cooldown - 3*time.Second + time.Second)
			view, err = service.RefreshAlertThread(ctx, id)
			if err != nil || !slices.Contains(threadReplyTexts(view), "late reply") {
				t.Fatalf("recovered refresh %#v err %v", view, err)
			}
			if archive.replyCalls != calls+2 {
				t.Fatal("recovered refresh did not read")
			}
		})
	}
}

// TestRefreshAlertThreadDefersChaiPolling shares one read between viewers and background polling.
func TestRefreshAlertThreadDefersChaiPolling(t *testing.T) {
	for _, tc := range []struct{ name string }{{name: "targeted read defers active thread"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			root := archive.roots[0].Timestamp
			id := "C_TEST:" + root
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			conversation, err := service.Converse(ctx, "operator", ConversationRequest{OccurrenceID: id, RequestID: "linked", Text: "Question?"})
			if err != nil {
				t.Fatal(err)
			}
			if !service.TouchConversation(conversation.ID) {
				t.Fatal("touch rejected retained chat")
			}
			archive.replies[root] = append(archive.replies[root], slack.Message{Msg: slack.Msg{Timestamp: fmt.Sprintf("%d.000200", clock.now.Unix()), ThreadTimestamp: root, User: "U_HUMAN", Text: "late reply"}})
			// The full-sync and write readbacks must age out before a targeted read is due.
			clock.now = clock.now.Add(6 * time.Second)
			view, err := service.RefreshAlertThread(ctx, id)
			if err != nil || !slices.Contains(threadReplyTexts(view), "late reply") {
				t.Fatalf("refresh %#v err %v", view, err)
			}
			calls := archive.replyCalls
			// The claimed budget pauses Chai polling immediately after.
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			if archive.replyCalls != calls {
				t.Fatal("Chai poll bypassed the shared budget")
			}
			// The deferred per-thread interest outlives the global budget.
			clock.now = clock.now.Add(6 * time.Second)
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			if archive.replyCalls != calls {
				t.Fatal("Chai poll ignored deferred interest")
			}
			clock.now = clock.now.Add(5 * time.Second)
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("Chai poll never resumed")
			}
		})
	}
}

// TestRefreshAlertThreadRateLimitSharedWithChai pauses both paths after one rate limit.
func TestRefreshAlertThreadRateLimitSharedWithChai(t *testing.T) {
	for _, tc := range []struct{ name string }{{name: "shared rate-limit state"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			root := archive.roots[0].Timestamp
			id := "C_TEST:" + root
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			conversation, err := service.Converse(ctx, "operator", ConversationRequest{OccurrenceID: id, RequestID: "linked", Text: "Question?"})
			if err != nil {
				t.Fatal(err)
			}
			if !service.TouchConversation(conversation.ID) {
				t.Fatal("touch rejected retained chat")
			}
			// Production shape: the request context dies while Slack reports a
			// long Retry-After, so the archive error joins both causes.
			requestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			archive.cancelOnReply = cancel
			archive.repliesError = errors.Join(context.DeadlineExceeded, &slack.RateLimitedError{RetryAfter: 30 * time.Second})
			clock.now = clock.now.Add(6 * time.Second)
			_, err = service.RefreshAlertThread(requestCtx, id)
			if !errors.Is(err, ErrNotReady) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("rate-limited refresh %v", err)
			}
			var refreshError *ThreadRefreshError
			if !errors.As(err, &refreshError) || refreshError.RetryAfter != 30*time.Second {
				t.Fatalf("rate-limited retry hint %#v", err)
			}
			archive.cancelOnReply = nil
			archive.repliesError = nil
			calls := archive.replyCalls
			// The active thread stays eligible, but the shared budget holds it back.
			clock.now = clock.now.Add(11 * time.Second)
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			if archive.replyCalls != calls {
				t.Fatal("Chai poll ignored the shared rate limit")
			}
			service.TouchConversation(conversation.ID)
			clock.now = clock.now.Add(20 * time.Second)
			if err := service.SyncConversation(ctx); err != nil {
				t.Fatal(err)
			}
			if archive.replyCalls != calls+1 {
				t.Fatal("Chai poll never resumed after the rate limit")
			}
		})
	}
}

// TestRefreshAlertThreadCancellation keeps canceled callers from reading or penalizing peers.
func TestRefreshAlertThreadCancellation(t *testing.T) {
	cases := []struct{ name, when string }{
		{name: "already canceled", when: "before"},
		{name: "canceled with queue occupied", when: "queued"},
		{name: "canceled during read", when: "read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := fixture(t)
			root := archive.roots[0].Timestamp
			id := "C_TEST:" + root
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			// The full-sync read must age out before a targeted read is due.
			clock.now = clock.now.Add(6 * time.Second)
			canceled, cancel := context.WithCancel(ctx)
			defer cancel()
			switch tc.when {
			case "before":
				cancel()
			case "queued":
				service.serial <- struct{}{}
				defer service.release()
				cancel()
			case "read":
				archive.cancelOnReply = cancel
				archive.repliesError = context.Canceled
			}
			calls := archive.replyCalls
			_, err := service.RefreshAlertThread(canceled, id)
			if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrNotReady) {
				t.Fatalf("cancellation error %v", err)
			}
			var refreshError *ThreadRefreshError
			if errors.As(err, &refreshError) {
				t.Fatal("cancellation surfaced as a throttle")
			}
			if tc.when == "read" {
				if archive.replyCalls != calls+1 {
					t.Fatal("canceled read was not attempted")
				}
				// A canceled waiter must not memoize a failure cooldown for others.
				archive.cancelOnReply = nil
				archive.repliesError = nil
				clock.now = clock.now.Add(6 * time.Second)
				view, err := service.RefreshAlertThread(ctx, id)
				if err != nil {
					t.Fatalf("cancellation memoized a failure cooldown: %v", err)
				}
				if archive.replyCalls != calls+2 {
					t.Fatal("follow-up refresh did not read")
				}
				if view.TotalReplies != 0 {
					t.Fatalf("unexpected replies %#v", view)
				}
				return
			}
			if archive.replyCalls != calls {
				t.Fatal("canceled refresh performed Slack I/O")
			}
		})
	}
}

// TestRefreshAlertThreadPreservesState keeps coverage, workflow, decorations and pending guards.
func TestRefreshAlertThreadPreservesState(t *testing.T) {
	for _, tc := range []struct{ name string }{{name: "read-only refresh"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			root := archive.roots[0].Timestamp
			id := "C_TEST:" + root
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			synced := service.Snapshot().Status.LastSync
			o := findOccurrence(t, service, id)
			if _, err := service.Act(ctx, "operator", ActionRequest{OccurrenceID: id, ExpectedRevision: o.Identity.Revision, RequestID: "preserve-ack", Kind: "acknowledge"}); err != nil {
				t.Fatal(err)
			}
			archive.postError = errors.New("lost acknowledgement")
			if _, err := service.Converse(ctx, "operator", ConversationRequest{OccurrenceID: id, RequestID: "preserve-pending", Text: "Pending question?"}); !errors.Is(err, ErrDeliveryUncertain) {
				t.Fatalf("pending delivery %v", err)
			}
			archive.postError = nil
			posts, reactions := archive.posts, len(archive.reactions)
			decorations := service.currentCache().Decorations
			acknowledged := findOccurrence(t, service, id).State
			archive.repliesError = errors.New("unavailable")
			clock.now = clock.now.Add(6 * time.Second)
			if _, err := service.RefreshAlertThread(ctx, id); !errors.Is(err, ErrNotReady) {
				t.Fatalf("read failure %v", err)
			}
			if service.Snapshot().Status.Phase != "ready" {
				t.Fatal("read failure degraded workflow coverage")
			}
			archive.repliesError = nil
			clock.now = clock.now.Add(11 * time.Second)
			view, err := service.RefreshAlertThread(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if view.TotalReplies != 1 {
				t.Fatalf("unexpected replies %#v", view)
			}
			snapshot := service.Snapshot()
			if snapshot.Status.Phase != "ready" || !snapshot.Status.LastSync.Equal(synced) {
				t.Fatalf("refresh moved coverage: %+v", snapshot.Status)
			}
			if archive.posts != posts || len(archive.reactions) != reactions {
				t.Fatalf("refresh wrote to Slack: posts %d reactions %d", archive.posts, len(archive.reactions))
			}
			if diff := cmp.Diff(decorations, service.currentCache().Decorations); diff != "" {
				t.Errorf("unexpected decoration change (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(acknowledged, findOccurrence(t, service, id).State); diff != "" {
				t.Errorf("unexpected workflow change (-want +got):\n%s", diff)
			}
			if _, err := service.Converse(ctx, "operator", ConversationRequest{OccurrenceID: id, RequestID: "preserve-pending", Text: "Pending question?"}); !errors.Is(err, ErrDeliveryUncertain) {
				t.Fatalf("pending guard lost: %v", err)
			}
			if archive.posts != posts {
				t.Fatal("refreshed pending guard reposted")
			}
		})
	}
}

// TestThreadRefreshErrorSanitization keeps the exported throttle safe for transport layers.
func TestThreadRefreshErrorSanitization(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantThrottle bool
	}{
		{name: "throttle satisfies not ready", err: &ThreadRefreshError{RetryAfter: 4 * time.Second}, wantThrottle: true},
		{name: "wrapped generic failure", err: errors.Join(ErrNotReady, fmt.Errorf("refreshing alert thread: %w", errors.New("token rejected")))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, ErrNotReady) {
				t.Fatalf("error %v is not sanitized", tc.err)
			}
			var refreshError *ThreadRefreshError
			if errors.As(tc.err, &refreshError) != tc.wantThrottle {
				t.Fatalf("throttle detection mismatch for %v", tc.err)
			}
			if tc.wantThrottle && refreshError.RetryAfter != 4*time.Second {
				t.Fatalf("retry after %s", refreshError.RetryAfter)
			}
			if tc.wantThrottle && strings.Contains(refreshError.Error(), "token") {
				t.Fatalf("throttle exposed content: %s", refreshError.Error())
			}
		})
	}
}
