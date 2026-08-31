package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/kube"
)

type minimalGhClient interface {
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
	CreateComment(org, repo string, number int, comment string) error
	GetPullRequestChanges(org string, repo string, number int) ([]github.PullRequestChange, error)
	CreateStatus(org, repo, ref string, s github.Status) error
	AddLabel(org, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	GetFile(org, repo, filepath, commit string) ([]byte, error)
}

func sendComment(presubmits presubmitTests, pj *v1.ProwJob, ghc minimalGhClient, deleteIds func(), pjLister ctrlruntimeclient.Reader) error {
	return sendCommentWithMode(presubmits, pj, ghc, deleteIds, pjLister, false)
}

func sendCommentWithMode(presubmits presubmitTests, pj *v1.ProwJob, ghc minimalGhClient, deleteIds func(), pjLister ctrlruntimeclient.Reader, isExplicitCommand bool) error {
	if pj.Spec.Refs == nil || len(pj.Spec.Refs.Pulls) == 0 {
		deleteIds()
		return fmt.Errorf("ProwJob %s does not have valid Refs.Pulls", pj.Name)
	}

	// Combine pipelineConditionallyRequired and pipelineSkipOnlyRequired for processing
	allConditionalTests := append([]config.Presubmit{}, presubmits.pipelineConditionallyRequired...)
	allConditionalTests = append(allConditionalTests, presubmits.pipelineSkipOnlyRequired...)

	testContexts, manualControlMessage, err := acquireConditionalContexts(context.Background(), pj, allConditionalTests, ghc, deleteIds, pjLister, isExplicitCommand)
	if err != nil {
		deleteIds()
		return err
	}

	var comment string

	repoBaseRef := pj.Spec.Refs.Repo + "-" + pj.Spec.Refs.BaseRef

	// If it's an explicit /pipeline required command, ignore manual control message
	// and proceed with scheduling tests
	if manualControlMessage != "" && !isExplicitCommand {
		comment = manualControlMessage
	} else {
		var protectedCommands string
		for _, presubmit := range presubmits.protected {
			if !strings.Contains(presubmit.Name, repoBaseRef) {
				continue
			}
			// Skip re-triggering if a ProwJob already exists at the same SHA
			// (unless this is an explicit /pipeline required command)
			if !isExplicitCommand && pjLister != nil && pj.Spec.Refs.Pulls[0].SHA != "" {
				if existsAtSHA(context.Background(), pjLister, pj, presubmit.Name) {
					continue
				}
			}
			protectedCommands += "\n" + presubmit.RerunCommand
		}
		if protectedCommands != "" {
			comment += "Scheduling required tests:" + protectedCommands
		}
		if testContexts != "" {
			if protectedCommands != "" {
				comment += "\n"
			}
			comment += "\nScheduling tests matching the `pipeline_run_if_changed`, `pipeline_run_if_dockerfile_changed`, or not excluded by `pipeline_skip_if_only_changed` parameters:"
			comment += testContexts
		}
	}

	// If no tests matched, send an informative comment instead of staying silent
	if comment == "" {
		comment = fmt.Sprintf("**Pipeline controller notification**\n\nNo second-stage tests were triggered for this PR.\n\nThis can happen when:\n- The changed files don't match any `pipeline_run_if_changed` patterns\n- All files match `pipeline_skip_if_only_changed` patterns\n- No pipeline-controlled jobs are defined for the `%s` branch\n\nUse `/test ?` to see all available tests.", pj.Spec.Refs.BaseRef)
	}

	if err := ghc.CreateComment(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.Pulls[0].Number, comment); err != nil {
		deleteIds()
		return err
	}
	return nil
}

func acquireConditionalContexts(ctx context.Context, pj *v1.ProwJob, pipelineConditionallyRequired []config.Presubmit, ghc minimalGhClient, deleteIds func(), pjLister ctrlruntimeclient.Reader, isExplicitCommand bool) (string, string, error) {
	if pj.Spec.Refs == nil || len(pj.Spec.Refs.Pulls) == 0 {
		return "", "", fmt.Errorf("ProwJob %s does not have valid Refs.Pulls", pj.Name)
	}

	repoBaseRef := pj.Spec.Refs.Repo + "-" + pj.Spec.Refs.BaseRef
	var testCommands string
	if len(pipelineConditionallyRequired) != 0 {
		cfp := config.NewGitHubDeferredChangedFilesProvider(ghc, pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.Pulls[0].Number)

		// First, determine which tests should run based on file changes
		var testsToRun []config.Presubmit
		for _, presubmit := range pipelineConditionallyRequired {
			if !strings.Contains(presubmit.Name, repoBaseRef) {
				continue
			}

			shouldRun := false
			// Check pipeline_run_if_changed first (takes precedence)
			if run, ok := presubmit.Annotations["pipeline_run_if_changed"]; ok && run != "" {
				psList := []config.Presubmit{presubmit}
				psList[0].RegexpChangeMatcher = config.RegexpChangeMatcher{RunIfChanged: run}
				if err := config.SetPresubmitRegexes(psList); err != nil {
					deleteIds()
					return "", "", err
				}
				_, shouldRunResult, err := psList[0].RegexpChangeMatcher.ShouldRun(cfp)
				if err != nil {
					deleteIds()
					return "", "", err
				}
				shouldRun = shouldRunResult
			} else if skip, ok := presubmit.Annotations["pipeline_skip_if_only_changed"]; ok && skip != "" {
				// Check pipeline_skip_if_only_changed if pipeline_run_if_changed is not present
				psList := []config.Presubmit{presubmit}
				psList[0].RegexpChangeMatcher = config.RegexpChangeMatcher{SkipIfOnlyChanged: skip}
				if err := config.SetPresubmitRegexes(psList); err != nil {
					deleteIds()
					return "", "", err
				}
				_, shouldRunResult, err := psList[0].RegexpChangeMatcher.ShouldRun(cfp)
				if err != nil {
					deleteIds()
					return "", "", err
				}
				shouldRun = shouldRunResult
			} else if dockerfileAnnotation, ok := presubmit.Annotations["pipeline_run_if_dockerfile_changed"]; ok && dockerfileAnnotation != "" {
				var entries []dockerfileEntry
				if err := json.Unmarshal([]byte(dockerfileAnnotation), &entries); err != nil {
					deleteIds()
					return "", "", fmt.Errorf("failed to parse pipeline_run_if_dockerfile_changed annotation: %w", err)
				}
				changedFiles, err := cfp()
				if err != nil {
					deleteIds()
					return "", "", err
				}
				shouldRun = evaluateDockerfileChanges(entries, changedFiles, pj, ghc)
			} else {
				shouldRun = true
			}

			if shouldRun {
				testsToRun = append(testsToRun, presubmit)
			}
		}

		// Check if any of the tests that should run have already been manually triggered
		// Skip this check if it's an explicit /pipeline required command
		if len(testsToRun) > 0 && pjLister != nil && pj.Spec.Refs.Pulls[0].SHA != "" && !isExplicitCommand {
			// Build label selector from ProwJob spec (same as in reconciler.go)
			selector := map[string]string{
				kube.OrgLabel:         pj.Spec.Refs.Org,
				kube.RepoLabel:        pj.Spec.Refs.Repo,
				kube.PullLabel:        fmt.Sprintf("%d", pj.Spec.Refs.Pulls[0].Number),
				kube.BaseRefLabel:     pj.Spec.Refs.BaseRef,
				kube.ProwJobTypeLabel: string(v1.PresubmitJob),
			}

			var pjs v1.ProwJobList
			if err := pjLister.List(ctx, &pjs, ctrlruntimeclient.MatchingLabels(selector)); err != nil {
				// If listing fails, skip check and proceed with creating comment
				deleteIds()
				testCommands := ""
				for _, presubmit := range testsToRun {
					testCommands += "\n" + presubmit.RerunCommand
				}
				return testCommands, "", nil
			}

			// Check if any of the tests we want to run have already been triggered
			// by looking for ProwJobs with matching job names and same SHA
			// If a job exists in ANY state, it means it was already triggered
			// so we should not run it and inform the user
			for _, presubmit := range testsToRun {
				testName := presubmit.Name
				// Only check presubmits that match the repo-baseRef pattern (same as reconciler)
				if !strings.Contains(testName, repoBaseRef) {
					continue
				}
				for _, pjob := range pjs.Items {
					if pjob.Spec.Job == testName &&
						pjob.Spec.Refs != nil &&
						len(pjob.Spec.Refs.Pulls) > 0 &&
						pjob.Spec.Refs.Pulls[0].SHA == pj.Spec.Refs.Pulls[0].SHA {
						return "", "Tests from second stage were triggered manually. Pipeline can be controlled only manually, until HEAD changes. Use command to trigger second stage.", nil
					}
				}
			}
		}

		for _, presubmit := range testsToRun {
			testCommands += "\n" + presubmit.RerunCommand
		}
	}
	return testCommands, "", nil
}

// existsAtSHA checks whether a ProwJob with the given job name already exists
// for the same org/repo/PR/baseRef at the same HEAD SHA. This is used to avoid
// re-triggering tests that were already triggered (e.g. via /pipeline required)
// when an event like /lgtm fires at the same commit.
func existsAtSHA(ctx context.Context, pjLister ctrlruntimeclient.Reader, pj *v1.ProwJob, jobName string) bool {
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

	for _, pjob := range pjs.Items {
		if pjob.Spec.Job == jobName &&
			pjob.Spec.Refs != nil &&
			len(pjob.Spec.Refs.Pulls) > 0 &&
			pjob.Spec.Refs.Pulls[0].SHA == pj.Spec.Refs.Pulls[0].SHA {
			return true
		}
	}
	return false
}

// checkFirstStageComplete checks if all first-stage tests have completed
// successfully for the given ProwJob's SHA. This is used by the /pipeline auto
// handler to trigger second-stage tests immediately when first-stage is already
// done, since the event-driven reconciler won't fire if all ProwJob updates
// occurred before the pipeline-auto label was added.
func checkFirstStageComplete(ctx context.Context, pjLister ctrlruntimeclient.Reader, pj *v1.ProwJob, presubmits presubmitTests) (bool, error) {
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

	latestBatch := make(map[string]v1.ProwJob)
	for _, pjob := range pjs.Items {
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

	// Second-stage (protected) jobs must not already be running
	for _, presubmit := range presubmits.protected {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if _, ok := latestBatch[presubmit.Name]; ok {
			return false, nil
		}
	}

	// All always-required first-stage jobs must have succeeded
	for _, presubmit := range presubmits.alwaysRequired {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if pjob, ok := latestBatch[presubmit.Name]; !ok || pjob.Status.State != v1.SuccessState {
			return false, nil
		}
	}

	// Conditionally-required first-stage jobs, if present, must have succeeded
	for _, presubmit := range presubmits.conditionallyRequired {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if pjob, ok := latestBatch[presubmit.Name]; ok && pjob.Status.State != v1.SuccessState {
			return false, nil
		}
	}

	return true, nil
}
