package opsalerts

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"maps"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/util/sets"
)

// ConversationMetadataEvent separates chat requests from workflow receipts.
const ConversationMetadataEvent = "ops_dashboard_conversation_v1"

// ThreadReplyMetadataEvent keeps ordinary discussion replies separate from Chai requests.
const ThreadReplyMetadataEvent = "ops_dashboard_thread_reply_v1"

// threadReplyHeader survives stripped metadata without resembling a workflow receipt.
const threadReplyHeader = "Dashboard reply (on behalf of an authenticated operator):\n\n"

// threadReplyPrefix recognizes attributed replies even if their metadata is stripped.
const threadReplyPrefix = "Dashboard reply (on behalf of "

const conversationFooter = "\n\n💬 Dashboard · Chai"

// ConversationRequest binds a stable delivery key to one operator question.
type ConversationRequest struct {
	ConversationID string `json:"conversation_id,omitempty"`
	OccurrenceID   string `json:"occurrence_id,omitempty"`
	RequestID      string `json:"request_id"`
	Text           string `json:"text"`
}

// ConversationMessage exposes readable evidence without Slack metadata or credentials.
type ConversationMessage struct {
	Timestamp  string `json:"timestamp"`
	Text       string `json:"text"`
	Author     string `json:"author"`
	IsChai     bool   `json:"is_chai"`
	IsOperator bool   `json:"is_operator"`
	RequestID  string `json:"request_id,omitempty"`
}

// Conversation groups retained questions and replies in one Slack thread.
type Conversation struct {
	ID                 string                `json:"id"`
	Title              string                `json:"title"`
	SlackURL           string                `json:"slack_url"`
	SourceOccurrenceID string                `json:"source_occurrence_id,omitempty"`
	CreatedAt          time.Time             `json:"created_at"`
	UpdatedAt          time.Time             `json:"updated_at"`
	Messages           []ConversationMessage `json:"messages"`
}

// ConversationSnapshot is a detached cache-only view of retained chats.
type ConversationSnapshot struct {
	Status        Coverage       `json:"status"`
	Conversations []Conversation `json:"conversations"`
	Enabled       bool           `json:"enabled"`
}

// conversationRecordRequest omits question text so Slack metadata stays bounded.
type conversationRecordRequest struct {
	ConversationID string `json:"conversation_id,omitempty"`
	OccurrenceID   string `json:"occurrence_id,omitempty"`
	RequestID      string `json:"request_id"`
	Text           string `json:"-"`
}

// conversationRecord retains the exact original request and its server-thread binding.
type conversationRecord struct {
	Mode               string                    `json:"mode,omitempty"`
	Version            int                       `json:"version"`
	ChaiUser           string                    `json:"chai_user"`
	TextHash           string                    `json:"text_hash"`
	Request            conversationRecordRequest `json:"request"`
	Actor              string                    `json:"actor"`
	At                 time.Time                 `json:"at"`
	Root               string                    `json:"root,omitempty"`
	SourceOccurrenceID string                    `json:"source_occurrence_id,omitempty"`
}

// conversationAttempt blocks retries when Slack has not confirmed delivery.
type conversationAttempt struct {
	Record conversationRecord
	Failed bool
}

// validateConversation bounds operator-controlled fields before serialization.
func validateConversation(actor string, req ConversationRequest) error {
	if strings.TrimSpace(actor) != actor || actor == "" || len(actor) > 256 || !utf8.ValidString(actor) || len(req.ConversationID) > 256 || len(req.OccurrenceID) > 256 || req.ConversationID != "" && req.OccurrenceID != "" || len(req.RequestID) > 128 || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(req.RequestID) || req.Text == "" || strings.TrimSpace(req.Text) != req.Text || len(req.Text) > 4000 || !utf8.ValidString(req.Text) {
		return fmt.Errorf("%w: missing, invalid or oversized conversation fields", ErrValidation)
	}
	for _, r := range actor {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: actor contains control characters", ErrValidation)
		}
	}
	for _, r := range req.Text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return fmt.Errorf("%w: conversation contains control characters", ErrValidation)
		}
	}
	return nil
}

