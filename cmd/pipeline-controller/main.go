package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bombsimon/logrusr/v3"
	"github.com/sirupsen/logrus"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntimelog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config/secret"
	prowflagutil "sigs.k8s.io/prow/pkg/flagutil"
	configflagutil "sigs.k8s.io/prow/pkg/flagutil/config"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/githubeventserver"
	"sigs.k8s.io/prow/pkg/interrupts"
	"sigs.k8s.io/prow/pkg/labels"
	"sigs.k8s.io/prow/pkg/logrusutil"
)

const pullRequestInfoComment = "**Pipeline controller notification**\nThis repo is configured to use the [pipeline controller](https://docs.ci.openshift.org/how-tos/creating-a-pipeline/). Second-stage tests will be triggered either automatically or after lgtm label is added, depending on the repository configuration. The pipeline controller will automatically detect which contexts are required and will utilize `/test` Prow commands to trigger the second stage.\n\nFor optional jobs, comment `/test ?` to see a list of all defined jobs. To trigger manually all jobs from second stage use `/pipeline required` command. \n\nThis repository is configured in: "

const RepoNotConfiguredMessage = "This repository is not currently configured for [pipeline controller](https://docs.ci.openshift.org/how-tos/creating-a-pipeline/) support."

const PipelinePendingMessage = "Waiting for pipeline condition to trigger this job"

// PipelineAutoLabel is the label that marks a PR to behave as automatic mode
const PipelineAutoLabel = "pipeline-auto"

type options struct {
	client                   prowflagutil.KubernetesOptions
	github                   prowflagutil.GitHubOptions
	githubEventServerOptions githubeventserver.Options
	config                   configflagutil.ConfigOptions
	configFile               string
	lgtmConfigFile           string
	dryrun                   bool
	webhookSecretFile        string
}

func (o *options) validate() error {
	for _, opt := range []interface{ Validate(bool) error }{&o.client, &o.config} {
		if err := opt.Validate(o.dryrun); err != nil {
			return err
		}
	}

	return nil
}

func (o *options) parseArgs(fs *flag.FlagSet, args []string) error {
	fs.BoolVar(&o.dryrun, "dry-run", false, "Run in dry-run mode.")
	fs.StringVar(&o.configFile, "config-file", "", "Config file with list of enabled orgs and repos.")
	fs.StringVar(&o.lgtmConfigFile, "lgtm-config-file", "", "Config file with list of enabled orgs and repos with second stage triggered by lgtm label.")
	fs.StringVar(&o.webhookSecretFile, "hmac-secret-file", "/etc/webhook/hmac", "Path to the file containing the GitHub HMAC secret.")

	o.config.AddFlags(fs)
	o.github.AddFlags(fs)
	o.client.AddFlags(fs)
	o.githubEventServerOptions.Bind(fs)

	if err := fs.Parse(args); err != nil {
		logrus.WithError(err).Fatal("Could not parse args.")
	}

	if o.configFile == "" {
		return fmt.Errorf("--config-file is mandatory")
	}
	if o.lgtmConfigFile == "" {
		return fmt.Errorf("--lgtm-config-file is mandatory")
	}
	if err := o.githubEventServerOptions.DefaultAndValidate(); err != nil {
		return err
	}

	return o.validate()
}

func parseOptions() options {
	var o options

	if err := o.parseArgs(flag.CommandLine, os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("invalid flag options")
	}

	return o
}

type clientWrapper struct {
	ghc                minimalGhClient
	configDataProvider *ConfigDataProvider
	watcher            *watcher
	lgtmWatcher        *watcher
	pjLister           ctrlruntimeclient.Reader
	pipelineAutoCache  *PipelineAutoCache
	// ids provides a per-SHA idempotency guard for the LGTM and /pipeline
	// remaining scheduling paths, which do not run under the reconciler's own
	// ids cache. Keyed by composeKey (org/repo/pr/baseRef/SHA).
	ids sync.Map
	mu  sync.RWMutex // Protects against race conditions in event handling
}

// cleanOldIds periodically evicts stale per-SHA idempotency keys from cw.ids so
// the map does not grow unbounded. It mirrors reconciler.cleanOldIds; the values
// stored are the time each key was added.
func (cw *clientWrapper) cleanOldIds(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		cw.ids.Range(func(key, value interface{}) bool {
			if time.Since(value.(time.Time)) >= interval {
				cw.ids.Delete(key)
			}
			return true
		})
	}
}

