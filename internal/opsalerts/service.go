package opsalerts

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
)

// realClock supplies production time.
type realClock struct{}

// Now implements Clock.
func (realClock) Now() time.Time { return time.Now().UTC() }

// threadCache retains complete threads when history metadata is unchanged.
// FetchedAt records the last confirmed complete-thread read, which only a
// full sync or a targeted reload can establish; a single posted message
// never advances it. RefreshAfter bounds the next targeted read and
// RefreshFailed marks a cooldown that originates from a failed read rather
// than a fresh success.
type threadCache struct {
	Root          slack.Message
	Replies       []slack.Message
	FetchedAt     time.Time
	RefreshAfter  time.Time
	RefreshFailed bool
}

// pendingDelivery prevents blind retries after an ambiguous transport failure.
type pendingDelivery struct {
	Request       ActionRequest
	Actor         string
	Status        string
	Investigation *Investigation
}

// serviceCache is published atomically after a complete replay.
type serviceCache struct {
	Coverage            Coverage
	Threads             map[string]threadCache
	Occurrences         map[string]Occurrence
	Records             map[string]actionRecord
	Rules               map[string]Snooze
	WIPRules            map[string]WIPRule
	WIPRevisions        map[string]string
	WIPOverrides        map[string]string
	Pending             map[string]pendingDelivery
	Decorations         decorationState
	Conversations       map[string]Conversation
	ConversationRecords map[string]conversationRecord
	ConversationPending map[string]conversationAttempt
	ConversationIssues  map[string]time.Time
}

// Service serializes recovery and mutations while keeping snapshots nonblocking.
type Service struct {
	cfg        Config
	classifier *Classifier
	archive    Archive
	clock      Clock
	serial     chan struct{}
	mu         sync.RWMutex
	cache      serviceCache
	activity   conversationActivity
}

// NewService validates trusted identities and starts with incomplete coverage.
func NewService(cfg Config, classifier *Classifier, archive Archive, clock Clock) (*Service, error) {
	if cfg.HistoryWindow == 0 {
		cfg.HistoryWindow = DefaultHistoryWindow
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if classifier == nil || archive == nil {
		return nil, fmt.Errorf("%w: classification and archive required", ErrValidation)
	}
	if clock == nil {
		clock = realClock{}
	}
	s := &Service{cfg: cfg, classifier: classifier, archive: archive, clock: clock, serial: make(chan struct{}, 1)}
	s.cache = serviceCache{Coverage: Coverage{Phase: "catching_up", Cutover: s.historyStart(clock.Now(), 0)}, Threads: map[string]threadCache{}, Occurrences: map[string]Occurrence{}, Records: map[string]actionRecord{}, Rules: map[string]Snooze{}, WIPRules: map[string]WIPRule{}, WIPRevisions: map[string]string{}, WIPOverrides: map[string]string{}, Pending: map[string]pendingDelivery{}}
	return s, nil
}

// validateConfig permits the SHIP writer identity while keeping producers distinct.
func validateConfig(cfg Config) error {
	if cfg.HistoryWindow < 0 {
		return fmt.Errorf("%w: history window must be positive", ErrValidation)
	}
	u, err := url.Parse(cfg.WorkspaceURL)
	if cfg.WorkspaceID == "" || cfg.ChannelID == "" || cfg.Writer.UserID == "" && cfg.Writer.BotID == "" || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: workspace, channel, HTTPS workspace URL and writer user/bot identity required", ErrValidation)
	}
	writerIDs := sets.New[string]()
	for kind, id := range map[string]string{"user:": cfg.Writer.UserID, "bot:": cfg.Writer.BotID, "app:": cfg.Writer.AppID} {
		if id != "" {
			writerIDs.Insert(kind + id)
		}
	}
	owners := map[string]string{}
	names := sets.New[string]()
	for _, source := range cfg.Sources {
		if source.Name == "" || source.Name == "unknown" {
			return fmt.Errorf("%w: source name required and cannot be unknown", ErrValidation)
		}
		if names.Has(source.Name) {
			return fmt.Errorf("%w: duplicate source name", ErrValidation)
		}
		names.Insert(source.Name)
		switch source.Kind {
		case "alertmanager", "prow", "ship", "chai":
		default:
			return fmt.Errorf("%w: unknown source kind", ErrValidation)
		}
		if len(source.UserIDs)+len(source.BotIDs)+len(source.AppIDs) == 0 {
			return fmt.Errorf("%w: source identity required", ErrValidation)
		}
		for kind, ids := range map[string][]string{"user:": source.UserIDs, "bot:": source.BotIDs, "app:": source.AppIDs} {
			for _, id := range ids {
				if id == "" || strings.TrimSpace(id) != id {
					return fmt.Errorf("%w: blank or padded source identity", ErrValidation)
				}
				if writerIDs.Has(kind+id) && source.Kind != "ship" {
					return fmt.Errorf("%w: identity shared by writer and %s", ErrValidation, source.Name)
				}
				if owner, exists := owners[kind+id]; exists {
					return fmt.Errorf("%w: identity shared by %s and %s", ErrValidation, owner, source.Name)
				}
				owners[kind+id] = source.Name
			}
		}
		if len(source.IdentifierPattern) > 512 {
			return fmt.Errorf("%w: identifier pattern too long", ErrValidation)
		}
		if source.IdentifierPattern != "" {
			rx, err := regexp.Compile(source.IdentifierPattern)
			if err != nil || rx.MatchString("") {
				return fmt.Errorf("%w: invalid or empty-matching identifier pattern", ErrValidation)
			}
		}
	}
	return nil
}

