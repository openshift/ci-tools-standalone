package opsalerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// ActionMarker identifies a single-line durable record in a trusted writer reply.
const ActionMarker = "OPS_ALERT_ACTION_V1 "

// ActionMetadataEvent identifies durable dashboard records in Slack metadata.
const ActionMetadataEvent = "ops_alert_action_v1"

// MaxActionMetadataBytes is a conservative application budget for serialized metadata.
// It is a local safety limit, not a claim about Slack's server limit.
const MaxActionMetadataBytes = 3000

// ActionReceiptPrefix reserves readable receipts so missing metadata fails closed.
const ActionReceiptPrefix = "Ops dashboard: "

// actionRecord carries complete occurrence state plus the associated rule mutation.
type actionRecord struct {
	ObservedWIP   string         `json:"observed_wip,omitempty"`
	WIPRule       *WIPRule       `json:"wip_rule,omitempty"`
	Unwip         string         `json:"unwip,omitempty"`
	Version       int            `json:"version"`
	Request       ActionRequest  `json:"request"`
	Actor         string         `json:"actor"`
	At            time.Time      `json:"at"`
	State         Workflow       `json:"state"`
	Snooze        *Snooze        `json:"snooze"`
	Unsnooze      string         `json:"unsnooze"`
	Investigation *Investigation `json:"investigation"`
}

// timedRecord orders accepted action records by Slack's server timestamp.
type timedRecord struct {
	Timestamp string
	Record    actionRecord
}

// validateAction bounds all untrusted text before parsing or posting.
func validateAction(actor string, req ActionRequest) error {
	if strings.TrimSpace(actor) == "" || len(actor) > 256 || len(req.OccurrenceID) > 256 || req.OccurrenceID == "" || req.ExpectedRevision == "" || len(req.ExpectedRevision) > 64 || len(req.RequestID) > 128 || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(req.RequestID) || len(req.Pattern) > 512 || len(req.Question) > 4000 || strings.Contains(req.Question, ActionMarker) || strings.Contains(req.Pattern, ActionMarker) {
		return fmt.Errorf("%w: missing or oversized action fields", ErrValidation)
	}
	switch req.Kind {
	case "acknowledge", "unacknowledge", "close", "reopen", "snooze", "unsnooze", "investigate":
	default:
		return fmt.Errorf("%w: unknown action kind", ErrValidation)
	}
	if req.Scope != "" && req.Scope != "occurrence" && req.Scope != "regex" {
		return fmt.Errorf("%w: unknown action scope", ErrValidation)
	}
	if req.Scope != "" && req.Kind != "acknowledge" && req.Kind != "unacknowledge" && req.Kind != "snooze" {
		return fmt.Errorf("%w: unexpected action scope", ErrValidation)
	}
	patternAllowed := req.Kind == "snooze" && req.Scope != "occurrence" || req.Kind == "acknowledge" && req.Scope == "regex"
	if !patternAllowed && req.Pattern != "" || req.Kind != "investigate" && req.Question != "" {
		return fmt.Errorf("%w: unexpected pattern or question", ErrValidation)
	}
	if req.Kind == "investigate" && strings.TrimSpace(req.Question) == "" {
		return fmt.Errorf("%w: investigation question required", ErrValidation)
	}
	if req.Pattern != "" {
		rx, err := regexp.Compile(req.Pattern)
		if err != nil || rx.MatchString("") {
			return fmt.Errorf("%w: invalid or empty-matching snooze expression", ErrValidation)
		}
	}
	return nil
}

