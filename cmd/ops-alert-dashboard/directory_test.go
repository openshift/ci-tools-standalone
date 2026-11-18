package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
)

// directoryClock supplies deterministic expiry and Retry-After boundaries.
type directoryClock struct{ now time.Time }

// Now implements the optional metadata clock.
func (c *directoryClock) Now() time.Time { return c.now }

// directoryFixture is one detached browser page for deterministic metadata tests.
func directoryFixture() opsalerts.AlertThread {
	return opsalerts.AlertThread{Root: opsalerts.ThreadMessage{Text: "Hello <@U_ONE> :custom:", Author: opsalerts.ThreadPerson{ID: "U_ONE", Name: "U_ONE"}}, People: map[string]opsalerts.ThreadPerson{"U_ONE": {ID: "U_ONE", Name: "U_ONE"}}, Emojis: map[string]string{}, Warnings: []string{}}
}

// TestSlackDirectoryCache checks singular profile APIs, bounded refresh and negative caching.
func TestSlackDirectoryCache(t *testing.T) {
	cases := []struct {
		name     string
		results  []slackHTTPResult
		wantName string
		warning  bool
		expire   bool
	}{
		{name: "success", results: []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_ONE","profile":{"display_name":"Ada","image_72":"https://avatars.slack-edge.com/ada.png"}}}`}, {body: `{"ok":true,"emoji":{"custom":"alias:target","target":"https://emoji.slack-edge.com/ok.png","unsafe":"https://evil.example/a.png"}}`}}, wantName: "Ada"},
		{name: "missing scopes cached", results: []slackHTTPResult{{body: `{"ok":false,"error":"missing_scope"}`}, {body: `{"ok":false,"error":"missing_scope"}`}}, wantName: "U_ONE", warning: true},
		{name: "wrong identity stays unresolved", results: []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_OTHER","profile":{"display_name":"Wrong identity"}}}`}, {body: `{"ok":true,"emoji":{}}`}}, wantName: "U_ONE", warning: true},
		{name: "expiry retries once", results: []slackHTTPResult{{body: `{"ok":false,"error":"missing_scope"}`}, {body: `{"ok":true,"emoji":{}}`}, {body: `{"ok":true,"user":{"id":"U_ONE","profile":{"display_name":"Ada"}}}`}, {body: `{"ok":true,"emoji":{}}`}}, wantName: "Ada", expire: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: tc.results}
			clock := &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
			directory := &slackDirectory{client: fakeSlackClient(fake), clock: clock}
			page := directoryFixture()
			directory.EnrichThread(context.Background(), &page)
			if tc.expire {
				clock.now = clock.now.Add(time.Hour)
			}
			page = directoryFixture()
			directory.EnrichThread(context.Background(), &page)
			if diff := cmp.Diff(tc.wantName, page.Root.Author.Name); diff != "" {
				t.Errorf("unexpected name (-want +got):\n%s", diff)
			}
			if (len(page.Warnings) > 0) != tc.warning {
				t.Fatalf("warnings %v", page.Warnings)
			}
			if diff := cmp.Diff(len(tc.results), len(fake.forms)); diff != "" {
				t.Errorf("unexpected call count (-want +got):\n%s", diff)
			}
			if fake.paths[0] != "/api/users.info" || fake.forms[0].Get("user") != "U_ONE" || fake.forms[0].Get("users") != "" {
				t.Fatalf("undocumented profile request %v %v", fake.paths, fake.forms)
			}
			if tc.name == "success" {
				if diff := cmp.Diff(map[string]string{"custom": "alias:target", "target": "https://emoji.slack-edge.com/ok.png"}, page.Emojis); diff != "" {
					t.Errorf("unexpected emoji (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestSlackDirectoryBounds exercises deterministic eviction and workspace emoji limits.
func TestSlackDirectoryBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		emoji bool
	}{{name: "identity limit"}, {name: "emoji dictionary limit", emoji: true}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{}
			directory := &slackDirectory{client: fakeSlackClient(fake), cache: directoryCache{people: map[string]directoryPerson{}}, clock: &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}}
			if tc.emoji {
				values := map[string]string{}
				for i := range 5000 {
					values[fmt.Sprintf("custom_%04d", i)] = "https://emoji.slack-edge.com/custom.png"
				}
				data, err := json.Marshal(map[string]any{"ok": true, "emoji": values})
				if err != nil {
					t.Fatal(err)
				}
				fake.results = []slackHTTPResult{{body: string(data)}}
				directory.lookupEmojis(context.Background())
				if diff := cmp.Diff(4096, len(directory.cache.emojis)); diff != "" {
					t.Errorf("unexpected emoji bound (-want +got):\n%s", diff)
				}
				return
			}
			for i := range 512 {
				directory.cache.people[fmt.Sprintf("U_%03d", i)] = directoryPerson{}
			}
			fake.results = []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_NEW","name":"New participant"}}`}}
			page := directoryFixture()
			page.Root.Text = ""
			page.Root.Author = opsalerts.ThreadPerson{ID: "U_NEW", Name: "U_NEW"}
			page.People = map[string]opsalerts.ThreadPerson{}
			directory.EnrichThread(context.Background(), &page)
			if diff := cmp.Diff(512, len(directory.cache.people)); diff != "" {
				t.Errorf("unexpected identity bound (-want +got):\n%s", diff)
			}
			if _, exists := directory.cache.people["U_000"]; exists {
				t.Fatal("eviction tie-break was not deterministic")
			}
		})
	}
}

// directoryConcurrentRead shares only the production directory, never mutable result maps.
func directoryConcurrentRead(directory *slackDirectory, start <-chan struct{}, done *sync.WaitGroup) {
	defer done.Done()
	<-start
	page := directoryFixture()
	directory.EnrichThread(context.Background(), &page)
}

// TestSlackDirectoryCoalescing proves concurrent browser misses share one API result.
func TestSlackDirectoryCoalescing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		readers int
	}{{name: "many readers", readers: 12}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_ONE","name":"Ada"}}`}, {body: `{"ok":true,"emoji":{}}`}}}
			transport := &directoryHTTPFake{fake: fake}
			directory := &slackDirectory{client: slack.New("unit-test-placeholder", slack.OptionHTTPClient(transport)), clock: &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}}
			start := make(chan struct{})
			var done sync.WaitGroup
			for range tc.readers {
				done.Add(1)
				go directoryConcurrentRead(directory, start, &done)
			}
			close(start)
			done.Wait()
			if diff := cmp.Diff(2, len(fake.forms)); diff != "" {
				t.Errorf("unexpected metadata requests (-want +got):\n%s", diff)
			}
		})
	}
}