// isBranchEnabled checks if the branch is enabled for the given repo configuration
// If branches list is empty, all branches are enabled
func isBranchEnabled(branches []string, branch string) bool {
	if len(branches) == 0 {
		return true // Empty list means all branches are enabled
	}
	for _, b := range branches {
		if b == branch {
			return true
		}
	}
	return false
}

func (cw *clientWrapper) handlePullRequestCreation(l *logrus.Entry, event github.PullRequestEvent) {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	logger := l.WithFields(logrus.Fields{
		"handler": "handlePullRequestCreation",
		"action":  event.Action,
		"org":     event.Repo.Owner.Login,
		"repo":    event.Repo.Name,
		"pr":      event.PullRequest.Number,
	})

	logger.Info("Processing pull request event")

	if github.PullRequestActionOpened == event.Action {
		org := event.Repo.Owner.Login
		repo := event.Repo.Name
		number := event.PullRequest.Number

		logger = logger.WithFields(logrus.Fields{
			"org":  org,
			"repo": repo,
			"pr":   number,
		})

		logger.Info("Processing PR opened event")

		// Check if repo is in configuration (either manual/auto mode or LGTM mode)
		currentCfg := cw.watcher.getConfig()
		repos, orgExists := currentCfg[org]
		repoConfig, repoExists := repos[repo]

		lgtmCfg := cw.lgtmWatcher.getConfig()
		lgtmRepos, lgtmOrgExists := lgtmCfg[org]
		_, lgtmRepoExists := lgtmRepos[repo]

		isInConfig := (orgExists && repoExists) || (lgtmOrgExists && lgtmRepoExists)

		logger.WithFields(logrus.Fields{
			"org_exists":       orgExists,
			"repo_exists":      repoExists,
			"lgtm_org_exists":  lgtmOrgExists,
			"lgtm_repo_exists": lgtmRepoExists,
			"is_in_config":     isInConfig,
		}).Debug("Configuration check results")

		if !isInConfig {
			logger.Debug("Repository not in configuration (neither regular nor LGTM), skipping")
			return
		}

		// Check if branch is enabled for this repo
		baseBranch := event.PullRequest.Base.Ref
		var branchEnabled bool
		if orgExists && repoExists {
			branchEnabled = isBranchEnabled(repoConfig.Branches, baseBranch)
		} else if lgtmOrgExists && lgtmRepoExists {
			lgtmRepoConfig := lgtmRepos[repo]
			branchEnabled = isBranchEnabled(lgtmRepoConfig.Branches, baseBranch)
		}

		if !branchEnabled {
			logger.WithField("base_branch", baseBranch).Debug("Branch not enabled for pipeline controller, skipping")
			return
		}

		// Check if repo is in regular config to determine trigger mode
		var isAutomaticPipeline bool
		isInLGTMConfig := lgtmOrgExists && lgtmRepoExists
		if orgExists && repoExists {
			isAutomaticPipeline = repoConfig.Trigger == "auto"
			logger.WithField("trigger_mode", repoConfig.Trigger).Debug("Repository trigger mode")
		}

		logger.Debug("Getting presubmits from config data provider")
		presubmits := cw.configDataProvider.GetPresubmits(org + "/" + repo)

		// Show pipeline info comment for automatic mode or LGTM mode
		if isAutomaticPipeline || isInLGTMConfig {
			hasPipelineJobs := len(presubmits.protected) > 0 || len(presubmits.alwaysRequired) > 0 ||
				len(presubmits.conditionallyRequired) > 0 || len(presubmits.pipelineConditionallyRequired) > 0 ||
				len(presubmits.pipelineSkipOnlyRequired) > 0

			logger.WithField("has_pipeline_jobs", hasPipelineJobs).Debug("Checking for pipeline jobs")

			if hasPipelineJobs {
				// Repo has pipeline-controlled jobs and is in automatic mode or LGTM mode, use pipeline info comment
				modeStr := "automatic mode"
				if isInLGTMConfig && !isAutomaticPipeline {
					modeStr = "LGTM mode"
				}
				logger.WithField("mode", modeStr).Info("Creating pipeline info comment")
				if err := cw.ghc.CreateComment(org, repo, number, pullRequestInfoComment+modeStr); err != nil {
					logger.WithError(err).Error("failed to create comment")
				} else {
					logger.Info("Successfully created pipeline info comment")
				}
			} else {
				logger.Debug("No pipeline jobs found, skipping comment creation")
			}
		} else {
			// Manual mode: Check for non-always-run jobs
			cfg := cw.configDataProvider.configGetter()
			presubmits := cfg.GetPresubmitsStatic(org + "/" + repo)

			hasNonAlwaysRunJobs := false
			for _, p := range presubmits {
				if !p.AlwaysRun {
					hasNonAlwaysRunJobs = true
					break
				}
			}

			if hasNonAlwaysRunJobs {
				comment := "There are test jobs defined for this repository which are not configured to run automatically. " +
					"Comment `/test ?` to see a list of all defined jobs. Review these jobs and use `/test <job>` to manually trigger jobs most likely to be impacted by the proposed changes." +
					"Comment `/pipeline required` to trigger all required & necessary jobs."

				if err := cw.ghc.CreateComment(org, repo, number, comment); err != nil {
					logger.WithError(err).Error("failed to create comment")
				}
			}
		}
	}
}

