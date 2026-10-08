package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/flagutil"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/labels"
)

type fakeAgenticGitHub struct {
	pr                    github.PullRequest
	comments              []github.IssueComment
	checks                []github.CheckRun
	changes               []github.PullRequestChange
	member                bool
	collaborator          bool
	statusWrites          int
	statusUpdates         []github.Status
	failStatus            bool
	failCheck             bool
	failCheckAfterCreate  bool
	failCommentAfterWrite bool
	failLabelAfterWrite   bool
	checkAttempts         int
	getPullRequestCalls   int
	getPullRequestError   error
	beforeComment         func(string)
	afterCheckWrite       func(github.CheckRun)
}

func (f *fakeAgenticGitHub) GetPullRequest(_, _ string, _ int) (*github.PullRequest, error) {
	f.getPullRequestCalls++
	copy := f.pr
	copy.Labels = append([]github.Label{}, f.pr.Labels...)
	return &copy, f.getPullRequestError
}
func (f *fakeAgenticGitHub) CreateComment(_, _ string, _ int, body string) error {
	if f.beforeComment != nil {
		f.beforeComment(body)
	}
	f.comments = append(f.comments, github.IssueComment{Body: body})
	if f.failCommentAfterWrite {
		f.failCommentAfterWrite = false
		return fmt.Errorf("response lost: %w", io.ErrUnexpectedEOF)
	}
	return nil
}
func (f *fakeAgenticGitHub) GetPullRequestChanges(_, _ string, _ int) ([]github.PullRequestChange, error) {
	return f.changes, nil
}
func (f *fakeAgenticGitHub) CreateStatus(_, _, _ string, status github.Status) error {
	f.statusWrites++
	if f.failStatus {
		return io.ErrUnexpectedEOF
	}
	f.statusUpdates = append(f.statusUpdates, status)
	return nil
}
func (f *fakeAgenticGitHub) AddLabel(_, _ string, _ int, label string) error {
	f.pr.Labels = append(f.pr.Labels, github.Label{Name: label})
	if f.failLabelAfterWrite {
		f.failLabelAfterWrite = false
		return io.ErrUnexpectedEOF
	}
	return nil
}
func (f *fakeAgenticGitHub) RemoveLabel(_, _ string, _ int, label string) error {
	var kept []github.Label
	for _, current := range f.pr.Labels {
		if current.Name != label {
			kept = append(kept, current)
		}
	}
	f.pr.Labels = kept
	return nil
}
func (f *fakeAgenticGitHub) GetIssueLabels(_, _ string, _ int) ([]github.Label, error) {
	return f.pr.Labels, nil
}
func (f *fakeAgenticGitHub) ListCheckRuns(_, _, sha string) (*github.CheckRunList, error) {
	result := &github.CheckRunList{}
	for _, check := range f.checks {
		if check.HeadSHA == sha {
			result.CheckRuns = append(result.CheckRuns, check)
		}
	}
	return result, nil
}
func (f *fakeAgenticGitHub) CreateCheckRun(_, _ string, check github.CheckRun) (int64, error) {
	f.checkAttempts++
	if f.failCheck {
		return 0, io.ErrUnexpectedEOF
	}
	check.ID, check.App.ID = int64(len(f.checks)+1), 101
	f.checks = append(f.checks, check)
	if f.afterCheckWrite != nil {
		f.afterCheckWrite(check)
	}
	if f.failCheckAfterCreate {
		f.failCheckAfterCreate = false
		return 0, io.ErrUnexpectedEOF
	}
	return check.ID, nil
}
func (f *fakeAgenticGitHub) UpdateCheckRun(_, _ string, id int64, check github.CheckRun) error {
	f.checkAttempts++
	if f.failCheck {
		return io.ErrUnexpectedEOF
	}
	for i := range f.checks {
		if f.checks[i].ID != id {
			continue
		}
		old := f.checks[i]
		if check.ExternalID != "" {
			old.ExternalID = check.ExternalID
		}
		if check.Name != "" {
			old.Name = check.Name
		}
		if check.Status != "" {
			old.Status = check.Status
		}
		if check.Conclusion != "" {
			old.Conclusion = check.Conclusion // PATCH omits empty conclusions.
		}
		old.Output = check.Output
		f.checks[i] = old
		if f.afterCheckWrite != nil {
			f.afterCheckWrite(old)
		}
		return nil
	}
	return errors.New("unknown check")
}
func (f *fakeAgenticGitHub) IsMember(_, _ string) (bool, error) { return f.member, nil }
func (f *fakeAgenticGitHub) IsCollaborator(_, _, _ string) (bool, error) {
	return f.collaborator, nil
}