// decodeConversation accepts only a small, versioned metadata record from a verified writer.
func (s *Service) decodeConversation(message slack.Message) (conversationRecord, error) {
	var record conversationRecord
	raw, ok := message.Metadata.EventPayload["record"].(string)
	envelope, err := json.Marshal(message.Metadata)
	if err != nil || (message.Metadata.EventType != ConversationMetadataEvent && message.Metadata.EventType != ThreadReplyMetadataEvent) || !ok || len(message.Metadata.EventPayload) != 1 || len(envelope) > MaxActionMetadataBytes || strings.ContainsAny(raw, "\r\n") {
		return record, fmt.Errorf("invalid conversation metadata envelope")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, fmt.Errorf("decoding conversation: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return record, fmt.Errorf("trailing conversation metadata")
	}
	if message.Metadata.EventType == ThreadReplyMetadataEvent {
		if err := strictPlainRecord(json.NewDecoder(strings.NewReader(raw)), false); err != nil {
			return record, fmt.Errorf("invalid thread reply metadata: %w", err)
		}
	}
	if record.Mode != "reply" && (record.Mode != "" || message.Metadata.EventType != ConversationMetadataEvent || !regexp.MustCompile(`^[A-Za-z0-9_]{1,128}$`).MatchString(record.ChaiUser)) {
		return record, fmt.Errorf("invalid conversation Chai user")
	}
	text, found := strings.CutPrefix(message.Text, "<@"+record.ChaiUser+"> ")
	if record.Mode == "reply" {
		if message.Metadata.EventType != ThreadReplyMetadataEvent || record.ChaiUser != "" || record.Root == "" || record.Request.OccurrenceID != s.cfg.ChannelID+":"+record.Root || record.SourceOccurrenceID != record.Request.OccurrenceID || record.Request.ConversationID != "" {
			return record, fmt.Errorf("invalid thread reply association")
		}
		text, found = threadReplyDisplayBody(message.Text)
	} else if record.Root == "" {
		text, found = trimConversationFooter(text)
		found = found && strings.HasPrefix(message.Text, "<@"+record.ChaiUser+"> ")
	}
	record.Request.Text = text
	if record.Mode != "reply" {
		record.Request.Text = html.UnescapeString(text)
	}
	if !found || record.Mode != "reply" && record.TextHash != fmt.Sprintf("%x", sha256.Sum256([]byte(record.Request.Text))) || record.Mode == "reply" && !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(record.TextHash) {
		return record, fmt.Errorf("conversation text does not match metadata")
	}
	validationRequest := ConversationRequest(record.Request)
	if record.Mode == "reply" {
		// Slack canonicalizes emoji and URLs and allows later edits. The immutable
		// digest confirms the submission; the current framed body is display only.
		validationRequest.Text = "reply"
	}
	if err := validateConversation(record.Actor, validationRequest); err != nil {
		return record, err
	}
	if record.Version != 1 || record.At.IsZero() || len(record.Root) > 64 || len(record.SourceOccurrenceID) > 256 {
		return record, fmt.Errorf("invalid conversation version, time or root")
	}
	messageAt, err := slackTime(message.Timestamp)
	if err != nil {
		return record, err
	}
	if record.At.Before(messageAt.Add(-5*time.Minute)) || record.At.After(messageAt.Add(5*time.Minute)) {
		return record, fmt.Errorf("conversation time outside delivery window")
	}
	root := message.ThreadTimestamp
	if root == "" || root == message.Timestamp {
		if record.Root != "" || record.SourceOccurrenceID != "" || record.Request.ConversationID != "" || record.Request.OccurrenceID != "" {
			return record, fmt.Errorf("invalid standalone conversation association")
		}
	} else {
		if record.Root != root || record.Request.ConversationID == "" && record.Request.OccurrenceID == "" {
			return record, fmt.Errorf("conversation outside associated thread")
		}
		if _, err := slackTime(record.Root); err != nil {
			return record, err
		}
	}
	return record, nil
}

// trimConversationFooter accepts the posted footer and Slack's emoji alias
// representation, leaving the operator's question unchanged for hash validation.
func trimConversationFooter(text string) (string, bool) {
	for _, footer := range []string{conversationFooter, "\n\n:speech_balloon: Dashboard · Chai"} {
		if question, found := strings.CutSuffix(text, footer); found {
			return question, true
		}
	}
	return text, false
}

// conversationCandidate preserves recovery evidence even when chat metadata is unreadable.
// Workflow receipt surfaces take precedence over mention-only chat recognition.
func (s *Service) conversationCandidate(m slack.Message) bool {
	if !matchesIdentity(m, s.cfg.Writer) || m.Hidden || m.SubType != "" && m.SubType != slack.MsgSubTypeBotMessage {
		return false
	}
	if m.Metadata.EventType == ThreadReplyMetadataEvent {
		return true
	}
	if m.Metadata.EventType == ActionMetadataEvent {
		return false
	}
	if strings.HasPrefix(m.Text, threadReplyPrefix) {
		return true
	}
	if strings.HasPrefix(m.Text, ActionReceiptPrefix) || strings.HasPrefix(m.Text, ActionMarker) {
		return false
	}
	// Legacy investigations start with a Chai mention and put their action
	// record on a later line. Explicit chat metadata can quote that marker.
	if m.Metadata.EventType != ConversationMetadataEvent && strings.Contains(m.Text, "\n"+ActionMarker) {
		return false
	}
	_, footer := trimConversationFooter(m.Text)
	return m.Metadata.EventType == ConversationMetadataEvent || footer || regexp.MustCompile(`^<@[A-Za-z0-9_]{1,128}> `).MatchString(m.Text)
}

// conversationRoot retains candidate standalone chats independently of decoding health.
func (s *Service) conversationRoot(m slack.Message, now time.Time) (bool, error) {
	if !s.conversationCandidate(m) || m.ThreadTimestamp != "" && m.ThreadTimestamp != m.Timestamp {
		return false, nil
	}
	at, err := slackTime(m.Timestamp)
	if err != nil {
		return false, err
	}
	return !at.Before(s.historyStart(now, 0)) && !at.After(now), nil
}

// conversationIssueCount applies the rolling window to chat-only recovery guards.
func (s *Service) conversationIssueCount(cache serviceCache, now time.Time) int {
	count := 0
	for _, at := range cache.ConversationIssues {
		if !at.Before(s.historyStart(now, 0)) && !at.After(now) {
			count++
		}
	}
	return count
}

// markConversationIssue keeps one recovery issue per affected thread without retaining errors.
func markConversationIssue(cache *serviceCache, root string, at time.Time) {
	if at.After(cache.ConversationIssues[root]) {
		cache.ConversationIssues[root] = at
	}
}

// Conversations returns copies and applies the rolling history window at read time.
func (s *Service) Conversations() ConversationSnapshot {
	cache := s.currentCache()
	result := ConversationSnapshot{Status: cache.Coverage, Enabled: s.chaiUser() != "", Conversations: []Conversation{}}
	now := s.clock.Now()
	result.Status.Cutover = s.historyStart(now, 0)
	if s.conversationIssueCount(cache, now) > 0 {
		result.Status.Phase = "degraded"
		result.Status.Error = "chat recovery incomplete; new chat requests are paused"
	}
	for _, conversation := range cache.Conversations {
		if conversation.CreatedAt.Before(s.historyStart(now, 0)) || conversation.CreatedAt.After(now) {
			continue
		}
		conversation.Messages = append([]ConversationMessage{}, conversation.Messages...)
		result.Conversations = append(result.Conversations, conversation)
	}
	sort.Slice(result.Conversations, func(i, j int) bool { return result.Conversations[i].UpdatedAt.After(result.Conversations[j].UpdatedAt) })
	return result
}

// replayConversations rebuilds chat history solely from retained trusted records.
func (s *Service) replayConversations(cache *serviceCache) error {
	cache.Conversations = map[string]Conversation{}
	cache.ConversationRecords = map[string]conversationRecord{}
	cache.ConversationIssues = map[string]time.Time{}
	conflicts := sets.New[string]()
	requestTimes := map[string]time.Time{}
	cache.ConversationPending = maps.Clone(s.currentCache().ConversationPending)
	now := s.clock.Now()
	for id, attempt := range cache.ConversationPending {
		if attempt.Record.At.Before(s.historyStart(now, 0)) {
			delete(cache.ConversationPending, id)
		}
	}
	for root, thread := range cache.Threads {
		rootAt, err := slackTime(root)
		if err != nil {
			return err
		}
		if rootAt.Before(s.historyStart(now, 0)) || rootAt.After(now) {
			continue
		}
		id := s.cfg.ChannelID + ":" + root
		conversation := Conversation{ID: id, SlackURL: strings.TrimRight(s.cfg.WorkspaceURL, "/") + "/archives/" + url.PathEscape(s.cfg.ChannelID) + "/p" + strings.ReplaceAll(root, ".", ""), CreatedAt: rootAt, UpdatedAt: rootAt, Messages: []ConversationMessage{}}
		if occurrence, exists := cache.Occurrences[id]; exists {
			conversation.SourceOccurrenceID = id
			conversation.Title = conversationTitle(strings.Join(occurrence.Message.Identifiers, " · "))
			if conversation.Title == "" {
				conversation.Title = conversationTitle(occurrence.Message.Title)
			}
		}
		messages := append([]slack.Message{thread.Root}, thread.Replies...)
		for i := range messages {
			messages[i] = visibleThreadMessage(messages[i])
		}
		sort.Slice(messages, func(i, j int) bool { return timestampLess(messages[i].Timestamp, messages[j].Timestamp) })
		seen := sets.New[string]()
		hasRequest := false
		for _, message := range messages {
			if seen.Has(message.Timestamp) {
				continue
			}
			seen.Insert(message.Timestamp)
			if message.Hidden || message.SubType == slack.MsgSubTypeMessageDeleted {
				continue
			}
			if message.ThreadTimestamp != "" && message.ThreadTimestamp != root {
				if s.conversationCandidate(message) {
					markConversationIssue(cache, root, rootAt)
					continue
				}
				return fmt.Errorf("conversation reply outside requested thread")
			}
			at, err := slackTime(message.Timestamp)
			if err != nil {
				return err
			}
			if at.Before(s.historyStart(now, 0)) || at.After(now) {
				continue
			}
			item := ConversationMessage{Timestamp: message.Timestamp, Text: flatten(message), Author: message.User}
			if matchesIdentity(message, s.cfg.Writer) {
				if s.conversationCandidate(message) {
					record, err := s.decodeConversation(message)
					if err != nil {
						markConversationIssue(cache, root, at)
						continue
					}
					if record.SourceOccurrenceID != conversation.SourceOccurrenceID || record.Request.ConversationID != "" && record.Request.ConversationID != id || record.Request.OccurrenceID != "" && record.Request.OccurrenceID != id {
						markConversationIssue(cache, root, at)
						continue
					}
					if _, exists := cache.Records[record.Request.RequestID]; exists {
						markConversationIssue(cache, root, at)
						continue
					}
					record.Root = root
					if previous, exists := cache.ConversationRecords[record.Request.RequestID]; exists {
						if at.After(requestTimes[record.Request.RequestID]) {
							requestTimes[record.Request.RequestID] = at
						}
						if !sameConversationRecord(previous, record) {
							issueAt := requestTimes[record.Request.RequestID]
							markConversationIssue(cache, root, issueAt)
							markConversationIssue(cache, previous.Root, issueAt)
							conflicts.Insert(record.Request.RequestID)
							delete(cache.ConversationRecords, record.Request.RequestID)
						}
						continue
					}
					if conflicts.Has(record.Request.RequestID) {
						markConversationIssue(cache, root, at)
						continue
					}
					cache.ConversationRecords[record.Request.RequestID] = record
					requestTimes[record.Request.RequestID] = at
					item.Text = record.Request.Text
					item.Author = record.Actor
					item.IsOperator = true
					item.RequestID = record.Request.RequestID
					hasRequest = hasRequest || record.Mode != "reply"
					if conversation.Title == "" {
						conversation.Title = conversationTitle(record.Request.Text)
					}
				} else {
					record, exists, err := decodeMessageRecord(message.Msg, id)
					if err != nil {
						return err
					}
					if !exists || record.Request.Kind != "investigate" {
						continue
					}
					item.Text = record.Request.Question
					item.Author = record.Actor
					item.IsOperator = true
					item.RequestID = record.Request.RequestID
					hasRequest = true
				}
			} else {
				source, err := s.source(message)
				if err != nil {
					return err
				}
				item.IsChai = source != nil && source.Kind == "chai"
				if !item.IsChai && (message.User == "" || message.BotID != "" || messageAppID(message) != "" || source != nil) {
					continue
				}
			}
			if strings.TrimSpace(item.Text) == "" {
				continue
			}
			conversation.Messages = append(conversation.Messages, item)
			if at.After(conversation.UpdatedAt) {
				conversation.UpdatedAt = at
			}
		}
		if hasRequest {
			cache.Conversations[id] = conversation
		}
	}
	for id, conversation := range cache.Conversations {
		messages := make([]ConversationMessage, 0, len(conversation.Messages))
		hasRequest := false
		for _, message := range conversation.Messages {
			if conflicts.Has(message.RequestID) {
				continue
			}
			record := cache.ConversationRecords[message.RequestID]
			hasRequest = hasRequest || message.IsOperator && record.Mode != "reply"
			messages = append(messages, message)
		}
		if !hasRequest {
			delete(cache.Conversations, id)
			continue
		}
		conversation.Messages = messages
		cache.Conversations[id] = conversation
	}
	for id := range cache.ConversationRecords {
		delete(cache.ConversationPending, id)
	}
	return nil
}

// conversationTitle keeps the conversation picker compact without cutting UTF-8.
func conversationTitle(text string) string {
	title, _, _ := strings.Cut(text, "\n")
	chars := []rune(title)
	if len(chars) > 100 {
		title = string(chars[:100]) + "…"
	}
	return title
}

// conversationMessage keeps Slack text conversational and durable data in metadata.
func (s *Service) conversationMessage(record conversationRecord, channelName, replyBody string) (slack.Msg, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return slack.Msg{}, err
	}
	text := "<@" + record.ChaiUser + "> " + escapeSlack(record.Request.Text)
	if record.Root == "" {
		text += conversationFooter
	}
	message := slack.Msg{Text: text, Metadata: slack.SlackMetadata{EventType: ConversationMetadataEvent, EventPayload: map[string]interface{}{"record": string(data)}}}
	if record.Mode == "reply" {
		body := replyBody
		if body == "" {
			var err error
			body, err = s.threadReplyBody(record.Request.Text, channelName)
			if err != nil {
				return slack.Msg{}, err
			}
		}
		message.Text = attributedThreadReplyHeader(record.Actor) + body
		message.Metadata.EventType = ThreadReplyMetadataEvent
	}
	envelope, err := json.Marshal(message.Metadata)
	if err != nil {
		return slack.Msg{}, err
	}
	if len(envelope) > MaxActionMetadataBytes {
		return slack.Msg{}, fmt.Errorf("%w: conversation metadata exceeds application budget", ErrValidation)
	}
	return message, nil
}

