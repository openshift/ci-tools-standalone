package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
	"github.com/slack-go/slack"
)

// demoThreadIcon is a static raster fixture, not a remote image dependency.
const demoThreadIcon = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAAAWElEQVR4nO3XsQ0AIAhEUfZyFLdyYW1tSI5CkfgvoSPeK2g029L6mDfGvPwJuFXqYp4HRAPgKEB5MLoPAEAtgBKlFEDdGwAAIB2gRCkFEAKcHgD5X7PvAQu4IJjQGgorQAAAAABJRU5ErkJggg=="

// demoThreadPersonIcon distinguishes a human's cached avatar from bot fixtures.
const demoThreadPersonIcon = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAAATUlEQVR4nGNgQALqtdn/6YEZcIGR6QB6WYrTMYPeAaSCUQfQ1AHEGEiq+lEHjDpg1AGjDhh1wKgDRh0wtNoDI9MBtMajDhj4rtmIdwAAb8wnLGCEECMAAAAASUVORK5CYII="

// demoThreadEmojiIcon demonstrates a custom Slack emoji without a CDN request.
const demoThreadEmojiIcon = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAAAVElEQVR4nO3XIRIAIAhEUS7jjTyS99ZKIYCIgb8zVPYFCiIqY81dMWKlJ6Cq1MQAAJC9UAdAGcBbCsAFsJbflAII30BWKYAw4MUAAAAAwP/XrD3gAPcuAwkw+MuhAAAAAElFTkSuQmCC"

// ResolveThreadReference supplies known demo handles without any Slack calls.
func (*demoArchive) ResolveThreadReference(kind, name string) (string, bool) {
	if kind != "@" {
		return "", false
	}
	people := map[string]string{"demo-operator": "U_DEMO_OPERATOR", "dptp-bot": "U_DPTP", "chai-bot": "U_CHAI"}
	id, found := people[strings.ToLower(name)]
	return id, found
}

// seedDemoDiscussion adds safe rich Slack content without changing workflow state.
func seedDemoDiscussion(archive *demoArchive, now time.Time) error {
	var reply slack.Message
	if err := json.Unmarshal([]byte(`{"user":"U_DEMO_OPERATOR","text":"Checking *Tide* with <@U_CHAI>. :dashboard_check: :eyes:","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"Checking *Tide* with <@U_CHAI>. :dashboard_check:"}},{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"Read-only triage: ","style":{"bold":true}},{"type":"emoji","name":"eyes"},{"type":"text","text":" queued merges"}]}]}],"attachments":[{"color":"#36a64f","title":"Synthetic triage evidence","title_link":"https://status.ci.openshift.org/","text":"No production systems were contacted.","fields":[{"title":"Queue","value":"Demo only","short":true}]}],"reactions":[{"name":"eyes","count":2,"users":["U_DEMO_OPERATOR","U_CHAI"]},{"name":"dashboard_check","count":1,"users":["U_DEMO_OPERATOR"]}]}`), &reply); err != nil {
		return fmt.Errorf("decoding demo discussion: %w", err)
	}
	for i := range archive.roots {
		root := &archive.roots[i]
		if root.BotID == "B_CHAI" {
			continue
		}
		item := reply
		item.ThreadTimestamp, item.Timestamp = root.Timestamp, protocolTimestamp(now.Add(-2*time.Minute).Add(time.Duration(i)*time.Microsecond))
		archive.threads[root.Timestamp] = append(archive.threads[root.Timestamp], item)
		root.ReplyCount++
		root.LatestReply = item.Timestamp
	}
	return nil
}

// EnrichThread supplies static names and icons in demo mode without network requests.
func (*demoArchive) EnrichThread(_ context.Context, thread *opsalerts.AlertThread) {
	names := map[string]string{"U_PROM": "Prometheus", "U_PROW": "Prow", "U_SHIP": "SHIP", "U_CHAI": "Chai", "U_DPTP": "DPTP dashboard", "U_DEMO_OPERATOR": "Demo Operator", "U_UNKNOWN": "Demo notification"}
	people, _ := threadReferences(*thread)
	for _, id := range people {
		person := thread.People[id]
		person.ID = id
		if name, exists := names[id]; exists {
			person.Name = name
			person.AvatarURL = demoThreadIcon
			if id == "U_DEMO_OPERATOR" {
				person.AvatarURL = demoThreadPersonIcon
			}
		}
		person.IsBot = id != "U_DEMO_OPERATOR"
		thread.People[id] = person
	}
	thread.Emojis["dashboard_check"] = demoThreadEmojiIcon
	thread.Root.Author = thread.People[thread.Root.Author.ID]
	for i := range thread.Messages {
		thread.Messages[i].Author = thread.People[thread.Messages[i].Author.ID]
	}
}
