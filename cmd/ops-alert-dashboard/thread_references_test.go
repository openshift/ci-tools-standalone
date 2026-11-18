package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// TestCachedThreadReferences resolves fresh authoritative handles without extra metadata requests.
func TestCachedThreadReferences(t *testing.T) {
	for _, tc := range []struct {
		name, kind, handle, want string
		expired, failed, clash   bool
	}{
		{name: "cached user", kind: "@", handle: "alice.smith", want: "U_ONE"},
		{name: "case-insensitive handle", kind: "@", handle: "ALICE.SMITH", want: "U_ONE"},
		{name: "cached bot user ID", kind: "@", handle: "helper-bot", want: "U_HELPER"},
		{name: "display name is not a handle", kind: "@", handle: "Alice"},
		{name: "unknown handle", kind: "@", handle: "missing"},
		{name: "expired handle", kind: "@", handle: "alice.smith", expired: true},
		{name: "failed lookup cannot reuse handle", kind: "@", handle: "alice.smith", failed: true},
		{name: "ambiguous handle", kind: "@", handle: "alice.smith", clash: true},
		{name: "no workspace channel scan", kind: "#", handle: "other-channel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &slackHTTPFake{results: []slackHTTPResult{
				{body: `{"ok":true,"user":{"id":"U_ONE","name":"alice.smith","profile":{"display_name":"Alice"}}}`},
				{body: `{"ok":true,"bot":{"id":"B_HELPER","user_id":"U_HELPER","name":"helper-bot"}}`},
			}}
			clock := &directoryClock{now: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
			directory := &slackDirectory{client: fakeSlackClient(fake), clock: clock}
			for _, id := range []string{"U_ONE", "B_HELPER"} {
				page := directoryPersonPage(id)
				directory.EnrichThread(context.Background(), &page)
			}
			if tc.expired {
				clock.now = clock.now.Add(time.Hour)
			}
			if tc.failed {
				entry := directory.cache.people["U_ONE"]
				entry.Warning = "metadata unavailable"
				directory.cache.people["U_ONE"] = entry
			}
			if tc.clash {
				directory.cache.people["U_OTHER"] = directoryPerson{Person: opsalerts.ThreadPerson{ID: "U_OTHER"}, UserID: "U_OTHER", Handle: "alice.smith", Until: clock.now.Add(time.Hour)}
			}
			archive := &slackArchive{directory: directory}
			got, found := archive.ResolveThreadReference(tc.kind, tc.handle)
			if found != (tc.want != "") {
				t.Fatalf("unexpected resolution status: %v", found)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected reference (-want +got):\n%s", diff)
			}
			if len(fake.forms) != 2 {
				t.Fatalf("reference resolution read Slack: %d requests", len(fake.forms))
			}
		})
	}
}