type agenticFixture struct {
	a    *agenticController
	gh   *fakeAgenticGitHub
	cfg  *config.Config
	now  time.Time
	work agenticWork
}

func newAgenticFixture(t *testing.T, mode string) *agenticFixture {
	t.Helper()
	f := &agenticFixture{now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), work: agenticWork{org: "org", repo: "repo", number: 42}}
	f.gh = &fakeAgenticGitHub{member: true, pr: github.PullRequest{Number: 42, State: github.PullRequestStateOpen,
		Head: github.PullRequestBranch{SHA: strings.Repeat("a", 40)},
		Base: github.PullRequestBranch{Ref: "main", Repo: github.Repo{Name: "repo", Owner: github.User{Login: "org"}}}}}
	static := []config.Presubmit{
		{JobBase: config.JobBase{Name: "pull-ci-org-repo-main-unit"}, AlwaysRun: true, Reporter: config.Reporter{Context: "ci/unit"}},
		{JobBase: config.JobBase{Name: "pull-ci-org-repo-main-e2e"}, Reporter: config.Reporter{Context: "ci/e2e"}, RerunCommand: "/test e2e"},
	}
	f.cfg = &config.Config{}
	require.NoError(t, f.cfg.SetPresubmits(map[string][]config.Presubmit{"org/repo": static}))
	var enrollment enabledConfig
	require.NoError(t, yaml.Unmarshal([]byte(fmt.Sprintf("orgs:\n- org: org\n  repos:\n  - name: repo\n    branches: [main]\n    mode: {trigger: %s, agentic: {mode: chai}}\n", mode)), &enrollment))
	w, lgtm := &watcher{config: enrollment}, &watcher{}
	if mode == "lgtm" {
		w, lgtm = lgtm, w
	}
	logger := logrus.NewEntry(logrus.New())
	f.a = &agenticController{gh: f.gh, reader: newFakePJLister(), watcher: w, lgtmWatcher: lgtm, logger: logger,
		configDataProvider: NewConfigDataProvider(func() *config.Config { return f.cfg }, func() []string { return []string{"org/repo"} }, logger),
		checks:             &dispatchChecks{gh: f.gh, appID: 101}, appID: 101, now: func() time.Time { return f.now },
		options: agenticOptions{timeout: defaultAgenticTimeout, trustedAuthors: flagutil.NewStrings("chai[bot]")}}
	return f
}

func (f *agenticFixture) passFirstStage() {
	pj := makeProwJob("pull-ci-org-repo-"+f.gh.pr.Base.Ref+"-unit", f.gh.pr.Head.SHA)
	pj.Spec.Refs = pullRefs(f.work.org, f.work.repo, &f.gh.pr)
	pj.Labels["prow.k8s.io/refs.org"], pj.Labels["prow.k8s.io/refs.repo"] = f.work.org, f.work.repo
	pj.Labels["prow.k8s.io/refs.base_ref"] = f.gh.pr.Base.Ref
	f.a.reader = newFakePJLister(pj)
}

func (f *agenticFixture) command(id int, body string) *github.IssueComment {
	return &github.IssueComment{ID: id, Body: "/pipeline " + body, User: github.User{Login: "maintainer"}, CreatedAt: f.now}
}

func (f *agenticFixture) reconcile(t *testing.T, comment *github.IssueComment) {
	t.Helper()
	f.a.mu.Lock()
	defer f.a.mu.Unlock()
	require.NoError(t, f.a.reconcilePull(context.Background(), f.work, comment))
}

func (f *agenticFixture) state() *agenticState { return f.a.states[f.work] }