// Converse serializes writes with archive recovery and never retries an uncertain post.
func (s *Service) Converse(ctx context.Context, actor string, req ConversationRequest) (Conversation, error) {
	return s.postConversation(ctx, actor, req, false, false, "")
}

// postConversation shares serialization and durable delivery guards across the two explicit write modes.
func (s *Service) postConversation(ctx context.Context, actor string, req ConversationRequest, plain, reconcileOnly bool, channelName string) (Conversation, error) {
	if !plain {
		req.Text = strings.TrimSpace(req.Text)
	}
	validationRequest := req
	if plain {
		validationRequest.Text = strings.TrimSpace(req.Text)
	}
	if err := validateConversation(actor, validationRequest); err != nil {
		return Conversation{}, err
	}
	if !plain && !regexp.MustCompile(`^[A-Za-z0-9_]{1,128}$`).MatchString(s.chaiUser()) {
		return Conversation{}, fmt.Errorf("%w: configured Chai user identity required", ErrValidation)
	}
	replyBody := ""
	if plain && !reconcileOnly {
		var err error
		replyBody, err = s.prepareThreadReply(ctx, req, channelName)
		if err != nil {
			return Conversation{}, err
		}
	}
	if err := s.acquire(ctx); err != nil {
		return Conversation{}, errors.Join(ErrNotReady, err)
	}
	defer s.release()
	cache := s.currentCache()
	if !plain && cache.Coverage.Phase != "ready" {
		return Conversation{}, ErrNotReady
	}
	if _, exists := cache.Pending[req.RequestID]; exists {
		return Conversation{}, ErrConflict
	}
	if _, exists := cache.Records[req.RequestID]; exists {
		return Conversation{}, ErrConflict
	}
	if previous, exists := cache.ConversationRecords[req.RequestID]; exists {
		if !matchesConversationRequest(previous, actor, req, plain) {
			return Conversation{}, ErrConflict
		}
		if plain {
			if o, exists := cache.Occurrences[req.OccurrenceID]; !exists || !s.retained(o, s.clock.Now(), 0) {
				return Conversation{}, ErrNotFound
			}
			return Conversation{ID: req.OccurrenceID}, nil
		}
		for _, c := range cache.Conversations {
			for _, m := range c.Messages {
				if m.RequestID == req.RequestID {
					if c.CreatedAt.Before(s.historyStart(s.clock.Now(), 0)) {
						return Conversation{}, ErrNotFound
					}
					return detachedConversation(c), nil
				}
			}
		}
		return Conversation{}, ErrNotFound
	}
	if attempt, exists := cache.ConversationPending[req.RequestID]; exists {
		if !matchesConversationRequest(attempt.Record, actor, req, plain) {
			return Conversation{}, ErrConflict
		}
		if !attempt.Failed {
			return Conversation{}, ErrDeliveryUncertain
		}
	}
	if reconcileOnly {
		return Conversation{}, ErrDeliveryUncertain
	}
	if plain && cache.Coverage.Phase != "ready" {
		return Conversation{}, ErrNotReady
	}
	if s.conversationIssueCount(cache, s.clock.Now()) > 0 {
		return Conversation{}, ErrNotReady
	}
	root := ""
	source := ""
	if req.ConversationID != "" {
		conversation, exists := cache.Conversations[req.ConversationID]
		if !exists || conversation.CreatedAt.Before(s.historyStart(s.clock.Now(), 0)) {
			return Conversation{}, ErrNotFound
		}
		root = strings.TrimPrefix(conversation.ID, s.cfg.ChannelID+":")
		source = conversation.SourceOccurrenceID
	}
	if req.OccurrenceID != "" {
		occurrence, exists := cache.Occurrences[req.OccurrenceID]
		if !exists || !s.retained(occurrence, s.clock.Now(), 0) {
			return Conversation{}, ErrNotFound
		}
		root = occurrence.Identity.Timestamp
		source = occurrence.Identity.ID
	}
	for _, attempt := range cache.ConversationPending {
		if !attempt.Failed && (root != "" && attempt.Record.Root == root || root == "" && attempt.Record.Root == "" && attempt.Record.Actor == actor) {
			return Conversation{}, ErrDeliveryUncertain
		}
	}
	record := conversationRecord{Version: 1, Request: conversationRecordRequest(req), ChaiUser: s.chaiUser(), TextHash: fmt.Sprintf("%x", sha256.Sum256([]byte(req.Text))), Actor: actor, At: s.clock.Now().UTC(), Root: root, SourceOccurrenceID: source}
	if plain {
		record.Mode, record.ChaiUser = "reply", ""
	}
	message, err := s.conversationMessage(record, channelName, replyBody)
	if err != nil {
		return Conversation{}, err
	}
	if err := ctx.Err(); err != nil {
		return Conversation{}, errors.Join(ErrNotReady, err)
	}
	ts, err := s.archive.Post(ctx, s.cfg.ChannelID, root, req.RequestID, message)
	if err == nil {
		_, err = slackTime(ts)
	}
	if err != nil {
		return Conversation{}, s.failedConversation(cache, record, err)
	}
	threads := maps.Clone(cache.Threads)
	saved := slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: root, User: s.cfg.Writer.UserID, BotID: s.cfg.Writer.BotID, Text: message.Text, Metadata: message.Metadata}}
	if root == "" {
		// A brand-new standalone root holds only the posted message, and no
		// complete-thread read has happened yet.
		root = ts
		threads[root] = threadCache{Root: saved}
	} else {
		saveThreadReply(threads, root, ts, saved)
	}
	updated, err := s.replay(threads, cache.Pending)
	if err != nil {
		deliveryErr := s.failedConversation(cache, record, fmt.Errorf("replaying saved conversation: %w", err))
		return Conversation{}, s.degrade(s.currentCache(), deliveryErr)
	}
	conversation, exists := updated.Conversations[s.cfg.ChannelID+":"+root]
	if plain {
		_, exists = updated.ConversationRecords[req.RequestID]
		conversation = Conversation{ID: req.OccurrenceID}
	}
	if !exists {
		return Conversation{}, s.failedConversation(cache, record, fmt.Errorf("saved conversation outside retained window"))
	}
	updated.Coverage = cache.Coverage
	updated.Decorations = cache.Decorations
	s.publish(updated)
	return detachedConversation(conversation), nil
}

