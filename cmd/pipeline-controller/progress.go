package main

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/interrupts"
	"sigs.k8s.io/prow/pkg/labels"
)

// progressPhase describes what is known about dispatch, not the status of tests.
type progressPhase string

const (
	// progressWaiting is the zero value until there is a dispatch observation.
	progressWaiting progressPhase = ""
	// progressChai means the check was published and completion is still pending.
	progressChai progressPhase = "Agentic dispatching"
	// progressPosted records a traditional test request that was posted.
	progressPosted progressPhase = "Tests dispatched"
	// progressChaiMarked retains who acknowledged a gate if the route changes later.
	progressChaiMarked progressPhase = "Chai marked dispatch complete"
	// progressEmpty means the traditional planner requested no new jobs.
	progressEmpty progressPhase = "Nothing new to dispatch"
	// progressMarked records a mark without inferring who sent it.
	progressMarked progressPhase = "Gate acknowledged"
	// progressComplete is a recovered result without a fresh dispatch observation.
	progressComplete progressPhase = "Dispatch complete"
	// progressFailed covers errors where some requests may already have gone through.
	progressFailed progressPhase = "Couldn't confirm dispatch"
)

// progressTrigger is a milestone, never the current stage.
type progressTrigger string

const (
	// progressStageOne is the normal automatic trigger.
	progressStageOne progressTrigger = "Stage 1 ready"
	// progressLGTM represents approval, not first-stage readiness.
	progressLGTM progressTrigger = "LGTM"
	// progressAuto records opting out of waiting for approval.
	progressAuto progressTrigger = "Auto enabled"
	// progressManual represents an explicit dispatch or handoff request.
	progressManual progressTrigger = "Manual request"
)

// progressView contains only observations the scheduler already made.
type progressView struct {
	trigger   progressTrigger
	satisfied bool
	phase     progressPhase
	chai      bool
	recovery  string
	replied   bool
	deadline  time.Time
}

// waitingProgress describes the current trigger without running the readiness check again.
func waitingProgress(cfg RepoConfig, pr *github.PullRequest, chai bool) progressView {
	v := progressView{trigger: progressStageOne, chai: chai}
	switch cfg.Trigger {
	case "manual":
		v.trigger = progressManual
	case "lgtm":
		v.trigger, v.satisfied = progressLGTM, hasAgenticLabel(pr, labels.LGTM)
		if hasAgenticLabel(pr, PipelineAutoLabel) {
			v.trigger, v.satisfied = progressAuto, true
		}
	}
	return v
}

// renderProgress formats the first bot comment without executable slash-command lines.
func renderProgress(refs *v1.Refs, v progressView) string {
	now, next, hint := v.copy()
	if v.recovery != "" {
		now = v.recovery + " " + now
	}
	if v.recovery != "" && !v.chai {
		hint += " A trusted user can re-enable Chai with `/pipeline agentic-mode`. If Traditional owns this commit and has already dispatched, Chai starts on the next push."
	}
	owner, middle := "Traditional", ""
	if v.chai {
		owner, middle = "Chai", ` --> handoff["Agentic dispatching"]`
	}
	current, end := "wait", string(progressPosted)
	var done []string
	switch v.phase {
	case progressWaiting:
		if v.satisfied {
			done = append(done, "trigger")
		}
	case progressChai:
		current = "handoff"
		done = append(done, "wait", "trigger")
	default:
		current = "outcome"
		if v.phase != progressChaiMarked {
			end = string(v.phase)
		}
		done = append(done, "wait", "trigger")
		if v.chai {
			done = append(done, "handoff")
		}
		switch v.phase {
		case progressPosted, progressChaiMarked, progressEmpty, progressMarked, progressComplete:
			current = ""
			done = append(done, "outcome")
		}
	}
	styles := ""
	if current != "" {
		styles = "  class " + current + " here\n"
	}
	if len(done) != 0 {
		styles += "  class " + strings.Join(done, ",") + " done\n"
	}
	sha := refs.Pulls[0].SHA
	sha = html.EscapeString(strings.NewReplacer("`", "&#96;", "\n", "", "\r", "").Replace(sha[:min(12, len(sha))]))
	return fmt.Sprintf("%s\n**Pipeline · %s** · `%s`\n\n**Now:** %s\n\n**Next:** %s\n\n"+
		"```mermaid\nflowchart LR\n  wait[\"Waiting\"] --> trigger[%q]%s --> outcome[%q]\n"+
		"  classDef here fill:#1757a6,stroke:#1757a6,color:#fff,stroke-width:3px\n"+
		"  classDef done fill:#dafbe1,stroke:#1a7f37,color:#116329,stroke-width:2px\n"+
		"%s```\n\n**If you're stuck:** %s\n\n<details>\n<summary>About the pipeline controller</summary>\n\n%s\n\n</details>",
		progressPrefix, owner, sha, now, next, v.trigger, middle, end, styles, hint, pipelineNotificationHelp)
}