func (cw *clientWrapper) handleLabelAddition(l *logrus.Entry, event github.PullRequestEvent) {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	logger := l.WithFields(logrus.Fields{
		"handler": "handleLabelAddition",
		"action":  event.Action,
		"org":     event.Repo.Owner.Login,
		"repo":    event.Repo.Name,
		"pr":      event.PullRequest.Number,
		"label":   event.Label.Name,
	})

	logger.Info("Processing label addition event")

	if github.PullRequestActionLabeled == event.Action && event.Label.Name == labels.LGTM {
		org := event.Repo.Owner.Login
		repo := event.Repo.Name

		logger = logger.WithFields(logrus.Fields{
			"org":  org,
			"repo": repo,
			"pr":   event.PullRequest.Number,
		})

		logger.Info("Processing LGTM label addition")

		logger.Debug("Getting LGTM configuration from watcher")
		currentCfg := cw.lgtmWatcher.getConfig()
		repos, orgExists := currentCfg[org]
		_, repoExists := repos[repo]

		logger.WithFields(logrus.Fields{
			"org_exists":  orgExists,
			"repo_exists": repoExists,
		}).Debug("LGTM configuration check results")

		if !orgExists || !repoExists {
			logger.Debug("Repository not in LGTM configuration, skipping")
			return
		}

		// Check if pipeline-auto label is present - if so, skip (reconciler will handle auto triggering)
		for _, label := range event.PullRequest.Labels {
			if label.Name == PipelineAutoLabel {
				logger.Info("PR has pipeline-auto label, skipping LGTM trigger (reconciler will handle automatic triggering)")
				return
			}
		}

		// Check if branch is enabled for this repo
		baseBranch := event.PullRequest.Base.Ref
		repoConfig := repos[repo]
		if !isBranchEnabled(repoConfig.Branches, baseBranch) {
			logger.WithField("base_branch", baseBranch).Debug("Branch not enabled for pipeline controller, skipping")
			return
		}

		prowJob := &v1.ProwJob{
			Spec: v1.ProwJobSpec{
				Refs: &v1.Refs{
					Org:     org,
					Repo:    repo,
					BaseRef: event.PullRequest.Base.Ref,
					Pulls: []v1.Pull{
						{Number: event.PullRequest.Number, SHA: event.PullRequest.Head.SHA},
					},
				},
			},
		}

		// If SHA is missing, log a warning but continue (status check will be skipped)
		if event.PullRequest.Head.SHA == "" {
			logger.Warn("PR head SHA is empty, status check will be skipped")
		}

		logger.WithFields(logrus.Fields{
			"org":       org,
			"repo":      repo,
			"pr_number": event.PullRequest.Number,
			"sha":       event.PullRequest.Head.SHA,
		}).Debug("ProwJob created for LGTM label addition")
		logger.Debug("Getting presubmits from config data provider")
		presubmits := cw.configDataProvider.GetPresubmits(prowJob.Spec.Refs.Org + "/" + prowJob.Spec.Refs.Repo)

		logger.WithFields(logrus.Fields{
			"protected_count":                       len(presubmits.protected),
			"always_required_count":                 len(presubmits.alwaysRequired),
			"conditionally_required_count":          len(presubmits.conditionallyRequired),
			"pipeline_conditionally_required_count": len(presubmits.pipelineConditionallyRequired),
			"pipeline_skip_only_required_count":     len(presubmits.pipelineSkipOnlyRequired),
		}).Debug("Presubmits retrieved for LGTM handler")

		hasPresubmits := len(presubmits.protected) > 0 || len(presubmits.alwaysRequired) > 0 ||
			len(presubmits.conditionallyRequired) > 0 || len(presubmits.pipelineConditionallyRequired) > 0 ||
			len(presubmits.pipelineSkipOnlyRequired) > 0

		if !hasPresubmits {
			logger.Debug("No presubmits found, skipping comment")
			return
		}

		// Per-SHA idempotency guard: the LGTM path does not run under the
		// reconciler's ids cache, so guard against repeated label events
		// double-posting /test within the ProwJob cache-latency window.
		key := composeKey(prowJob.Spec.Refs)
		if _, loaded := cw.ids.LoadOrStore(key, time.Now()); loaded {
			logger.Info("Second-stage tests already scheduled for this HEAD, skipping duplicate LGTM trigger")
			return
		}

		logger.Info("Sending comment for LGTM label addition")
		if err := sendComment(presubmits, prowJob, cw.ghc, func() { cw.ids.Delete(key) }, cw.pjLister); err != nil {
			logger.WithError(err).Error("failed to send a comment")
		} else {
			logger.Info("Successfully sent comment for LGTM label addition")
		}
	}
}