// decodeRecord rejects malformed trusted records rather than guessing workflow state.
func decodeRecord(text, target string) (actionRecord, bool, error) {
	var record actionRecord
	if !strings.Contains(text, strings.TrimSpace(ActionMarker)) {
		return record, false, nil
	}
	var line string
	for _, candidate := range strings.Split(text, "\n") {
		if strings.HasPrefix(candidate, ActionMarker) {
			if line != "" {
				return record, false, fmt.Errorf("multiple action records")
			}
			line = strings.TrimPrefix(candidate, ActionMarker)
		}
	}
	if line == "" {
		return record, false, fmt.Errorf("malformed action marker")
	}
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, false, fmt.Errorf("decoding action: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return record, false, fmt.Errorf("trailing action data")
	}
	if err := validateAction(record.Actor, record.Request); err != nil {
		return record, false, err
	}
	if record.Version != 1 || record.Request.OccurrenceID != target || record.At.IsZero() {
		return record, false, fmt.Errorf("invalid action version, target or timestamp")
	}
	if (record.State.AcknowledgedAt == nil) != (record.State.AcknowledgedBy == "") || (record.State.ClosedAt == nil) != (record.State.ClosedBy == "") {
		return record, false, fmt.Errorf("incomplete workflow state")
	}
	if record.Request.Kind == "snooze" {
		if record.Snooze == nil || record.Snooze.ID != record.Request.RequestID || record.Snooze.Actor != record.Actor || record.Snooze.Source == "" || record.Snooze.Channel == "" || !record.Snooze.Until.After(record.At) || !record.Snooze.Until.Equal(nextSixUTC(record.At)) {
			return record, false, fmt.Errorf("invalid snooze state")
		}
		if record.Snooze.Scope != record.Request.Scope || record.Snooze.Scope == "occurrence" && record.Snooze.OccurrenceID != target || record.Snooze.Scope != "occurrence" && record.Snooze.OccurrenceID != "" {
			return record, false, fmt.Errorf("invalid snooze scope")
		}
		rx, err := regexp.Compile(record.Snooze.Pattern)
		if record.Snooze.Scope != "occurrence" && (err != nil || len(record.Snooze.Pattern) > 4096 || rx.MatchString("")) || record.Snooze.Scope == "occurrence" && record.Snooze.Pattern != "" {
			return record, false, fmt.Errorf("invalid durable snooze expression")
		}
	} else if record.Snooze != nil {
		return record, false, fmt.Errorf("unexpected snooze rule")
	}
	if record.Request.Kind == "acknowledge" && record.Request.Scope == "regex" {
		rule := record.WIPRule
		if rule == nil || rule.ID != record.Request.RequestID || rule.Actor != record.Actor || rule.Channel == "" || rule.Source == "" || !rule.Until.Equal(nextSixUTC(record.At)) {
			return record, false, fmt.Errorf("invalid WIP rule")
		}
		rx, err := regexp.Compile(rule.Pattern)
		if err != nil || len(rule.Pattern) > 4096 || rx.MatchString("") {
			return record, false, fmt.Errorf("invalid durable WIP expression")
		}
	} else if record.WIPRule != nil {
		return record, false, fmt.Errorf("unexpected WIP rule")
	}
	if record.Unwip != "" && record.ObservedWIP != "" && record.Unwip != record.ObservedWIP {
		return record, false, fmt.Errorf("WIP cancellation differs from observed rule")
	}
	if (record.Request.Kind == "unacknowledge" && record.Request.Scope == "regex") != (record.Unwip != "") {
		return record, false, fmt.Errorf("invalid WIP cancellation")
	}

	if (record.Request.Kind == "unsnooze") != (record.Unsnooze != "") {
		return record, false, fmt.Errorf("invalid unsnooze state")
	}
	if record.Investigation != nil {
		investigation := record.Investigation
		if investigation.RequestID == "" || investigation.Question == "" || investigation.RequestedBy == "" || investigation.RequestedAt.IsZero() || investigation.Status != "sent" && investigation.Status != "reply_received" {
			return record, false, fmt.Errorf("invalid investigation state")
		}
	}
	if record.Request.Kind == "investigate" && (record.Investigation == nil || record.Investigation.RequestID != record.Request.RequestID || record.Investigation.Question != record.Request.Question) {
		return record, false, fmt.Errorf("missing investigation state")
	}
	return record, true, nil
}