func TestAgenticReadinessAndCompletion(t *testing.T) {
	for _, mode := range []string{"auto", "manual", "lgtm"} {
		t.Run(mode, func(t *testing.T) {
			f := newAgenticFixture(t, mode)
			f.reconcile(t, nil)
			require.Empty(t, f.gh.checks, "first-stage success must precede CheckRun creation")
			f.passFirstStage()
			f.reconcile(t, &github.IssueComment{ID: 90, Body: "Chai has not seen readiness yet.",
				User: github.User{Login: "chai[bot]"}, CreatedAt: f.now})
			require.Len(t, f.gh.checks, 1)
			require.Equal(t, pipelineGate, f.gh.checks[0].Name)
			require.Equal(t, "queued", f.gh.checks[0].Status)
			deadline := f.state().waitingUntil
			require.False(t, deadline.IsZero(), "a comment before readiness must not cancel the new timer")
			f.now = f.now.Add(time.Minute)
			f.reconcile(t, nil)
			require.Equal(t, deadline, f.state().waitingUntil)
			ack := &github.IssueComment{ID: 100, Body: "Chai is reviewing this PR.",
				User: github.User{Login: "chai[bot]"}, CreatedAt: f.now}
			f.reconcile(t, ack)
			require.True(t, f.state().waitingUntil.IsZero())
			require.Equal(t, "queued", f.gh.checks[0].Status)
			f.now = f.now.Add(time.Hour)
			f.reconcile(t, nil)
			require.Empty(t, f.gh.comments)
			require.Zero(t, f.gh.statusWrites, "agentic handoff must not publish job contexts")
			mark := f.command(200, "mark-pipeline-gate "+f.gh.pr.Head.SHA)
			mark.User = ack.User
			f.gh.member = false // Configured Chai need not be an organization member.
			f.reconcile(t, mark)
			require.Equal(t, "success", f.gh.checks[0].Conclusion)
			require.Len(t, f.gh.checks, 1)
			require.Empty(t, f.gh.comments)
		})
	}
}

func TestAgenticChaiCommentResponse(t *testing.T) {
	for _, scenario := range []string{"plain", "help", "human", "edited", "old", "late"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAgenticFixture(t, "manual")
			f.passFirstStage()
			f.reconcile(t, nil)
			deadline, gate, writes := f.state().waitingUntil, f.gh.checks[0], f.gh.checkAttempts
			f.now = f.now.Add(time.Minute)
			comment := github.IssueComment{ID: 100, Body: "Looking at this PR.", User: github.User{Login: "chai[bot]"}, CreatedAt: f.now}
			event := github.IssueCommentEvent{Action: github.IssueCommentActionCreated, Repo: f.gh.pr.Base.Repo,
				Issue: github.Issue{Number: f.work.number, PullRequest: &struct{}{}}, Comment: comment}
			switch scenario {
			case "help":
				event.Comment.Body = "/pipeline help"
			case "human":
				event.Comment.User.Login = "maintainer"
			case "edited":
				event.Action = github.IssueCommentActionEdited
			case "old":
				event.Comment.CreatedAt = f.state().publishedAt.Add(-time.Second)
			case "late":
				f.now = deadline.Add(time.Second)
				event.Comment.CreatedAt = f.now
			}
			f.a.queue = workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedItemExponentialFailureRateLimiter[agenticWork](time.Second, time.Minute))
			defer f.a.queue.ShutDown()
			cw := &clientWrapper{ghc: f.gh, agentic: f.a}
			cw.handleIssueComment(f.a.logger, event)
			if len(f.a.inputs) != 0 {
				require.True(t, f.a.processNext(context.Background()))
			}
			switch scenario {
			case "plain", "help":
				require.True(t, f.state().waitingUntil.IsZero())
			case "late":
				require.True(t, f.state().timedOut, "a late comment cannot cancel fallback even before timer delivery")
			default:
				require.Equal(t, deadline, f.state().waitingUntil)
			}
			require.Equal(t, gate, f.gh.checks[0], "a response must leave the check pending and need no GitHub write")
			require.Equal(t, writes, f.gh.checkAttempts)
			require.Zero(t, f.gh.statusWrites)
		})
	}
}

