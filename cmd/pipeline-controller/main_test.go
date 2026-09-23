package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"

	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/labels"
)

func mustEnabledConfig(t *testing.T) enabledConfig {
	t.Helper()
	const doc = `
orgs:
- org: openshift
  repos:
  - name: myrepo
    branches:
    - main
`
	var enabled enabledConfig
	if err := yaml.Unmarshal([]byte(doc), &enabled); err != nil {
		t.Fatalf("failed to parse enabled config: %v", err)
	}
	return enabled
}

func testRepo(t *testing.T) github.Repo {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"owner": map[string]string{"login": "openshift"},
		"name":  "myrepo",
	})
	if err != nil {
		t.Fatalf("marshal test repository: %v", err)
	}

	var result github.Repo
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("unmarshal test repository: %v", err)
	}
	return result
}

// TestHandleLabelAdditionLGTMIdempotent verifies that the LGTM scheduling path is
// idempotent per SHA: a repeated LGTM label event does not double-post /test.
func TestHandleLabelAdditionLGTMIdempotent(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	lgtmEnabled := mustEnabledConfig(t)

	protectedJob := "pull-ci-openshift-myrepo-main-protected"
	provider := &ConfigDataProvider{updatedPresubmits: map[string]presubmitTests{
		org + "/" + repo: {
			protected: []config.Presubmit{{
				JobBase:      config.JobBase{Name: protectedJob},
				Reporter:     config.Reporter{Context: protectedJob},
				RerunCommand: "/test " + protectedJob,
			}},
		},
	}}

	ghc := &fakeGhClient{}
	cw := &clientWrapper{
		ghc:                ghc,
		configDataProvider: provider,
		watcher:            &watcher{},
		lgtmWatcher:        &watcher{config: lgtmEnabled},
		pjLister:           nil,
	}

	event := github.PullRequestEvent{
		Action: github.PullRequestActionLabeled,
		Label:  github.Label{Name: labels.LGTM},
		Repo:   testRepo(t),
		PullRequest: github.PullRequest{
			Number: prNum,
			Base:   github.PullRequestBranch{Ref: baseRef, SHA: "base-sha"},
			Head:   github.PullRequestBranch{SHA: sha},
		},
	}

	l := logrus.NewEntry(logrus.New())
	cw.handleLabelAddition(l, event)
	cw.handleLabelAddition(l, event)

	if len(ghc.comments) != 1 {
		t.Fatalf("expected exactly 1 comment across two LGTM events, got %d: %v", len(ghc.comments), ghc.comments)
	}
	if !strings.Contains(ghc.comments[0], "/test "+protectedJob) {
		t.Errorf("expected the protected job to be scheduled, got: %q", ghc.comments[0])
	}
}

// TestHandleIssueCommentRemainingDeltaAndIdempotent verifies that /pipeline
// remaining schedules only the missing jobs (delta) and is idempotent per SHA.
func TestHandleIssueCommentRemainingDeltaAndIdempotent(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	enabled := mustEnabledConfig(t)

	jobA := "pull-ci-openshift-myrepo-main-conditional-a"
	jobB := "pull-ci-openshift-myrepo-main-conditional-b"
	presubmitFor := func(name string) config.Presubmit {
		return config.Presubmit{
			JobBase:      config.JobBase{Name: name, Annotations: map[string]string{"pipeline_run_if_changed": "^cmd/"}},
			Reporter:     config.Reporter{Context: name},
			RerunCommand: "/test " + name,
		}
	}
	provider := &ConfigDataProvider{updatedPresubmits: map[string]presubmitTests{
		org + "/" + repo: {pipelineConditionallyRequired: []config.Presubmit{presubmitFor(jobA), presubmitFor(jobB)}},
	}}

	ghc := &fakeGhClient{
		changes:     []github.PullRequestChange{{Filename: "cmd/main.go"}},
		pullRequest: &github.PullRequest{Base: github.PullRequestBranch{Ref: baseRef, SHA: "base-sha"}, Head: github.PullRequestBranch{SHA: sha}},
	}
	// jobA already exists at HEAD; the delta must schedule only jobB.
	pjLister := newFakePJLister(makeProwJob(jobA, sha))

	cw := &clientWrapper{
		ghc:                ghc,
		configDataProvider: provider,
		watcher:            &watcher{config: enabled},
		lgtmWatcher:        &watcher{},
		pjLister:           pjLister,
	}

	event := github.IssueCommentEvent{
		Repo:    testRepo(t),
		Issue:   github.Issue{Number: prNum, PullRequest: &struct{}{}},
		Comment: github.IssueComment{Body: "/pipeline remaining"},
	}

	l := logrus.NewEntry(logrus.New())
	cw.handleIssueComment(l, event)
	cw.handleIssueComment(l, event)

	// First call schedules the delta; second call is suppressed by the per-SHA
	// guard but, being an explicit command, posts a feedback comment.
	if len(ghc.comments) != 2 {
		t.Fatalf("expected 2 comments (schedule + guard feedback) across two /pipeline remaining comments, got %d: %v", len(ghc.comments), ghc.comments)
	}
	scheduleComment := ghc.comments[0]
	if !strings.Contains(scheduleComment, "/test "+jobB) {
		t.Errorf("expected the missing job %s to be scheduled, got: %q", jobB, scheduleComment)
	}
	if strings.Contains(scheduleComment, "/test "+jobA) {
		t.Errorf("expected the already-present job %s to be skipped by the delta, got: %q", jobA, scheduleComment)
	}
	feedbackComment := ghc.comments[1]
	if !strings.Contains(feedbackComment, "already been scheduled for this HEAD") {
		t.Errorf("expected guard feedback on the second /pipeline remaining, got: %q", feedbackComment)
	}
	if !strings.Contains(feedbackComment, sha) {
		t.Errorf("expected the feedback comment to mention the HEAD SHA %q, got: %q", sha, feedbackComment)
	}
}