// decodeMessageRecord accepts legacy records and strictly validated metadata copies.
// Callers must verify the writer identity before trusting either surface.
func decodeMessageRecord(message slack.Msg, target string) (actionRecord, bool, error) {
	if message.Metadata.EventType == ConversationMetadataEvent && !strings.HasPrefix(message.Text, ActionReceiptPrefix) && !strings.HasPrefix(message.Text, ActionMarker) {
		return actionRecord{}, false, nil
	}
	legacy, hasLegacy, err := decodeRecord(message.Text, target)
	if err != nil {
		return actionRecord{}, false, err
	}
	metadata := message.Metadata
	if metadata.EventType != ActionMetadataEvent {
		if strings.HasPrefix(message.Text, ActionReceiptPrefix) && !hasLegacy {
			return actionRecord{}, false, fmt.Errorf("action receipt missing supported metadata or legacy record")
		}
		return legacy, hasLegacy, nil
	}
	raw, ok := metadata.EventPayload["record"].(string)
	if !ok || len(metadata.EventPayload) != 1 || raw == "" || strings.ContainsAny(raw, "\r\n") {
		return actionRecord{}, false, fmt.Errorf("invalid action metadata envelope")
	}
	record, exists, err := decodeRecord(ActionMarker+raw, target)
	if err != nil {
		return actionRecord{}, false, fmt.Errorf("decoding action metadata: %w", err)
	}
	if !exists {
		return actionRecord{}, false, fmt.Errorf("missing action metadata record")
	}
	if hasLegacy && !sameRecord(legacy, record) {
		return actionRecord{}, false, fmt.Errorf("conflicting legacy and metadata action records")
	}
	return record, true, nil
}

// sameRecord compares durable duplicate deliveries, including their full state.
func sameRecord(a, b actionRecord) bool {
	aa, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(aa, bb)
}

// nextSixUTC never expires immediately at exactly 06:00.
func nextSixUTC(now time.Time) time.Time {
	now = now.UTC()
	until := time.Date(now.Year(), now.Month(), now.Day(), 6, 0, 0, 0, time.UTC)
	if !until.After(now) {
		until = until.AddDate(0, 0, 1)
	}
	return until
}

// defaultPattern anchors all parsed identifiers rather than arbitrary message text.
func defaultPattern(identifiers []string) string {
	quoted := make([]string, 0, len(identifiers))
	for _, identifier := range identifiers {
		quoted = append(quoted, regexp.QuoteMeta(identifier))
	}
	return "^(?:" + strings.Join(quoted, "|") + ")$"
}