func (cw *clientWrapper) handleIssueComment(l *logrus.Entry, event github.IssueCommentEvent) {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	logger := l.WithFields(logrus.Fields{
		"handler":    "handleIssueComment",
		"org":        event.Repo.Owner.Login,
		"repo":       event.Repo.Name,
		"issue":      event.Issue.Number,
		"comment_id": event.Comment.ID,
	})

	// Only handle issue comments on PRs
	if !event.Issue.IsPullRequest() {
		return
	}

	// Check if the comment contains "/pipeline required", "/pipeline remaining", or
	// "/pipeline auto" as a command (at start of line).
	// Use (?m) for multiline mode so ^ matches start of any line, not just start of string
	pipelineRequiredRegex := regexp.MustCompile(`(?im)^/pipeline\s+required`)
	pipelineRemainingRegex := regexp.MustCompile(`(?im)^/pipeline\s+remaining`)
	pipelineAutoRegex := regexp.MustCompile(`(?im)^/pipeline\s+auto`)

	matchesRequired := pipelineRequiredRegex.MatchString(event.Comment.Body)
	matchesRemaining := pipelineRemainingRegex.MatchString(event.Comment.Body)
	matchesAuto := pipelineAutoRegex.MatchString(event.Comment.Body)

	if !matchesRequired && !matchesRemaining && !matchesAuto {
		return
	}

	org := event.Repo.Owner.Login
	repo := event.Repo.Name
	number := event.Issue.Number

	logger = logger.WithFields(logrus.Fields{
		"org":  org,
		"repo": repo,
		"pr":   number,
	})

	switch {
	case matchesAuto:
		logger.Info("Processing /pipeline auto comment")
	case matchesRemaining && !matchesRequired:
		logger.Info("Processing /pipeline remaining comment")
	default:
		logger.Info("Processing /pipeline required comment")
	}

	// Get presubmits for this repo
	logger.Debug("Getting presubmits from config data provider")
	presubmits := cw.configDataProvider.GetPresubmits(org + "/" + repo)

	// Check if there are any pipeline-controlled jobs
	if len(presubmits.protected) == 0 && len(presubmits.alwaysRequired) == 0 &&
		len(presubmits.conditionallyRequired) == 0 && len(presubmits.pipelineConditionallyRequired) == 0 &&
		len(presubmits.pipelineSkipOnlyRequired) == 0 {
		return
	}

	// Check if repo is in configuration (either manual/auto mode or LGTM mode)
	currentCfg := cw.watcher.getConfig()
	repos, orgExists := currentCfg[org]
	_, repoExists := repos[repo]

	lgtmCfg := cw.lgtmWatcher.getConfig()
	lgtmRepos, lgtmOrgExists := lgtmCfg[org]
	_, lgtmRepoExists := lgtmRepos[repo]

	if (!orgExists || !repoExists) && (!lgtmOrgExists || !lgtmRepoExists) {
		if err := cw.ghc.CreateComment(org, repo, number, RepoNotConfiguredMessage); err != nil {
			logger.WithError(err).Error("failed to create comment")
		}
		return
	}

	// Fetch PR details
	pr, err := cw.ghc.GetPullRequest(org, repo, number)
	if err != nil {
		logger.WithError(err).Error("failed to get PR details")
		return
	}

	// Check if branch is enabled for this repo
	baseBranch := pr.Base.Ref
	var branchEnabled bool
	if orgExists && repoExists {
		repoConfig := repos[repo]
		branchEnabled = isBranchEnabled(repoConfig.Branches, baseBranch)
	} else if lgtmOrgExists && lgtmRepoExists {
		lgtmRepoConfig := lgtmRepos[repo]
		branchEnabled = isBranchEnabled(lgtmRepoConfig.Branches, baseBranch)
	}

	if !branchEnabled {
		logger.WithField("base_branch", baseBranch).Debug("Branch not enabled for pipeline controller, skipping")
		return
	}

	// If this is a /pipeline auto command, add the pipeline-auto label and return
	// The reconciler will trigger second-stage tests when first-stage tests pass
	if matchesAuto {
		// /pipeline auto is only available for LGTM-configured repos
		if !lgtmOrgExists || !lgtmRepoExists {
			comment := "The `/pipeline auto` command is only available for LGTM-mode repositories. " +
				"For repositories in automatic mode, second-stage tests are already triggered automatically."
			if err := cw.ghc.CreateComment(org, repo, number, comment); err != nil {
				logger.WithError(err).Error("failed to create comment")
			}
			return
		}

		if err := cw.ghc.AddLabel(org, repo, number, PipelineAutoLabel); err != nil {
			logger.WithError(err).Error("failed to add pipeline-auto label")
			return
		}
		logger.Info("Successfully added pipeline-auto label")
		// Cache that this PR has pipeline-auto label
		if cw.pipelineAutoCache != nil {
			cw.pipelineAutoCache.Set(org, repo, number)
		}
		comment := "**Pipeline controller notification**\n\nThe `pipeline-auto` label has been added to this PR. Second-stage tests will be triggered automatically when all first-stage tests pass."
		if err := cw.ghc.CreateComment(org, repo, number, comment); err != nil {
			logger.WithError(err).Error("failed to create confirmation comment")
		}

		// Check if first-stage tests have already completed for the current SHA.
		// The reconciler is event-driven and only fires on ProwJob updates, so if
		// all first-stage tests completed before the label was added, no future
		// event will trigger second-stage tests. In that case, fall through to
		// trigger second-stage tests immediately.
		firstStageComplete := false
		if cw.pjLister != nil && pr.Head.SHA != "" {
			prowJob := &v1.ProwJob{
				Spec: v1.ProwJobSpec{
					Refs: &v1.Refs{
						Org:     org,
						Repo:    repo,
						BaseRef: pr.Base.Ref,
						Pulls: []v1.Pull{
							{Number: number, SHA: pr.Head.SHA},
						},
					},
				},
			}
			var err error
			firstStageComplete, err = checkFirstStageComplete(context.Background(), cw.pjLister, prowJob, presubmits)
			if err != nil {
				logger.WithError(err).Error("failed to check first-stage test status")
			}
		}
		if !firstStageComplete {
			logger.Info("First-stage tests not yet complete, reconciler will trigger second-stage when ready")
			return
		}
		logger.Info("All first-stage tests already passed, triggering second-stage tests immediately")
	}

	// For /pipeline required, /pipeline remaining, or /pipeline auto (when the
	// first stage already passed), trigger tests immediately. Create a ProwJob to
	// reuse existing logic.
	prowJob := &v1.ProwJob{
		Spec: v1.ProwJobSpec{
			Refs: &v1.Refs{
				Org:     org,
				Repo:    repo,
				BaseRef: pr.Base.Ref,
				Pulls: []v1.Pull{
					{Number: number, SHA: pr.Head.SHA},
				},
			},
		},
	}

	// /pipeline required force-schedules the whole required set (re-firing jobs
	// already running at HEAD). /pipeline remaining and the /pipeline auto
	// immediate-trigger fallthrough behave like the reconciler's automatic path:
	// they run the delta planner under a per-SHA idempotency guard (neither runs
	// under the reconciler's own ids cache).
	mode := modeForce
	deleteIds := func() {}
	if !matchesRequired && (matchesRemaining || matchesAuto) {
		mode = modeDelta
		// Per-SHA idempotency guard: guard against repeated comments/events
		// double-posting /test within the ProwJob cache-latency window.
		key := composeKey(prowJob.Spec.Refs)
		if _, loaded := cw.ids.LoadOrStore(key, time.Now()); loaded {
			logger.Info("Second-stage tests already scheduled for this HEAD, skipping duplicate")
			// /pipeline remaining is an explicit user command; tell the user the
			// guard suppressed it instead of returning silently. The /pipeline auto
			// fallthrough is event-driven and stays silent.
			if matchesRemaining {
				msg := fmt.Sprintf("**Pipeline controller notification**\n\nThe second stage has already been scheduled for this HEAD (`%s`); nothing further to trigger.", prowJob.Spec.Refs.Pulls[0].SHA)
				if err := cw.ghc.CreateComment(org, repo, number, msg); err != nil {
					logger.WithError(err).Error("failed to create comment")
				}
			}
			return
		}
		deleteIds = func() { cw.ids.Delete(key) }
	}

	// Generate the comment with test/override commands. explicit=true: a human
	// /pipeline command always gets a response, even when nothing needs scheduling.
	if err := sendCommentWithMode(presubmits, prowJob, cw.ghc, deleteIds, cw.pjLister, mode, true); err != nil {
		logger.WithError(err).Error("failed to send comment in response to pipeline command")
	}
}

