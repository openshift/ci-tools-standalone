package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

const (
	retention = 24 * time.Hour
)

type pullRequest struct {
	closed    bool
	checkTime time.Time
}

type closedPRsCache struct {
	prs       map[string]pullRequest
	m         sync.Mutex
	ghc       minimalGhClient
	clearTime time.Time
}

func composePRIdentifier(refs *v1.Refs) string {
	return fmt.Sprintf("%s/%s/%d", refs.Org, refs.Repo, refs.Pulls[0].Number)
}

// isPRClosed quieries either github or short-term cache to determine if PR is closed. Draft PRs are
// also quialified as closed due to potential, unexpected side effects
func (c *closedPRsCache) isPRClosed(refs *v1.Refs) (bool, error) {
	id := composePRIdentifier(refs)
	c.m.Lock()
	defer c.m.Unlock()
	c.clearCache()
	pr, ok := c.prs[id]
	if ok && (time.Since(pr.checkTime) < 5*time.Minute || pr.closed) {
		return pr.closed, nil
	}
	ghPr, err := c.ghc.GetPullRequest(refs.Org, refs.Repo, refs.Pulls[0].Number)
	if err != nil {
		return false, fmt.Errorf("error getting pull request: %w", err)
	}
	c.prs[id] = pullRequest{closed: ghPr.State != github.PullRequestStateOpen || ghPr.Draft, checkTime: time.Now()}
	return ghPr.State != github.PullRequestStateOpen || ghPr.Draft, nil
}

func (c *closedPRsCache) clearCache() {
	if time.Since(c.clearTime) < retention {
		return
	}
	for k, v := range c.prs {
		if time.Since(v.checkTime) >= retention {
			delete(c.prs, k)
		}
	}
	c.clearTime = time.Now()
}

func composeKey(refs *v1.Refs) string {
	return fmt.Sprintf("%s/%s/%d/%s/%s", refs.Org, refs.Repo, refs.Pulls[0].Number, refs.BaseRef, refs.Pulls[0].SHA)
}

type reconciler struct {
	pjclientset        ctrlruntimeclient.Client
	lister             ctrlruntimeclient.Reader
	configDataProvider *ConfigDataProvider
	ghc                minimalGhClient
	closedPRsCache     closedPRsCache
	ids                sync.Map
	logger             *logrus.Entry
	watcher            *watcher
	lgtmWatcher        *watcher
	pipelineAutoCache  *PipelineAutoCache
}

func NewReconciler(
	mgr manager.Manager,
	configDataProvider *ConfigDataProvider,
	ghc github.Client,
	logger *logrus.Entry,
	w *watcher,
	lgtmW *watcher,
	pipelineAutoCache *PipelineAutoCache,
) (*reconciler, error) {
	reconciler := &reconciler{
		pjclientset:        mgr.GetClient(),
		lister:             mgr.GetCache(),
		configDataProvider: configDataProvider,
		ghc:                ghc,
		ids:                sync.Map{},
		logger:             logger,
		watcher:            w,
		lgtmWatcher:        lgtmW,
		pipelineAutoCache:  pipelineAutoCache,
		closedPRsCache: closedPRsCache{
			prs:       map[string]pullRequest{},
			m:         sync.Mutex{},
			ghc:       ghc,
			clearTime: time.Now(),
		},
	}
	if err := builder.
		ControllerManagedBy(mgr).
		Named("pipeline-controller").
		For(&v1.ProwJob{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(reconciler); err != nil {
		return nil, fmt.Errorf("failed to construct controller: %w", err)
	}
	return reconciler, nil
}

func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := r.logger.WithField("key", req.String()).WithField("prowjob", req.Name)
	err := r.reconcile(ctx, req)
	if err != nil {
		log.WithError(err).Error("reconciliation failed")
	}
	return reconcile.Result{}, err
}

func (r *reconciler) cleanOldIds(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		r.ids.Range(func(key, value interface{}) bool {
			if time.Since(value.(time.Time)) >= interval {
				r.ids.Delete(key)
			}
			return true
		})
	}
}