// matchesConversationRequest uses the immutable original hash for Slack-normalized plain replies.
func matchesConversationRequest(record conversationRecord, actor string, req ConversationRequest, plain bool) bool {
	if record.Actor != actor || (record.Mode == "reply") != plain {
		return false
	}
	if !plain {
		return ConversationRequest(record.Request) == req
	}
	return record.Request.RequestID == req.RequestID && record.Request.OccurrenceID == req.OccurrenceID && record.Request.ConversationID == req.ConversationID && record.TextHash == fmt.Sprintf("%x", sha256.Sum256([]byte(req.Text)))
}

// sameConversationRecord ignores Slack's mutable plain reply presentation, not its submission ledger.
func sameConversationRecord(a, b conversationRecord) bool {
	if a.Mode != "reply" || b.Mode != "reply" {
		return a == b
	}
	a.Request.Text, b.Request.Text = "", ""
	a.At, b.At = time.Time{}, time.Time{}
	return a == b
}

// strictPlainRecord checks canonical plain-only schemas, leaving legacy Chai decoding unchanged.
func strictPlainRecord(decoder *json.Decoder, request bool) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("plain ledger must be an object")
	}
	keys := sets.New[string]()
	allowed := sets.New("mode", "version", "chai_user", "text_hash", "request", "actor", "at", "root", "source_occurrence_id")
	if request {
		allowed = sets.New("conversation_id", "occurrence_id", "request_id")
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || !allowed.Has(name) || keys.Has(name) {
			return fmt.Errorf("duplicate or noncanonical plain ledger field")
		}
		keys.Insert(name)
		if name == "request" {
			if err := strictPlainRecord(decoder, true); err != nil {
				return err
			}
			continue
		}
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if name == "version" {
			if _, ok := value.(float64); !ok {
				return fmt.Errorf("invalid plain ledger version")
			}
			continue
		}
		if _, ok := value.(string); !ok {
			return fmt.Errorf("plain ledger fields must be strings")
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return fmt.Errorf("invalid plain ledger object")
	}
	if request && !keys.HasAll("occurrence_id", "request_id") || !request && !keys.HasAll("mode", "version", "chai_user", "text_hash", "request", "actor", "at", "root", "source_occurrence_id") {
		return fmt.Errorf("incomplete plain ledger")
	}
	return nil
}

