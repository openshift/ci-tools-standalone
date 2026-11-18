package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
	"github.com/slack-go/slack"
)

// TestDemoConversationPosts keeps new chat roots, replies and retries in one synthetic thread.
func TestDemoConversationPosts(t *testing.T) {
	cases := []struct {
		name, root string
		chai       bool
		plain      bool
	}{
		{name: "new question with Chai", chai: true},
		{name: "new question without mention"},
		{name: "followup with Chai", root: "100.000001", chai: true},
		{name: "plain reply", root: "100.000001", plain: true},
		{name: "plain reply explicitly mentions Chai", root: "100.000001", plain: true, chai: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := &demoArchive{threads: map[string][]slack.Message{}, next: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), writer: opsalerts.Identity{UserID: "U_DPTP", BotID: "B_DPTP"}}
			if tc.root != "" {
				archive.roots = []slack.Message{{Msg: slack.Msg{Timestamp: tc.root, Text: "Existing root"}}}
			}
			text := "Explain the controller backlog."
			if tc.chai {
				text = "<@U_CHAI> " + text
			}
			message := slack.Msg{Text: text, Metadata: slack.SlackMetadata{EventType: opsalerts.ConversationMetadataEvent, EventPayload: map[string]interface{}{"record": "test-record"}}}
			if tc.plain {
				message.Metadata.EventType = opsalerts.ThreadReplyMetadataEvent
			}
			timestamp, err := archive.Post(context.Background(), "CHY2E1BL4", tc.root, "stable-chat-request", message)
			if err != nil {
				t.Fatal(err)
			}
			if timestamp == "" {
				t.Fatal("post returned no timestamp")
			}
			root := tc.root
			if root == "" {
				root = timestamp
			}
			roots, err := archive.History(context.Background(), "CHY2E1BL4", time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(1, len(roots)); diff != "" {
				t.Fatalf("unexpected root count (-want +got):\n%s", diff)
			}
			messages, err := archive.Replies(context.Background(), "CHY2E1BL4", root)
			if err != nil {
				t.Fatal(err)
			}
			wantReplies := 0
			if tc.root != "" {
				wantReplies++
			}
			if tc.chai {
				wantReplies++
			}
			if diff := cmp.Diff(wantReplies, len(messages)); diff != "" {
				t.Fatalf("unexpected replies (-want +got):\n%s", diff)
			}
			posted := roots[0].Msg
			if tc.root != "" {
				posted = messages[0].Msg
			}
			if diff := cmp.Diff(text, posted.Text); diff != "" {
				t.Errorf("unexpected posted text (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(message.Metadata, posted.Metadata); diff != "" {
				t.Errorf("unexpected metadata (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.root, posted.ThreadTimestamp); diff != "" {
				t.Errorf("unexpected post thread (-want +got):\n%s", diff)
			}
			for _, reply := range messages {
				if diff := cmp.Diff(root, reply.ThreadTimestamp); diff != "" {
					t.Errorf("unexpected reply thread (-want +got):\n%s", diff)
				}
			}
			retry, err := archive.Post(context.Background(), "CHY2E1BL4", tc.root, "stable-chat-request", message)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(timestamp, retry); diff != "" {
				t.Errorf("unexpected retry confirmation (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(roots, archive.roots); diff != "" {
				t.Errorf("retry changed roots (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(messages, archive.threads[root]); diff != "" {
				t.Errorf("retry changed replies (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDemoMessageTime keeps new roots within replay coverage while preserving Slack timestamp precision.
func TestDemoMessageTime(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		previous time.Time
		want     time.Time
	}{
		{name: "first message has replay headroom", want: now.Add(-time.Second)},
		{name: "next microsecond under frozen clock", previous: now.Add(-time.Second), want: now.Add(-time.Second + time.Microsecond)},
		{name: "clock advancement starts a fresh past timestamp", previous: now.Add(-time.Minute), want: now.Add(-time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := demoMessageTime(now, tc.previous)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected timestamp (-want +got):\n%s", diff)
			}
			if got.After(now) || !got.After(tc.previous) {
				t.Error("demo message must advance and remain within replay coverage")
			}
		})
	}
}