func (r *reconciler) reconcile(ctx context.Context, req reconcile.Request) error {
	var pj v1.ProwJob
	if err := r.pjclientset.Get(ctx, req.NamespacedName, &pj); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("failed to get prowjob %s: %w", req.String(), err)
	}

	if pj.Spec.Refs == nil || pj.Spec.Type != v1.PresubmitJob {
		return nil
	}

	presubmits := r.configDataProvider.GetPresubmits(pj.Spec.Refs.Org + "/" + pj.Spec.Refs.Repo)
	if len(presubmits.protected) == 0 && len(presubmits.alwaysRequired) == 0 &&
		len(presubmits.conditionallyRequired) == 0 && len(presubmits.pipelineConditionallyRequired) == 0 {
		return nil
	}

	currentCfg := r.watcher.getConfig()
	repos, orgExists := currentCfg[pj.Spec.Refs.Org]
	repoConfig, repoExists := repos[pj.Spec.Refs.Repo]

	// Also check LGTM config for pipeline-auto label support
	lgtmCfg := r.lgtmWatcher.getConfig()
	lgtmRepos, lgtmOrgExists := lgtmCfg[pj.Spec.Refs.Org]
	lgtmRepoConfig, lgtmRepoExists := lgtmRepos[pj.Spec.Refs.Repo]

	isInMainConfig := orgExists && repoExists
	isInLgtmConfig := lgtmOrgExists && lgtmRepoExists

	if !isInMainConfig && !isInLgtmConfig {
		return nil
	}

	// Check if branch is enabled for this repo
	var branchEnabled bool
	if isInMainConfig {
		branchEnabled = isBranchEnabled(repoConfig.Branches, pj.Spec.Refs.BaseRef)
	} else if isInLgtmConfig {
		branchEnabled = isBranchEnabled(lgtmRepoConfig.Branches, pj.Spec.Refs.BaseRef)
	}

	if !branchEnabled {
		if r.logger != nil {
			log := r.logger.WithFields(logrus.Fields{
				"org":     pj.Spec.Refs.Org,
				"repo":    pj.Spec.Refs.Repo,
				"branch":  pj.Spec.Refs.BaseRef,
				"prowjob": pj.Name,
			})
			log.Debug("Branch not enabled for pipeline controller, skipping reconcile")
		}
		return nil
	}

	// Determine if we should proceed with automatic triggering
	// Case 1: Repo is in main config with trigger mode "auto"
	// Case 2: Repo is in LGTM config and has the pipeline-auto label
	shouldProceedAuto := false

	if isInMainConfig && repoConfig.Trigger == "auto" {
		shouldProceedAuto = true
	} else if isInLgtmConfig && len(pj.Spec.Refs.Pulls) > 0 {
		// Check if PR has the pipeline-auto label
		// First check cache (populated when /pipeline auto is used)
		prNumber := pj.Spec.Refs.Pulls[0].Number
		if r.pipelineAutoCache != nil && r.pipelineAutoCache.Has(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, prNumber) {
			shouldProceedAuto = true
		} else {
			// Cache miss - query GitHub API
			labels, err := r.ghc.GetIssueLabels(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, prNumber)
			if err != nil {
				if r.logger != nil {
					r.logger.WithError(err).WithField("prowjob", pj.Name).Debug("Failed to get PR labels, skipping")
				}
				return nil
			}
			for _, label := range labels {
				if label.Name == PipelineAutoLabel {
					shouldProceedAuto = true
					// Add to cache for future lookups
					if r.pipelineAutoCache != nil {
						r.pipelineAutoCache.Set(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, prNumber)
					}
					break
				}
			}
		}
	}

	if !shouldProceedAuto {
		return nil
	}

	labelsPresent, err := requiredLabelsPresent(r.ghc, pj.Spec.Refs, repoConfig.RequiredLabels)
	if err != nil {
		if r.logger != nil {
			r.logger.WithError(err).WithField("prowjob", pj.Name).Debug("Failed to evaluate required labels, skipping")
		}
		return nil
	}
	if !labelsPresent {
		if r.logger != nil {
			r.logger.WithFields(logrus.Fields{
				"prowjob":         pj.Name,
				"required_labels": repoConfig.RequiredLabels,
			}).Debug("Required labels are absent, skipping second-stage scheduling")
		}
		return nil
	}

	status, err := r.reportSuccessOnPR(ctx, &pj, presubmits)
	if err != nil || !status {
		return err
	}

	return sendComment(presubmits, &pj, r.ghc, func() { r.ids.Delete(composeKey(pj.Spec.Refs)) }, r.lister)
}

func (r *reconciler) reportSuccessOnPR(ctx context.Context, pj *v1.ProwJob, presubmits presubmitTests) (bool, error) {
	complete, err := checkFirstStageComplete(ctx, r.lister, pj, presubmits)
	if err != nil || !complete {
		return false, err
	}

	if closed, err := r.closedPRsCache.isPRClosed(pj.Spec.Refs); err != nil || closed {
		return false, err
	}

	if _, loaded := r.ids.LoadOrStore(composeKey(pj.Spec.Refs), time.Now()); loaded {
		return false, nil
	}
	return true, nil
}
