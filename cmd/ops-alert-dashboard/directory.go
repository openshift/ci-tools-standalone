package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
)

// directoryPerson caches failures as well as successful names for a bounded period.
type directoryPerson struct {
	Person  opsalerts.ThreadPerson
	Until   time.Time
	Warning string
	Handle  string
	UserID  string
}

// directoryBackoff suppresses misses across different identities for one Slack method.
type directoryBackoff struct {
	Until   time.Time
	Warning string
}

// directoryCache keeps immutable cards and bounded method cooldowns behind the short-lived cache lock.
type directoryCache struct {
	people       map[string]directoryPerson
	emojis       map[string]string
	emojiUntil   time.Time
	emojiWarning string
	backoff      map[string]directoryBackoff
}

// directorySnapshot keeps eligibility and presentation on the same detached cache view.
type directorySnapshot struct {
	cache directoryCache
	now   time.Time
}

// slackDirectory coalesces misses with a cancellable gate, never a network-held cache lock.
// Positive entries live 1 hour, failures 5 minutes; at most 512 identities and 4096 emojis are retained.
type slackDirectory struct {
	client    *slack.Client
	clock     opsalerts.Clock
	once      sync.Once
	gate      chan struct{}
	mu        sync.RWMutex
	cache     directoryCache
	workspace *workspaceUsers
}

// initGate allows zero-value construction without sharing mutable package state.
func (d *slackDirectory) initGate() { d.gate = make(chan struct{}, 1) }

// now makes TTL and Retry-After boundaries testable without sleeps.
func (d *slackDirectory) now() time.Time {
	if d.clock != nil {
		return d.clock.Now()
	}
	return time.Now()
}

// acquire allows cancelled waiters to leave while another metadata request is in flight.
func (d *slackDirectory) acquire(ctx context.Context) error {
	d.once.Do(d.initGate)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case d.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-d.gate
			return err
		}
		return nil
	}
}

// release lets the next live waiter reuse the completed metadata result.
func (d *slackDirectory) release() { <-d.gate }

// EnrichThread uses the production archive's shared directory, never its history methods.
func (a *slackArchive) EnrichThread(ctx context.Context, thread *opsalerts.AlertThread) {
	if a.directory != nil {
		a.directory.EnrichThread(ctx, thread)
	}
}

// EnrichThread resolves only identities and emoji actually referenced by this page.
func (d *slackDirectory) EnrichThread(ctx context.Context, thread *opsalerts.AlertThread) {
	people, emojis := threadReferences(*thread)
	warnings := sets.New[string]()
	if snapshot, ready := d.cachedSnapshot(people, emojis); ready {
		snapshot.cache.presentCached(thread, people, emojis, warnings, snapshot.now)
		return
	}
	if err := d.acquire(ctx); err != nil {
		warnings.Insert("Slack metadata lookup cancelled; showing cached names, icons and shortcodes")
		d.presentCached(thread, people, emojis, warnings)
		return
	}
	defer d.release()
	for _, id := range people {
		if ctx.Err() != nil {
			warnings.Insert("Slack metadata lookup cancelled; showing cached names, icons and shortcodes")
			break
		}
		entry := d.lookupPerson(ctx, id)
		if entry.Warning != "" {
			warnings.Insert(entry.Warning)
		}
	}
	if len(emojis) > 0 {
		if warning := d.lookupEmojis(ctx); warning != "" {
			warnings.Insert(warning)
		}
	}
	d.presentCached(thread, people, emojis, warnings)
}

// cachedSnapshot detaches cache-only results before another lookup can evict their entries.
func (d *slackDirectory) cachedSnapshot(people, emojis []string) (directorySnapshot, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	now := d.now()
	snapshot := directorySnapshot{now: now, cache: directoryCache{people: make(map[string]directoryPerson, len(people))}}
	for _, id := range people {
		entry := d.cache.people[id]
		snapshot.cache.people[id] = entry
		if now.Before(entry.Until) {
			continue
		}
		method := "users"
		if strings.HasPrefix(id, "B") {
			method = "bots"
		}
		if !now.Before(d.cache.backoff[method].Until) {
			return directorySnapshot{}, false
		}
	}
	if len(emojis) > 0 && !now.Before(d.cache.emojiUntil) && !now.Before(d.cache.backoff["emoji"].Until) {
		return directorySnapshot{}, false
	}
	snapshot.cache.backoff = maps.Clone(d.cache.backoff)
	snapshot.cache.emojiUntil, snapshot.cache.emojiWarning = d.cache.emojiUntil, d.cache.emojiWarning
	snapshot.cache.emojis = make(map[string]string, len(emojis))
	for _, name := range emojis {
		d.cache.copyEmoji(snapshot.cache.emojis, name, 0)
	}
	return snapshot, true
}