func TestAgenticResponseTimeoutUsesTraditionalTrigger(t *testing.T) {
	for _, mode := range []string{"auto", "manual", "lgtm", "lgtm-approved"} {
		t.Run(mode, func(t *testing.T) {
			trigger := strings.TrimSuffix(mode, "-approved")
			f := newAgenticFixture(t, trigger)
			if mode == "lgtm-approved" {
				f.gh.pr.Labels = []github.Label{{Name: labels.LGTM}}
			}
			f.passFirstStage()
			f.reconcile(t, nil)
			f.now = f.state().waitingUntil
			f.gh.beforeComment = func(body string) {
				if strings.Contains(body, "/test e2e") {
					require.NotEqual(t, "success", f.gh.checks[0].Conclusion)
					require.Equal(t, []github.Status{{Context: "ci/e2e", State: "pending", Description: PipelinePendingMessage}}, f.gh.statusUpdates)
				}
			}
			f.reconcile(t, nil)
			require.True(t, f.state().timedOut)
			if mode == "auto" || mode == "lgtm-approved" {
				require.Len(t, f.gh.comments, 1)
				require.Contains(t, f.gh.comments[0].Body, "/test e2e")
				require.Equal(t, "success", f.gh.checks[0].Conclusion)
				f.reconcile(t, nil)
				require.Len(t, f.gh.comments, 1, "fallback must dispatch once")
			} else {
				require.Empty(t, f.gh.comments)
				require.Equal(t, "queued", f.gh.checks[0].Status)
				if mode == "lgtm" {
					f.gh.pr.Labels = []github.Label{{Name: labels.LGTM}}
					f.reconcile(t, nil)
					require.Equal(t, "success", f.gh.checks[0].Conclusion)
				}
			}
			late := f.command(100, "mark-pipeline-gate "+f.gh.pr.Head.SHA)
			late.User = github.User{Login: "chai[bot]"}
			f.reconcile(t, late)
			require.True(t, f.state().timedOut, "late Chai messages cannot reclaim dispatch")
		})
	}
}

func TestAgenticModeSwitchAndIdempotence(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.passFirstStage()
	f.reconcile(t, nil)
	deadline := f.state().waitingUntil
	f.reconcile(t, f.command(100, "agentic-mode"))
	require.Len(t, f.gh.checks, 1)
	require.Equal(t, deadline, f.state().waitingUntil)
	f.reconcile(t, f.command(200, "skip-agentic-mode"))
	require.True(t, hasAgenticLabel(&f.gh.pr, agenticSkipLabel))
	require.True(t, f.state().optOut)
	require.True(t, f.state().waitingUntil.IsZero())
	f.reconcile(t, f.command(300, "mark-pipeline-gate"))
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
	f.reconcile(t, f.command(400, "agentic-mode"))
	require.False(t, hasAgenticLabel(&f.gh.pr, agenticSkipLabel))
	require.False(t, f.state().optOut)
	require.Len(t, f.gh.checks, 2, "reenabling must emit check_run.created")
	require.Equal(t, "failure", f.gh.checks[0].Conclusion, "old green run must not bypass the new pending gate")
	require.Equal(t, "queued", f.gh.checks[1].Status)
	deadline = f.state().waitingUntil
	f.now = f.now.Add(time.Minute)
	f.reconcile(t, f.command(500, "agentic-mode"))
	require.Len(t, f.gh.checks, 2)
	require.Equal(t, deadline, f.state().waitingUntil)
}