// historyStart applies the optional go-live floor without extending the rolling window.
func (s *Service) historyStart(now time.Time, padding time.Duration) time.Time {
	start := now.Add(-s.cfg.HistoryWindow).Add(-padding)
	if s.cfg.Cutover.After(start) {
		start = s.cfg.Cutover
	}
	return start
}

// retained bounds both visible occurrences and the extra day needed for active snooze rules.
func (s *Service) retained(o Occurrence, now time.Time, padding time.Duration) bool {
	return !o.Message.ReportedAt.Before(s.historyStart(now, padding)) && !o.Message.ReportedAt.After(now)
}

// currentCache borrows an immutable published cache.
func (s *Service) currentCache() serviceCache {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cache
}

// publish swaps complete cache state without holding a lock across I/O.
func (s *Service) publish(cache serviceCache) {
	s.mu.Lock()
	previousIssues := s.conversationIssueCount(s.cache, s.clock.Now())
	issues := s.conversationIssueCount(cache, s.clock.Now())
	s.cache = cache
	s.mu.Unlock()
	if issues != previousIssues {
		if issues > 0 {
			logrus.WithField("affected_threads", issues).Warn("chat recovery incomplete; new chat requests paused")
		} else {
			logrus.Info("chat recovery complete; new chat requests resumed")
		}
	}
}

// degrade preserves the last complete queue while disabling writes.
func (s *Service) degrade(cache serviceCache, err error) error {
	cache.Coverage.Phase = "degraded"
	if cache.Coverage.LastSync.IsZero() {
		cache.Coverage.Phase = "catching_up"
	}
	cache.Coverage.Error = err.Error()
	s.publish(cache)
	return err
}

