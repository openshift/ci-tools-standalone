package main

import (
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/prow/pkg/github"
)

func TestPipelineHelpDoesNotReconcile(t *testing.T) {
	for _, action := range []github.IssueCommentEventAction{github.IssueCommentActionCreated, github.IssueCommentActionEdited} {
		f := newAgenticFixture(t, "auto")
		cw := &clientWrapper{ghc: f.gh, agentic: f.a}
		cw.handleIssueComment(f.a.logger, github.IssueCommentEvent{Action: action, Repo: f.gh.pr.Base.Repo,
			Issue: github.Issue{Number: 42, PullRequest: &struct{}{}}, Comment: github.IssueComment{Body: "/pipeline help"}})
		if action == github.IssueCommentActionCreated {
			require.Len(t, f.gh.comments, 1)
			for _, command := range []string{"help", "required", "remaining", "auto", "agentic-mode", "skip-agentic-mode", "mark-pipeline-gate"} {
				require.Contains(t, f.gh.comments[0].Body, "/pipeline "+command)
			}
		} else {
			require.Empty(t, f.gh.comments)
		}
		require.Zero(t, f.gh.getPullRequestCalls)
		require.Empty(t, f.gh.checks)
	}
}

func TestTraditionalMarkPipelineGate(t *testing.T) {
	for _, scenario := range []string{"member", "collaborator", "untrusted", "stale-head", "explicit-head", "missing-gate", "disabled", "edited", "suffix", "multiple", "no-app", "write-error"} {
		t.Run(scenario, func(t *testing.T) {
			refs := makeTriggerPJ(strings.Repeat("a", 40)).Spec.Refs
			foreign := traditionalCheck(refs, "completed", "success", "another app")
			foreign.ID, foreign.App.ID = 900, 202
			gh := &fakeAgenticGitHub{member: true, checks: []github.CheckRun{foreign},
				pr: github.PullRequest{Number: refs.Pulls[0].Number, State: github.PullRequestStateOpen,
					Head: github.PullRequestBranch{SHA: refs.Pulls[0].SHA},
					Base: github.PullRequestBranch{Ref: refs.BaseRef, Repo: github.Repo{Name: refs.Repo, Owner: github.User{Login: refs.Org}}}}}
			checks := &dispatchChecks{gh: gh, appID: 101}
			if scenario != "missing-gate" {
				require.NoError(t, checks.pending(refs))
			}
			cw := &clientWrapper{ghc: gh, checks: checks, watcher: &watcher{config: mustEnabledConfig(t)}, lgtmWatcher: &watcher{},
				configDataProvider: &ConfigDataProvider{updatedPresubmits: map[string]presubmitTests{}}}
			event := github.IssueCommentEvent{Action: github.IssueCommentActionCreated, Repo: gh.pr.Base.Repo,
				Issue:   github.Issue{Number: gh.pr.Number, PullRequest: &struct{}{}},
				Comment: github.IssueComment{ID: 500, Body: "/pipeline mark-pipeline-gate", User: github.User{Login: "maintainer"}}}
			switch scenario {
			case "collaborator":
				gh.member, gh.collaborator = false, true
			case "untrusted":
				gh.member = false
			case "stale-head":
				event.Comment.Body += " " + strings.Repeat("b", 40)
			case "explicit-head":
				event.Comment.Body += " " + refs.Pulls[0].SHA
			case "disabled":
				cw.watcher.config.Orgs[0].Repos[0].Branches = []string{"release"}
			case "edited":
				event.Action = github.IssueCommentActionEdited
			case "suffix":
				event.Comment.Body += " please"
			case "multiple":
				event.Comment.Body += "\n/pipeline skip-agentic-mode"
			case "no-app":
				cw.checks = nil
			case "write-error":
				gh.failCheck = true
			}
			cw.handleIssueComment(logrus.NewEntry(logrus.New()), event)
			require.Equal(t, foreign, gh.checks[0])
			switch scenario {
			case "missing-gate":
				require.Len(t, gh.checks, 1, "mark must not create a new successful gate")
			case "member", "collaborator", "explicit-head":
				require.Equal(t, "success", gh.checks[1].Conclusion)
				require.Contains(t, gh.checks[1].Output.Summary, "comment 500")
			default:
				require.NotEqual(t, "success", gh.checks[1].Conclusion)
			}
			require.Zero(t, gh.statusWrites)
		})
	}
}