func TestAgenticSkipInvalidatesCompletionBeforeTraditionalDispatch(t *testing.T) {
	for _, scenario := range []string{"manual", "failed-plan", "lost-label-response"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAgenticFixture(t, "manual")
			f.passFirstStage()
			f.reconcile(t, nil)
			f.reconcile(t, f.command(100, "mark-pipeline-gate"))
			f.gh.failLabelAfterWrite = scenario == "lost-label-response"
			skip := f.command(200, "skip-agentic-mode")
			if f.gh.failLabelAfterWrite {
				require.Error(t, f.a.reconcilePull(context.Background(), f.work, skip))
				require.NotEqual(t, "success", f.gh.checks[0].Conclusion)
			}
			f.reconcile(t, skip)
			require.True(t, hasAgenticLabel(&f.gh.pr, agenticSkipLabel))
			require.Len(t, f.gh.checks, 1, "skip must PATCH, not create a Chai readiness signal")
			require.Equal(t, "queued", f.gh.checks[0].Status)
			require.NotEqual(t, "success", f.gh.checks[0].Conclusion)
			require.Zero(t, f.gh.statusWrites, "manual fallback waits for a traditional command")
			if scenario == "failed-plan" {
				f.a.reader = errorLister{}
				require.Error(t, f.a.reconcilePull(context.Background(), f.work, f.command(300, "remaining")))
				require.NotEqual(t, "success", f.gh.checks[0].Conclusion)
				return
			}
			f.reconcile(t, f.command(300, "required"))
			require.Equal(t, "success", f.gh.checks[0].Conclusion)
			require.Contains(t, f.gh.comments[len(f.gh.comments)-1].Body, "/test e2e")
		})
	}
}

func TestAgenticTraditionalCompletionSurvivesSkipAndRestart(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.passFirstStage()
	f.reconcile(t, nil)
	f.now = f.state().waitingUntil
	f.reconcile(t, nil)
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
	attempts, statuses := f.gh.checkAttempts, f.gh.statusWrites
	f.reconcile(t, f.command(100, "skip-agentic-mode"))
	f.a.states = nil
	f.reconcile(t, nil)
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
	require.Equal(t, attempts, f.gh.checkAttempts, "already-traditional completion must not be reset")
	require.Equal(t, statuses, f.gh.statusWrites, "restart must reuse native dispatch completion")
}

func TestAgenticFallbackHumanMarkDoesNotDispatch(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.reconcile(t, f.command(100, "skip-agentic-mode"))
	f.passFirstStage()
	f.reconcile(t, f.command(200, "mark-pipeline-gate"))
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
	require.Zero(t, f.gh.statusWrites, "marking completion must not start tests")
	f.a.states = nil
	f.reconcile(t, nil)
	require.Zero(t, f.gh.statusWrites, "native completion survives memory loss")
}

func TestAgenticCommandRevisionAndTrust(t *testing.T) {
	for _, scenario := range []string{"foreign-head", "untrusted", "early", "optional-head", "explicit-head"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAgenticFixture(t, "auto")
			if scenario != "early" {
				f.passFirstStage()
			}
			f.reconcile(t, nil)
			command := f.command(100, "mark-pipeline-gate")
			switch scenario {
			case "foreign-head":
				command.Body += " " + strings.Repeat("b", 40)
			case "untrusted":
				f.gh.member = false
			case "explicit-head":
				command.Body += " " + f.gh.pr.Head.SHA
			}
			f.reconcile(t, command)
			switch scenario {
			case "early":
				require.Empty(t, f.gh.checks)
			case "optional-head", "explicit-head":
				require.Equal(t, "success", f.gh.checks[0].Conclusion)
			default:
				require.NotEqual(t, "success", f.gh.checks[0].Conclusion)
				require.False(t, f.state().waitingUntil.IsZero())
			}
			require.Zero(t, f.gh.statusWrites)
		})
	}
}

func TestAgenticPushAndRestart(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.passFirstStage()
	f.reconcile(t, nil)
	f.reconcile(t, f.command(100, "mark-pipeline-gate"))
	f.a.states = nil // Restart: completion remains GitHub's native check.
	f.reconcile(t, nil)
	require.Len(t, f.gh.checks, 1)
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
	require.True(t, f.state().waitingUntil.IsZero())
	oldHead := f.gh.pr.Head.SHA
	f.gh.pr.Head.SHA = strings.Repeat("b", 40)
	f.reconcile(t, nil)
	require.Len(t, f.gh.checks, 1)
	f.reconcile(t, f.command(200, "mark-pipeline-gate "+oldHead))
	f.passFirstStage()
	f.reconcile(t, nil)
	require.Len(t, f.gh.checks, 2)
	require.Equal(t, "queued", f.gh.checks[1].Status)
	f.reconcile(t, f.command(300, "mark-pipeline-gate"))
	require.Equal(t, "success", f.gh.checks[1].Conclusion)
}