// lookupPerson deliberately uses the documented singular users.info API.
func (d *slackDirectory) lookupPerson(ctx context.Context, id string) directoryPerson {
	method := "users"
	if strings.HasPrefix(id, "B") {
		method = "bots"
	}
	d.mu.RLock()
	entry := d.cache.people[id]
	cooldown := d.cache.backoff[method]
	d.mu.RUnlock()
	if d.now().Before(entry.Until) {
		return entry
	}
	if ctx.Err() != nil {
		entry.Warning = "Slack profile lookup cancelled; showing cached names or original identities"
		return entry
	}
	if d.now().Before(cooldown.Until) {
		entry.Warning = cooldown.Warning
		return entry
	}
	person, err := d.fetchPerson(ctx, id)
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		entry.Warning = "Slack profile lookup cancelled; showing cached names or original identities"
		return entry
	}
	now := d.now()
	entry.Until, entry.Warning = now.Add(time.Hour), ""
	if err == nil {
		entry.Person, entry.Handle, entry.UserID = person.Person, person.Handle, person.UserID
	}
	if err != nil {
		failure, shared := metadataFailure(err, method, now)
		entry.Until, entry.Warning = now.Add(5*time.Minute), failure.Warning
		if failure.Until.After(entry.Until) {
			entry.Until = failure.Until
		}
		if shared {
			d.mu.Lock()
			d.setBackoff(method, failure)
			d.mu.Unlock()
		}
	}
	d.mu.Lock()
	if d.cache.people == nil {
		d.cache.people = map[string]directoryPerson{}
	}
	if _, exists := d.cache.people[id]; !exists && len(d.cache.people) >= 512 {
		d.evictPerson()
	}
	d.cache.people[id] = entry
	d.mu.Unlock()
	return entry
}

// fetchPerson returns only the requested identity and never exposes upstream errors to a browser.
func (d *slackDirectory) fetchPerson(ctx context.Context, id string) (directoryPerson, error) {
	if strings.HasPrefix(id, "B") {
		bot, err := d.client.GetBotInfoContext(ctx, slack.GetBotInfoParameters{Bot: id})
		if err != nil {
			return directoryPerson{}, err
		}
		if bot == nil || bot.ID != id {
			return directoryPerson{}, fmt.Errorf("slack returned a different bot identity")
		}
		return directoryPerson{Person: opsalerts.ThreadPerson{ID: id, Name: bot.Name, IsBot: true, AvatarURL: opsalerts.SafeThreadImage(bot.Icons.Image48)}, Handle: bot.Name, UserID: bot.UserID}, nil
	}
	user, err := d.client.GetUserInfoContext(ctx, id)
	if err != nil {
		return directoryPerson{}, err
	}
	if user != nil && user.ID == id {
		name := user.Profile.DisplayName
		if name == "" {
			name = user.Profile.RealName
		}
		if name == "" {
			name = user.RealName
		}
		if name == "" {
			name = user.Name
		}
		avatar := opsalerts.SafeThreadImage(user.Profile.Image72)
		if avatar == "" {
			avatar = opsalerts.SafeThreadImage(user.Profile.Image48)
		}
		return directoryPerson{Person: opsalerts.ThreadPerson{ID: id, Name: name, IsBot: user.IsBot, AvatarURL: avatar}, Handle: user.Name, UserID: user.ID}, nil
	}
	return directoryPerson{}, fmt.Errorf("slack returned a different user identity")
}