// handlePipelineContextCreation handles PR events (open, push, reopen) and creates contexts for matching tests
func (cw *clientWrapper) handlePipelineContextCreation(l *logrus.Entry, event github.PullRequestEvent) {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	logger := l.WithFields(logrus.Fields{
		"handler": "handlePipelineContextCreation",
		"action":  event.Action,
		"org":     event.Repo.Owner.Login,
		"repo":    event.Repo.Name,
		"pr":      event.PullRequest.Number,
	})

	if event.Action != github.PullRequestActionOpened &&
		event.Action != github.PullRequestActionSynchronize &&
		event.Action != github.PullRequestActionReopened {
		return
	}

	org := event.Repo.Owner.Login
	repo := event.Repo.Name
	number := event.PullRequest.Number
	sha := event.PullRequest.Head.SHA

	presubmits := cw.configDataProvider.GetPresubmits(org + "/" + repo)

	if len(presubmits.pipelineConditionallyRequired) == 0 && len(presubmits.pipelineSkipOnlyRequired) == 0 &&
		len(presubmits.protected) == 0 {
		return
	}

	// Check if repo is in configuration (either manual/auto mode or LGTM mode)
	currentCfg := cw.watcher.getConfig()
	repos, orgExists := currentCfg[org]
	_, repoExists := repos[repo]

	lgtmCfg := cw.lgtmWatcher.getConfig()
	lgtmRepos, lgtmOrgExists := lgtmCfg[org]
	_, lgtmRepoExists := lgtmRepos[repo]

	isInConfig := (orgExists && repoExists) || (lgtmOrgExists && lgtmRepoExists)

	if !isInConfig {
		return
	}

	// Check if branch is enabled for this repo
	baseBranch := event.PullRequest.Base.Ref
	var branchEnabled bool
	if orgExists && repoExists {
		repoConfig := repos[repo]
		branchEnabled = isBranchEnabled(repoConfig.Branches, baseBranch)
	} else if lgtmOrgExists && lgtmRepoExists {
		lgtmRepoConfig := lgtmRepos[repo]
		branchEnabled = isBranchEnabled(lgtmRepoConfig.Branches, baseBranch)
	}

	if !branchEnabled {
		logger.WithField("base_branch", baseBranch).Debug("Branch not enabled for pipeline controller, skipping")
		return
	}

	logger = logger.WithFields(logrus.Fields{
		"org":  org,
		"repo": repo,
		"pr":   number,
		"sha":  sha,
	})

	// Get changed files for this PR
	changedFiles, err := cw.ghc.GetPullRequestChanges(org, repo, number)
	if err != nil {
		logger.WithError(err).Error("failed to get PR changes")
		return
	}

	filenames := make([]string, 0, len(changedFiles))
	for _, change := range changedFiles {
		filenames = append(filenames, change.Filename)
	}

	// Filter tests by branch - only process tests that match the target branch
	repoBaseRef := repo + "-" + baseBranch

	// Evaluate pipeline_run_if_changed tests
	for _, presubmit := range presubmits.pipelineConditionallyRequired {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if pattern, ok := presubmit.Annotations["pipeline_run_if_changed"]; ok && pattern != "" {
			if shouldRun, err := matchesPattern(pattern, filenames); err != nil {
				logger.WithError(err).WithField("test", presubmit.Name).WithField("pattern", pattern).Error("failed to evaluate pattern")
				continue
			} else if shouldRun {
				if err := cw.createContext(org, repo, sha, presubmit.Context, "pending", PipelinePendingMessage); err != nil {
					logger.WithError(err).WithField("test", presubmit.Name).Error("failed to create context")
				} else {
					logger.WithField("test", presubmit.Name).Info("created pending context for pipeline test")
				}
			}
		}
	}

	// Evaluate pipeline_skip_if_only_changed tests
	for _, presubmit := range presubmits.pipelineSkipOnlyRequired {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if pattern, ok := presubmit.Annotations["pipeline_skip_if_only_changed"]; ok && pattern != "" {
			if shouldSkip, err := allFilesMatchPattern(pattern, filenames); err != nil {
				logger.WithError(err).WithField("test", presubmit.Name).WithField("pattern", pattern).Error("failed to evaluate skip pattern")
				continue
			} else if !shouldSkip {
				// If not all files match the skip pattern, we should run the test
				if err := cw.createContext(org, repo, sha, presubmit.Context, "pending", PipelinePendingMessage); err != nil {
					logger.WithError(err).WithField("test", presubmit.Name).Error("failed to create context")
				} else {
					logger.WithField("test", presubmit.Name).Info("created pending context for pipeline test")
				}
			}
		}
	}

	// Create contexts for protected jobs (always_run: false, optional: false, no run conditions)
	for _, presubmit := range presubmits.protected {
		if !strings.Contains(presubmit.Name, repoBaseRef) {
			continue
		}
		if err := cw.createContext(org, repo, sha, presubmit.Context, "pending", PipelinePendingMessage); err != nil {
			logger.WithError(err).WithField("test", presubmit.Name).Error("failed to create context")
		} else {
			logger.WithField("test", presubmit.Name).Info("created pending context for protected test")
		}
	}
}