// failedConversation preserves a delivery guard until trusted archive replay confirms it.
func (s *Service) failedConversation(cache serviceCache, record conversationRecord, err error) error {
	cache.ConversationPending = maps.Clone(cache.ConversationPending)
	if cache.ConversationPending == nil {
		cache.ConversationPending = map[string]conversationAttempt{}
	}
	failed := errors.Is(err, ErrDeliveryFailed)
	cache.ConversationPending[record.Request.RequestID] = conversationAttempt{Record: record, Failed: failed}
	s.publish(cache)
	sentinel := ErrDeliveryUncertain
	if failed {
		sentinel = ErrDeliveryFailed
	}
	return fmt.Errorf("%w: %v", sentinel, err)
}

// detachedConversation prevents callers from mutating published message slices.
func detachedConversation(c Conversation) Conversation {
	c.Messages = append([]ConversationMessage{}, c.Messages...)
	return c
}

// conversationRefreshBudget is the shared global pause between any two targeted Slack reads.
const conversationRefreshBudget = 5 * time.Second

// conversationPollInterval is the per-thread pause between background refreshes.
const conversationPollInterval = 10 * time.Second

// conversationInterest bounds background refreshes to recently viewed threads.
type conversationInterest struct {
	Until time.Time
	Next  time.Time
}