// sameTime compares optional workflow times independently of their location encoding.
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// validateTransition detects inconsistent full-state records during recovery.
func validateTransition(o Occurrence, r actionRecord, cache serviceCache, historicalRule bool) error {
	if r.Request.ExpectedRevision != workflowRevision(o.Identity.Revision, r.ObservedWIP) {
		return fmt.Errorf("stale durable action revision")
	}
	if r.ObservedWIP != "" {
		creation, exists := cache.Records[r.ObservedWIP]
		rule := creation.WIPRule
		// A valid observation survives parent edits and cancellation before Slack confirms it.
		if !exists && !historicalRule || exists && (rule == nil || rule.Channel != o.Identity.Channel || rule.Source != o.Message.Source || r.At.Before(creation.At) || !r.At.Before(rule.Until)) {
			return fmt.Errorf("observation without matching WIP rule")
		}
		for _, previous := range cache.Records {
			if previous.Unwip == r.ObservedWIP && r.At.After(previous.At) {
				return fmt.Errorf("observation after WIP cancellation")
			}
		}
	}
	expected := o.State
	switch r.Request.Kind {
	case "acknowledge":
		if r.Request.Scope == "regex" {
			if r.WIPRule.Source != o.Message.Source || r.WIPRule.Channel != o.Identity.Channel || r.Request.Pattern != "" && r.Request.Pattern != r.WIPRule.Pattern {
				return fmt.Errorf("invalid durable WIP scope or expression")
			}
			break
		}
		if expected.AcknowledgedAt != nil && r.ObservedWIP == "" {
			return fmt.Errorf("duplicate acknowledgement")
		}
		expected.AcknowledgedBy, expected.AcknowledgedAt = r.Actor, &r.At
	case "unacknowledge":
		if r.Request.Scope == "regex" {
			rule, exists := cache.WIPRules[r.Unwip]
			if !historicalRule && (!exists && r.ObservedWIP != r.Unwip || exists && (rule.Source != o.Message.Source || rule.Channel != o.Identity.Channel || !r.At.Before(rule.Until))) {
				return fmt.Errorf("undo without matching WIP rule")
			}
			break
		}
		if expected.AcknowledgedAt == nil && r.ObservedWIP == "" {
			return fmt.Errorf("undo without acknowledgement")
		}
		expected.AcknowledgedBy, expected.AcknowledgedAt = "", nil
	case "close":
		if expected.ClosedAt != nil {
			return fmt.Errorf("duplicate closure")
		}
		expected.ClosedBy, expected.ClosedAt = r.Actor, &r.At
	case "reopen":
		if expected.ClosedAt == nil {
			return fmt.Errorf("reopen without closure")
		}
		expected.ClosedBy, expected.ClosedAt = "", nil
	case "snooze":
		if r.Snooze.Source != o.Message.Source || r.Snooze.Channel != o.Identity.Channel {
			return fmt.Errorf("invalid durable snooze scope")
		}
		// Parent edits may change identifiers after the rule was saved.
		if r.Request.Pattern != "" && r.Request.Pattern != r.Snooze.Pattern {
			return fmt.Errorf("inconsistent durable snooze expression")
		}
	case "unsnooze":
		rule, exists := cache.Rules[r.Unsnooze]
		// Expired historical rules can live on parents outside the bounded replay.
		if !historicalRule && (!exists || rule.Source != o.Message.Source || rule.Channel != o.Identity.Channel || rule.Scope == "occurrence" && rule.OccurrenceID != o.Identity.ID || !r.At.Before(rule.Until)) {
			return fmt.Errorf("undo without matching snooze rule")
		}
	case "investigate":
		if o.Investigation != nil && o.Investigation.Status != "reply_received" || r.Investigation.RequestedBy != r.Actor || !r.Investigation.RequestedAt.Equal(r.At) || r.Investigation.Status != "sent" {
			return fmt.Errorf("invalid investigation transition")
		}
	}
	if r.State.AcknowledgedBy != expected.AcknowledgedBy || !sameTime(r.State.AcknowledgedAt, expected.AcknowledgedAt) || r.State.ClosedBy != expected.ClosedBy || !sameTime(r.State.ClosedAt, expected.ClosedAt) {
		return fmt.Errorf("inconsistent durable workflow state")
	}
	if r.Request.Kind != "investigate" {
		if (o.Investigation == nil) != (r.Investigation == nil) {
			return fmt.Errorf("investigation state erased or introduced")
		}
		if o.Investigation != nil && (o.Investigation.RequestID != r.Investigation.RequestID || o.Investigation.Question != r.Investigation.Question || o.Investigation.RequestedBy != r.Investigation.RequestedBy || !o.Investigation.RequestedAt.Equal(r.Investigation.RequestedAt)) {
			return fmt.Errorf("investigation identity changed")
		}
	}
	return nil
}

// reloadTarget reconciles confirmed request IDs before considering a new post.
// The read stays mandatory: delivery proof is never satisfied by cache freshness.
func (s *Service) reloadTarget(ctx context.Context, cache serviceCache, target Occurrence) (serviceCache, error) {
	updated, err := s.reloadThread(ctx, cache, target.Identity.Timestamp)
	if err != nil {
		return cache, fmt.Errorf("reloading target thread: %w", err)
	}
	return updated, nil
}