// directoryPersonPage isolates profile metadata requests from mentions and emoji requests.
func directoryPersonPage(id string) opsalerts.AlertThread {
	return opsalerts.AlertThread{Root: opsalerts.ThreadMessage{Author: opsalerts.ThreadPerson{ID: id, Name: id}}, People: map[string]opsalerts.ThreadPerson{id: {ID: id, Name: id}}, Emojis: map[string]string{}, Warnings: []string{}}
}

// TestSlackDirectoryMethodBackoff shares rate and permission cooldowns across different identities.
func TestSlackDirectoryMethodBackoff(t *testing.T) {
	cases := []struct {
		name, prefix string
		rate         bool
	}{{name: "users rate limit", prefix: "U", rate: true}, {name: "users missing scope", prefix: "U"}, {name: "bots rate limit", prefix: "B", rate: true}, {name: "bots missing scope", prefix: "B"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := slackHTTPResult{body: `{"ok":false,"error":"missing_scope"}`}
			wait := 5 * time.Minute
			if tc.rate {
				first = slackHTTPResult{status: 429}
				wait = time.Hour
			}
			response := fmt.Sprintf(`{"ok":true,"user":{"id":"%s_THREE","name":"Recovered"}}`, tc.prefix)
			if tc.prefix == "B" {
				response = fmt.Sprintf(`{"ok":true,"bot":{"id":"%s_THREE","name":"Recovered"}}`, tc.prefix)
			}
			fake := &slackHTTPFake{results: []slackHTTPResult{first, {body: response}}}
			clock := &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
			directory := &slackDirectory{client: fakeSlackClient(fake), clock: clock}
			page := directoryPersonPage(tc.prefix + "_ONE")
			directory.EnrichThread(context.Background(), &page)
			clock.now = clock.now.Add(wait - time.Nanosecond)
			page = directoryPersonPage(tc.prefix + "_TWO")
			directory.EnrichThread(context.Background(), &page)
			if len(fake.forms) != 1 {
				t.Fatal("different identity bypassed shared cooldown")
			}
			if len(page.Warnings) == 0 {
				t.Fatal("cooldown was not surfaced safely")
			}
			if _, exists := directory.cache.people[tc.prefix+"_TWO"]; exists {
				t.Fatal("unattempted identity was negatively cached")
			}
			clock.now = clock.now.Add(time.Nanosecond)
			page = directoryPersonPage(tc.prefix + "_THREE")
			directory.EnrichThread(context.Background(), &page)
			if diff := cmp.Diff(2, len(fake.forms)); diff != "" {
				t.Errorf("unexpected boundary request count (-want +got):\n%s", diff)
			}
			if page.Root.Author.Name != "Recovered" {
				t.Fatalf("metadata did not recover: %#v", page)
			}
		})
	}
}