// copy keeps the usual next action separate from the always-visible recovery path.
func (v progressView) copy() (now, next, hint string) {
	results := "See which jobs started and how they finished in the **PR status table** below."
	pending := "See the **PR status table** below for pending or failed jobs."
	if v.phase == progressFailed {
		return "The controller couldn't confirm dispatch.", "Look for existing runs in the **PR status table** below before retrying.", "Use `/test <job>` to request one missing job."
	}
	if v.phase == progressEmpty {
		return "No new jobs needed to be requested.", "See the **PR status table** below for existing runs.", "Use `/test <job>` if you need to run a specific job."
	}
	if v.phase == progressMarked || v.phase == progressComplete {
		return "The dispatch gate is complete. This does not confirm that jobs started.", results, "Use `/test <job>` to request one missing job."
	}
	if v.phase == progressChaiMarked {
		hint = "Use `/test <job>` for one missing run."
		if v.chai {
			hint += " To switch to Traditional, a trusted user can run `/pipeline skip-agentic-mode`."
		}
		return "Chai marked dispatch complete. The gate doesn't confirm that jobs started.", results, hint
	}
	if v.chai {
		if v.phase == progressChai {
			hint = "A trusted user can switch to Traditional with `/pipeline skip-agentic-mode`."
			if !v.deadline.IsZero() {
				return "Waiting for Chai to reply.", "If Chai doesn't reply before the timeout, Traditional takes over.", hint
			}
			if v.replied {
				return "Chai replied, but the gate is still pending. There is no automatic fallback after a reply.", "Check Chai's latest comment and look for runs in the **PR status table** below.", hint
			}
			return "The Chai gate is pending. No response deadline was recovered.", "Check Chai's latest comment and the **PR status table** below.", hint
		}
		hint = "A trusted user can notify Chai now with `/pipeline required`."
		if v.trigger == progressManual {
			return "Waiting for a request to notify Chai.", "A trusted user can use `/pipeline remaining` or `/pipeline required`.", "To use Traditional instead, a trusted user can run `/pipeline skip-agentic-mode`."
		}
		if v.trigger == progressLGTM {
			if !v.satisfied {
				return "Waiting for `/lgtm`.", "Ask a reviewer for `/lgtm`.", "A trusted user can use `/pipeline auto` to wait only for stage 1, or `/pipeline required` to notify Chai now."
			}
			return "Waiting to notify Chai.", "The PR has `/lgtm`; stage 1 is not required for handoff.", hint
		}
		return "Waiting for stage 1 before notifying Chai.", pending, hint
	}
	switch {
	case v.phase == progressPosted:
		return "The controller posted stage-2 test requests.", results, "Use `/test <job>` for one failed or missing run."
	case v.trigger == progressManual:
		return "Waiting for a request to run stage 2.", "Use `/pipeline remaining` to request missing jobs.", "Use `/pipeline required` for the full applicable set, or `/test <job>` for one job."
	case v.trigger == progressLGTM:
		if !v.satisfied {
			return "Waiting for `/lgtm`.", "Ask a reviewer for `/lgtm`.", "Use `/pipeline auto` to wait for stage 1 instead, or `/pipeline remaining` for missing jobs now."
		}
		return "Waiting to request stage-2 jobs.", "The PR has `/lgtm`; stage 1 is not required for dispatch.", "Use `/pipeline remaining` to request missing stage-2 jobs now."
	default:
		return "Waiting for stage 1.", pending, "Use `/pipeline remaining` to request missing stage-2 jobs now."
	}
}

// submitProgress keeps the feature off unless the matching configuration enables it.
func (p *progressPublisher) submitProgress(cfg RepoConfig, refs *v1.Refs, v progressView, follow, baseline bool) {
	if p == nil || !cfg.ProgressComment || refs == nil || len(refs.Pulls) != 1 || refs.Pulls[0].SHA == "" {
		return
	}
	p.submit(refs, renderProgress(refs, v), follow, baseline)
}