// createContext creates a GitHub status context
func (cw *clientWrapper) createContext(org, repo, sha, context, state, description string) error {
	return cw.ghc.CreateStatus(org, repo, sha, github.Status{
		Context:     context,
		State:       state,
		Description: description,
	})
}

// matchesPattern checks if any of the filenames match the given regex pattern
func matchesPattern(pattern string, filenames []string) (bool, error) {
	if pattern == "" {
		return false, nil
	}

	regex, err := regexp.Compile(pattern)
	if err != nil {
		return false, fmt.Errorf("invalid regex pattern %q: %w", pattern, err)
	}

	for _, filename := range filenames {
		if regex.MatchString(filename) {
			return true, nil
		}
	}

	return false, nil
}

// allFilesMatchPattern checks if ALL filenames match the given regex pattern
func allFilesMatchPattern(pattern string, filenames []string) (bool, error) {
	if pattern == "" {
		return false, nil
	}

	if len(filenames) == 0 {
		return false, nil
	}

	regex, err := regexp.Compile(pattern)
	if err != nil {
		return false, fmt.Errorf("invalid regex pattern %q: %w", pattern, err)
	}

	for _, filename := range filenames {
		if !regex.MatchString(filename) {
			return false, nil
		}
	}

	return true, nil
}

func main() {
	logrusutil.ComponentInit()
	logger := logrus.WithField("component", "pipeline-controller")
	ctrlruntimelog.SetLogger(logrusr.New(logger))

	o := parseOptions()

	configAgent, err := o.config.ConfigAgent()
	if err != nil {
		logger.WithError(err).Fatal("error starting config agent")
	}
	cfg := configAgent.Config

	restCfg, err := o.client.InfrastructureClusterConfig(o.dryrun)
	if err != nil {
		logger.WithError(err).Fatal("failed to get kubeconfig")
	}
	mgr, err := manager.New(restCfg, manager.Options{
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				cfg().ProwJobNamespace: {},
			},
		},
		Metrics: server.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		logger.WithError(err).Fatal("failed to create manager")
	}

	if err := o.client.AddKubeconfigChangeCallback(func() {
		logger.Info("kubeconfig changed, exiting to trigger a restart")
		interrupts.Terminate()
	}); err != nil {
		logger.WithError(err).Fatal("failed to register kubeconfig callback")
	}

	githubClient, err := o.github.GitHubClient(o.dryrun)
	if err != nil {
		logger.WithError(err).Fatal("error getting GitHub client")
	}

	watcher := newWatcher(o.configFile, logger)
	go watcher.watch()

	lgtmWatcher := newWatcher(o.lgtmConfigFile, logger)
	go lgtmWatcher.watch()

	// Create a function that returns repos from both config and lgtm config
	repoLister := func() []string {
		var repos []string

		// Get repos from main config
		mainConfig := watcher.getConfig()
		for org, repoConfigs := range mainConfig {
			for repo := range repoConfigs {
				repos = append(repos, org+"/"+repo)
			}
		}

		// Get repos from lgtm config
		lgtmConfig := lgtmWatcher.getConfig()
		for org, repoConfigs := range lgtmConfig {
			for repo := range repoConfigs {
				orgRepo := org + "/" + repo
				// Avoid duplicates
				found := false
				for _, existing := range repos {
					if existing == orgRepo {
						found = true
						break
					}
				}
				if !found {
					repos = append(repos, orgRepo)
				}
			}
		}

		// If no repos found, retry once after a short delay
		if len(repos) == 0 {
			time.Sleep(100 * time.Millisecond)

			// Retry getting configs
			mainConfig = watcher.getConfig()
			lgtmConfig = lgtmWatcher.getConfig()

			for org, repoConfigs := range mainConfig {
				for repo := range repoConfigs {
					repos = append(repos, org+"/"+repo)
				}
			}

			for org, repoConfigs := range lgtmConfig {
				for repo := range repoConfigs {
					orgRepo := org + "/" + repo
					found := false
					for _, existing := range repos {
						if existing == orgRepo {
							found = true
							break
						}
					}
					if !found {
						repos = append(repos, orgRepo)
					}
				}
			}
		}

		return repos
	}

	configDataProvider := NewConfigDataProvider(cfg, repoLister, logger.WithField("component", "config-data-provider"))
	go configDataProvider.Run()

	// Wait for config data provider to be ready
	logger.Info("Waiting for config data provider to be ready...")
	time.Sleep(2 * time.Second) // Give it time to load initial data
	logger.Info("Config data provider should be ready")

	pipelineAutoCache := NewPipelineAutoCache()
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			pipelineAutoCache.CleanExpired()
		}
	}()

	reconciler, err := NewReconciler(mgr, configDataProvider, githubClient, logger, watcher, lgtmWatcher, pipelineAutoCache)
	if err != nil {
		logger.WithError(err).Fatal("failed to construct github reporter controller")
	}
	go reconciler.cleanOldIds(24 * time.Hour)

	if err = secret.Add(o.webhookSecretFile); err != nil {
		logger.WithError(err).Fatal("error starting secrets agent")
	}
	webhookTokenGenerator := secret.GetTokenGenerator(o.webhookSecretFile)

	cw := &clientWrapper{
		ghc:                githubClient,
		configDataProvider: configDataProvider,
		watcher:            watcher,
		lgtmWatcher:        lgtmWatcher,
		pjLister:           mgr.GetCache(),
		pipelineAutoCache:  pipelineAutoCache,
	}

	// Evict stale per-SHA idempotency keys from the LGTM / /pipeline remaining
	// guard, mirroring the reconciler's ids cleanup.
	go cw.cleanOldIds(24 * time.Hour)

	eventServer := githubeventserver.New(o.githubEventServerOptions, webhookTokenGenerator, logger)

	// Register event handlers with proper logging
	logger.Info("Registering event handlers")
	eventServer.RegisterHandlePullRequestEvent(cw.handlePullRequestCreation)
	eventServer.RegisterHandlePullRequestEvent(cw.handleLabelAddition)
	eventServer.RegisterHandlePullRequestEvent(cw.handlePipelineContextCreation)
	eventServer.RegisterHandleIssueCommentEvent(cw.handleIssueComment)

	logger.Info("All event handlers registered successfully")

	interrupts.OnInterrupt(func() {
		eventServer.GracefulShutdown()
	})

	interrupts.ListenAndServe(eventServer, time.Second*30)
	interrupts.Run(func(ctx context.Context) {
		if err := mgr.Start(ctx); err != nil {
			logger.WithError(err).Fatal("controller manager exited with error")
		}
	})
	interrupts.WaitForGracefulShutdown()
}
