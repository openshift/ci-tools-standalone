package main

import (
	"fmt"
	"strings"
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
	if !chai || command.name != "mark-pipeline-gate" {
		trusted, err := trustedPipelineCommandAuthor(a.gh, work.org, work.repo, comment.User.Login)
		if err != nil || !trusted {
			return err
		}
	}
	reply := func(message string) error { return a.gh.CreateComment(work.org, work.repo, work.number, message) }
	var err error
	switch command.name {
	case "required", "remaining":
		if !s.optOut && !s.timedOut {
			err = reply("The controller does not dispatch second-stage tests in agentic mode. Use `/pipeline skip-agentic-mode` first, then repeat this command. The request was not queued.")
			break
		}
		if command.name == "remaining" && s.dispatched {
			err = reply("The second stage has already been scheduled for this HEAD; nothing further to trigger.")
			break
		}
		mode := modeForce
		if command.name == "remaining" {
			mode = modeDelta
		}
		pj := &v1.ProwJob{Spec: v1.ProwJobSpec{Refs: pullRefs(work.org, work.repo, pr)}}
		err = sendCommentWithMode(a.configDataProvider.GetPresubmits(work.org+"/"+work.repo), pj, a.gh, func() {}, a.reader, mode, true, a.checks)
		if err == nil {
			s.dispatched = true
		}
	case "auto":
		if cfg.Trigger != "lgtm" {
			err = reply("`/pipeline auto` is available only in LGTM mode.")
		} else if !hasAgenticLabel(pr, PipelineAutoLabel) {
			err = a.gh.AddLabel(work.org, work.repo, work.number, PipelineAutoLabel)
			if err == nil {
				pr.Labels = append(pr.Labels, github.Label{Name: PipelineAutoLabel})
			}
		}
	case "skip-agentic-mode":
		if !s.optOut && !s.timedOut {
			// Invalidate agentic completion before persisting the ownership change,
			// including when GitHub applies the label but loses its response.
			if err = a.checks.pendingWithReset(pullRefs(work.org, work.repo, pr), true); err != nil {
				break
			}
			s.dispatched = false
		}
		if !s.optOut {
			err = a.gh.AddLabel(work.org, work.repo, work.number, agenticSkipLabel)
		}
		if err == nil {
			s.optOut, s.waitingUntil = true, time.Time{}
			err = reply("Traditional pipeline scheduling is enabled for this PR and future pushes. Running tests are unchanged.")
		}
	case "agentic-mode":
		if !s.optOut && !s.timedOut {
			err = reply("Agentic mode is already enabled. The gate and response timeout are unchanged.")
			break
		}
		if err == nil {
			var gate *github.CheckRun
			gate, err = findPipelineCheck(a.gh, a.appID, work.org, work.repo, pr.Head.SHA)
			if err == nil && gate.ID != 0 {
				if !strings.HasPrefix(gate.ExternalID, pipelinePRIdentity(work.org, work.repo, pr.Number)+":") {
					err = fmt.Errorf("no active owned pipeline gate for this PR/revision")
				} else {
					err = a.retireCheck(work, *gate)
				}
			}
		}
		if err == nil && s.optOut {
			err = a.gh.RemoveLabel(work.org, work.repo, work.number, agenticSkipLabel)
		}
		if err == nil {
			s.optOut, s.timedOut, s.dispatched = false, false, false
			s.ready, s.fresh, s.waitingUntil = false, true, time.Time{}
			err = reply("Agentic mode is restored. First-stage success will create a fresh pending pipeline gate. Running tests are unchanged.")
		}
	case "mark-pipeline-gate":
		if chai && (s.optOut || s.timedOut) {
			err = reply("Agentic command rejected: traditional scheduling owns this revision. Do not start agentic dispatch unless agentic mode is restored.")
			break
		}
		if command.head != "" && command.head != pr.Head.SHA {
			err = reply(fmt.Sprintf("Command rejected: HEAD `%s` is not the current PR HEAD `%s`.", command.head, pr.Head.SHA))
			break
		}
		if s.optOut || s.timedOut {
			err = a.checks.markGate(pullRefs(work.org, work.repo, pr), comment.ID)
			if err == nil {
				s.dispatched = true
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
		if !s.ready || (chai && !s.publishedAt.IsZero() && !comment.CreatedAt.IsZero() && comment.CreatedAt.Before(s.publishedAt)) {
			err = reply("There is no active agentic readiness gate for this revision. Wait for first-stage success and repeat the command.")
			break
		}
		if chai && !s.waitingUntil.IsZero() && comment.CreatedAt.After(s.waitingUntil) {
			err = reply("Agentic completion rejected: the response timeout expired. Traditional scheduling owns this revision.")
			break
		}
		s.waitingUntil = time.Time{} // This is a response timeout, not a dispatch deadline.
		err = a.checks.markGate(pullRefs(work.org, work.repo, pr), comment.ID)
		if err == nil {
			s.gate.Status, s.gate.Conclusion = "completed", "success"
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

// Protect mixed-mode branches from delayed webhook and ProwJob snapshots.
func (a *agenticController) allowSnapshot(org, repo string, number int, head, base string, allowAgentic bool) (bool, error) {
	if !a.hasRepo(org, repo) {
		return true, nil
	}
	if _, enabled := a.repoConfig(org, repo, base); enabled && !allowAgentic {
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
	return (allowAgentic || !enabled) && pr.State == github.PullRequestStateOpen && !pr.Draft && pr.Head.SHA == head && pr.Base.Ref == base, nil
}

func (cw *clientWrapper) allowPullRequestSnapshot(logger *logrus.Entry, event github.PullRequestEvent, allowAgentic bool) bool {
	allowed, err := cw.agentic.allowSnapshot(event.Repo.Owner.Login, event.Repo.Name, event.PullRequest.Number, event.PullRequest.Head.SHA, event.PullRequest.Base.Ref, allowAgentic)
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