// ResolveThreadReference prefers the complete workspace index over potentially ambiguous participant aliases.
func (d *slackDirectory) ResolveThreadReference(kind, name string) (string, bool) {
	if kind != "@" {
		return "", false
	}
	if d.workspace != nil {
		return d.workspace.resolve(name)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	now := d.now()
	id := ""
	for _, entry := range d.cache.people {
		if entry.Warning != "" || !now.Before(entry.Until) || entry.Handle == "" || !strings.EqualFold(entry.Handle, name) {
			continue
		}
		target := entry.UserID
		if target == "" {
			target = entry.Person.ID
		}
		if id != "" && target != id {
			return "", false
		}
		id = target
	}
	return id, id != ""
}

// ResolveThreadReference keeps reply mentions on the shared metadata cache.
func (a *slackArchive) ResolveThreadReference(kind, name string) (string, bool) {
	if a.directory == nil {
		return "", false
	}
	return a.directory.ResolveThreadReference(kind, name)
}

// metadataFailure applies method-wide cooldowns only to rate limits and missing permissions.
func metadataFailure(err error, method string, now time.Time) (directoryBackoff, bool) {
	warning := "Slack profile metadata unavailable; unresolved identities retain original IDs or bot profiles"
	scope := "users:read"
	if method == "emoji" {
		warning = "Slack custom emoji metadata unavailable; unresolved emoji retain original shortcodes"
		scope = "emoji:read"
	}
	failure := directoryBackoff{Until: now.Add(5 * time.Minute), Warning: warning}
	var limited *slack.RateLimitedError
	if errors.As(err, &limited) {
		failure.Until = now.Add(max(limited.RetryAfter, time.Second))
		failure.Warning = warning + " (rate limited; shared metadata requests paused)"
		return failure, true
	}
	var response slack.SlackErrorResponse
	if errors.As(err, &response) && response.Err == "missing_scope" {
		failure.Warning = warning + " (missing_scope: " + scope + "; shared metadata requests paused)"
		return failure, true
	}
	return failure, false
}

// setBackoff keeps one cooldown per fixed Slack method while the cache lock is held.
func (d *slackDirectory) setBackoff(method string, cooldown directoryBackoff) {
	if d.cache.backoff == nil {
		d.cache.backoff = map[string]directoryBackoff{}
	}
	d.cache.backoff[method] = cooldown
}

// evictPerson bounds the shared cache using expiry and deterministic tie-breaking.
func (d *slackDirectory) evictPerson() {
	oldest := ""
	for id, entry := range d.cache.people {
		if oldest == "" || entry.Until.Before(d.cache.people[oldest].Until) || entry.Until.Equal(d.cache.people[oldest].Until) && id < oldest {
			oldest = id
		}
	}
	delete(d.cache.people, oldest)
}

// lookupEmojis caches the workspace dictionary and failures, not one API call per shortcode.
func (d *slackDirectory) lookupEmojis(ctx context.Context) string {
	d.mu.RLock()
	until, warning := d.cache.emojiUntil, d.cache.emojiWarning
	cooldown := d.cache.backoff["emoji"]
	d.mu.RUnlock()
	if d.now().Before(until) {
		return warning
	}
	if ctx.Err() != nil {
		return "Slack emoji lookup cancelled; showing cached emoji or original shortcodes"
	}
	if d.now().Before(cooldown.Until) {
		return cooldown.Warning
	}
	values, err := d.client.GetEmojiContext(ctx)
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "Slack emoji lookup cancelled; showing cached emoji or original shortcodes"
	}
	if err != nil {
		failure, shared := metadataFailure(err, "emoji", d.now())
		d.mu.Lock()
		d.cache.emojiUntil, d.cache.emojiWarning = d.now().Add(5*time.Minute), failure.Warning
		if failure.Until.After(d.cache.emojiUntil) {
			d.cache.emojiUntil = failure.Until
		}
		if shared {
			d.setBackoff("emoji", failure)
		}
		d.mu.Unlock()
		return failure.Warning
	}
	emojis := map[string]string{}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names[:min(4096, len(names))] {
		value := values[name]
		if opsalerts.SafeThreadImage(value) != "" || regexp.MustCompile(`^alias:[A-Za-z0-9_+-]{1,100}$`).MatchString(value) {
			emojis[name] = value
		}
	}
	d.mu.Lock()
	d.cache.emojis = emojis
	d.cache.emojiUntil, d.cache.emojiWarning = d.now().Add(time.Hour), ""
	d.mu.Unlock()
	return ""
}

// presentCached also works for cancelled gate waiters without waiting for an in-flight network call.
func (d *slackDirectory) presentCached(thread *opsalerts.AlertThread, people, emojis []string, warnings sets.Set[string]) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	d.cache.presentCached(thread, people, emojis, warnings, d.now())
}

// presentCached renders either a locked live cache or a detached snapshot into page-owned maps.
func (c *directoryCache) presentCached(thread *opsalerts.AlertThread, people, emojis []string, warnings sets.Set[string], now time.Time) {
	if thread.People == nil {
		thread.People = map[string]opsalerts.ThreadPerson{}
	}
	if thread.Emojis == nil {
		thread.Emojis = map[string]string{}
	}
	for _, id := range people {
		fallback := thread.People[id]
		if fallback.ID == "" {
			fallback = opsalerts.ThreadPerson{ID: id, Name: id, IsBot: strings.HasPrefix(id, "B")}
		}
		entry := c.people[id]
		card := entry.Person
		if card.ID == "" {
			card = fallback
		}
		thread.People[id] = mergeThreadPerson(card, fallback)
		if entry.Warning != "" {
			warnings.Insert(entry.Warning)
		}
		method := "users"
		if strings.HasPrefix(id, "B") {
			method = "bots"
		}
		if cooldown := c.backoff[method]; !now.Before(entry.Until) && now.Before(cooldown.Until) && cooldown.Warning != "" {
			warnings.Insert(cooldown.Warning)
		}
	}
	if len(emojis) > 0 {
		warning := ""
		if now.Before(c.emojiUntil) {
			warning = c.emojiWarning
		}
		if cooldown := c.backoff["emoji"]; !now.Before(c.emojiUntil) && now.Before(cooldown.Until) {
			warning = cooldown.Warning
		}
		if warning != "" {
			warnings.Insert(warning)
		}
	}
	for _, name := range emojis {
		c.copyEmoji(thread.Emojis, name, 0)
	}
	thread.Root.Author = mergeThreadPerson(thread.Root.Author, thread.People[thread.Root.Author.ID])
	for i := range thread.Messages {
		thread.Messages[i].Author = mergeThreadPerson(thread.Messages[i].Author, thread.People[thread.Messages[i].Author.ID])
	}
	thread.Warnings = append(thread.Warnings, sets.List(warnings)...)
}