// Act persists a trusted reply before publishing any successful workflow change.
func (s *Service) Act(ctx context.Context, actor string, req ActionRequest) (Occurrence, error) {
	if err := validateAction(actor, req); err != nil {
		return Occurrence{}, err
	}
	if err := s.acquire(ctx); err != nil {
		return Occurrence{}, errors.Join(ErrNotReady, err)
	}
	defer s.release()
	cache := s.currentCache()
	if cache.Coverage.Phase != "ready" {
		return Occurrence{}, ErrNotReady
	}
	if _, exists := cache.ConversationRecords[req.RequestID]; exists {
		return Occurrence{}, ErrConflict
	}
	if _, exists := cache.ConversationPending[req.RequestID]; exists {
		return Occurrence{}, ErrConflict
	}
	target, exists := cache.Occurrences[req.OccurrenceID]
	if !exists || !s.retained(target, s.clock.Now(), 0) {
		return Occurrence{}, ErrNotFound
	}
	updated, err := s.reloadTarget(ctx, cache, target)
	if err != nil {
		return Occurrence{}, s.degrade(cache, fmt.Errorf("%w: %v", ErrNotReady, err))
	}
	cache = updated
	s.publish(cache)
	target, exists = cache.Occurrences[req.OccurrenceID]
	if !exists || !s.retained(target, s.clock.Now(), 0) {
		return Occurrence{}, ErrNotFound
	}
	target = effectiveOccurrence(target, cache, s.clock.Now())
	if record, exists := cache.Records[req.RequestID]; exists {
		if record.Request != req || record.Actor != actor {
			return target, ErrConflict
		}
		return target, nil
	}
	if pending, exists := cache.Pending[req.RequestID]; exists {
		if pending.Request != req || pending.Actor != actor {
			return target, ErrConflict
		}
		if pending.Status == "delivery_uncertain" {
			return target, ErrDeliveryUncertain
		}
	}
	for _, pending := range cache.Pending {
		if pending.Status != "delivery_uncertain" {
			continue
		}
		// A cancellation can affect every parent before its trusted reply is confirmed.
		if pending.Request.Kind == "unacknowledge" && pending.Request.Scope == "regex" {
			return target, fmt.Errorf("%w: family cancellation awaiting archive confirmation", ErrNotReady)
		}
		if pending.Request.OccurrenceID == req.OccurrenceID {
			return target, ErrDeliveryUncertain
		}
	}
	if req.ExpectedRevision != target.Identity.Revision {
		return target, ErrConflict
	}
	if target.View == "chai_reports" {
		return target, fmt.Errorf("%w: scheduled reports have no workflow actions", ErrValidation)
	}
	record, err := s.prepareAction(actor, req, target, cache)
	if err != nil {
		return target, err
	}
	message, err := s.actionMessage(record, target)
	if err != nil {
		return target, err
	}
	if err := ctx.Err(); err != nil {
		return target, errors.Join(ErrNotReady, err)
	}
	if !s.retained(target, s.clock.Now(), 0) {
		return Occurrence{}, ErrNotFound
	}
	ts, err := s.archive.Post(ctx, s.cfg.ChannelID, target.Identity.Timestamp, req.RequestID, message)
	if err == nil {
		if _, tsErr := slackTime(ts); tsErr != nil {
			err = fmt.Errorf("invalid successful post timestamp: %w", tsErr)
		}
	}
	if err != nil {
		return s.failedDelivery(cache, target, record, err)
	}
	threads := maps.Clone(cache.Threads)
	saveThreadReply(threads, target.Identity.Timestamp, ts, slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: target.Identity.Timestamp, User: s.cfg.Writer.UserID, BotID: s.cfg.Writer.BotID, Text: message.Text, Metadata: message.Metadata}})
	updated, err = s.replay(threads, cache.Pending)
	if err != nil {
		return target, s.degrade(cache, fmt.Errorf("replaying saved action: %w", err))
	}
	updated.Coverage = cache.Coverage
	updated.Decorations = cache.Decorations
	s.publish(updated)
	s.publish(s.decorate(ctx, updated))
	return effectiveOccurrence(updated.Occurrences[req.OccurrenceID], updated, s.clock.Now()), nil
}