func TestAgenticFailedPublicationAndFallbackRetry(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.passFirstStage()
	f.gh.failCheck = true
	require.Error(t, f.a.reconcilePull(context.Background(), f.work, nil))
	require.Empty(t, f.gh.checks)
	require.True(t, f.state().waitingUntil.IsZero(), "timeout starts only after successful publication")
	f.gh.failCheck = false
	f.reconcile(t, nil)
	f.now = f.state().waitingUntil
	f.gh.failStatus = true
	require.Error(t, f.a.reconcilePull(context.Background(), f.work, nil))
	require.Empty(t, f.gh.comments)
	require.NotEqual(t, "success", f.gh.checks[0].Conclusion)
	f.gh.failStatus = false
	f.reconcile(t, nil)
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
}

func TestAgenticTraditionalCommandsAreNotQueued(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.passFirstStage()
	f.reconcile(t, f.command(100, "required"))
	require.Len(t, f.gh.comments, 1)
	require.Contains(t, f.gh.comments[0].Body, "not queued")
	require.NotContains(t, f.gh.comments[0].Body, "/test e2e")
	f.reconcile(t, nil)
	require.Len(t, f.gh.comments, 1)
	require.Zero(t, f.gh.statusWrites)
}

func TestAgenticOptOutPersistsAcrossPush(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.reconcile(t, f.command(100, "skip-agentic-mode"))
	f.a.states = nil
	f.gh.pr.Head.SHA = strings.Repeat("b", 40)
	f.passFirstStage()
	f.reconcile(t, nil)
	require.Len(t, f.gh.comments, 2)
	require.Contains(t, f.gh.comments[1].Body, "/test e2e")
	require.Equal(t, "success", f.gh.checks[len(f.gh.checks)-1].Conclusion)
}

func TestAgenticReconcilerIgnoresSecondStageUpdates(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	pj := makeProwJob("pull-ci-org-repo-main-e2e", f.gh.pr.Head.SHA)
	pj.Spec.Refs = pullRefs(f.work.org, f.work.repo, &f.gh.pr)
	reader := newFakePJLister(pj)
	r := &reconciler{pjclientset: reader.(ctrlruntimeclient.Client), configDataProvider: f.a.configDataProvider, agentic: f.a}
	require.NoError(t, r.reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pj.Namespace, Name: pj.Name}}))
	require.Empty(t, f.a.inputs)
	require.Zero(t, f.gh.getPullRequestCalls)
}

func TestAgenticRetargetInvalidatesCompletionBeforeReadiness(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.a.watcher.config.Orgs[0].Repos[0].Branches = []string{"main", "release"}
	presubmits := f.a.configDataProvider.updatedPresubmits["org/repo"]
	presubmits.alwaysRequired = append(presubmits.alwaysRequired, config.Presubmit{JobBase: config.JobBase{Name: "pull-ci-org-repo-release-unit"}, AlwaysRun: true})
	f.a.configDataProvider.updatedPresubmits["org/repo"] = presubmits
	f.passFirstStage()
	f.reconcile(t, nil)
	f.reconcile(t, f.command(100, "mark-pipeline-gate"))
	f.gh.pr.Base.Ref = "release"
	f.now = f.now.Add(time.Minute)
	f.a.states = nil // Retarget validation must not depend on surviving process memory.
	f.reconcile(t, nil)
	require.Len(t, f.gh.checks, 1, "the next check must wait for target-branch first-stage success")
	require.Equal(t, "failure", f.gh.checks[0].Conclusion)
	f.passFirstStage()
	f.reconcile(t, nil)
	require.Len(t, f.gh.checks, 2)
	require.Equal(t, "queued", f.gh.checks[1].Status)
}