// mergeThreadPerson preserves per-message app overrides and uses directory metadata only as a fallback.
func mergeThreadPerson(original, fallback opsalerts.ThreadPerson) opsalerts.ThreadPerson {
	if original.Name == "" || original.Name == original.ID {
		original.Name = fallback.Name
	}
	original.AvatarURL = opsalerts.SafeThreadImage(original.AvatarURL)
	if original.AvatarURL == "" {
		original.AvatarURL = opsalerts.SafeThreadImage(fallback.AvatarURL)
	}
	original.IsBot = original.IsBot || fallback.IsBot
	return original
}

// copyEmoji follows bounded aliases so the browser can resolve the target without another API call.
func (c *directoryCache) copyEmoji(target map[string]string, name string, depth int) {
	if depth > 8 {
		return
	}
	if _, exists := target[name]; exists {
		return
	}
	value := c.emojis[name]
	if value == "" {
		return
	}
	target[name] = value
	if alias, ok := strings.CutPrefix(value, "alias:"); ok {
		c.copyEmoji(target, alias, depth+1)
	}
}

// threadReferences limits lazy enrichment to visible authors, mentions, rich text and reactions.
func threadReferences(thread opsalerts.AlertThread) ([]string, []string) {
	people, emojis := sets.New[string](), sets.New[string]()
	messages := append([]opsalerts.ThreadMessage{thread.Root}, thread.Messages...)
	for _, message := range messages {
		if message.Author.ID != "" {
			people.Insert(message.Author.ID)
		}
		for _, block := range message.Blocks {
			richThreadReferences(block, people, emojis)
		}
		for _, attachment := range message.Attachments {
			richThreadReferences(attachment, people, emojis)
		}
		data, err := json.Marshal(message)
		if err != nil {
			continue
		}
		text := string(data)
		// JSON escapes angle brackets by default; restore only for mention scanning.
		text = strings.ReplaceAll(strings.ReplaceAll(text, `\u003c`, "<"), `\u003e`, ">")
		for _, match := range regexp.MustCompile(`<@([A-Za-z0-9_]{1,128})(?:\|[^>]*)?>|"user_id":"([A-Za-z0-9_]{1,128})"`).FindAllStringSubmatch(text, -1) {
			id := match[1]
			if id == "" {
				id = match[2]
			}
			people.Insert(id)
		}
		for _, match := range regexp.MustCompile(`:([A-Za-z0-9_+-]{1,100}):|"type":"emoji","name":"([A-Za-z0-9_+-]{1,100})"|"name":"([A-Za-z0-9_+-]{1,100})","type":"emoji"`).FindAllStringSubmatch(text, -1) {
			for _, name := range match[1:] {
				if name != "" {
					emojis.Insert(name)
				}
			}
		}
		for _, reaction := range message.Reactions {
			emojis.Insert(reaction.Name)
			people.Insert(reaction.Users...)
		}
	}
	ids, names := sets.List(people), sets.List(emojis)
	return ids[:min(64, len(ids))], names[:min(128, len(names))]
}

// richThreadReferences reads typed rich text nodes without relying on JSON property ordering.
func richThreadReferences(value any, people, emojis sets.Set[string]) {
	switch value := value.(type) {
	case map[string]any:
		if value["type"] == "emoji" {
			if name, ok := value["name"].(string); ok {
				emojis.Insert(name)
			}
		}
		if value["type"] == "user" {
			if id, ok := value["user_id"].(string); ok {
				people.Insert(id)
			}
		}
		for _, nested := range value {
			richThreadReferences(nested, people, emojis)
		}
	case []map[string]any:
		for _, nested := range value {
			richThreadReferences(nested, people, emojis)
		}
	case []any:
		for _, nested := range value {
			richThreadReferences(nested, people, emojis)
		}
	}
}