// prepareAction computes the complete new state without mutating cached state.
func (s *Service) prepareAction(actor string, req ActionRequest, o Occurrence, cache serviceCache) (actionRecord, error) {
	now := s.clock.Now().UTC()
	// Delivery failures are transient overlays, never durable workflow state.
	r := actionRecord{Version: 1, Request: req, Actor: actor, At: now, State: cache.Occurrences[o.Identity.ID].State}
	if cached := cache.Occurrences[o.Identity.ID].Investigation; cached != nil {
		investigation := *cached
		// Reply is reconstructed from trusted thread evidence, not durable action state.
		investigation.Reply = ""
		r.Investigation = &investigation
	}
	if o.WIPRule != nil {
		r.ObservedWIP = o.WIPRule.ID
	}
	switch req.Kind {
	case "acknowledge":
		if req.Scope == "regex" {
			pattern, err := actionPattern(req.Pattern, o.Message.Identifiers)
			if err != nil {
				return r, err
			}
			r.WIPRule = &WIPRule{ID: req.RequestID, Pattern: pattern, Actor: actor, Until: nextSixUTC(now), Channel: o.Identity.Channel, Source: o.Message.Source}
			break
		}
		if o.State.AcknowledgedAt != nil && o.WIPRule == nil {
			return r, ErrConflict
		}
		r.State.AcknowledgedBy, r.State.AcknowledgedAt = actor, &now
	case "unacknowledge":
		if req.Scope == "regex" {
			if o.WIPRule == nil {
				return r, ErrConflict
			}
			r.Unwip = o.WIPRule.ID
			break
		}
		if o.State.AcknowledgedAt == nil {
			return r, ErrConflict
		}
		r.State.AcknowledgedBy, r.State.AcknowledgedAt = "", nil
	case "close":
		if o.State.ClosedAt != nil {
			return r, ErrConflict
		}
		r.State.ClosedBy, r.State.ClosedAt = actor, &now
	case "reopen":
		if o.State.ClosedAt == nil {
			return r, ErrConflict
		}
		r.State.ClosedBy, r.State.ClosedAt = "", nil
	case "snooze":
		if o.Snooze != nil {
			return r, ErrConflict
		}
		rule := &Snooze{ID: req.RequestID, Scope: req.Scope, Source: o.Message.Source, Channel: o.Identity.Channel, Actor: actor, Until: nextSixUTC(now)}
		if req.Scope == "occurrence" {
			rule.OccurrenceID = o.Identity.ID
		} else {
			pattern, err := actionPattern(req.Pattern, o.Message.Identifiers)
			if err != nil {
				return r, err
			}
			rule.Pattern = pattern
		}
		r.Snooze = rule
	case "unsnooze":
		if o.Snooze == nil {
			return r, ErrConflict
		}
		r.Unsnooze = o.Snooze.ID
	case "investigate":
		if o.Investigation != nil && o.Investigation.Status != "failed" && o.Investigation.Status != "reply_received" {
			return r, ErrConflict
		}
		for _, pending := range cache.Pending {
			if pending.Request.OccurrenceID == o.Identity.ID && pending.Request.Kind == "investigate" && pending.Status != "failed" {
				return r, ErrConflict
			}
		}
		if s.chaiUser() == "" {
			return r, fmt.Errorf("%w: configured Chai user identity required", ErrValidation)
		}
		r.Investigation = &Investigation{RequestID: req.RequestID, Status: "sent", Question: req.Question, RequestedBy: actor, RequestedAt: now}
	}
	return r, nil
}

// actionPattern requires a bounded RE2 expression matching a selected identifier.
func actionPattern(pattern string, identifiers []string) (string, error) {
	if len(identifiers) == 0 {
		return "", fmt.Errorf("%w: parsed identifier required for regex action", ErrValidation)
	}
	if pattern == "" {
		pattern = defaultPattern(identifiers)
	}
	rx, err := regexp.Compile(pattern)
	if err != nil || len(pattern) > 4096 || rx.MatchString("") {
		return "", fmt.Errorf("%w: invalid action expression", ErrValidation)
	}
	for _, identifier := range identifiers {
		if rx.MatchString(identifier) {
			return pattern, nil
		}
	}
	return "", fmt.Errorf("%w: expression must match a selected identifier", ErrValidation)
}

