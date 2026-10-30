package opsalerts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// TestConversationSlackFooterReplay covers Slack's emoji normalization during
// history recovery without relaxing metadata or question integrity checks.
func TestConversationSlackFooterReplay(t *testing.T) {
	cases := []struct {
		name, mutation string
	}{
		{name: "normalized footer"},
		{name: "edited question", mutation: "text"},
		{name: "missing metadata", mutation: "metadata"},
		{name: "altered footer", mutation: "footer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, archive, clock := chatFixture(t)
			ctx := context.Background()
			if err := service.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			request := ConversationRequest{RequestID: "original", Text: "What is <happening> & why?"}
			conversation, err := service.Converse(ctx, "operator", request)
			if err != nil {
				t.Fatal(err)
			}
			root := &archive.roots[len(archive.roots)-1]
			root.Text = strings.TrimSuffix(root.Text, "\n\n💬 Dashboard · Chai") + "\n\n:speech_balloon: Dashboard · Chai"
			switch tc.mutation {
			case "text":
				root.Text = strings.Replace(root.Text, "happening", "changed", 1)
			case "metadata":
				root.Metadata = slack.SlackMetadata{}
			case "footer":
				root.Text += " changed"
			}
			restarted, err := NewService(service.cfg, service.classifier, archive, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if restarted.Snapshot().Status.Phase != "ready" {
				t.Fatal("chat recovery affected alert workflow readiness")
			}
			snapshot := restarted.Conversations()
			followup := ConversationRequest{ConversationID: conversation.ID, RequestID: "followup", Text: "What should we check next?"}
			if tc.mutation != "" {
				if snapshot.Status.Phase != "degraded" {
					t.Fatalf("corrupt chat was accepted: %#v", snapshot)
				}
				if _, err := restarted.Converse(ctx, "operator", followup); !errors.Is(err, ErrNotReady) {
					t.Fatalf("new request during quarantine: %v", err)
				}
				if archive.posts != 1 {
					t.Fatal("quarantined chat sent another request")
				}
				return
			}
			if snapshot.Status.Phase != "ready" || len(snapshot.Conversations) != 1 || len(snapshot.Conversations[0].Messages) != 1 || snapshot.Conversations[0].Messages[0].Text != request.Text {
				t.Fatalf("normalized Slack history did not recover: %#v", snapshot)
			}
			if _, err := restarted.Converse(ctx, "operator", request); err != nil || archive.posts != 1 {
				t.Fatalf("recovered request was not idempotent: posts=%d error=%v", archive.posts, err)
			}
			if _, err := restarted.Converse(ctx, "operator", followup); err != nil {
				t.Fatalf("new request after recovery: %v", err)
			}
		})
	}
}
