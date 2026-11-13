package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/kube"
)

// scheduleMode selects how second-stage tests are scheduled.
type scheduleMode int

const (
	// modeForce schedules the entire required set, re-firing jobs that already
	// exist at HEAD. Used only by the explicit /pipeline required command.
	modeForce scheduleMode = iota
	// modeDelta schedules only the required jobs that do not yet have a ProwJob
	// at HEAD (the delta). Used by automatic, LGTM, and /pipeline remaining.
	modeDelta
)

type minimalGhClient interface {
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
	CreateComment(org, repo string, number int, comment string) error
	GetPullRequestChanges(org string, repo string, number int) ([]github.PullRequestChange, error)
	CreateStatus(org, repo, ref string, s github.Status) error
	AddLabel(org, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	GetCombinedStatus(org, repo, ref string) (*github.CombinedStatus, error)
}

// sendCommentWithMode plans and posts the second-stage scheduling comment.
// explicit is true when the run was requested by a human /pipeline command; it
// controls whether a "nothing to schedule" acknowledgment is posted. Automatic
// triggers (explicit=false) stay silent when there is nothing to schedule, so
// the controller only comments when it actually acts — this avoids fleet-wide
// duplicate "already triggered"/"no tests" notes that otherwise recur on every
// reconcile/LGTM pass and across restarts (the per-handler dedup caches are
// separate and in-memory).
func sendCommentWithMode(presubmits presubmitTests, pj *v1.ProwJob, ghc minimalGhClient, deleteIds func(), pjLister ctrlruntimeclient.Reader, mode scheduleMode, explicit bool, checks *dispatchChecks) error {
	if checks != nil {
		checks.mu.Lock()
		defer checks.mu.Unlock()
	}
	comment, jobs, err := planDispatchComment(presubmits, pj, ghc, pjLister, mode, explicit)
	if err == nil {
		err = checks.dispatch(pj.Spec.Refs, len(jobs) != 0, func() error {
			if comment == "" {
				return nil
			}
			if checks != nil {
				if err := publishPendingContexts(ghc, pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.Pulls[0].SHA, jobs); err != nil {
					return err
				}
			}
			return ghc.CreateComment(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.Pulls[0].Number, comment)
		})
	}
	if err != nil {
		deleteIds() // Check publication failures must also release dispatch retries.
	}
	return err
}

func planDispatchComment(presubmits presubmitTests, pj *v1.ProwJob, ghc minimalGhClient, pjLister ctrlruntimeclient.Reader, mode scheduleMode, explicit bool) (string, []config.Presubmit, error) {
	if pj.Spec.Refs == nil || len(pj.Spec.Refs.Pulls) == 0 {
		return "", nil, fmt.Errorf("ProwJob %s does not have valid Refs.Pulls", pj.Name)
	}

	// Combine pipelineConditionallyRequired and pipelineSkipOnlyRequired for processing
	allConditionalTests := append([]config.Presubmit{}, presubmits.pipelineConditionallyRequired...)
	allConditionalTests = append(allConditionalTests, presubmits.pipelineSkipOnlyRequired...)

	conditionalJobs, err := selectConditionalJobs(context.Background(), pj, allConditionalTests, ghc, pjLister, mode)
	if err != nil {
		// In modeDelta a list error fails closed: schedule nothing and post no
		// comment. The caller lets the automatic path retry on the next event
		// (and, because no comment is posted here, does not defeat the
		// one-comment-per-SHA guarantee).
		return "", nil, err
	}

	var comment string

	repoBaseRef := pj.Spec.Refs.Repo + "-" + pj.Spec.Refs.BaseRef

	var protectedJobs []config.Presubmit
	for _, presubmit := range presubmits.protected {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		// Skip re-triggering if a ProwJob already exists at the same SHA
		// (unless this is an explicit /pipeline required command).
		if mode == modeDelta && pjLister != nil && pj.Spec.Refs.Pulls[0].SHA != "" {
			exists, err := existsAtSHA(context.Background(), pjLister, pj, presubmit.Name)
			if err != nil {
				return "", nil, err
			}
			if exists {
				continue
			}
		}
		protectedJobs = append(protectedJobs, presubmit)
	}

	// Sinker may have pruned the finished second-stage ProwJobs, so the
	// cache-only dedupe above can re-fire jobs that already completed at this
	// HEAD: one batched status lookup recovers those runs, failing closed like
	// the lister errors above. modeForce never consults statuses.
	proven := sets.New[string]()
	if mode == modeDelta && pjLister != nil && pj.Spec.Refs.Pulls[0].SHA != "" {
		proven, err = terminalReportedContexts(ghc, pj.Spec.Refs, presubmits.statusContextOwners, append(protectedJobs, conditionalJobs...))
		if err != nil {
			return "", nil, err
		}
		if proven.Len() > 0 {
			protectedJobs = withoutReportedJobs(protectedJobs, proven, pj.Spec.Refs.BaseRef, presubmits.statusContextOwners)
			conditionalJobs = withoutReportedJobs(conditionalJobs, proven, pj.Spec.Refs.BaseRef, presubmits.statusContextOwners)
		}
	}

	protectedCommands := testCommands(protectedJobs)
	testContexts := testCommands(conditionalJobs)
	if protectedCommands != "" {
		comment += "Scheduling required tests:" + protectedCommands
	}
	if testContexts != "" {
		if protectedCommands != "" {
			comment += "\n"
		}
		comment += "\nScheduling tests matching the `pipeline_run_if_changed` or not excluded by `pipeline_skip_if_only_changed` parameters:"
		comment += testContexts
	}

	// Nothing was scheduled. Automatic triggers stay silent (they only announce
	// real scheduling); explicit /pipeline commands get an acknowledgment. In
	// modeDelta the empty result can mean the second stage was already triggered
	// earlier for this HEAD; in that case avoid the misleading "no second-stage
	// tests were triggered" wording.
	jobs := append(protectedJobs, conditionalJobs...)
	if len(jobs) == 0 {
		if !explicit {
			return "", nil, nil
		}
		// A recovered terminal proof already says the second stage ran at this
		// HEAD; prefer it over repeating the cache lookup below.
		if mode == modeDelta && (proven.Len() > 0 || secondStageTriggeredAtSHA(context.Background(), pjLister, pj, presubmits)) {
			comment = fmt.Sprintf("**Pipeline controller notification**\n\nAll applicable second-stage tests for this HEAD have already been triggered. Nothing new to schedule.\n\nUse `/test ?` to see all available tests, or `/pipeline required` to re-run the full required set for the `%s` branch.", pj.Spec.Refs.BaseRef)
		} else {
			comment = fmt.Sprintf("**Pipeline controller notification**\n\nNo second-stage tests were triggered for this PR.\n\nThis can happen when:\n- The changed files don't match any `pipeline_run_if_changed` patterns\n- All files match `pipeline_skip_if_only_changed` patterns\n- No pipeline-controlled jobs are defined for the `%s` branch\n\nUse `/test ?` to see all available tests.", pj.Spec.Refs.BaseRef)
		}
	}

	return comment, jobs, nil
}

// Publish only the jobs being requested, before Hook can start them. Native
// contexts must block merging while the dispatch check is already successful.
func publishPendingContexts(gh minimalGhClient, org, repo, sha string, jobs []config.Presubmit) error {
	for _, job := range jobs {
		if job.SkipReport || job.Context == "" {
			continue
		}
		if err := gh.CreateStatus(org, repo, sha, github.Status{Context: job.Context, State: "pending", Description: PipelinePendingMessage}); err != nil {
			return fmt.Errorf("publishing pending context %q: %w", job.Context, err)
		}
	}
	return nil
}

// testCommands keeps Hook commands outside code fences in both modes.
func testCommands(jobs []config.Presubmit) string {
	var commands strings.Builder
	for _, job := range jobs {
		commands.WriteString("\n" + job.RerunCommand)
	}
	return commands.String()
}

// secondStageTriggeredAtSHA reports whether any second-stage (protected or
// conditional) job already has a ProwJob at the ProwJob's HEAD SHA. It reads the
// controller cache only (no GitHub calls) and is used solely to word the
// empty-delta comment. It returns false when there is no lister or HEAD SHA.
func secondStageTriggeredAtSHA(ctx context.Context, pjLister ctrlruntimeclient.Reader, pj *v1.ProwJob, presubmits presubmitTests) bool {
	if pjLister == nil || pj.Spec.Refs == nil || len(pj.Spec.Refs.Pulls) == 0 || pj.Spec.Refs.Pulls[0].SHA == "" {
		return false
	}

	selector := map[string]string{
		kube.OrgLabel:         pj.Spec.Refs.Org,
		kube.RepoLabel:        pj.Spec.Refs.Repo,
		kube.PullLabel:        fmt.Sprintf("%d", pj.Spec.Refs.Pulls[0].Number),
		kube.BaseRefLabel:     pj.Spec.Refs.BaseRef,
		kube.ProwJobTypeLabel: string(v1.PresubmitJob),
	}

	var pjs v1.ProwJobList
	if err := pjLister.List(ctx, &pjs, ctrlruntimeclient.MatchingLabels(selector)); err != nil {
		return false
	}

	existing := map[string]bool{}
	for _, pjob := range pjs.Items {
		if pjob.Spec.Refs != nil && len(pjob.Spec.Refs.Pulls) > 0 &&
			pjob.Spec.Refs.Pulls[0].SHA == pj.Spec.Refs.Pulls[0].SHA {
			existing[pjob.Spec.Job] = true
		}
	}

	repoBaseRef := pj.Spec.Refs.Repo + "-" + pj.Spec.Refs.BaseRef
	secondStage := append([]config.Presubmit{}, presubmits.protected...)
	secondStage = append(secondStage, presubmits.pipelineConditionallyRequired...)
	secondStage = append(secondStage, presubmits.pipelineSkipOnlyRequired...)
	for _, presubmit := range secondStage {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if existing[presubmit.Name] {
			return true
		}
	}
	return false
}

// canReuseStatus accepts only contexts uniquely owned in the complete static
// inventory by a presubmit restricted to the literal target base.
func canReuseStatus(presubmit config.Presubmit, base string, owners map[string]string) bool {
	contextKey := strings.ToLower(presubmit.Context)
	if base == "" || presubmit.SkipReport || contextKey == "" || contextKey == pipelineGate {
		return false // unreportable contexts cannot be verified on GitHub
	}
	if owner := owners[contextKey]; owner == "" || owner != presubmit.Name {
		return false // unknown or shared context ownership
	}
	return len(presubmit.Branches) == 1 &&
		presubmit.Branches[0] == "^"+regexp.QuoteMeta(base)+"$" &&
		len(presubmit.SkipBranches) == 0
}

// terminalReportedContexts recovers attempted second-stage contexts from terminal statuses.
// Only unambiguous single-base contexts qualify, and the PR must still match refs.
// Pending may be our own placeholder and is never evidence of an attempt.
func terminalReportedContexts(ghc minimalGhClient, refs *v1.Refs, owners map[string]string, candidates []config.Presubmit) (sets.Set[string], error) {
	if ghc == nil || refs == nil || len(refs.Pulls) == 0 || refs.Pulls[0].SHA == "" {
		return nil, nil
	}
	sha := refs.Pulls[0].SHA
	prName := fmt.Sprintf("%s/%s#%d", refs.Org, refs.Repo, refs.Pulls[0].Number)

	var reportable []config.Presubmit
	for _, presubmit := range candidates {
		if !canReuseStatus(presubmit, refs.BaseRef, owners) {
			continue // the context cannot be attributed to this base branch
		}
		reportable = append(reportable, presubmit)
	}
	if len(reportable) == 0 {
		return nil, nil
	}

	combined, err := ghc.GetCombinedStatus(refs.Org, refs.Repo, sha)
	if err != nil {
		return nil, fmt.Errorf("getting combined status for pruned second-stage jobs of %s: %w", prName, err)
	}
	if combined == nil {
		return nil, fmt.Errorf("empty combined status response for pruned second-stage jobs of %s", prName)
	}
	if combined.SHA != sha {
		return nil, fmt.Errorf("combined status response for pruned second-stage jobs of %s has sha %q, want %q", prName, combined.SHA, sha)
	}

	// GitHub reports the latest status of every context on the ref; the
	// aggregate combined state is deliberately ignored.
	states := make(map[string]string, len(combined.Statuses))
	for _, status := range combined.Statuses {
		states[status.Context] = status.State
	}
	terminal := sets.New[string]()
	for _, presubmit := range reportable {
		switch states[presubmit.Context] {
		case github.StatusSuccess, github.StatusFailure, github.StatusError:
			terminal.Insert(presubmit.Context)
		}
	}
	if terminal.Len() == 0 {
		return terminal, nil
	}

	pr, err := ghc.GetPullRequest(refs.Org, refs.Repo, refs.Pulls[0].Number)
	if err != nil {
		return nil, fmt.Errorf("getting pull request for pruned second-stage jobs of %s: %w", prName, err)
	}
	if pr == nil {
		return nil, fmt.Errorf("empty pull request response for pruned second-stage jobs of %s", prName)
	}
	if pr.State != github.PullRequestStateOpen || pr.Draft ||
		pr.Head.SHA != sha || pr.Base.Ref != refs.BaseRef {
		return nil, fmt.Errorf("stale terminal statuses for pruned second-stage jobs of %s: pull request state %q draft %t head %q base %q", prName, pr.State, pr.Draft, pr.Head.SHA, pr.Base.Ref)
	}
	return terminal, nil
}

// withoutReportedJobs removes only eligible definitions with a reported context;
// another definition sharing a job name must remain schedulable.
func withoutReportedJobs(jobs []config.Presubmit, reported sets.Set[string], base string, owners map[string]string) []config.Presubmit {
	var kept []config.Presubmit
	for _, job := range jobs {
		if canReuseStatus(job, base, owners) && reported.Has(job.Context) {
			continue
		}
		kept = append(kept, job)
	}
	return kept
}

// pipelineAnnotationMatches gives run-if-changed precedence over skip-if-only-changed.
func pipelineAnnotationMatches(p config.Presubmit, changes config.ChangedFilesProvider) (bool, error) {
	run, skip := p.Annotations["pipeline_run_if_changed"], p.Annotations["pipeline_skip_if_only_changed"]
	if run == "" && skip == "" {
		return true, nil
	}
	job := []config.Presubmit{p}
	if run != "" {
		job[0].RegexpChangeMatcher = config.RegexpChangeMatcher{RunIfChanged: run}
	} else {
		job[0].RegexpChangeMatcher = config.RegexpChangeMatcher{SkipIfOnlyChanged: skip}
	}
	if err := config.SetPresubmitRegexes(job); err != nil {
		return false, err
	}
	_, shouldRun, err := job[0].RegexpChangeMatcher.ShouldRun(changes)
	return shouldRun, err
}

func selectConditionalJobs(ctx context.Context, pj *v1.ProwJob, pipelineConditionallyRequired []config.Presubmit, ghc minimalGhClient, pjLister ctrlruntimeclient.Reader, mode scheduleMode) ([]config.Presubmit, error) {
	if pj.Spec.Refs == nil || len(pj.Spec.Refs.Pulls) == 0 {
		return nil, fmt.Errorf("ProwJob %s does not have valid Refs.Pulls", pj.Name)
	}

	repoBaseRef := pj.Spec.Refs.Repo + "-" + pj.Spec.Refs.BaseRef
	var selected []config.Presubmit
	if len(pipelineConditionallyRequired) != 0 {
		cfp := config.NewGitHubDeferredChangedFilesProvider(ghc, pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.Pulls[0].Number)

		// First, determine which tests should run based on file changes
		var testsToRun []config.Presubmit
		for _, presubmit := range pipelineConditionallyRequired {
			if !strings.Contains(presubmit.Name, repoBaseRef) {
				continue
			}

			shouldRun, err := pipelineAnnotationMatches(presubmit, cfp)
			if err != nil {
				return nil, err
			}

			if shouldRun {
				testsToRun = append(testsToRun, presubmit)
			}
		}

		// In modeDelta, complement the jobs already triggered at HEAD by scheduling
		// only the ones still missing. List the PR's ProwJobs from the controller
		// cache once (not per job) and subtract the ones already present at the same
		// SHA. modeForce (/pipeline required) skips this entirely and schedules all.
		//
		// Preserve today's guards: skip the cache lookup when there is no lister or
		// no HEAD SHA (several tests pass a nil lister).
		existing := map[string]bool{}
		if mode == modeDelta && pjLister != nil && pj.Spec.Refs.Pulls[0].SHA != "" {
			// Same label selector today's latch block built (from ProwJob spec).
			selector := map[string]string{
				kube.OrgLabel:         pj.Spec.Refs.Org,
				kube.RepoLabel:        pj.Spec.Refs.Repo,
				kube.PullLabel:        fmt.Sprintf("%d", pj.Spec.Refs.Pulls[0].Number),
				kube.BaseRefLabel:     pj.Spec.Refs.BaseRef,
				kube.ProwJobTypeLabel: string(v1.PresubmitJob),
			}

			var pjs v1.ProwJobList
			if err := pjLister.List(ctx, &pjs, ctrlruntimeclient.MatchingLabels(selector)); err != nil {
				// Fail closed: a list error must NOT be treated as "all jobs absent"
				// (that would mass-trigger). Return the error so sendCommentWithMode
				// short-circuits and schedules nothing; the automatic path retries on
				// the next ProwJob event.
				return nil, fmt.Errorf("listing prowjobs for delta: %w", err)
			}
			for _, pjob := range pjs.Items {
				if pjob.Spec.Refs != nil && len(pjob.Spec.Refs.Pulls) > 0 &&
					pjob.Spec.Refs.Pulls[0].SHA == pj.Spec.Refs.Pulls[0].SHA {
					existing[pjob.Spec.Job] = true
				}
			}
		}

		for _, presubmit := range testsToRun {
			if !strings.Contains(presubmit.Name, repoBaseRef) {
				continue
			}
			if mode == modeForce || !existing[presubmit.Name] {
				selected = append(selected, presubmit)
			}
			// else: already present at HEAD (manual trigger or a prior delta) → skip it
		}
	}
	return selected, nil
}

// existsAtSHA checks whether a ProwJob with the given job name already exists
// for the same org/repo/PR/baseRef at the same HEAD SHA. This is used to avoid
// re-triggering tests that were already triggered (e.g. via /pipeline required)
// when an event like /lgtm fires at the same commit.
func existsAtSHA(ctx context.Context, pjLister ctrlruntimeclient.Reader, pj *v1.ProwJob, jobName string) (bool, error) {
	selector := map[string]string{
		kube.OrgLabel:         pj.Spec.Refs.Org,
		kube.RepoLabel:        pj.Spec.Refs.Repo,
		kube.PullLabel:        fmt.Sprintf("%d", pj.Spec.Refs.Pulls[0].Number),
		kube.BaseRefLabel:     pj.Spec.Refs.BaseRef,
		kube.ProwJobTypeLabel: string(v1.PresubmitJob),
	}

	var pjs v1.ProwJobList
	if err := pjLister.List(ctx, &pjs, ctrlruntimeclient.MatchingLabels(selector)); err != nil {
		return false, fmt.Errorf("listing prowjobs for delta: %w", err)
	}

	for _, pjob := range pjs.Items {
		if pjob.Spec.Job == jobName &&
			pjob.Spec.Refs != nil &&
			len(pjob.Spec.Refs.Pulls) > 0 &&
			pjob.Spec.Refs.Pulls[0].SHA == pj.Spec.Refs.Pulls[0].SHA {
			return true, nil
		}
	}
	return false, nil
}

func isFirstStageJob(presubmits presubmitTests, name string) bool {
	for _, jobs := range [][]config.Presubmit{presubmits.alwaysRequired, presubmits.conditionallyRequired} {
		for _, job := range jobs {
			if job.Name == name {
				return true
			}
		}
	}
	return false
}

// checkFirstStageComplete trusts live ProwJobs first and recovers missing
// always-required results from GitHub statuses for the PR's current revision.
// Recovered statuses count only for jobs that pass the canReuseStatus
// eligibility guard, so a context shared across base branches (or a repo with
// an unknown reporting inventory) stays incomplete instead of guessing.
func checkFirstStageComplete(ctx context.Context, pjLister ctrlruntimeclient.Reader, ghc minimalGhClient, pj *v1.ProwJob, presubmits presubmitTests) (bool, error) {
	if pj == nil || pj.Spec.Refs == nil || len(pj.Spec.Refs.Pulls) != 1 {
		return false, nil
	}

	selector := map[string]string{
		kube.OrgLabel:         pj.Spec.Refs.Org,
		kube.RepoLabel:        pj.Spec.Refs.Repo,
		kube.PullLabel:        fmt.Sprintf("%d", pj.Spec.Refs.Pulls[0].Number),
		kube.BaseRefLabel:     pj.Spec.Refs.BaseRef,
		kube.ProwJobTypeLabel: string(v1.PresubmitJob),
	}

	var pjs v1.ProwJobList
	if err := pjLister.List(ctx, &pjs, ctrlruntimeclient.MatchingLabels(selector)); err != nil {
		return false, fmt.Errorf("cannot list prowjobs: %w", err)
	}

	latestBatch := make(map[string]*v1.ProwJob)
	for i := range pjs.Items {
		pjob := &pjs.Items[i]
		if pjob.Spec.Refs == nil || len(pjob.Spec.Refs.Pulls) == 0 {
			continue
		}
		if pjob.Spec.Refs.Pulls[0].SHA == pj.Spec.Refs.Pulls[0].SHA {
			if existing, ok := latestBatch[pjob.Spec.Job]; !ok {
				latestBatch[pjob.Spec.Job] = pjob
			} else if pjob.CreationTimestamp.After(existing.CreationTimestamp.Time) {
				latestBatch[pjob.Spec.Job] = pjob
			}
		}
	}

	repoBaseRef := pj.Spec.Refs.Repo + "-" + pj.Spec.Refs.BaseRef

	// All always-required first-stage jobs must have succeeded; ones missing
	// from the cache may have been pruned after succeeding and are decided by
	// the GitHub status fallback below.
	var missing []config.Presubmit
	for _, presubmit := range presubmits.alwaysRequired {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if job := latestBatch[presubmit.Name]; job == nil {
			missing = append(missing, presubmit)
		} else if job.Status.State != v1.SuccessState {
			return false, nil
		}
	}

	// Conditionally-required first-stage jobs, if present, must have succeeded.
	for _, presubmit := range presubmits.conditionallyRequired {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if job := latestBatch[presubmit.Name]; job != nil && job.Status.State != v1.SuccessState {
			return false, nil
		}
	}

	if len(missing) == 0 {
		return true, nil
	}

	// A late LGTM or override can arrive after sinker deleted the successful
	// runs. Only the missing jobs' exact contexts count; the aggregate combined
	// state (pipeline gate, second-stage contexts) must not block. A context
	// that fails the canReuseStatus eligibility guard also stays incomplete:
	// its status cannot be attributed to this PR's base branch, so the first
	// stage must be proven again rather than guessed from an ambiguous green.
	if ghc == nil {
		return false, fmt.Errorf("no github client to verify pruned first-stage jobs of %s", pj.Name)
	}
	if pj.Spec.Refs.Pulls[0].SHA == "" {
		return false, nil
	}
	for _, presubmit := range missing {
		if !canReuseStatus(presubmit, pj.Spec.Refs.BaseRef, presubmits.statusContextOwners) {
			return false, nil
		}
	}
	// A stale ProwJob event says nothing about the current PR, so its statuses
	// count only while it is open, not a draft, and on the same HEAD and base.
	pr, err := ghc.GetPullRequest(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.Pulls[0].Number)
	if err != nil {
		return false, fmt.Errorf("getting pull request for pruned first-stage jobs of %s: %w", pj.Name, err)
	}
	if pr == nil {
		return false, fmt.Errorf("empty pull request response for pruned first-stage jobs of %s", pj.Name)
	}
	if pr.State != github.PullRequestStateOpen || pr.Draft ||
		pr.Head.SHA != pj.Spec.Refs.Pulls[0].SHA || pr.Base.Ref != pj.Spec.Refs.BaseRef {
		return false, nil
	}
	combined, err := ghc.GetCombinedStatus(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.Pulls[0].SHA)
	if err != nil {
		return false, fmt.Errorf("getting combined status for pruned first-stage jobs of %s: %w", pj.Name, err)
	}
	if combined == nil {
		return false, fmt.Errorf("empty combined status response for pruned first-stage jobs of %s", pj.Name)
	}
	if combined.SHA != pj.Spec.Refs.Pulls[0].SHA {
		return false, nil
	}
	states := make(map[string]string, len(combined.Statuses))
	for _, status := range combined.Statuses {
		states[status.Context] = status.State
	}
	for _, presubmit := range missing {
		if states[presubmit.Context] != github.StatusSuccess {
			return false, nil
		}
	}
	// The GitHub methods above do not accept ctx.
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("checking first-stage completion after GitHub lookups: %w", err)
	}
	return true, nil
}