// recordedProgress conservatively interprets a persisted check without fetching job runs or comments.
func recordedProgress(gate github.CheckRun) progressPhase {
	if gate.Status != "completed" {
		return progressWaiting
	}
	if gate.Conclusion != "success" {
		return progressFailed
	}
	if strings.HasPrefix(gate.Output.Summary, "Dispatch completion acknowledged with ") {
		return progressMarked
	}
	return progressComplete
}

// agenticProgress stores display observations that cannot be inferred from a check run alone.
type agenticProgress struct {
	trigger progressTrigger
	outcome progressPhase
	replied bool
}

// publishProgress projects agentic state after reconciliation without changing any scheduling decisions.
func (a *agenticController) publishProgress(work agenticWork, pr *github.PullRequest, cfg RepoConfig, s *agenticState) {
	traditional := s.traditionalMode()
	v := waitingProgress(cfg, pr, !traditional)
	if s.timedOut {
		v.recovery = "Chai did not reply before fallback; Traditional took over."
	} else if s.optOut {
		v.recovery = "Traditional scheduling is enabled for this PR."
	} else if s.traditional {
		v.recovery = "Traditional owns this commit. Chai is enabled for the next push."
	}
	v.phase = s.progress.outcome
	if v.phase == progressWaiting && s.dispatched {
		v.phase = recordedProgress(s.gate)
		if v.phase == progressWaiting {
			v.phase = progressComplete
		}
	}
	if v.phase == progressWaiting && !traditional && s.ready {
		v.phase, v.deadline, v.replied = progressChai, s.waitingUntil, s.progress.replied
	}
	if s.progress.trigger != "" && v.phase != progressWaiting {
		v.trigger, v.satisfied = s.progress.trigger, true
	}
	a.progress.submitProgress(cfg, pullRefs(work.org, work.repo, pr), v, true, false)
}

// progressEnabled rechecks local policy so a queued update cannot undo a configuration pause.
func (cw *clientWrapper) progressEnabled(work agenticWork, revision progressRevision) bool {
	return pipelineBranchConfig(cw.watcher, cw.lgtmWatcher, work.org, work.repo, revision.base).ProgressComment
}

// handleProgressRevision resets the traditional display on pushes or retargets.
// The agentic worker independently projects its live revision.
func (cw *clientWrapper) handleProgressRevision(logger *logrus.Entry, event github.PullRequestEvent) {
	if cw.progress == nil {
		return
	}
	pr := &event.PullRequest
	org, repo := event.Repo.Owner.Login, event.Repo.Name
	switch event.Action {
	case github.PullRequestActionSynchronize, github.PullRequestActionReopened, github.PullRequestActionReadyForReview,
		github.PullRequestActionClosed, github.PullRequestActionConvertedToDraft:
	case github.PullRequestActionEdited:
		var change struct{ Base json.RawMessage }
		if err := json.Unmarshal(event.Changes, &change); err != nil || len(change.Base) == 0 {
			return
		}
	default:
		return
	}
	cfg := pipelineBranchConfig(cw.watcher, cw.lgtmWatcher, org, repo, pr.Base.Ref)
	active := pr.State == github.PullRequestStateOpen && !pr.Draft
	inactive := !cfg.ProgressComment || !active
	if !inactive && cfg.Agentic.enabled() {
		return
	}
	work := agenticWork{org: org, repo: repo, number: pr.Number}
	rev := progressRevision{head: pr.Head.SHA, base: pr.Base.Ref}
	// Reverify once if another observation superseded this lookup. Do not trust
	// old pushes, draft/close events, or out-of-date ineligible target branches.
	for range 2 {
		generation, verify := cw.progress.lifecycleGeneration(work, rev, inactive)
		if !verify {
			return
		}
		findComment := !inactive && generation.entry == nil
		current, err := cw.progress.lookup(interrupts.Context(), work, findComment)
		if err != nil {
			logger.WithError(err).Debug("Could not verify pipeline progress revision")
			return
		}
		liveRev, liveActive := current.progressSnapshot()
		if liveRev != rev || liveActive != active {
			return
		}
		liveCfg := pipelineBranchConfig(cw.watcher, cw.lgtmWatcher, org, repo, liveRev.base)
		if liveCfg.ProgressComment != cfg.ProgressComment || liveCfg.Agentic.enabled() != cfg.Agentic.enabled() {
			return
		}
		body := ""
		if !inactive {
			body = renderProgress(pullRefs(org, repo, pr), waitingProgress(liveCfg, pr, false))
		}
		var comment progressComment
		if findComment {
			comment = originalProgressComment(current.Repository.PullRequest.Comments.Nodes, time.Now())
		}
		if cw.progress.applyLifecycle(work, generation, rev, body, comment) {
			return
		}
	}
}