// Sync scans every retained parent and publishes only after complete thread replay.
func (s *Service) Sync(ctx context.Context) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	previous := s.currentCache()
	catchingUp := previous
	catchingUp.Coverage.Phase = "catching_up"
	s.publish(catchingUp)
	now := s.clock.Now()
	history, err := s.archive.History(ctx, s.cfg.ChannelID, s.historyStart(now, 24*time.Hour))
	if err != nil {
		return s.degrade(previous, fmt.Errorf("reading retained history: %w", err))
	}
	threads := map[string]threadCache{}
	for _, root := range history {
		if root.SubType == slack.MsgSubTypeMessageChanged && root.SubMessage != nil {
			root = slack.Message{Msg: *root.SubMessage}
		}
		occurrence, admitted, normalizeErr := s.normalize(root)
		if normalizeErr != nil {
			return s.degrade(previous, normalizeErr)
		}
		chatRoot, chatErr := s.conversationRoot(root, now)
		if chatErr != nil {
			return s.degrade(previous, chatErr)
		}
		if !chatRoot && (!admitted || !s.retained(occurrence, now, 24*time.Hour)) {
			continue
		}
		if existing, exists := threads[root.Timestamp]; exists {
			if root.Edited == nil || existing.Root.Edited != nil && !timestampLess(existing.Root.Edited.Timestamp, root.Edited.Timestamp) {
				continue
			}
		}
		thread, exists := previous.Threads[root.Timestamp]
		_, chatIssue := previous.ConversationIssues[root.Timestamp]
		if !exists || chatIssue || thread.Root.ReplyCount != root.ReplyCount || thread.Root.LatestReply != root.LatestReply {
			thread.Replies, err = s.archive.Replies(ctx, s.cfg.ChannelID, root.Timestamp)
			if err != nil {
				return s.degrade(previous, fmt.Errorf("reading thread %s: %w", root.Timestamp, err))
			}
			// Only an actual reply read advances the per-root fetch time and
			// resets memoized targeted-read failures; unchanged roots keep
			// their fetch time and any valid rate cooldown untouched.
			thread.FetchedAt = s.clock.Now()
			thread.RefreshFailed = false
			thread.RefreshAfter = time.Time{}
		}
		thread.Root = root
		threads[root.Timestamp] = thread
	}
	cache, err := s.replay(threads, previous.Pending)
	if err != nil {
		return s.degrade(previous, err)
	}
	now = s.clock.Now()
	cache.Coverage = Coverage{Phase: "ready", LastSync: now, Cutover: s.historyStart(now, 0)}
	cache.Decorations = previous.Decorations
	s.publish(cache)
	s.publish(s.decorate(ctx, cache))
	return nil
}

