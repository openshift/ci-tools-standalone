package main

import (
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"sigs.k8s.io/prow/pkg/config"
)

// RepoLister is a function that returns a list of "org/repo" strings that should be processed
type RepoLister func() []string

type presubmitTests struct {
	protected                     []config.Presubmit
	alwaysRequired                []config.Presubmit
	conditionallyRequired         []config.Presubmit
	pipelineConditionallyRequired []config.Presubmit
	pipelineSkipOnlyRequired      []config.Presubmit
	// statusContextOwners maps case-folded contexts to sole static presubmit
	// owners. Empty values mark ambiguous ownership; nil means unknown inventory.
	statusContextOwners map[string]string
}

type ConfigDataProvider struct {
	configGetter      config.Getter
	repoLister        RepoLister
	updatedPresubmits map[string]presubmitTests
	logger            *logrus.Entry
	m                 sync.Mutex
}

func NewConfigDataProvider(configGetter config.Getter, repoLister RepoLister, logger *logrus.Entry) *ConfigDataProvider {
	provider := &ConfigDataProvider{
		configGetter:      configGetter,
		repoLister:        repoLister,
		updatedPresubmits: make(map[string]presubmitTests),
		logger:            logger,
		m:                 sync.Mutex{},
	}
	// Initialize with first load
	provider.gatherData()
	return provider
}

func (c *ConfigDataProvider) GetPresubmits(orgRepo string) presubmitTests {
	c.m.Lock()
	defer c.m.Unlock()
	if presubmits, ok := c.updatedPresubmits[orgRepo]; ok {
		return presubmits
	}
	return presubmitTests{}
}

func (c *ConfigDataProvider) Run() {
	for {
		time.Sleep(10 * time.Minute)
		// Always refresh job data to pick up added/removed tests
		c.gatherData()
	}
}

func (c *ConfigDataProvider) gatherData() {
	// Get the list of repositories from the pipeline controller configs (config + lgtm config)
	orgRepos := c.repoLister()
	c.gatherDataForRepos(orgRepos)
}

// gatherDataForRepos processes the given list of repositories and updates the presubmits data
func (c *ConfigDataProvider) gatherDataForRepos(orgRepos []string) {
	cfg := c.configGetter()

	updatedPresubmits := make(map[string]presubmitTests)
	for _, orgRepo := range orgRepos {
		// Skip if we've already processed this repo (avoid duplicates)
		if _, exists := updatedPresubmits[orgRepo]; exists {
			continue
		}
		presubmits := cfg.GetPresubmitsStatic(orgRepo)

		// Include reportable definitions outside the scheduling buckets too.
		// Duplicate and postsubmit contexts are ambiguous; in-repo config
		// makes this static inventory incomplete.
		var statusContextOwners map[string]string
		if !cfg.InRepoConfigEnabled(orgRepo) {
			for _, p := range presubmits {
				if p.SkipReport || p.Context == "" {
					continue
				}
				if statusContextOwners == nil {
					statusContextOwners = map[string]string{}
				}
				contextKey := strings.ToLower(p.Context)
				if _, claimed := statusContextOwners[contextKey]; claimed {
					statusContextOwners[contextKey] = "" // a second definition makes ownership ambiguous
				} else {
					statusContextOwners[contextKey] = p.Name
				}
			}
			for _, p := range cfg.GetPostsubmitsStatic(orgRepo) {
				if p.SkipReport || p.Context == "" {
					continue
				}
				contextKey := strings.ToLower(p.Context)
				if _, claimed := statusContextOwners[contextKey]; claimed {
					statusContextOwners[contextKey] = "" // postsubmits report the same contexts on base branches
				}
			}
		}

		for _, p := range presubmits {
			if !p.AlwaysRun && p.RunIfChanged == "" && p.SkipIfOnlyChanged == "" {
				if val, ok := p.Annotations["pipeline_run_if_changed"]; ok && val != "" {
					pre := updatedPresubmits[orgRepo]
					pre.pipelineConditionallyRequired = append(pre.pipelineConditionallyRequired, p)
					updatedPresubmits[orgRepo] = pre
					continue
				}
				// Also check for pipeline_skip_if_only_changed annotation
				if val, ok := p.Annotations["pipeline_skip_if_only_changed"]; ok && val != "" {
					pre := updatedPresubmits[orgRepo]
					pre.pipelineSkipOnlyRequired = append(pre.pipelineSkipOnlyRequired, p)
					updatedPresubmits[orgRepo] = pre
					continue
				}
				// Only categorize as protected if it doesn't have pipeline annotations
				if !p.Optional {
					if _, hasPipelineRun := p.Annotations["pipeline_run_if_changed"]; !hasPipelineRun {
						if _, hasPipelineSkip := p.Annotations["pipeline_skip_if_only_changed"]; !hasPipelineSkip {
							pre := updatedPresubmits[orgRepo]
							pre.protected = append(pre.protected, p)
							updatedPresubmits[orgRepo] = pre
						}
					}
				}
			}
			if !p.Optional && p.AlwaysRun {
				pre := updatedPresubmits[orgRepo]
				pre.alwaysRequired = append(pre.alwaysRequired, p)
				updatedPresubmits[orgRepo] = pre
				continue
			}
			if !p.Optional && (p.RunIfChanged != "" || p.SkipIfOnlyChanged != "") {
				pre := updatedPresubmits[orgRepo]
				pre.conditionallyRequired = append(pre.conditionallyRequired, p)
				updatedPresubmits[orgRepo] = pre
				continue
			}
		}

		// Keep the inventory next to the buckets; nil stays nil so an unknown
		// inventory stays distinguishable from a repo without reportable jobs.
		if statusContextOwners != nil {
			pre := updatedPresubmits[orgRepo]
			pre.statusContextOwners = statusContextOwners
			updatedPresubmits[orgRepo] = pre
		}
	}

	c.m.Lock()
	defer c.m.Unlock()
	c.updatedPresubmits = updatedPresubmits
}
