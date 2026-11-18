package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
	"github.com/slack-go/slack"
)

// TestSlackPlainReplyPost retains client IDs, metadata readback and the no-retry transport boundary.
func TestSlackPlainReplyPost(t *testing.T) {
	cases := []struct {
		name    string
		missing bool
		wantErr error
	}{
		{name: "confirmed new event"},
		{name: "stripped metadata stays uncertain", missing: true, wantErr: opsalerts.ErrDeliveryUncertain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := slack.Msg{Text: "Dashboard reply (on behalf of operator@example.test):\n\n<@U_CHAI> check <#C_TEAM> *bold* _italic_", Metadata: slack.SlackMetadata{EventType: opsalerts.ThreadReplyMetadataEvent, EventPayload: map[string]any{"record": `{"request":{"request_id":"plain"}}`}}}
			metadata, err := json.Marshal(message.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			readback := fmt.Sprintf(`{"ok":true,"messages":[{"ts":"200.000001","thread_ts":"100.000001","metadata":%s}]}`, metadata)
			if tc.missing {
				readback = `{"ok":true,"messages":[{"ts":"200.000001","thread_ts":"100.000001"}]}`
			}
			fake := &slackHTTPFake{results: []slackHTTPResult{{body: `{"ok":true,"channel":"C","ts":"200.000001"}`}, {body: readback}}}
			client := fakeSlackClient(fake)
			archive := &slackArchive{reader: client, writer: client}
			_, err = archive.Post(context.Background(), "C", "100.000001", "plain", message)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v, want %v", err, tc.wantErr)
			}
			if diff := cmp.Diff([]string{"/api/chat.postMessage", "/api/conversations.replies"}, fake.paths); diff != "" {
				t.Errorf("unexpected transport (-want +got):\n%s", diff)
			}
			if fake.forms[0].Get("client_msg_id") != "plain" || fake.forms[0].Get("thread_ts") != "100.000001" || fake.forms[0].Get("metadata") != string(metadata) {
				t.Fatalf("post lost durable delivery fields: %v", fake.forms[0])
			}
			if diff := cmp.Diff(message.Text, fake.forms[0].Get("text")); diff != "" {
				t.Errorf("Slack references were escaped (-want +got):\n%s", diff)
			}
			if fake.forms[0].Get("parse") != "none" || fake.forms[0].Get("link_names") != "" {
				t.Fatal("explicit references enabled automatic mention expansion")
			}
			if fake.forms[1].Get("oldest") != "200.000001" || fake.forms[1].Get("latest") != "200.000001" || fake.forms[1].Get("limit") != "1" {
				t.Fatalf("unbounded metadata confirmation: %v", fake.forms[1])
			}
		})
	}
}