// replay treats trusted action records as truth and orders rules across all threads.
func (s *Service) replay(threads map[string]threadCache, pending map[string]pendingDelivery) (serviceCache, error) {
	cache := serviceCache{Threads: threads, Occurrences: map[string]Occurrence{}, Records: map[string]actionRecord{}, Rules: map[string]Snooze{}, WIPRules: map[string]WIPRule{}, WIPRevisions: map[string]string{}, WIPOverrides: map[string]string{}, Pending: maps.Clone(pending)}
	now := s.clock.Now()
	reset := nextSixUTC(now).Add(-24 * time.Hour)
	retainedThreads := map[string]threadCache{}
	var records []timedRecord
	investigationTimestamps := map[string]string{}
	for _, thread := range threads {
		o, admitted, err := s.normalize(thread.Root)
		if err != nil {
			return cache, err
		}
		chatRoot, chatErr := s.conversationRoot(thread.Root, now)
		if chatErr != nil {
			return cache, chatErr
		}
		if chatRoot {
			retainedThreads[thread.Root.Timestamp] = thread
			continue
		}
		if !admitted || !s.retained(o, now, 24*time.Hour) {
			continue
		}
		retainedThreads[thread.Root.Timestamp] = thread
		seen := sets.New[string]()
		replies := append([]slack.Message(nil), thread.Replies...)
		sort.Slice(replies, func(i, j int) bool { return timestampLess(replies[i].Timestamp, replies[j].Timestamp) })
		for _, reply := range replies {
			if reply.Timestamp == thread.Root.Timestamp || reply.SubType == slack.MsgSubTypeMessageDeleted {
				continue
			}
			if s.conversationCandidate(reply) {
				continue
			}
			if reply.ThreadTimestamp != "" && reply.ThreadTimestamp != thread.Root.Timestamp {
				return cache, fmt.Errorf("reply outside requested thread")
			}
			if seen.Has(reply.Timestamp) {
				continue
			}
			seen.Insert(reply.Timestamp)
			if _, err := slackTime(reply.Timestamp); err != nil {
				return cache, err
			}
			if matchesIdentity(reply, s.cfg.Writer) {
				record, exists, err := decodeMessageRecord(reply.Msg, o.Identity.ID)
				if err != nil {
					return cache, fmt.Errorf("trusted action at %s: %w", reply.Timestamp, err)
				}
				if exists {
					records = append(records, timedRecord{Timestamp: reply.Timestamp, Record: record})
					continue
				}
			}
			source, err := s.source(reply)
			if err != nil {
				return cache, err
			}
			o.Message.Replies = append(o.Message.Replies, Reply{Text: flatten(reply), User: reply.User, Timestamp: reply.Timestamp, IsChai: source != nil && source.Kind == "chai"})
		}
		cache.Occurrences[o.Identity.ID] = o
	}
	sort.Slice(records, func(i, j int) bool { return timestampLess(records[i].Timestamp, records[j].Timestamp) })
	for _, timed := range records {
		r := timed.Record
		if previous, exists := cache.Records[r.Request.RequestID]; exists {
			if !sameRecord(previous, r) {
				return cache, fmt.Errorf("conflicting durable request ID %s", r.Request.RequestID)
			}
			continue
		}
		o := cache.Occurrences[r.Request.OccurrenceID]
		o = enrichInvestigation(o, investigationTimestamps[o.Identity.ID], timed.Timestamp)
		// Rules expire by action time even when Slack confirms the post after reset.
		historicalRule := r.At.Before(reset)
		if err := validateTransition(o, r, cache, historicalRule); err != nil {
			return cache, fmt.Errorf("trusted action at %s: %w", timed.Timestamp, err)
		}
		o.State, o.Investigation, o.Identity.Revision = r.State, r.Investigation, timed.Timestamp
		cache.Occurrences[o.Identity.ID] = o
		cache.Records[r.Request.RequestID] = r
		delete(cache.Pending, r.Request.RequestID)
		if r.Request.Kind == "investigate" {
			investigationTimestamps[o.Identity.ID] = timed.Timestamp
			for requestID, attempt := range cache.Pending {
				if attempt.Request.OccurrenceID == r.Request.OccurrenceID && attempt.Request.Kind == "investigate" {
					delete(cache.Pending, requestID)
				}
			}
		}
		if r.WIPRule != nil {
			cache.WIPRules[r.WIPRule.ID] = *r.WIPRule
			cache.WIPRevisions[r.WIPRule.ID] = timed.Timestamp
		}
		if r.Unwip != "" {
			delete(cache.WIPRules, r.Unwip)
		}
		if (r.Request.Kind == "acknowledge" || r.Request.Kind == "unacknowledge") && r.Request.Scope != "regex" {
			cache.WIPOverrides[o.Identity.ID] = timed.Timestamp
		}
		if r.Snooze != nil && !historicalRule {
			cache.Rules[r.Snooze.ID] = *r.Snooze
		}
		if r.Unsnooze != "" && !historicalRule {
			delete(cache.Rules, r.Unsnooze)
		}
	}
	for id, o := range cache.Occurrences {
		cache.Occurrences[id] = enrichInvestigation(o, investigationTimestamps[o.Identity.ID], "")
	}
	for id, rule := range cache.WIPRules {
		if !now.Before(rule.Until) {
			delete(cache.WIPRules, id)
			delete(cache.WIPRevisions, id)
		}
	}
	cache.Threads = retainedThreads
	for id, attempt := range cache.Pending {
		if _, exists := cache.Occurrences[attempt.Request.OccurrenceID]; !exists {
			delete(cache.Pending, id)
		}
	}
	if err := s.replayConversations(&cache); err != nil {
		return cache, err
	}
	return cache, nil
}

// acquire lets timed-out requests leave the serialized archive queue immediately.
func (s *Service) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.serial <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release returns the archive serialization token.
func (s *Service) release() { <-s.serial }

// enrichInvestigation ties trusted replies to the most recent preceding request.
func enrichInvestigation(o Occurrence, requestTS, beforeTS string) Occurrence {
	if o.Investigation == nil || requestTS == "" {
		return o
	}
	investigation := *o.Investigation
	for _, reply := range o.Message.Replies {
		if reply.IsChai && timestampLess(requestTS, reply.Timestamp) && (beforeTS == "" || timestampLess(reply.Timestamp, beforeTS)) {
			investigation.Status, investigation.Reply = "reply_received", reply.Text
		}
	}
	o.Investigation = &investigation
	return o
}

