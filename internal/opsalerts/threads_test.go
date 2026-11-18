package opsalerts

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/slack-go/slack"
)

// TestThreadBlockDepth makes server-side truncation visible to the browser renderer.
func TestThreadBlockDepth(t *testing.T) {
	for _, tc := range []struct {
		name  string
		depth int
		want  []string
	}{
		{name: "leaf", depth: 0},
		{name: "last supported level", depth: 8},
		{name: "first truncated level", depth: 9, want: []string{"deeply nested blocks"}},
		{name: "excessive depth", depth: 64, want: []string{"deeply nested blocks"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := []any{map[string]any{"type": "text", "text": "Retained text"}}
			for range tc.depth {
				blocks = []any{map[string]any{"type": "rich_text", "elements": blocks}}
			}
			var unsupported []string
			threadBlocks(blocks, &unsupported, 0)
			if diff := cmp.Diff(tc.want, unsupported); diff != "" {
				t.Errorf("unexpected unsupported content (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAlertThreadPages checks cache-only pagination, visible edits and detached rich data.
func TestAlertThreadPages(t *testing.T) {
	cases := []struct {
		name    string
		before  string
		count   int
		first   int
		cursor  bool
		wantErr error
	}{
		{name: "latest", count: 30, first: 6, cursor: true},
		{name: "older", before: "1793156400.000006", count: 5, first: 1},
		{name: "invalid cursor", before: "nope", wantErr: ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, _ := fixture(t)
			root := archive.roots[0].Timestamp
			for i := 1; i <= 35; i++ {
				var m slack.Message
				if err := json.Unmarshal([]byte(`{"text":"hello <@U_HUMAN>","user":"U_HUMAN","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"*bold*"},"accessory":{"type":"button","text":{"type":"plain_text","text":"Never"}}},{"type":"actions","elements":[]}],"attachments":[{"title":"Evidence","author_icon":"https://evil.example/a.png","fields":[{"title":"Cluster","value":"build01","short":true}],"actions":[{}]}],"reactions":[{"name":"eyes","count":1,"users":["U_HUMAN"]}]}`), &m); err != nil {
					t.Fatal(err)
				}
				m.Timestamp = fmt.Sprintf("1793156400.%06d", i)
				m.ThreadTimestamp = root
				archive.replies[root] = append(archive.replies[root], m)
			}
			archive.roots[0].ReplyCount = 35
			if err := service.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			calls := archive.replyCalls
			view, err := service.AlertThread("C_TEST:"+root, tc.before)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v, want %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(tc.count, len(view.Messages)); diff != "" {
				t.Errorf("unexpected count (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(fmt.Sprintf("1793156400.%06d", tc.first), view.Messages[0].Timestamp); diff != "" {
				t.Errorf("unexpected oldest (-want +got):\n%s", diff)
			}
			if (view.NextCursor != "") != tc.cursor || view.TotalReplies != 35 || view.FetchedAt.IsZero() {
				t.Fatalf("page %#v", view)
			}
			if archive.replyCalls != calls {
				t.Fatal("cache read called Slack")
			}
			message := &view.Messages[0]
			if diff := cmp.Diff([]string{"accessory", "actions", "actions"}, message.Unsupported); diff != "" {
				t.Errorf("unexpected unsupported (-want +got):\n%s", diff)
			}
			if message.Attachments[0]["author_icon"] != "" {
				t.Fatal("unsafe image survived")
			}
			message.Reactions[0].Users[0] = "mutated"
			message.Blocks[0]["text"].(map[string]any)["text"] = "mutated"
			again, err := service.AlertThread("C_TEST:"+root, tc.before)
			if err != nil {
				t.Fatal(err)
			}
			if again.Messages[0].Reactions[0].Users[0] != "U_HUMAN" || again.Messages[0].Blocks[0]["text"].(map[string]any)["text"] != "*bold*" {
				t.Fatal("view mutated cache")
			}
		})
	}
}

// TestPlainReplyRecovery exercises Slack canonicalization independently of submission fingerprints.
func TestPlainReplyRecovery(t *testing.T) {
	cases := []struct {
		name, original, canonical, retry string
		edited                           bool
		wantErr                          error
	}{
		{name: "emoji", original: "Great 🎉", canonical: "Great :tada:"},
		{name: "URL", original: "See https://example.com", canonical: "See <https://example.com|example.com>"},
		{name: "edited", original: "Original", canonical: "Edited by bot", edited: true},
		{name: "different canonical-looking retry", original: "Great 🎉", canonical: "Great :tada:", retry: "Great :tada:", wantErr: ErrConflict},
		{name: "quoted receipt prefix", original: ActionReceiptPrefix + "quoted receipt", canonical: ActionReceiptPrefix + "quoted receipt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			service.cfg.Sources = service.cfg.Sources[:3] // No configured Chai is necessary.
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			id := "C_TEST:" + archive.roots[0].Timestamp
			req := ThreadReplyRequest{OccurrenceID: id, RequestID: "plain", Text: tc.original}
			before := findOccurrence(t, service, id)
			view, err := service.ReplyToAlert(ctx, "operator", req)
			if err != nil {
				t.Fatal(err)
			}
			if len(service.Conversations().Conversations) != 0 {
				t.Fatal("plain reply admitted Chai history")
			}
			if diff := cmp.Diff(before.State, findOccurrence(t, service, id).State); diff != "" {
				t.Errorf("unexpected workflow (-want +got):\n%s", diff)
			}
			if len(view.Messages) != 1 || view.Messages[0].Author.ID != "U_WRITER" || view.Messages[0].Actor != "operator" || strings.Contains(archive.lastText, "<@U_CHAI>") {
				t.Fatalf("reply %#v", view.Messages)
			}
			root := archive.roots[0].Timestamp
			stored := &archive.replies[root][0]
			stored.Text = threadReplyHeader + tc.canonical
			if tc.edited {
				stored.Edited = &slack.Edited{Timestamp: stored.Timestamp}
			}
			clock.now = clock.now.Add(time.Second)
			restarted, err := NewService(service.cfg, service.classifier, archive, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if restarted.conversationIssueCount(restarted.currentCache(), clock.now) != 0 {
				t.Fatal("valid canonical reply quarantined")
			}
			req.ReconcileOnly = true
			if tc.retry != "" {
				req.Text = tc.retry
			}
			view, err = restarted.ReplyToAlert(ctx, "operator", req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("retry error %v, want %v", err, tc.wantErr)
			}
			if archive.posts != 1 {
				t.Fatalf("retry posts %d", archive.posts)
			}
			if err == nil && (view.Messages[0].Text != tc.canonical || view.Messages[0].Edited != tc.edited) {
				t.Fatalf("canonical view %#v", view.Messages)
			}
			if err == nil && (view.Messages[0].Actor != "operator" || view.Messages[0].RequestID != req.RequestID) {
				t.Fatalf("valid canonical ledger lost proof: %#v", view.Messages[0])
			}
		})
	}
}

// TestPlainReplyGuards ensures unknown reconciliation and corrupt recovery never cause duplicate sends.
func TestPlainReplyGuards(t *testing.T) {
	cases := []struct {
		name, mutation string
		reconcile      bool
		wantErr        error
	}{
		{name: "unknown reconciliation", reconcile: true, wantErr: ErrDeliveryUncertain},
		{name: "stripped metadata", mutation: "strip", wantErr: ErrNotReady},
		{name: "invalid metadata", mutation: "invalid", wantErr: ErrNotReady},
		{name: "missing frame", mutation: "frame", wantErr: ErrNotReady},
		{name: "duplicate metadata keys", mutation: "duplicate", wantErr: ErrNotReady},
		{name: "case variant actor", mutation: "case_actor", wantErr: ErrNotReady},
		{name: "case variant digest", mutation: "case_hash", wantErr: ErrNotReady},
		{name: "case variant request ID", mutation: "case_requestid", wantErr: ErrNotReady},
		{name: "actor conflict", mutation: "actor", wantErr: ErrConflict},
		{name: "text conflict", mutation: "text", wantErr: ErrConflict},
		{name: "mode conflict", mutation: "mode", wantErr: ErrConflict},
		{name: "target conflict", mutation: "target", wantErr: ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			root := archive.roots[0].Timestamp
			req := ThreadReplyRequest{OccurrenceID: "C_TEST:" + root, RequestID: "stable", Text: "hello"}
			actor := "operator"
			if tc.mutation != "" {
				if _, err := service.ReplyToAlert(ctx, actor, req); err != nil {
					t.Fatal(err)
				}
				stored := &archive.replies[root][0]
				switch tc.mutation {
				case "strip":
					stored.Metadata = slack.SlackMetadata{}
				case "invalid":
					stored.Metadata.EventPayload = map[string]any{"record": "bad"}
				case "frame":
					stored.Text = "hello"
				case "duplicate":
					raw := stored.Metadata.EventPayload["record"].(string)
					stored.Metadata.EventPayload = map[string]any{"record": strings.Replace(raw, `"version":1`, `"version":1,"version":1`, 1)}
				case "case_actor":
					raw := stored.Metadata.EventPayload["record"].(string)
					stored.Metadata.EventPayload = map[string]any{"record": strings.Replace(raw, `"actor":"operator"`, `"actor":"operator","Actor":"operator"`, 1)}
				case "case_hash":
					raw := stored.Metadata.EventPayload["record"].(string)
					stored.Metadata.EventPayload = map[string]any{"record": strings.Replace(raw, `"text_hash":`, `"Text_hash":`, 1)}
				case "case_requestid":
					raw := stored.Metadata.EventPayload["record"].(string)
					stored.Metadata.EventPayload = map[string]any{"record": strings.Replace(raw, `"request_id":"stable"`, `"request_id":"stable","Request_id":"stable"`, 1)}
				case "actor":
					actor = "other"
				case "text":
					req.Text = "different"
				case "target":
					req.OccurrenceID = "C_TEST:" + archive.roots[1].Timestamp
				}
				clock.now = clock.now.Add(time.Second)
				var err error
				service, err = NewService(service.cfg, service.classifier, archive, clock)
				if err != nil {
					t.Fatal(err)
				}
				if err := service.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				if errors.Is(tc.wantErr, ErrNotReady) {
					req.RequestID = "new"
				}
			}
			posts := archive.posts
			req.ReconcileOnly = tc.reconcile
			var err error
			if tc.mutation == "mode" {
				_, err = service.Converse(ctx, actor, ConversationRequest{OccurrenceID: req.OccurrenceID, RequestID: req.RequestID, Text: req.Text})
			} else {
				_, err = service.ReplyToAlert(ctx, actor, req)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v, want %v", err, tc.wantErr)
			}
			if archive.posts != posts {
				t.Fatal("guard performed a post")
			}
			if errors.Is(tc.wantErr, ErrNotReady) {
				view, err := service.AlertThread(req.OccurrenceID, "")
				if err != nil {
					t.Fatal(err)
				}
				if view.Messages[0].Actor != "" || view.Messages[0].RequestID != "" {
					t.Fatal("malformed reply retained verified attribution")
				}
				if _, err := service.Converse(ctx, "operator", ConversationRequest{OccurrenceID: req.OccurrenceID, RequestID: "chai-after-corruption", Text: "Why?"}); !errors.Is(err, ErrNotReady) {
					t.Fatalf("corruption did not quarantine Chai: %v", err)
				}
				o := findOccurrence(t, service, req.OccurrenceID)
				if _, err := service.Act(ctx, "operator", ActionRequest{OccurrenceID: req.OccurrenceID, ExpectedRevision: o.Identity.Revision, RequestID: "workflow-after-corruption", Kind: "acknowledge"}); err != nil {
					t.Fatalf("chat corruption disabled workflow: %v", err)
				}
			}
		})
	}
}

// TestPlainReplyUncertainRestart reconciles accepted ambiguous posts without ever reposting absent ones.
func TestPlainReplyUncertainRestart(t *testing.T) {
	cases := []struct {
		name   string
		stored bool
	}{{name: "accepted before disconnect", stored: true}, {name: "not found after restart"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			req := ThreadReplyRequest{OccurrenceID: "C_TEST:" + archive.roots[0].Timestamp, RequestID: "uncertain", Text: "hello"}
			archive.postError = errors.New("connection reset")
			archive.storeOnError = tc.stored
			if _, err := service.ReplyToAlert(ctx, "operator", req); !errors.Is(err, ErrDeliveryUncertain) {
				t.Fatalf("initial delivery %v", err)
			}
			archive.postError = nil
			req.ReconcileOnly = true
			if _, err := service.ReplyToAlert(ctx, "operator", req); !errors.Is(err, ErrDeliveryUncertain) {
				t.Fatalf("in-memory guard %v", err)
			}
			restarted, err := NewService(service.cfg, service.classifier, archive, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			_, err = restarted.ReplyToAlert(ctx, "operator", req)
			wantErr := ErrDeliveryUncertain
			if tc.stored {
				wantErr = nil
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("reconcile error %v, want %v", err, wantErr)
			}
			if archive.posts != 1 {
				t.Fatalf("reconciliation blindly posted %d times", archive.posts)
			}
		})
	}
}

// TestThreadVisibility covers duplicate timestamps, deletion tombstones and changed wrappers.
func TestThreadVisibility(t *testing.T) {
	cases := []struct {
		name, extra string
		count       int
		text        string
		edited      bool
	}{
		{name: "duplicate", extra: `{"ts":"1793156400.000001","user":"U_HUMAN","text":"duplicate"}`, count: 1, text: "duplicate"},
		{name: "deleted tombstone", extra: `{"ts":"1793156400.000002","subtype":"message_deleted","deleted_ts":"1793156400.000001"}`},
		{name: "hidden", extra: `{"ts":"1793156400.000001","hidden":true}`},
		{name: "changed", extra: `{"subtype":"message_changed","hidden":true,"message":{"ts":"1793156400.000001","user":"U_HUMAN","text":"edited","edited":{"ts":"1793156400.000002"}}}`, count: 1, text: "edited", edited: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, _, _ := fixture(t)
			if err := service.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			cache := service.currentCache()
			id := service.Snapshot().Occurrences[0].Identity.ID
			root := cache.Occurrences[id].Identity.Timestamp
			thread := cache.Threads[root]
			thread.Replies = []slack.Message{{Msg: slack.Msg{Timestamp: "1793156400.000001", User: "U_HUMAN", Text: "original"}}}
			var extra slack.Message
			if err := json.Unmarshal([]byte(tc.extra), &extra); err != nil {
				t.Fatal(err)
			}
			thread.Replies = append(thread.Replies, extra)
			cache.Threads[root] = thread
			service.publish(cache)
			view, err := service.AlertThread(id, "")
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.count, len(view.Messages)); diff != "" {
				t.Fatalf("unexpected visibility (-want +got):\n%s", diff)
			}
			if tc.count > 0 && (view.Messages[0].Text != tc.text || view.Messages[0].Edited != tc.edited) {
				t.Fatalf("message %#v", view.Messages[0])
			}
		})
	}
}

// TestThreadBotProfileFallback preserves Slack's own names and raster icons without a directory.
func TestThreadBotProfileFallback(t *testing.T) {
	cases := []struct{ name, raw, wantID, wantName string }{
		{name: "profile", raw: `{"bot_id":"B_ACTUAL","bot_profile":{"name":"Actual bot","icons":{"image_48":"https://avatars.slack-edge.com/bot.png"}}}`, wantID: "B_ACTUAL", wantName: "Actual bot"},
		{name: "username", raw: `{"user":"U_ACTUAL","bot_id":"B_ACTUAL","username":"Webhook bot","icons":{"icon_url":"https://avatars.slack-edge.com/bot.png"}}`, wantID: "U_ACTUAL", wantName: "Webhook bot"},
		{name: "message overrides profile", raw: `{"user":"U_ACTUAL","bot_id":"B_ACTUAL","username":"Webhook bot","icons":{"icon_url":"https://avatars.slack-edge.com/bot.png"},"bot_profile":{"name":"Default bot","icons":{"image_48":"https://avatars.slack-edge.com/default.png"}}}`, wantID: "U_ACTUAL", wantName: "Webhook bot"},
		{name: "profile fills missing avatar", raw: `{"user":"U_ACTUAL","bot_id":"B_ACTUAL","username":"Webhook bot","bot_profile":{"name":"Default bot","icons":{"image_48":"https://avatars.slack-edge.com/bot.png"}}}`, wantID: "U_ACTUAL", wantName: "Webhook bot"},
		{name: "profile fills missing name", raw: `{"user":"U_ACTUAL","bot_id":"B_ACTUAL","icons":{"icon_url":"https://avatars.slack-edge.com/bot.png"},"bot_profile":{"name":"Default bot","icons":{"image_48":"https://avatars.slack-edge.com/default.png"}}}`, wantID: "U_ACTUAL", wantName: "Default bot"},
		{name: "unsafe message image uses profile", raw: `{"user":"U_ACTUAL","bot_id":"B_ACTUAL","username":"Webhook bot","icons":{"icon_url":"https://evil.example/bot.png"},"bot_profile":{"name":"Default bot","icons":{"image_48":"https://avatars.slack-edge.com/bot.png"}}}`, wantID: "U_ACTUAL", wantName: "Webhook bot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, _, _ := fixture(t)
			var raw slack.Message
			if err := json.Unmarshal([]byte(tc.raw), &raw); err != nil {
				t.Fatal(err)
			}
			got := service.threadMessage(raw, nil).Author
			want := ThreadPerson{ID: tc.wantID, Name: tc.wantName, AvatarURL: "https://avatars.slack-edge.com/bot.png", IsBot: true}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("unexpected bot fallback (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPlainReplyPageIndependentConfirmation proves old submissions without altering the latest cached page.
func TestPlainReplyPageIndependentConfirmation(t *testing.T) {
	cases := []struct {
		name, mutation string
		newer          int
		wantErr        error
	}{
		{name: "initial submission"},
		{name: "old receipt outside latest page", newer: 31},
		{name: "old receipt text conflict", newer: 31, mutation: "text", wantErr: ErrConflict},
		{name: "old receipt actor conflict", newer: 31, mutation: "actor", wantErr: ErrConflict},
		{name: "old receipt target conflict", newer: 31, mutation: "target", wantErr: ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			root := archive.roots[0].Timestamp
			req := ThreadReplyRequest{OccurrenceID: "C_TEST:" + root, RequestID: "page-independent", Text: "original submission"}
			view, err := service.ReplyToAlert(ctx, "operator", req)
			if err != nil {
				t.Fatal(err)
			}
			want := &ThreadReplyConfirmation{OccurrenceID: req.OccurrenceID, RequestID: req.RequestID, Actor: "operator"}
			if diff := cmp.Diff(want, view.Confirmation); diff != "" {
				t.Errorf("unexpected initial confirmation (-want +got):\n%s", diff)
			}
			var timestamps []string
			for i := 1; i <= tc.newer; i++ {
				ts := fmt.Sprintf("%d.%06d", clock.now.Unix(), 1000+i)
				timestamps = append(timestamps, ts)
				archive.replies[root] = append(archive.replies[root], slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: root, User: "U_HUMAN", Text: fmt.Sprintf("later %d", i)}})
				archive.roots[0].ReplyCount++
				archive.roots[0].LatestReply = ts
			}
			clock.now = clock.now.Add(time.Second)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			get, err := service.AlertThread(req.OccurrenceID, "")
			if err != nil {
				t.Fatal(err)
			}
			if get.Confirmation != nil {
				t.Fatal("ordinary GET page exposed POST confirmation")
			}
			calls, posts := archive.replyCalls, archive.posts
			actor := "operator"
			switch tc.mutation {
			case "text":
				req.Text = "different submission"
			case "actor":
				actor = "other"
			case "target":
				req.OccurrenceID = "C_TEST:" + archive.roots[1].Timestamp
			}
			req.ReconcileOnly = true
			view, err = service.ReplyToAlert(ctx, actor, req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("reconcile error %v, want %v", err, tc.wantErr)
			}
			if archive.posts != posts || archive.replyCalls != calls {
				t.Fatal("cached confirmation performed Slack I/O")
			}
			if err != nil {
				if view.Confirmation != nil {
					t.Fatal("conflicting fingerprint returned proof")
				}
				return
			}
			if diff := cmp.Diff(want, view.Confirmation); diff != "" {
				t.Errorf("unexpected reconciliation confirmation (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(get.Messages, view.Messages); diff != "" {
				t.Errorf("confirmation changed page (-want +got):\n%s", diff)
			}
			if tc.newer > 30 {
				if len(view.Messages) != 30 || view.TotalReplies != tc.newer+1 || view.NextCursor != timestamps[1] {
					t.Fatalf("latest page changed: %#v", view)
				}
				for _, message := range view.Messages {
					if message.RequestID == req.RequestID {
						t.Fatal("old receipt was inserted into the latest page")
					}
				}
			}
		})
	}
}

// TestPlainReplyConfirmationSnapshotChange refuses proof when replay changes between posting and projection.
func TestPlainReplyConfirmationSnapshotChange(t *testing.T) {
	cases := []struct{ name, mutation string }{{name: "record removed", mutation: "remove"}, {name: "actor changed", mutation: "actor"}, {name: "digest changed", mutation: "digest"}, {name: "mode changed", mutation: "mode"}, {name: "root changed", mutation: "root"}, {name: "source changed", mutation: "source"}, {name: "thread removed", mutation: "thread"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, _ := chatFixture(t)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			root := archive.roots[0].Timestamp
			req := ConversationRequest{OccurrenceID: "C_TEST:" + root, RequestID: "changing", Text: "original"}
			if _, err := service.postConversation(ctx, "operator", req, true, false, ""); err != nil {
				t.Fatal(err)
			}
			cache := service.currentCache()
			cache.ConversationRecords = maps.Clone(cache.ConversationRecords)
			record := cache.ConversationRecords[req.RequestID]
			switch tc.mutation {
			case "actor":
				record.Actor = "other"
			case "digest":
				record.TextHash = fmt.Sprintf("%x", sha256.Sum256([]byte("different")))
			case "mode":
				record.Mode = ""
			case "root":
				record.Root = archive.roots[1].Timestamp
			case "source":
				record.SourceOccurrenceID = "C_TEST:" + archive.roots[1].Timestamp
			case "thread":
				cache.Threads = maps.Clone(cache.Threads)
				delete(cache.Threads, root)
			}
			cache.ConversationRecords[req.RequestID] = record
			if tc.mutation == "remove" {
				delete(cache.ConversationRecords, req.RequestID)
			}
			service.publish(cache)
			view, err := service.confirmedAlertThread("operator", req)
			if !errors.Is(err, ErrDeliveryUncertain) || view.Confirmation != nil {
				t.Fatalf("changed snapshot returned proof %#v, error %v", view.Confirmation, err)
			}
			if archive.posts != 1 {
				t.Fatal("projection retried a post")
			}
		})
	}
}

// TestThreadColorProjection keeps color leaves as literal display text without active presentation fields.
func TestThreadColorProjection(t *testing.T) {
	for _, tc := range []struct{ name, value string }{{name: "hex", value: "#aa33bb"}, {name: "literal text", value: "<b>not HTML or CSS</b>"}} {
		t.Run(tc.name, func(t *testing.T) {
			service, _, _ := fixture(t)
			data, err := json.Marshal(map[string]any{"blocks": []any{map[string]any{"type": "rich_text", "elements": []any{map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "color", "value": tc.value}}}}}}})
			if err != nil {
				t.Fatal(err)
			}
			var message slack.Message
			if err := json.Unmarshal(data, &message); err != nil {
				t.Fatal(err)
			}
			view := service.threadMessage(message, nil)
			got := view.Blocks[0]["elements"].([]map[string]any)[0]["elements"].([]map[string]any)[0]
			if diff := cmp.Diff(map[string]any{"type": "color", "value": tc.value}, got); diff != "" {
				t.Errorf("unexpected color leaf (-want +got):\n%s", diff)
			}
			if len(view.Unsupported) != 0 {
				t.Fatalf("supported color marked unsupported: %v", view.Unsupported)
			}
		})
	}
}

// TestPlainReplyReservedMarker prevents legacy rollback decoders from interpreting new replies as workflow records.
func TestPlainReplyReservedMarker(t *testing.T) {
	cases := []struct{ name, text string }{{name: "start", text: strings.TrimSpace(ActionMarker)}, {name: "middle", text: "quoted " + ActionMarker + "payload"}, {name: "newline", text: "quoted\n" + ActionMarker + "payload"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, _ := chatFixture(t)
			if err := service.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, err := service.ReplyToAlert(context.Background(), "operator", ThreadReplyRequest{OccurrenceID: "C_TEST:" + archive.roots[0].Timestamp, RequestID: "reserved", Text: tc.text})
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("reserved marker error %v", err)
			}
			if archive.posts != 0 {
				t.Fatal("reserved marker was posted")
			}
		})
	}
}

