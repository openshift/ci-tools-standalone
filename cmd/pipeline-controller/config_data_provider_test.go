package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sirupsen/logrus"

	"sigs.k8s.io/prow/pkg/config"
)

// TestGatherDataStatusContextOwners covers the context-ownership inventory the
// commit-status fallbacks rely on: every reportable static presubmit
// definition claims its context (lower-cased, because GitHub compares status
// contexts case-insensitively; optional definitions outside the buckets
// included), any second claimant of a context blanks the entry, postsubmits
// count as claimants because Prow reports them with the same contexts on base
// branches, and repos with in-repo config enabled keep a nil inventory since
// their full definition set is unknown.
func TestGatherDataStatusContextOwners(t *testing.T) {
	const orgRepo = "org/repo"

	unit := config.Presubmit{JobBase: config.JobBase{Name: "pull-ci-org-repo-main-unit"},
		Brancher: config.Brancher{Branches: []string{"^main$"}}, AlwaysRun: true, Reporter: config.Reporter{Context: "ci/unit"}}
	e2e := config.Presubmit{JobBase: config.JobBase{Name: "pull-ci-org-repo-main-e2e"},
		Brancher: config.Brancher{Branches: []string{"^main$"}}, Reporter: config.Reporter{Context: "ci/e2e"}, RerunCommand: "/test e2e"}
	optionalLint := config.Presubmit{JobBase: config.JobBase{Name: "pull-ci-org-repo-main-lint"}, Optional: true,
		Brancher: config.Brancher{Branches: []string{"^main$"}}, Reporter: config.Reporter{Context: "ci/lint"}}
	// The same job name repeated on a disjoint branch is a distinct
	// definition: names do not deduplicate, definitions do.
	unitOnRelease := config.Presubmit{JobBase: config.JobBase{Name: "pull-ci-org-repo-main-unit"},
		Brancher: config.Brancher{Branches: []string{"^release$"}}, AlwaysRun: true, Reporter: config.Reporter{Context: "ci/unit"}}
	optionalCaseAliasedUnit := config.Presubmit{JobBase: config.JobBase{Name: "pull-ci-org-repo-release-unit"}, Optional: true,
		Brancher: config.Brancher{Branches: []string{"^release$"}}, Reporter: config.Reporter{Context: "CI/UNIT"}}
	postsubmitUnit := config.Postsubmit{JobBase: config.JobBase{Name: "post-ci-org-repo-main"},
		Reporter: config.Reporter{Context: "ci/unit"}}

	tests := []struct {
		name        string
		presubmits  []config.Presubmit
		postsubmits []config.Postsubmit
		inRepo      bool
		want        map[string]string
	}{
		{
			name:       "unique reportable owners include optional definitions outside the buckets",
			presubmits: []config.Presubmit{unit, e2e, optionalLint},
			want:       map[string]string{"ci/unit": unit.Name, "ci/e2e": e2e.Name, "ci/lint": optionalLint.Name},
		},
		{
			name:       "same name on a disjoint branch still blanks the owner",
			presubmits: []config.Presubmit{unit, unitOnRelease},
			want:       map[string]string{"ci/unit": ""},
		},
		{
			name:       "case-aliased optional context shadows the owner",
			presubmits: []config.Presubmit{unit, optionalCaseAliasedUnit},
			want:       map[string]string{"ci/unit": ""},
		},
		{
			name:        "postsubmit sharing the context blanks the owner",
			presubmits:  []config.Presubmit{unit, e2e},
			postsubmits: []config.Postsubmit{postsubmitUnit},
			want:        map[string]string{"ci/unit": "", "ci/e2e": e2e.Name},
		},
		{
			name:       "in-repo config enabled keeps the inventory unknown",
			presubmits: []config.Presubmit{unit, e2e},
			inRepo:     true,
		},
		{
			name: "unreportable definitions leave no inventory",
			presubmits: []config.Presubmit{
				{JobBase: config.JobBase{Name: "quiet"}, Reporter: config.Reporter{SkipReport: true, Context: "ci/quiet"}},
				{JobBase: config.JobBase{Name: "contextless"}, Reporter: config.Reporter{}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			if err := cfg.SetPresubmits(map[string][]config.Presubmit{orgRepo: tc.presubmits}); err != nil {
				t.Fatalf("failed to set presubmits: %v", err)
			}
			if len(tc.postsubmits) > 0 {
				if err := cfg.SetPostsubmits(map[string][]config.Postsubmit{orgRepo: tc.postsubmits}); err != nil {
					t.Fatalf("failed to set postsubmits: %v", err)
				}
			}
			if tc.inRepo {
				enabled := true
				cfg.InRepoConfig.Enabled = map[string]*bool{orgRepo: &enabled}
			}
			logger := logrus.NewEntry(logrus.New())
			provider := NewConfigDataProvider(func() *config.Config { return cfg }, func() []string { return []string{orgRepo} }, logger)

			if diff := cmp.Diff(tc.want, provider.GetPresubmits(orgRepo).statusContextOwners); diff != "" {
				t.Errorf("unexpected status context owners (-want +got):\n%s", diff)
			}
		})
	}
}