// TestSlackDirectoryTTL checks positive, negative and emoji expiry strictly before and at the boundary.
func TestSlackDirectoryTTL(t *testing.T) {
	cases := []struct {
		name                  string
		negative, emoji, rate bool
		ttl                   time.Duration
	}{
		{name: "profile positive", ttl: time.Hour},
		{name: "profile negative", negative: true, ttl: 5 * time.Minute},
		{name: "emoji positive", emoji: true, ttl: time.Hour},
		{name: "emoji negative", emoji: true, negative: true, ttl: 5 * time.Minute},
		{name: "emoji long Retry-After", emoji: true, rate: true, ttl: time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := slackHTTPResult{body: `{"ok":true,"user":{"id":"U_ONE","name":"First"}}`}
			last := slackHTTPResult{body: `{"ok":true,"user":{"id":"U_ONE","name":"Second"}}`}
			if tc.emoji {
				first.body = `{"ok":true,"emoji":{"custom":"https://emoji.slack-edge.com/first.png"}}`
				last.body = `{"ok":true,"emoji":{"custom":"https://emoji.slack-edge.com/second.png"}}`
			}
			if tc.negative {
				first.body = `{"ok":false,"error":"user_not_found"}`
			}
			if tc.rate {
				first = slackHTTPResult{status: 429}
			}
			fake := &slackHTTPFake{results: []slackHTTPResult{first, last}}
			clock := &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
			directory := &slackDirectory{client: fakeSlackClient(fake), clock: clock}
			page := directoryPersonPage("U_ONE")
			if tc.emoji {
				page = directoryPersonPage("")
				page.Root.Text = ":custom:"
			}
			directory.EnrichThread(context.Background(), &page)
			clock.now = clock.now.Add(tc.ttl - time.Nanosecond)
			directory.EnrichThread(context.Background(), &page)
			if len(fake.forms) != 1 {
				t.Fatal("metadata refreshed before its TTL or Retry-After")
			}
			clock.now = clock.now.Add(time.Nanosecond)
			directory.EnrichThread(context.Background(), &page)
			if diff := cmp.Diff(2, len(fake.forms)); diff != "" {
				t.Errorf("unexpected TTL boundary (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSlackDirectoryMessageIdentity preserves distinct per-message bot identities for the same sender ID.
func TestSlackDirectoryMessageIdentity(t *testing.T) {
	cases := []struct {
		name   string
		custom bool
	}{{name: "custom bot names and icons", custom: true}, {name: "unresolved human uses directory"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_ONE","profile":{"display_name":"Directory profile","image_72":"https://avatars.slack-edge.com/profile.png"}}}`}}}
			directory := &slackDirectory{client: fakeSlackClient(fake), clock: &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}}
			page := directoryPersonPage("U_ONE")
			page.Messages = []opsalerts.ThreadMessage{{Author: page.Root.Author}}
			if tc.custom {
				page.Root.Author.Name = "Root app"
				page.Root.Author.AvatarURL = "https://avatars.slack-edge.com/root.png"
				page.Root.Author.IsBot = true
				page.Messages[0].Author.Name = "Reply app"
				page.Messages[0].Author.AvatarURL = "https://avatars.slack-edge.com/reply.png"
				page.Messages[0].Author.IsBot = true
			}
			wantRoot, wantReply := page.Root.Author, page.Messages[0].Author
			if !tc.custom {
				wantRoot.Name = "Directory profile"
				wantRoot.AvatarURL = "https://avatars.slack-edge.com/profile.png"
				wantReply = wantRoot
			}
			directory.EnrichThread(context.Background(), &page)
			if diff := cmp.Diff(wantRoot, page.Root.Author); diff != "" {
				t.Errorf("unexpected root identity (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(wantReply, page.Messages[0].Author); diff != "" {
				t.Errorf("unexpected reply identity (-want +got):\n%s", diff)
			}
		})
	}
}

// directoryHTTPFake synchronizes the existing SDK fake and optionally holds one in-flight response.
type directoryHTTPFake struct {
	mu      sync.Mutex
	fake    *slackHTTPFake
	entered chan struct{}
	release <-chan struct{}
	blocked bool
}

// Do holds no test mutex while waiting for cancellation or an explicitly released response.
func (f *directoryHTTPFake) Do(request *http.Request) (*http.Response, error) {
	f.mu.Lock()
	response, err := f.fake.Do(request)
	block := f.entered != nil && !f.blocked
	f.blocked = true
	f.mu.Unlock()
	if block {
		close(f.entered)
		select {
		case <-f.release:
		case <-request.Context().Done():
			if response != nil {
				err = errors.Join(err, response.Body.Close())
			}
			return nil, errors.Join(err, request.Context().Err())
		}
	}
	return response, err
}

// count reads the fake's request log without racing a metadata holder.
func (f *directoryHTTPFake) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.fake.forms) }

// directoryEnrichResult transports a detached result from a named test goroutine.
type directoryEnrichResult struct{ page opsalerts.AlertThread }

// directoryWaitContext signals that acquisition has reached its cancellable wait.
type directoryWaitContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

// observe records the first Done lookup without relying on scheduler timing.
func (c *directoryWaitContext) observe() { close(c.observed) }

// Done lets tests cancel an actual waiter while the network holder remains blocked.
func (c *directoryWaitContext) Done() <-chan struct{} { c.once.Do(c.observe); return c.Context.Done() }

// enrichDirectoryAsync exercises a real in-flight SDK request without closures or sleeps.
func enrichDirectoryAsync(ctx context.Context, directory *slackDirectory, page opsalerts.AlertThread, done chan<- directoryEnrichResult) {
	directory.EnrichThread(ctx, &page)
	done <- directoryEnrichResult{page: page}
}

// TestSlackDirectorySnapshotEviction keeps checked metadata stable across an unrelated cache eviction.
func TestSlackDirectorySnapshotEviction(t *testing.T) {
	for _, tc := range []struct {
		name               string
		negative, cooldown bool
	}{
		{name: "warm profile and emoji"},
		{name: "negative profile and emoji warnings", negative: true},
		{name: "shared cooldown warnings", cooldown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
			clock := &directoryClock{now: now}
			person := opsalerts.ThreadPerson{ID: "U_ONE", Name: "Cached profile", AvatarURL: "https://avatars.slack-edge.com/cached.png"}
			entry := directoryPerson{Person: person, Until: now.Add(time.Hour)}
			cache := directoryCache{
				people:     map[string]directoryPerson{},
				emojis:     map[string]string{"custom": "alias:target", "target": "https://emoji.slack-edge.com/cached.png"},
				emojiUntil: now.Add(time.Hour),
				backoff:    map[string]directoryBackoff{},
			}
			wantWarnings := []string{}
			if tc.negative {
				profileFailure, _ := metadataFailure(errors.New("unavailable"), "users", now)
				emojiFailure, _ := metadataFailure(errors.New("unavailable"), "emoji", now)
				entry.Warning, cache.emojiWarning = profileFailure.Warning, emojiFailure.Warning
				wantWarnings = []string{emojiFailure.Warning, profileFailure.Warning}
			}
			if tc.cooldown {
				profileFailure, _ := metadataFailure(&slack.RateLimitedError{RetryAfter: time.Hour}, "users", now)
				emojiFailure, _ := metadataFailure(&slack.RateLimitedError{RetryAfter: time.Hour}, "emoji", now)
				entry.Until, cache.emojiUntil = now, now
				cache.backoff["users"], cache.backoff["emoji"] = profileFailure, emojiFailure
				wantWarnings = []string{emojiFailure.Warning, profileFailure.Warning}
			}
			cache.people[person.ID] = entry
			for i := range 511 {
				cache.people[fmt.Sprintf("U_FILL_%03d", i)] = directoryPerson{Until: now.Add(2 * time.Hour)}
			}
			fake := &slackHTTPFake{results: []slackHTTPResult{{body: `{"ok":true,"bot":{"id":"B_NEW","name":"Unrelated"}}`}}}
			directory := &slackDirectory{client: fakeSlackClient(fake), clock: clock, cache: cache}
			page := directoryFixture()
			reply := opsalerts.ThreadPerson{ID: person.ID, Name: "Message app", AvatarURL: "https://avatars.slack-edge.com/message.png", IsBot: true}
			page.Messages = []opsalerts.ThreadMessage{{Author: reply}}
			people, emojis := threadReferences(page)
			snapshot, ready := directory.cachedSnapshot(people, emojis)
			if !ready {
				t.Fatal("cached metadata required a lookup")
			}
			unrelated := directoryPersonPage("B_NEW")
			directory.EnrichThread(context.Background(), &unrelated)
			if _, exists := directory.cache.people[person.ID]; exists || len(directory.cache.people) != 512 {
				t.Fatal("unrelated cold lookup did not evict the checked profile at the cache bound")
			}
			if len(fake.forms) != 1 {
				t.Fatalf("unexpected HTTP requests: %d", len(fake.forms))
			}
			// Mutate the live maps and advance time before presenting the checked snapshot.
			directory.mu.Lock()
			directory.cache.emojis["target"] = "https://emoji.slack-edge.com/changed.png"
			directory.cache.backoff["users"] = directoryBackoff{Until: now.Add(3 * time.Hour), Warning: "changed user cooldown"}
			directory.cache.backoff["emoji"] = directoryBackoff{Until: now.Add(3 * time.Hour), Warning: "changed emoji cooldown"}
			directory.mu.Unlock()
			clock.now = now.Add(2 * time.Hour)
			snapshot.cache.presentCached(&page, people, emojis, sets.New[string](), snapshot.now)
			if diff := cmp.Diff(person, page.Root.Author); diff != "" {
				t.Errorf("unexpected snapshotted author (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(reply, page.Messages[0].Author); diff != "" {
				t.Errorf("unexpected message override (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(wantWarnings, page.Warnings); diff != "" {
				t.Errorf("unexpected snapshotted warnings (-want +got):\n%s", diff)
			}
			wantEmojis := map[string]string{"custom": "alias:target", "target": "https://emoji.slack-edge.com/cached.png"}
			if diff := cmp.Diff(wantEmojis, page.Emojis); diff != "" {
				t.Errorf("unexpected snapshotted emoji (-want +got):\n%s", diff)
			}
			page.People[person.ID] = opsalerts.ThreadPerson{Name: "Browser mutation"}
			page.Emojis["target"] = "Browser mutation"
			if diff := cmp.Diff(person, snapshot.cache.people[person.ID].Person); diff != "" {
				t.Errorf("page mutated snapshot profile (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(wantEmojis, snapshot.cache.emojis); diff != "" {
				t.Errorf("page mutated snapshot emoji (-want +got):\n%s", diff)
			}
			if directory.cache.emojis["target"] != "https://emoji.slack-edge.com/changed.png" {
				t.Error("page mutated the live emoji cache")
			}
		})
	}
}

// TestSlackDirectoryWarmReaders proves cached pages never wait behind unrelated SDK calls.
func TestSlackDirectoryWarmReaders(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	profileFailure, _ := metadataFailure(errors.New("profile unavailable"), "users", now)
	emojiFailure, _ := metadataFailure(errors.New("emoji unavailable"), "emoji", now)
	profileCooldown, _ := metadataFailure(&slack.RateLimitedError{RetryAfter: time.Hour}, "users", now)
	emojiCooldown, _ := metadataFailure(&slack.RateLimitedError{RetryAfter: time.Hour}, "emoji", now)
	cases := []struct {
		name                        string
		bot, negative, expired      bool
		profileBackoff              bool
		emojiBackoff, missing       bool
		missingReference, coldEmoji bool
		missingEmoji                bool
		wantWarnings                []string
	}{
		{name: "warm profiles and emoji"},
		{name: "negative profile and emoji caches", negative: true, wantWarnings: []string{emojiFailure.Warning, profileFailure.Warning}},
		{name: "missing user under shared cooldown", missing: true, profileBackoff: true, wantWarnings: []string{profileCooldown.Warning}},
		{name: "missing bot under shared cooldown", bot: true, missing: true, profileBackoff: true, wantWarnings: []string{profileCooldown.Warning}},
		{name: "expired user under shared cooldown", expired: true, profileBackoff: true, wantWarnings: []string{profileCooldown.Warning}},
		{name: "expired bot under shared cooldown", bot: true, expired: true, profileBackoff: true, wantWarnings: []string{profileCooldown.Warning}},
		{name: "expired emoji under shared cooldown", coldEmoji: true, emojiBackoff: true, wantWarnings: []string{emojiCooldown.Warning}},
		{name: "missing emoji under shared cooldown", missingEmoji: true, emojiBackoff: true, wantWarnings: []string{emojiCooldown.Warning}},
		{name: "fresh user under another identity cooldown", profileBackoff: true},
		{name: "fresh bot under another identity cooldown", bot: true, profileBackoff: true},
		{name: "fresh emoji under shared cooldown", emojiBackoff: true},
		{name: "negative caches under shared cooldowns", negative: true, profileBackoff: true, emojiBackoff: true, wantWarnings: []string{emojiFailure.Warning, profileFailure.Warning}},
		{name: "mixed fresh and missing references", missingReference: true},
		{name: "fresh profile and expired emoji", coldEmoji: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, method := "U_ONE", "users"
			if tc.bot {
				id, method = "B_ONE", "bots"
			}
			person := opsalerts.ThreadPerson{ID: id, Name: "Cached profile", AvatarURL: "https://avatars.slack-edge.com/cached.png", IsBot: tc.bot}
			entry := directoryPerson{Person: person, Until: now.Add(time.Hour)}
			cache := directoryCache{
				people:     map[string]directoryPerson{},
				emojis:     map[string]string{"custom": "alias:target", "target": "https://emoji.slack-edge.com/cached.png"},
				emojiUntil: now.Add(time.Hour),
				backoff:    map[string]directoryBackoff{},
			}
			if tc.expired {
				entry.Until = now
			}
			if tc.negative {
				entry.Person = opsalerts.ThreadPerson{}
				entry.Warning = profileFailure.Warning
				cache.emojiWarning = emojiFailure.Warning
				cache.emojis = nil
			}
			if !tc.missing {
				cache.people[id] = entry
			}
			if tc.profileBackoff {
				cache.backoff[method] = profileCooldown
			}
			if tc.emojiBackoff {
				cache.backoff["emoji"] = emojiCooldown
			}
			if tc.coldEmoji {
				cache.emojiUntil = now
			}
			if tc.missingEmoji {
				cache.emojis = nil
				cache.emojiUntil = time.Time{}
			}
			holderID := "U_HOLDER"
			holderResponse := `{"ok":true,"user":{"id":"U_HOLDER","name":"Unrelated"}}`
			if tc.profileBackoff && method == "users" {
				holderID = "B_HOLDER"
				holderResponse = `{"ok":true,"bot":{"id":"B_HOLDER","name":"Unrelated"}}`
			}
			results := []slackHTTPResult{{body: holderResponse}}
			wantLookup := tc.missingReference || tc.coldEmoji && !tc.emojiBackoff
			if tc.missingReference {
				results = append(results, slackHTTPResult{body: `{"ok":true,"user":{"id":"U_MISSING","name":"Fetched profile"}}`})
			}
			if tc.coldEmoji && !tc.emojiBackoff {
				results = append(results, slackHTTPResult{body: `{"ok":true,"emoji":{"custom":"https://emoji.slack-edge.com/refreshed.png"}}`})
			}
			release, entered := make(chan struct{}), make(chan struct{})
			transport := &directoryHTTPFake{fake: &slackHTTPFake{results: results}, entered: entered, release: release}
			directory := &slackDirectory{client: slack.New("unit-test-placeholder", slack.OptionHTTPClient(transport)), clock: &directoryClock{now: now}, cache: cache}
			holderDone := make(chan directoryEnrichResult, 1)
			go enrichDirectoryAsync(context.Background(), directory, directoryPersonPage(holderID), holderDone)
			<-entered
			page := directoryPersonPage(id)
			page.Root.Text = ":custom:"
			if tc.missingReference {
				page.Root.Text += " <@U_MISSING>"
			}
			wantPerson := person
			if tc.missing || tc.negative {
				wantPerson = opsalerts.ThreadPerson{ID: id, Name: "Message fallback", AvatarURL: "https://avatars.slack-edge.com/fallback.png", IsBot: tc.bot}
				page.People[id] = wantPerson
			}
			observed := make(chan struct{})
			ctx := &directoryWaitContext{Context: context.Background(), observed: observed}
			warmDone := make(chan directoryEnrichResult, 1)
			go enrichDirectoryAsync(ctx, directory, page, warmDone)
			var result directoryEnrichResult
			select {
			case result = <-warmDone:
				calls := transport.count()
				close(release)
				<-holderDone
				if wantLookup {
					t.Fatal("missing or expired metadata did not acquire the lookup gate")
				}
				if calls != 1 {
					t.Fatalf("cached reader performed extra HTTP requests: %d", calls)
				}
			case <-observed:
				close(release)
				<-holderDone
				result = <-warmDone
				if !wantLookup {
					t.Fatal("cache-only reader waited for the unrelated lookup gate")
				}
			}
			if diff := cmp.Diff(wantPerson, result.page.Root.Author); diff != "" {
				t.Errorf("unexpected author (-want +got):\n%s", diff)
			}
			wantWarnings := tc.wantWarnings
			if wantWarnings == nil {
				wantWarnings = []string{}
			}
			if diff := cmp.Diff(wantWarnings, result.page.Warnings); diff != "" {
				t.Errorf("unexpected warnings (-want +got):\n%s", diff)
			}
			wantEmojis := cache.emojis
			if tc.negative || tc.missingEmoji {
				wantEmojis = map[string]string{}
			}
			if tc.coldEmoji && !tc.emojiBackoff {
				wantEmojis = map[string]string{"custom": "https://emoji.slack-edge.com/refreshed.png"}
			}
			if diff := cmp.Diff(wantEmojis, result.page.Emojis); diff != "" {
				t.Errorf("unexpected emoji (-want +got):\n%s", diff)
			}
			if tc.missingReference && result.page.People["U_MISSING"].Name != "Fetched profile" {
				t.Error("mixed references did not fetch the missing identity")
			}
			if diff := cmp.Diff(len(results), transport.count()); diff != "" {
				t.Errorf("unexpected request count (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSlackDirectoryCancellation checks both cancelled gate waiters and cancelled first loads without cache poisoning.
func TestSlackDirectoryCancellation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cancelHolder bool
	}{{name: "gate waiter"}, {name: "first load", cancelHolder: true}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: []slackHTTPResult{{body: `{"ok":true,"user":{"id":"U_ONE","name":"Ada"}}`}, {body: `{"ok":true,"user":{"id":"U_ONE","name":"Ada"}}`}}}
			release := make(chan struct{})
			entered := make(chan struct{})
			transport := &directoryHTTPFake{fake: fake, entered: entered, release: release}
			directory := &slackDirectory{client: slack.New("unit-test-placeholder", slack.OptionHTTPClient(transport)), clock: &directoryClock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			page := directoryPersonPage("U_ONE")
			if tc.cancelHolder {
				page.Root.Text = "Also <@U_TWO>"
			}
			done := make(chan directoryEnrichResult, 1)
			go enrichDirectoryAsync(ctx, directory, page, done)
			<-entered
			if tc.cancelHolder {
				cancel()
				<-done
				if len(directory.cache.people) != 0 {
					t.Fatal("cancelled or unattempted profile was cached")
				}
				page = directoryPersonPage("U_ONE")
				directory.EnrichThread(context.Background(), &page)
				if page.Root.Author.Name != "Ada" || transport.count() != 2 {
					t.Fatal("cancelled first load poisoned a subsequent request")
				}
				return
			}
			waiterCtx, cancelWaiter := context.WithCancel(context.Background())
			defer cancelWaiter()
			observed := make(chan struct{})
			waiting := &directoryWaitContext{Context: waiterCtx, observed: observed}
			waiter := directoryPersonPage("U_TWO")
			waiterDone := make(chan directoryEnrichResult, 1)
			go enrichDirectoryAsync(waiting, directory, waiter, waiterDone)
			<-observed
			cancelWaiter()
			result := <-waiterDone
			if transport.count() != 1 || len(result.page.Warnings) == 0 {
				t.Fatal("cancelled gate waiter performed I/O or lost fallback warning")
			}
			close(release)
			<-done
			if _, exists := directory.cache.people["U_TWO"]; exists {
				t.Fatal("cancelled waiter poisoned another identity")
			}
			page = directoryPersonPage("U_ONE")
			directory.EnrichThread(context.Background(), &page)
			if page.Root.Author.Name != "Ada" || transport.count() != 1 {
				t.Fatal("gate waiter interfered with the holder's successful cache")
			}
		})
	}
}
