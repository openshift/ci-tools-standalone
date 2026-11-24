package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

// TestProgressCopy keeps readiness, bot acknowledgement, and job results distinct.
func TestProgressCopy(t *testing.T) {
	refs := &v1.Refs{Org: "openshift", Repo: "installer", BaseRef: "main", Pulls: []v1.Pull{{Number: 11087, SHA: "aaaa1111"}}}
	for _, tc := range []struct {
		name string
		view progressView
		want []string
		omit string
	}{
		{name: "traditional no fake intermediate step", view: progressView{trigger: progressStageOne, satisfied: true, phase: progressPosted}, want: []string{"posted stage-2 test requests", "**PR status table** below"}, omit: "Agentic dispatching"},
		{name: "LGTM handoff does not wait for stage one", view: progressView{trigger: progressLGTM, satisfied: true, chai: true}, want: []string{"Waiting to notify Chai."}, omit: "Waiting for stage 1"},
		{name: "traditional LGTM dispatch does not wait for stage one", view: progressView{trigger: progressLGTM, satisfied: true}, want: []string{"Waiting to request stage-2 jobs."}, omit: "Waiting for stage 1"},
		{name: "auto override still needs stage one", view: waitingProgress(RepoConfig{Trigger: "lgtm"}, &github.PullRequest{Labels: []github.Label{{Name: PipelineAutoLabel}}}, true), want: []string{"Auto enabled", "class trigger done", "class wait here", "Waiting for stage 1", "**PR status table** below"}},
		{name: "live Chai deadline", view: progressView{trigger: progressManual, satisfied: true, phase: progressChai, chai: true, deadline: time.Unix(1, 0)}, want: []string{"class handoff here", "before the timeout", "**If you're stuck:**", "/pipeline skip-agentic-mode"}},
		{name: "Chai replied without finishing", view: progressView{trigger: progressLGTM, phase: progressChai, chai: true, replied: true}, want: []string{"no automatic fallback after a reply", "/pipeline skip-agentic-mode"}},
		{name: "Chai mark not a test result", view: progressView{trigger: progressManual, satisfied: true, phase: progressChaiMarked, chai: true}, want: []string{"Chai marked dispatch complete", "The gate doesn't confirm that jobs started", "**PR status table** below", "Tests dispatched"}, omit: "passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderProgress(refs, tc.view)
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q in progress comment", want)
				}
			}
			if tc.omit != "" && strings.Contains(body, tc.omit) {
				t.Errorf("progress comment unexpectedly contains %q", tc.omit)
			}
			visible, help, found := strings.Cut(body, "\n\n<details>\n<summary>About the pipeline controller</summary>\n\n")
			if !found || help != pipelineNotificationHelp+"\n\n</details>" || strings.Contains(visible, pipelineNotificationHelp) {
				t.Error("the original help must appear only inside the collapsed section")
			}
			if !strings.Contains(visible, "**If you're stuck:**") || strings.Contains(body, "**Checks**") || strings.Contains(body, "/checks)") {
				t.Error("recovery guidance must be visible and must not point to the Checks tab")
			}
			if _, executable := parsePipelineCommand(body); executable || strings.Contains(body, "\n/test ") {
				t.Error("progress comment contains an executable command")
			}
		})
	}
}