// conversationActivity is guarded by Service.mu independently of published cache values.
type conversationActivity struct {
	Threads map[string]conversationInterest
	Next    time.Time
}

// TouchConversation marks a retained chat active without reading Slack.
func (s *Service) TouchConversation(id string) bool {
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	conversation, exists := s.cache.Conversations[id]
	if !exists || conversation.CreatedAt.Before(s.historyStart(now, 0)) || conversation.CreatedAt.After(now) {
		return false
	}
	if s.activity.Threads == nil {
		s.activity.Threads = map[string]conversationInterest{}
	}
	for key, interest := range s.activity.Threads {
		if !now.Before(interest.Until) {
			delete(s.activity.Threads, key)
		}
	}
	interest, exists := s.activity.Threads[id]
	if !exists && len(s.activity.Threads) >= 8 {
		oldest := ""
		for key, value := range s.activity.Threads {
			if oldest == "" || value.Until.Before(s.activity.Threads[oldest].Until) || value.Until.Equal(s.activity.Threads[oldest].Until) && key < oldest {
				oldest = key
			}
		}
		delete(s.activity.Threads, oldest)
	}
	interest.Until = now.Add(30 * time.Second)
	s.activity.Threads[id] = interest
	return true
}

// nextConversationRefresh selects one active thread and reserves the shared
// targeted-read budget for it. It returns the selected thread and the global
// deadline claimed for the read; both are zero when nothing was selected.
func (s *Service) nextConversationRefresh(now time.Time) (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.activity.Next) {
		return "", time.Time{}
	}
	selected := ""
	for id, interest := range s.activity.Threads {
		if !now.Before(interest.Until) {
			delete(s.activity.Threads, id)
			continue
		}
		if now.Before(interest.Next) {
			continue
		}
		if selected == "" || interest.Next.Before(s.activity.Threads[selected].Next) || interest.Next.Equal(s.activity.Threads[selected].Next) && id < selected {
			selected = id
		}
	}
	claimed := time.Time{}
	if selected != "" {
		interest := s.activity.Threads[selected]
		interest.Next = now.Add(conversationPollInterval)
		s.activity.Threads[selected] = interest
		claimed = now.Add(conversationRefreshBudget)
		s.activity.Next = claimed
	}
	return selected, claimed
}