// TestHandleIssueCommentAutoImmediateTriggerUsesDelta verifies that when
// /pipeline auto is used and the first stage is already complete, the synchronous
// immediate trigger runs the DELTA planner: it does not re-fire a second-stage
// job already present at HEAD (contrast: /pipeline required force-fires it, as
// TestHandleIssueCommentRequiredForcesAll shows).
func TestHandleIssueCommentAutoImmediateTriggerUsesDelta(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	lgtmEnabled := mustEnabledConfig(t)

	firstStageJob := "pull-ci-openshift-myrepo-main-unit"
	conditionalJob := "pull-ci-openshift-myrepo-main-conditional"
	provider := &ConfigDataProvider{updatedPresubmits: map[string]presubmitTests{
		org + "/" + repo: {
			alwaysRequired: []config.Presubmit{{
				JobBase:      config.JobBase{Name: firstStageJob},
				Reporter:     config.Reporter{Context: firstStageJob},
				RerunCommand: "/test " + firstStageJob,
			}},
			pipelineConditionallyRequired: []config.Presubmit{{
				JobBase:      config.JobBase{Name: conditionalJob, Annotations: map[string]string{"pipeline_run_if_changed": "^cmd/"}},
				Reporter:     config.Reporter{Context: conditionalJob},
				RerunCommand: "/test " + conditionalJob,
			}},
		},
	}}

	ghc := &fakeGhClient{
		changes:     []github.PullRequestChange{{Filename: "cmd/main.go"}},
		pullRequest: &github.PullRequest{Base: github.PullRequestBranch{Ref: baseRef, SHA: "base-sha"}, Head: github.PullRequestBranch{SHA: sha}},
	}
	// First-stage job succeeded at HEAD (so the first stage is complete) and the
	// conditional second-stage job was already triggered manually.
	pjLister := newFakePJLister(makeProwJob(firstStageJob, sha), makeProwJob(conditionalJob, sha))

	cw := &clientWrapper{
		ghc:                ghc,
		configDataProvider: provider,
		watcher:            &watcher{},
		lgtmWatcher:        &watcher{config: lgtmEnabled},
		pjLister:           pjLister,
		pipelineAutoCache:  NewPipelineAutoCache(),
	}

	event := github.IssueCommentEvent{
		Repo:    testRepo(t),
		Issue:   github.Issue{Number: prNum, PullRequest: &struct{}{}},
		Comment: github.IssueComment{Body: "/pipeline auto"},
	}

	cw.handleIssueComment(logrus.NewEntry(logrus.New()), event)

	// The /pipeline auto immediate trigger must NOT re-fire the already-present
	// conditional job.
	for _, c := range ghc.comments {
		if strings.Contains(c, "/test "+conditionalJob) {
			t.Errorf("/pipeline auto immediate trigger must not re-fire the already-present job (delta), got comment: %q", c)
		}
	}
}

// TestHandleIssueCommentRequiredForcesAll verifies that /pipeline required keeps
// its force-all semantics: it re-schedules a job even when it already exists at
// HEAD.
func TestHandleIssueCommentRequiredForcesAll(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	enabled := mustEnabledConfig(t)

	job := "pull-ci-openshift-myrepo-main-conditional"
	provider := &ConfigDataProvider{updatedPresubmits: map[string]presubmitTests{
		org + "/" + repo: {pipelineConditionallyRequired: []config.Presubmit{{
			JobBase:      config.JobBase{Name: job, Annotations: map[string]string{"pipeline_run_if_changed": "^cmd/"}},
			Reporter:     config.Reporter{Context: job},
			RerunCommand: "/test " + job,
		}}},
	}}

	ghc := &fakeGhClient{
		changes:     []github.PullRequestChange{{Filename: "cmd/main.go"}},
		pullRequest: &github.PullRequest{Base: github.PullRequestBranch{Ref: baseRef, SHA: "base-sha"}, Head: github.PullRequestBranch{SHA: sha}},
	}
	// The job already exists at HEAD; force must still re-schedule it.
	pjLister := newFakePJLister(makeProwJob(job, sha))

	cw := &clientWrapper{
		ghc:                ghc,
		configDataProvider: provider,
		watcher:            &watcher{config: enabled},
		lgtmWatcher:        &watcher{},
		pjLister:           pjLister,
	}

	event := github.IssueCommentEvent{
		Repo:    testRepo(t),
		Issue:   github.Issue{Number: prNum, PullRequest: &struct{}{}},
		Comment: github.IssueComment{Body: "/pipeline required"},
	}

	cw.handleIssueComment(logrus.NewEntry(logrus.New()), event)

	if len(ghc.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d: %v", len(ghc.comments), ghc.comments)
	}
	if !strings.Contains(ghc.comments[0], "/test "+job) {
		t.Errorf("expected /pipeline required to force-schedule the already-present job, got: %q", ghc.comments[0])
	}
}