// TestProgressStageColors checks that only completed steps turn green.
func TestProgressStageColors(t *testing.T) {
	refs := &v1.Refs{Pulls: []v1.Pull{{SHA: "aaaa1111"}}}
	for _, tc := range []struct {
		name string
		view progressView
		want []string
	}{
		{name: "waiting for LGTM", view: progressView{trigger: progressLGTM, chai: true}, want: []string{"wait here"}},
		{name: "LGTM reached before handoff", view: progressView{trigger: progressLGTM, satisfied: true, chai: true}, want: []string{"wait here", "trigger done"}},
		{name: "Chai dispatch in progress", view: progressView{trigger: progressLGTM, phase: progressChai, chai: true}, want: []string{"handoff here", "wait,trigger done"}},
		{name: "Chai dispatch complete", view: progressView{trigger: progressLGTM, phase: progressChaiMarked, chai: true}, want: []string{"wait,trigger,handoff,outcome done"}},
		{name: "traditional jobs requested", view: progressView{trigger: progressStageOne, phase: progressPosted}, want: []string{"wait,trigger,outcome done"}},
		{name: "no new jobs needed", view: progressView{trigger: progressManual, phase: progressEmpty}, want: []string{"wait,trigger,outcome done"}},
		{name: "recovered completion", view: progressView{trigger: progressStageOne, phase: progressComplete, chai: true}, want: []string{"wait,trigger,handoff,outcome done"}},
		{name: "gate acknowledged", view: progressView{trigger: progressLGTM, phase: progressMarked}, want: []string{"wait,trigger,outcome done"}},
		{name: "dispatch unconfirmed", view: progressView{trigger: progressLGTM, phase: progressFailed}, want: []string{"outcome here", "wait,trigger done"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for line := range strings.SplitSeq(renderProgress(refs, tc.view), "\n") {
				if class, ok := strings.CutPrefix(line, "  class "); ok {
					got = append(got, class)
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}

// TestProgressRevision keeps delayed push events from replacing the current comment.
func TestProgressRevision(t *testing.T) {
	for _, tc := range []struct {
		name, live, base string
		enabled, tracked bool
		active, draft    bool
		coldStale        bool
		action           github.PullRequestEventAction
		wantEdit         bool
		wantQueries      int
	}{
		{name: "current opt-in revision", live: "new", base: "main", enabled: true, active: true, wantEdit: true, wantQueries: 1},
		{name: "delayed push", live: "newer", base: "main", enabled: true, active: true, wantQueries: 1},
		{name: "current push after rejecting stale cold dispatch", live: "new", base: "main", enabled: true, active: true, coldStale: true, wantEdit: true, wantQueries: 2},
		{name: "no opt-in", live: "new", base: "main", active: true},
		{name: "delayed ineligible retarget", live: "old", base: "main", tracked: true, active: true, action: github.PullRequestActionEdited, wantQueries: 1},
		{name: "delayed draft event", live: "new", base: "main", enabled: true, tracked: true, active: true, action: github.PullRequestActionConvertedToDraft, wantQueries: 1},
		{name: "current draft event", live: "new", base: "main", enabled: true, tracked: true, active: true, draft: true, action: github.PullRequestActionConvertedToDraft, wantQueries: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			pr := f.gh.pr
			pr.Head.SHA = "new"
			w := &watcher{}
			config := "orgs:\n- org: org\n  repos:\n  - name: repo\n    mode: {progress_comment: true}\n"
			if !tc.enabled {
				config = "orgs:\n- org: org\n  repos: [repo]\n"
			}
			if err := yaml.Unmarshal([]byte(config), &w.config); err != nil {
				t.Fatal(err)
			}
			gh := &progressCommentFake{head: tc.live, base: tc.base, closed: !tc.active, draft: tc.draft, comments: []progressCommentNode{{FullDatabaseID: "10", Body: pullRequestInfoComment, Author: progressCommentAuthor{Login: progressBot, Type: "Bot"}}}}
			logger := logrus.NewEntry(logrus.New())
			p := newProgressPublisher(gh, logger)
			cw := &clientWrapper{ghc: f.gh, lgtmWatcher: w, progress: p}
			work := agenticWork{org: pr.Base.Repo.Owner.Login, repo: pr.Base.Repo.Name, number: pr.Number}
			if tc.coldStale {
				stale := pr
				stale.Head.SHA = "old"
				p.submit(pullRefs(work.org, work.repo, &stale), "stale dispatch", false, false)
				p.publishNext(t.Context())
				if len(gh.edits) != 0 {
					t.Fatal("cold stale dispatch edited the comment")
				}
			}
			oldHead := "new"
			if tc.action == github.PullRequestActionEdited {
				oldHead, pr.Base.Ref = "old", "release"
			}
			if tc.tracked {
				p.prs[work] = &progressEntry{verified: progressRevision{head: oldHead, base: "main"}}
			}
			action := tc.action
			if action == "" {
				action = github.PullRequestActionSynchronize
			}
			pr.Draft = action == github.PullRequestActionConvertedToDraft
			cw.handleProgressRevision(logger, github.PullRequestEvent{Action: action, Repo: pr.Base.Repo, PullRequest: pr, Changes: []byte(`{"base":{"ref":{"from":"main"}}}`)})
			if e := p.prs[work]; tc.tracked && (e.verified.head != oldHead || e.verified.base != "main" || e.paused != tc.draft) {
				t.Fatalf("lifecycle event unexpectedly changed the tracked state: revision=%+v paused=%t", e.verified, e.paused)
			}
			p.publishNext(t.Context())
			if (len(gh.edits) != 0) != tc.wantEdit || gh.queries != tc.wantQueries {
				t.Errorf("got %d edits and %d queries, want edit=%t and %d queries", len(gh.edits), gh.queries, tc.wantEdit, tc.wantQueries)
			}
			if tc.wantEdit && (len(gh.bodies) != 1 || !strings.Contains(gh.bodies[0], "Waiting for `/lgtm`")) {
				t.Error("LGTM config did not project an LGTM waiting comment")
			}
		})
	}
}