// claimRefreshBudget reserves the global targeted-read budget shared with
// active conversation polling, returning the wait when it is already spent.
func (s *Service) claimRefreshBudget(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.activity.Next) {
		return s.activity.Next.Sub(now)
	}
	s.activity.Next = now.Add(conversationRefreshBudget)
	return 0
}

// releaseRefreshBudget returns a claimed global budget deadline when the
// claiming selection ends up reading nothing, so unrelated viewers are not
// throttled by a skip. Only the exact claimed deadline is cleared, so newer
// reservations such as a rate-limit backoff survive. A selection only happens
// once any previous deadline has elapsed, so zeroing the claim cannot drop an
// older unexpired deadline.
func (s *Service) releaseRefreshBudget(claimed time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activity.Next.Equal(claimed) {
		s.activity.Next = time.Time{}
	}
}

// deferConversationRefresh postpones background polling of one root until at least the given time.
func (s *Service) deferConversationRefresh(root string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.cfg.ChannelID + ":" + root
	interest, exists := s.activity.Threads[id]
	if !exists {
		return
	}
	if interest.Next.Before(until) {
		interest.Next = until
		s.activity.Threads[id] = interest
	}
}

// SyncConversation refreshes at most one active thread from a background worker.
// A read failure leaves the last complete workflow coverage available.
func (s *Service) SyncConversation(ctx context.Context) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	cache := s.currentCache()
	if cache.Coverage.Phase != "ready" {
		return ErrNotReady
	}
	now := s.clock.Now()
	id, claimed := s.nextConversationRefresh(now)
	if id == "" {
		return nil
	}
	conversation, exists := cache.Conversations[id]
	if !exists || conversation.CreatedAt.Before(s.historyStart(now, 0)) {
		// The selected chat is no longer retained, so no Slack read happens.
		s.releaseRefreshBudget(claimed)
		return nil
	}
	root := strings.TrimPrefix(id, s.cfg.ChannelID+":")
	if thread, exists := cache.Threads[root]; exists && now.Before(thread.RefreshAfter) {
		// A recent targeted read or memoized failure already covers this root,
		// and a skip that reads nothing must not hold the shared budget.
		s.deferConversationRefresh(root, thread.RefreshAfter)
		s.releaseRefreshBudget(claimed)
		return nil
	}
	updated, err := s.reloadThread(ctx, cache, root)
	if err != nil {
		s.memoizeThreadReadFailure(ctx, cache, root, err)
		return fmt.Errorf("refreshing active conversation: %w", err)
	}
	s.publish(updated)
	return nil
}
