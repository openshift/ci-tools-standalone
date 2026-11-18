package main

import (
	"context"
	"encoding/base64"
	"image/png"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// TestDemoThreadPresentation keeps names and distinct offline icons demonstrable without Slack.
func TestDemoThreadPresentation(t *testing.T) {
	for _, tc := range []struct {
		name string
		want opsalerts.ThreadPerson
	}{
		{name: "human", want: opsalerts.ThreadPerson{ID: "U_DEMO_OPERATOR", Name: "Demo Operator", AvatarURL: demoThreadPersonIcon}},
		{name: "source bot", want: opsalerts.ThreadPerson{ID: "U_PROM", Name: "Prometheus", AvatarURL: demoThreadIcon, IsBot: true}},
		{name: "dashboard bot", want: opsalerts.ThreadPerson{ID: "U_DPTP", Name: "DPTP dashboard", AvatarURL: demoThreadIcon, IsBot: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			thread := opsalerts.AlertThread{
				Root:   opsalerts.ThreadMessage{Author: opsalerts.ThreadPerson{ID: tc.want.ID}},
				People: map[string]opsalerts.ThreadPerson{},
				Emojis: map[string]string{},
			}
			new(demoArchive).EnrichThread(context.Background(), &thread)
			if diff := cmp.Diff(tc.want, thread.Root.Author); diff != "" {
				t.Errorf("unexpected author (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(demoThreadEmojiIcon, thread.Emojis["dashboard_check"]); diff != "" {
				t.Errorf("unexpected custom emoji (-want +got):\n%s", diff)
			}
			for _, image := range []string{thread.Root.Author.AvatarURL, thread.Emojis["dashboard_check"]} {
				if opsalerts.SafeThreadImage(image) != image {
					t.Fatalf("demo icon is not an allowed image")
				}
				encoded := strings.TrimPrefix(image, "data:image/png;base64,")
				decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))
				config, err := png.DecodeConfig(decoder)
				if err != nil {
					t.Fatalf("decoding demo icon: %v", err)
				}
				if config.Width != 32 || config.Height != 32 {
					t.Errorf("unexpected icon dimensions: %dx%d", config.Width, config.Height)
				}
			}
		})
	}
}