// TestPlainReplyLegacyMarkerReconcile preserves existing trusted submissions without permitting a new post.
func TestPlainReplyLegacyMarkerReconcile(t *testing.T) {
	for _, tc := range []struct{ name, text string }{{name: "legacy quoted marker", text: "quoted\n" + ActionMarker + "legacy text"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			req := ConversationRequest{OccurrenceID: "C_TEST:" + archive.roots[0].Timestamp, RequestID: "legacy", Text: tc.text}
			if _, err := service.postConversation(ctx, "operator", req, true, false, ""); err != nil {
				t.Fatal(err)
			}
			restarted, err := NewService(service.cfg, service.classifier, archive, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			view, err := restarted.ReplyToAlert(ctx, "operator", ThreadReplyRequest{OccurrenceID: req.OccurrenceID, RequestID: req.RequestID, Text: req.Text, ReconcileOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.text, view.Messages[0].Text); diff != "" {
				t.Errorf("unexpected legacy text (-want +got):\n%s", diff)
			}
			if archive.posts != 1 {
				t.Fatal("legacy reconciliation posted again")
			}
		})
	}
}

// TestPlainReplyConflictingProof never exposes a receipt rejected by shared durable replay.
func TestPlainReplyConflictingProof(t *testing.T) {
	for _, tc := range []struct{ name, mutation string }{{name: "digest conflict", mutation: "digest"}, {name: "actor conflict", mutation: "actor"}, {name: "target conflict", mutation: "target"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, archive, clock := chatFixture(t)
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			root := archive.roots[0].Timestamp
			req := ThreadReplyRequest{OccurrenceID: "C_TEST:" + root, RequestID: "conflicting", Text: "original"}
			if _, err := service.ReplyToAlert(ctx, "operator", req); err != nil {
				t.Fatal(err)
			}
			var record conversationRecord
			if err := json.Unmarshal([]byte(archive.lastMessage.Metadata.EventPayload["record"].(string)), &record); err != nil {
				t.Fatal(err)
			}
			record.Request.Text = "different current body"
			target := 0
			switch tc.mutation {
			case "digest":
				record.TextHash = fmt.Sprintf("%x", sha256.Sum256([]byte("different submission")))
			case "actor":
				record.Actor = "other"
			case "target":
				target = 1
				record.Root = archive.roots[target].Timestamp
				record.SourceOccurrenceID = "C_TEST:" + record.Root
				record.Request.OccurrenceID = record.SourceOccurrenceID
			}
			message, err := service.conversationMessage(record, "", "")
			if err != nil {
				t.Fatal(err)
			}
			ts := fmt.Sprintf("%d.900000", clock.now.Unix())
			archive.replies[record.Root] = append(archive.replies[record.Root], slack.Message{Msg: slack.Msg{Timestamp: ts, ThreadTimestamp: record.Root, User: "U_WRITER", BotID: "B_WRITER", Text: message.Text, Metadata: message.Metadata}})
			archive.roots[target].ReplyCount++
			archive.roots[target].LatestReply = ts
			clock.now = clock.now.Add(time.Second)
			restarted, err := NewService(service.cfg, service.classifier, archive, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if _, exists := restarted.currentCache().ConversationRecords[req.RequestID]; exists {
				t.Fatal("conflicting ledger survived replay")
			}
			ids := []string{req.OccurrenceID}
			if target != 0 {
				ids = append(ids, record.SourceOccurrenceID)
			}
			for _, id := range ids {
				view, err := restarted.AlertThread(id, "")
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range view.Messages {
					if m.Actor != "" || m.RequestID != "" || m.Author.ID != "U_WRITER" || !strings.HasPrefix(m.Text, threadReplyPrefix) {
						t.Fatalf("conflicting receipt exposed proof: %#v", m)
					}
				}
			}
			req.ReconcileOnly = true
			if _, err := restarted.ReplyToAlert(ctx, "operator", req); !errors.Is(err, ErrDeliveryUncertain) {
				t.Fatalf("conflicting reconciliation %v", err)
			}
			if archive.posts != 1 {
				t.Fatal("conflicting reconciliation posted")
			}
		})
	}
}

// TestThreadImageHosts keeps public image families narrow and credential-free.
func TestThreadImageHosts(t *testing.T) {
	cases := []struct {
		name, url string
		allowed   bool
	}{
		{name: "Slack avatar", url: "https://avatars.slack-edge.com/a.png", allowed: true},
		{name: "gravatar", url: "https://secure.gravatar.com/avatar/a", allowed: true},
		{name: "lookalike", url: "https://slack-edge.com.evil.test/a.png"},
		{name: "credentials", url: "https://secret@files.slack.com/a.png"},
		{name: "SVG", url: "data:image/svg+xml;base64,AAAA"},
		{name: "raster", url: "data:image/png;base64,AAAA", allowed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := ""
			if tc.allowed {
				want = tc.url
			}
			if diff := cmp.Diff(want, SafeThreadImage(tc.url)); diff != "" {
				t.Errorf("unexpected image (-want +got):\n%s", diff)
			}
		})
	}
}