// Snapshot evaluates rule expiry at read time and returns detached values.
func (s *Service) Snapshot() Snapshot {
	cache := s.currentCache()
	snapshot := Snapshot{Status: cache.Coverage, Occurrences: []Occurrence{}}
	now := s.clock.Now()
	snapshot.Status.Cutover = s.historyStart(now, 0)
	for _, occurrence := range cache.Occurrences {
		if !s.retained(occurrence, now, 0) {
			continue
		}
		snapshot.Occurrences = append(snapshot.Occurrences, effectiveOccurrence(occurrence, cache, now))
	}
	sort.Slice(snapshot.Occurrences, func(i, j int) bool {
		return timestampLess(snapshot.Occurrences[j].Identity.Timestamp, snapshot.Occurrences[i].Identity.Timestamp)
	})
	return snapshot
}

// effectiveOccurrence overlays active rules without changing the workflow history.
func effectiveOccurrence(o Occurrence, cache serviceCache, now time.Time) Occurrence {
	o.Message.Identifiers = append([]string{}, o.Message.Identifiers...)
	o.Message.PrometheusAlerts = append([]string(nil), o.Message.PrometheusAlerts...)
	o.Message.SOPURLs = append([]string(nil), o.Message.SOPURLs...)
	o.Message.Replies = append([]Reply{}, o.Message.Replies...)
	if o.State.AcknowledgedAt != nil {
		value := *o.State.AcknowledgedAt
		o.State.AcknowledgedAt = &value
	}
	if o.State.ClosedAt != nil {
		value := *o.State.ClosedAt
		o.State.ClosedAt = &value
	}
	if o.Investigation != nil {
		value := *o.Investigation
		o.Investigation = &value
	}
	for _, pending := range cache.Pending {
		if pending.Request.OccurrenceID == o.Identity.ID && pending.Investigation != nil {
			value := *pending.Investigation
			o.Investigation = &value
		}
	}
	o.View, o.Snooze, o.WIPRule = "feed", nil, nil
	for _, rule := range cache.WIPRules {
		revision := cache.WIPRevisions[rule.ID]
		override := cache.WIPOverrides[o.Identity.ID]
		if !now.Before(rule.Until) || rule.Channel != o.Identity.Channel || rule.Source != o.Message.Source || override != "" && !timestampLess(override, revision) || !matchesPattern(rule.Pattern, o.Message.Identifiers) {
			continue
		}
		if o.WIPRule == nil || timestampLess(cache.WIPRevisions[o.WIPRule.ID], revision) {
			value := rule
			o.WIPRule = &value
		}
	}
	if o.WIPRule != nil && o.State.ClosedAt == nil {
		at := cache.Records[o.WIPRule.ID].At
		o.State.AcknowledgedBy, o.State.AcknowledgedAt = o.WIPRule.Actor, &at
	}
	if o.State.AcknowledgedAt != nil {
		o.View = "acknowledged"
	}
	for _, rule := range cache.Rules {
		if !now.Before(rule.Until) || rule.Channel != o.Identity.Channel || rule.Source != o.Message.Source {
			continue
		}
		matched := rule.Scope == "occurrence" && rule.OccurrenceID == o.Identity.ID || rule.Scope != "occurrence" && matchesPattern(rule.Pattern, o.Message.Identifiers)
		if matched && (o.Snooze == nil || rule.ID > o.Snooze.ID) {
			value := rule
			o.Snooze = &value
			o.View = "snoozed"
		}
	}
	if o.State.ClosedAt != nil {
		o.View = "closed"
	}
	if o.Classification.Category == "chai_reports" {
		o.View = "chai_reports"
	}
	baseRevision := o.Identity.Revision
	if raw, exists := cache.Occurrences[o.Identity.ID]; exists {
		baseRevision = raw.Identity.Revision
	}
	ruleID := ""
	if o.WIPRule != nil {
		ruleID = o.WIPRule.ID
	}
	o.Identity.Revision = workflowRevision(baseRevision, ruleID)
	return o
}