// chaiUser uses an explicitly configured user mention, never a display name or DM.
func (s *Service) chaiUser() string {
	for _, source := range s.cfg.Sources {
		if source.Kind == "chai" && len(source.UserIDs) > 0 {
			return source.UserIDs[0]
		}
	}
	return ""
}

// escapeSlack prevents operator/context text from creating additional mentions.
func escapeSlack(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

// actionReceipt describes the visible outcome while keeping identifiers in metadata.
func actionReceipt(r actionRecord) string {
	label := "👀 WIP"
	switch r.Request.Kind {
	case "unacknowledge":
		label = "Released"
	case "close":
		label = "✅ Closed"
	case "reopen":
		label = "Reopened"
	case "snooze":
		label = "💤 Snoozed"
	case "unsnooze":
		label = "Unsnoozed"
	}
	scope := ""
	if r.Request.Scope == "regex" || r.Request.Kind == "snooze" && r.Request.Scope == "" {
		scope = " matching alerts"
	}
	expiry := ""
	if r.WIPRule != nil {
		expiry = " until " + r.WIPRule.Until.UTC().Format("2006-01-02 15:04 UTC")
	}
	if r.Snooze != nil {
		expiry = " until " + r.Snooze.Until.UTC().Format("2006-01-02 15:04 UTC")
	}
	return fmt.Sprintf("%s%s%s by %s%s.", ActionReceiptPrefix, label, scope, escapeSlack(r.Actor), expiry)
}

// actionMessage keeps durable state in metadata and only readable confirmation in text.
func (s *Service) actionMessage(r actionRecord, o Occurrence) (slack.Msg, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return slack.Msg{}, fmt.Errorf("encoding action record: %w", err)
	}
	confirmation := actionReceipt(r)
	if r.Request.Kind == "investigate" {
		confirmation = fmt.Sprintf("%s<@%s> Please investigate this alert in this thread. Request ID: %s\nSource: <%s|Slack occurrence>\nContext: %s\nQuestion: %s\nUse read-only evidence. Summarize findings, evidence links, and suggested next checks here. Do not make changes, acknowledge, or close the alert.", ActionReceiptPrefix, s.chaiUser(), r.Request.RequestID, o.Message.SlackURL, escapeSlack(o.Message.Title), escapeSlack(r.Request.Question))
	}
	message := slack.Msg{Text: confirmation, Metadata: slack.SlackMetadata{EventType: ActionMetadataEvent, EventPayload: map[string]interface{}{"record": string(data)}}}
	envelope, err := json.Marshal(message.Metadata)
	if err != nil {
		return slack.Msg{}, fmt.Errorf("encoding action metadata: %w", err)
	}
	if len(envelope) > MaxActionMetadataBytes {
		return slack.Msg{}, fmt.Errorf("%w: serialized action metadata is %d bytes, exceeds application budget of %d bytes; shorten the question or retained record", ErrValidation, len(envelope), MaxActionMetadataBytes)
	}
	return message, nil
}

// failedDelivery exposes uncertainty while retaining a retry guard in this process.
func (s *Service) failedDelivery(cache serviceCache, o Occurrence, r actionRecord, err error) (Occurrence, error) {
	status, sentinel := "delivery_uncertain", ErrDeliveryUncertain
	if errors.Is(err, ErrDeliveryFailed) {
		status, sentinel = "failed", ErrDeliveryFailed
	}
	pending := pendingDelivery{Request: r.Request, Actor: r.Actor, Status: status}
	if r.Request.Kind == "investigate" {
		investigation := *r.Investigation
		investigation.Status = status
		pending.Investigation = &investigation
	}
	cache.Pending = maps.Clone(cache.Pending)
	cache.Pending[r.Request.RequestID] = pending
	s.publish(cache)
	return effectiveOccurrence(o, cache, s.clock.Now()), fmt.Errorf("%w: %v", sentinel, err)
}