func TestAgenticFallbackManualCommandAndReenable(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.passFirstStage()
	f.reconcile(t, nil)
	f.now = f.state().waitingUntil
	f.reconcile(t, nil)
	f.reconcile(t, f.command(100, "remaining"))
	require.Len(t, f.gh.comments, 1)
	require.Contains(t, f.gh.comments[0].Body, "/test e2e")
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
	f.reconcile(t, f.command(200, "remaining"))
	require.Len(t, f.gh.statusUpdates, 1, "duplicate deltas must not reset contexts or repost /test")
	f.reconcile(t, f.command(300, "agentic-mode"))
	require.False(t, f.state().timedOut)
	require.Len(t, f.gh.checks, 2)
	require.Equal(t, "failure", f.gh.checks[0].Conclusion)
	require.Equal(t, "queued", f.gh.checks[1].Status)
}

func TestAgenticFreshReadinessRetryDoesNotDuplicateCreation(t *testing.T) {
	for _, reply := range []bool{false, true} {
		t.Run(fmt.Sprintf("reply=%t", reply), func(t *testing.T) {
			f := newAgenticFixture(t, "manual")
			f.passFirstStage()
			f.reconcile(t, nil)
			f.reconcile(t, f.command(100, "skip-agentic-mode"))
			f.gh.failCheckAfterCreate = true
			require.Error(t, f.a.reconcilePull(context.Background(), f.work, f.command(200, "agentic-mode")))
			require.Len(t, f.gh.checks, 2, "GitHub accepted the fresh readiness despite a lost response")
			var comment *github.IssueComment
			if reply {
				comment = &github.IssueComment{ID: 300, Body: "Chai is reviewing this PR.", User: github.User{Login: "chai[bot]"}, CreatedAt: f.now}
			}
			f.reconcile(t, comment)
			require.Len(t, f.gh.checks, 2, "retry must reuse the queued check, not start Chai twice")
			require.Equal(t, reply, f.state().waitingUntil.IsZero(), "a reply during the lost response must cancel the timer")
		})
	}
}

func TestAgenticPendingReadinessSurvivesRestartWithoutTimerRecovery(t *testing.T) {
	f := newAgenticFixture(t, "manual")
	f.passFirstStage()
	f.reconcile(t, nil)
	f.a.states, f.a.reader = nil, errorLister{}
	f.now = f.now.Add(time.Hour)
	f.reconcile(t, nil)
	require.True(t, f.state().waitingUntil.IsZero())
	require.Len(t, f.gh.checks, 1)
	require.Equal(t, "queued", f.gh.checks[0].Status)
	f.reconcile(t, f.command(100, "mark-pipeline-gate"))
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
}

func TestAgenticReenableRetiresLiveTraditionalGateBeforeFirstStage(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.reconcile(t, f.command(100, "skip-agentic-mode"))
	f.passFirstStage()
	f.reconcile(t, nil)
	require.Equal(t, "success", f.gh.checks[0].Conclusion)
	f.a.reader = newFakePJLister() // First-stage jobs were cleaned before re-enable.
	f.reconcile(t, f.command(200, "agentic-mode"))
	require.Equal(t, "failure", f.gh.checks[0].Conclusion)
	require.Len(t, f.gh.checks, 1, "fresh readiness must wait for first-stage success")
	require.False(t, f.state().ready)
	require.True(t, f.state().waitingUntil.IsZero())
}

func TestAgenticRetiredGateCannotAuthorizeReadinessAfterRestart(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.passFirstStage()
	f.reconcile(t, nil)
	f.reconcile(t, f.command(100, "mark-pipeline-gate"))
	f.reconcile(t, f.command(200, "skip-agentic-mode"))
	f.a.reader = newFakePJLister()
	f.reconcile(t, f.command(300, "agentic-mode"))
	require.Equal(t, "failure", f.gh.checks[0].Conclusion)
	f.a.states = nil
	f.reconcile(t, f.command(400, "mark-pipeline-gate"))
	ack := &github.IssueComment{ID: 500, Body: "Chai is reviewing this PR.",
		User: github.User{Login: "chai[bot]"}, CreatedAt: f.now}
	f.reconcile(t, ack)
	require.Len(t, f.gh.checks, 1)
	require.Equal(t, "failure", f.gh.checks[0].Conclusion)
	require.False(t, f.state().ready)
}