// workflowRevision binds optimistic concurrency to the displayed family assignment.
func workflowRevision(baseRevision, ruleID string) string {
	if ruleID == "" {
		return baseRevision
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(baseRevision+"\x00"+ruleID)))
}

// matchesPattern evaluates validated durable rules against stable identifiers.
func matchesPattern(pattern string, identifiers []string) bool {
	rx, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	for _, identifier := range identifiers {
		if rx.MatchString(identifier) {
			return true
		}
	}
	return false
}

// reactionPresence distinguishes uncertain startup metadata from confirmed writes.
type reactionPresence uint8

// Reaction states retain successful writes even when another icon fails.
const (
	reactionUnknown reactionPresence = iota
	reactionAbsent
	reactionPresent
)

// decorationState bounds optional writes independently of durable workflow actions.
type decorationState struct {
	Icons    map[string][3]reactionPresence
	RetryAt  time.Time
	Disabled bool
}

// initialReactions uses names as removal candidates, since Slack can truncate users.
func initialReactions(root slack.Message, writer string) [3]reactionPresence {
	states := [3]reactionPresence{reactionAbsent, reactionAbsent, reactionAbsent}
	for i, emoji := range []string{"eyes", "zzz", "white_check_mark"} {
		for _, reaction := range root.Reactions {
			if reaction.Name != emoji {
				continue
			}
			states[i] = reactionUnknown
			if writer != "" && slices.Contains(reaction.Users, writer) {
				states[i] = reactionPresent
			}
		}
	}
	return states
}

// decorate reconciles only this writer's optional icons, never durable state.
func (s *Service) decorate(ctx context.Context, cache serviceCache) serviceCache {
	state := cache.Decorations
	state.Icons = maps.Clone(state.Icons)
	if state.Icons == nil {
		state.Icons = map[string][3]reactionPresence{}
	}
	for id := range state.Icons {
		if o, exists := cache.Occurrences[id]; !exists || !s.retained(o, s.clock.Now(), 0) {
			delete(state.Icons, id)
		}
	}
	cache.Decorations = state
	if state.Disabled || s.clock.Now().Before(state.RetryAt) {
		return cache
	}
	now := s.clock.Now()
	for id, occurrence := range cache.Occurrences {
		if ctx.Err() != nil {
			return cache
		}
		if !s.retained(occurrence, now, 0) {
			continue
		}
		o := effectiveOccurrence(occurrence, cache, now)
		if o.View == "chai_reports" {
			continue
		}
		desired := ""
		switch o.View {
		case "acknowledged":
			desired = "eyes"
		case "snoozed":
			desired = "zzz"
		case "closed":
			desired = "white_check_mark"
		}
		icons, known := state.Icons[id]
		if !known {
			icons = initialReactions(cache.Threads[o.Identity.Timestamp].Root, s.cfg.Writer.UserID)
		}
		state.Icons[id] = icons
		for i, emoji := range []string{"eyes", "zzz", "white_check_mark"} {
			want := reactionAbsent
			if emoji == desired {
				want = reactionPresent
			}
			if icons[i] == want {
				continue
			}
			if ctx.Err() != nil {
				return cache
			}
			if err := s.archive.SetReaction(ctx, o.Identity.Channel, o.Identity.Timestamp, emoji, emoji == desired); err != nil {
				var limited *slack.RateLimitedError
				if errors.As(err, &limited) {
					cache.Decorations.RetryAt = s.clock.Now().Add(limited.RetryAfter)
				}
				message := "could not reconcile dashboard reaction"
				var response slack.SlackErrorResponse
				if errors.As(err, &response) && response.Err == "missing_scope" {
					cache.Decorations.Disabled = true
					message = "dashboard reaction updates disabled until restart: missing reactions:write scope"
				}
				logrus.WithError(err).WithFields(logrus.Fields{"channel": o.Identity.Channel, "timestamp": o.Identity.Timestamp, "emoji": emoji, "present": emoji == desired}).Warn(message)
				if limited != nil || cache.Decorations.Disabled || ctx.Err() != nil {
					return cache
				}
				continue
			}
			icons[i] = want
			state.Icons[id] = icons
		}
	}
	return cache
}
