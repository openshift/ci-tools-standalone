package main

import (
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

func (a *agenticController) applyCommand(work agenticWork, s *agenticState, pr *github.PullRequest, cfg RepoConfig, comment github.IssueComment) error {
	command, ok := parsePipelineCommand(comment.Body)
	if !ok || comment.ID <= 0 || comment.ID <= s.lastCommand {
		return nil
	}
	chai := a.trustedChai(comment.User.Login)
	if command.name != "mark-pipeline-gate" {
		trusted, err := trustedPipelineCommandAuthor(a.gh, work.org, work.repo, comment.User.Login)
		if err != nil || !trusted {
			return err
		}
	}
	reply := func(message string) {
		// Confirmation is not part of the operation: a lost response must not
		// replay an applied mode change over a newer manual label choice.
		s.lastCommand = comment.ID
		if err := a.gh.CreateComment(work.org, work.repo, work.number, message); err != nil {
			a.logger.WithError(err).WithField("pr", work.number).Warn("Pipeline command handled; confirmation could not be delivered")
		}
	}
	var err error
	switch command.name {
	case "required", "remaining":
		if !s.traditionalMode() {
			created := false
			if !s.ready {
				s.progress.trigger = progressManual
				created, err = a.publishReadiness(work, s, pr, true)
			}
			if err == nil && !created {
				reply(fmt.Sprintf("Agentic mode triggers Chai only once per HEAD SHA. `ci/pipeline-gate` already exists for `%s`. Repeating `/pipeline required` or `/pipeline remaining` will not trigger Chai again. This command does not reopen the gate or restart its response timeout.", pr.Head.SHA))
			}
			break
		}
		if command.name == "remaining" && s.dispatched {
			reply("The second stage has already been scheduled for this HEAD; nothing further to trigger.")
			break
		}
		mode := modeForce
		if command.name == "remaining" {
			mode = modeDelta
		}
		pj := &v1.ProwJob{Spec: v1.ProwJobSpec{Refs: pullRefs(work.org, work.repo, pr)}}
		s.progress.trigger = progressManual
		err = sendCommentWithMode(a.configDataProvider.GetPresubmits(work.org+"/"+work.repo), pj, a.gh, func() {}, a.reader, mode, true, a.checks, &s.progress.outcome)
		if err == nil {
			s.dispatched = true
		}
	case "auto":
		if cfg.Trigger != "lgtm" {
			reply("`/pipeline auto` is available only in LGTM mode.")
		} else if !hasAgenticLabel(pr, PipelineAutoLabel) {
			err = a.gh.AddLabel(work.org, work.repo, work.number, PipelineAutoLabel)
			if err == nil {
				pr.Labels = append(pr.Labels, github.Label{Name: PipelineAutoLabel})
			}
		}
	case "skip-agentic-mode":
		if !s.traditionalMode() {
			// Invalidate agentic completion before persisting the ownership change,
			// including when GitHub applies the label but loses its response.
			if err = a.checks.pendingWithReset(pullRefs(work.org, work.repo, pr), true); err != nil {
				break
			}
			s.dispatched = false
			s.progress = agenticProgress{}
		}
		if !s.optOut {
			err = a.gh.AddLabel(work.org, work.repo, work.number, agenticSkipLabel)
		}
		if err == nil {
			s.optOut, s.waitingUntil = true, time.Time{}
			reply("Traditional pipeline scheduling is enabled for this PR and future pushes. Running tests are unchanged.")
		}
	case "agentic-mode":
		if !s.traditionalMode() {
			reply("Agentic mode is already enabled. The gate and response timeout are unchanged.")
			break
		}
		var deferred bool
		deferred, err = a.restoreAgenticMode(work, s, pr, true)
		if err == nil {
			if deferred && s.dispatched && s.traditional {
				reply("Agentic mode is enabled for the next HEAD SHA. Second-stage dispatch is already complete for this HEAD; its pipeline gate and running tests are unchanged.")
			} else if deferred && s.dispatched {
				reply("Agentic mode is restored. This HEAD already has a completed agentic gate; its ownership and running tests are unchanged. The next HEAD SHA will start fresh agentic readiness.")
			} else if deferred {
				reply("Agentic mode is enabled for the next HEAD SHA. Traditional scheduling still owns this HEAD; its rerun gate is unchanged. Use `/pipeline required` or `/pipeline remaining` to recover the traditional rerun.")
			} else if cfg.Trigger == "manual" {
				reply("Agentic mode is restored. Use `/pipeline required` or `/pipeline remaining` to create a pending pipeline gate and trigger Chai for this HEAD. Running tests are unchanged.")
			} else if pipelineWaitsForFirstStage(cfg.Trigger, pr) {
				reply("Agentic mode is restored. First-stage success will create a fresh pending pipeline gate. Running tests are unchanged.")
			} else {
				reply("Agentic mode is restored. The `lgtm` label triggers a fresh pending pipeline gate without waiting for stage 1. Running tests are unchanged.")
			}
		}
	case "mark-pipeline-gate":
		var allowed bool
		allowed, err = a.checks.authorizeMarkGate(work.org, work.repo, comment.User.Login, pr.Base.Ref)
		if err != nil || !allowed {
			return err
		}
		if chai && s.traditionalMode() {
			reply("Agentic command rejected: traditional scheduling owns this revision. Do not start agentic dispatch unless agentic mode is restored.")
			break
		}
		if command.head != "" && command.head != pr.Head.SHA {
			reply(fmt.Sprintf("Command rejected: HEAD `%s` is not the current PR HEAD `%s`.", command.head, pr.Head.SHA))
			break
		}
		if s.traditionalMode() {
			err = a.checks.markGateWithTitle(pullRefs(work.org, work.repo, pr), comment.ID, traditionalCheckTitle)
			if err == nil {
				s.dispatched = true
				s.progress.outcome = progressMarked
			}
			break
		}
		if !s.ready && !s.fresh {
			var gate *github.CheckRun
			gate, err = findPipelineCheck(a.gh, a.appID, work.org, work.repo, pr.Head.SHA)
			if err == nil && gate.ExternalID == pipelineExternalID(pullRefs(work.org, work.repo, pr)) && nativeAgenticReadiness(gate) {
				s.gate, s.ready = *gate, true
			}
		}
		if err != nil {
			break
		}
		if chai && (!s.ready || (!s.publishedAt.IsZero() && !comment.CreatedAt.IsZero() && comment.CreatedAt.Before(s.publishedAt))) {
			message := "There is no active agentic readiness gate for this revision. Wait for the controller to request Chai handoff before repeating the command."
			if cfg.Trigger == "manual" {
				message = "There is no active agentic readiness gate for this revision. A trusted user must first run `/pipeline required` or `/pipeline remaining` to trigger Chai."
			}
			reply(message)
			break
		}
		if chai && !s.waitingUntil.IsZero() && comment.CreatedAt.After(s.waitingUntil) {
			reply("Agentic completion rejected: the response timeout expired. Traditional scheduling owns this revision.")
			break
		}
		// Preserve an existing owner title; a human backfill belongs to the
		// agentic workflow and must survive reconciliation and restart.
		title := s.gate.Output.Title
		if title == "" {
			title = agenticCheckTitle
		}
		var gate *github.CheckRun
		gate, err = a.checks.markGateCheck(pullRefs(work.org, work.repo, pr), comment.ID, title, !chai)
		if err == nil {
			s.gate, s.ready, s.dispatched, s.fresh = *gate, true, true, false
			s.waitingUntil = time.Time{} // This is a response timeout, not a dispatch deadline.
			s.progress.outcome = progressMarked
			if chai {
				s.progress.outcome = progressChaiMarked
			}
		}
	}
	if err == nil {
		s.lastCommand = comment.ID
	}
	return err
}

func (a *agenticController) handleIssueComment(logger *logrus.Entry, event github.IssueCommentEvent) bool {
	if a == nil || !event.Issue.IsPullRequest() || !a.hasRepo(event.Repo.Owner.Login, event.Repo.Name) {
		return false
	}
	command, ok := parsePipelineCommand(event.Comment.Body)
	if !ok {
		if !a.trustedChai(event.Comment.User.Login) {
			return false
		}
		if event.Action == github.IssueCommentActionCreated {
			a.enqueue(event.Repo.Owner.Login, event.Repo.Name, event.Issue.Number, &event.Comment)
		}
		return true
	}
	if event.Action != github.IssueCommentActionCreated {
		return true
	}
	org, repo := event.Repo.Owner.Login, event.Repo.Name
	pr, err := a.gh.GetPullRequest(org, repo, event.Issue.Number)
	if err != nil {
		logger.WithError(err).Error("Cannot safely route pipeline command")
		a.enqueue(org, repo, event.Issue.Number, &event.Comment)
		return true
	}
	if _, enabled := a.repoConfig(org, repo, pr.Base.Ref); !enabled {
		if command.name == "agentic-mode" || command.name == "skip-agentic-mode" {
			if err := a.gh.CreateComment(org, repo, pr.Number, "Agentic mode is not configured for this repository/branch."); err != nil {
				logger.WithError(err).Error("Cannot explain unavailable mode")
			}
			return true
		}
		return false
	}
	a.enqueue(org, repo, event.Issue.Number, &event.Comment)
	return true
}

func (a *agenticController) handlePullRequest(_ *logrus.Entry, event github.PullRequestEvent) {
	if a.hasRepo(event.Repo.Owner.Login, event.Repo.Name) {
		a.enqueue(event.Repo.Owner.Login, event.Repo.Name, event.PullRequest.Number, nil)
	}
}

// allowSnapshot protects mixed-mode branches from delayed snapshots. The opening
// notification can go to drafts and Chai branches because it does not schedule tests.
func (a *agenticController) allowSnapshot(org, repo string, number int, head, base string, forWelcome bool) (bool, error) {
	if !a.hasRepo(org, repo) {
		return true, nil
	}
	if _, enabled := a.repoConfig(org, repo, base); enabled && !forWelcome {
		return false, nil // Agentic and fallback events are handled by the worker.
	}
	if a.dryRun {
		return false, nil
	}
	pr, err := a.gh.GetPullRequest(org, repo, number)
	if err != nil {
		return false, err
	}
	_, enabled := a.repoConfig(org, repo, pr.Base.Ref)
	return pr.State == github.PullRequestStateOpen && pr.Head.SHA == head && pr.Base.Ref == base &&
		(forWelcome || (!enabled && !pr.Draft)), nil
}

// allowPullRequestSnapshot rejects stale webhook snapshots before posting or scheduling.
func (cw *clientWrapper) allowPullRequestSnapshot(logger *logrus.Entry, event github.PullRequestEvent, forWelcome bool) bool {
	allowed, err := cw.agentic.allowSnapshot(event.Repo.Owner.Login, event.Repo.Name, event.PullRequest.Number, event.PullRequest.Head.SHA, event.PullRequest.Base.Ref, forWelcome)
	if err != nil {
		logger.WithError(err).Error("Cannot safely route pull request event")
	}
	return allowed && err == nil
}

func (a *agenticController) configurationChanged() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for work := range a.states {
		a.enqueue(work.org, work.repo, work.number, nil)
	}
}
