package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"

	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeGhClient implements minimalGhClient for testing.
type fakeGhClient struct {
	comments               []string
	changes                []github.PullRequestChange
	pullRequest            *github.PullRequest
	addedLabels            []string
	combinedStatus         *github.CombinedStatus
	getCombinedStatusErr   error
	getPullRequestCalls    int
	getCombinedStatusCalls int
	cancelStatusLookup     context.CancelFunc
}

func (f *fakeGhClient) GetPullRequest(org, repo string, number int) (*github.PullRequest, error) {
	f.getPullRequestCalls++
	if f.pullRequest != nil {
		return f.pullRequest, nil
	}
	return &github.PullRequest{}, nil
}

func (f *fakeGhClient) CreateComment(org, repo string, number int, comment string) error {
	f.comments = append(f.comments, comment)
	return nil
}

func (f *fakeGhClient) GetPullRequestChanges(org string, repo string, number int) ([]github.PullRequestChange, error) {
	return f.changes, nil
}

func (f *fakeGhClient) CreateStatus(org, repo, ref string, s github.Status) error {
	return nil
}

func (f *fakeGhClient) AddLabel(org, repo string, number int, label string) error {
	f.addedLabels = append(f.addedLabels, label)
	return nil
}

func (f *fakeGhClient) GetIssueLabels(org, repo string, number int) ([]github.Label, error) {
	return nil, nil
}

// GetCombinedStatus records fallback lookups and returns the configured result.
func (f *fakeGhClient) GetCombinedStatus(org, repo, ref string) (*github.CombinedStatus, error) {
	f.getCombinedStatusCalls++
	if f.cancelStatusLookup != nil {
		f.cancelStatusLookup()
	}
	if f.getCombinedStatusErr != nil {
		return nil, f.getCombinedStatusErr
	}
	if f.combinedStatus != nil {
		return f.combinedStatus, nil
	}
	// Default to the requested ref with no statuses, so a first dispatch with
	// nothing to prove keeps its normal behavior.
	return &github.CombinedStatus{SHA: ref}, nil
}

func newFakePJLister(pjs ...v1.ProwJob) ctrlruntimeclient.Reader {
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	objs := make([]ctrlruntimeclient.Object, 0, len(pjs))
	for i := range pjs {
		objs = append(objs, &pjs[i])
	}
	return fakectrlruntimeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

func makeProwJob(name string, sha string) v1.ProwJob {
	const (
		org      = "openshift"
		repo     = "myrepo"
		baseRef  = "main"
		prNumber = 42
	)
	return v1.ProwJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("pj-%s-%s", name, sha[:7]),
			Namespace: "default",
			Labels: map[string]string{
				"prow.k8s.io/type":          "presubmit",
				"prow.k8s.io/refs.org":      org,
				"prow.k8s.io/refs.repo":     repo,
				"prow.k8s.io/refs.pull":     fmt.Sprintf("%d", prNumber),
				"prow.k8s.io/refs.base_ref": baseRef,
			},
		},
		Spec: v1.ProwJobSpec{
			Job:  name,
			Type: v1.PresubmitJob,
			Refs: &v1.Refs{
				Org:     org,
				Repo:    repo,
				BaseRef: baseRef,
				Pulls: []v1.Pull{
					{Number: prNumber, SHA: sha},
				},
			},
		},
		Status: v1.ProwJobStatus{
			State: v1.SuccessState,
		},
	}
}

func makeTriggerPJ(sha string) *v1.ProwJob {
	return &v1.ProwJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "trigger-pj",
			Namespace: "default",
		},
		Spec: v1.ProwJobSpec{
			Job:  "trigger-job",
			Type: v1.PresubmitJob,
			Refs: &v1.Refs{
				Org:     "openshift",
				Repo:    "myrepo",
				BaseRef: "main",
				Pulls: []v1.Pull{
					{Number: 42, SHA: sha},
				},
			},
		},
	}
}

func TestSendCommentWithMode_ProtectedDedup(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	protectedJobName := fmt.Sprintf("pull-ci-%s-%s-%s-e2e-test", org, repo, baseRef)
	repoBaseRef := repo + "-" + baseRef

	protectedPresubmit := config.Presubmit{
		JobBase: config.JobBase{
			Name: protectedJobName,
		},
		Reporter: config.Reporter{
			Context: protectedJobName,
		},
		RerunCommand: "/test " + protectedJobName,
	}

	// Verify our test job name contains repoBaseRef (required for the filter)
	if !strings.Contains(protectedJobName, repoBaseRef) {
		t.Fatalf("test setup error: job name %q does not contain %q", protectedJobName, repoBaseRef)
	}

	tests := []struct {
		name        string
		mode        scheduleMode
		existingPJs []v1.ProwJob
		wantComment string // substring to check for
		wantNoRerun bool   // true if we expect the RerunCommand NOT to appear
	}{
		{
			name:        "protected tests triggered normally when no existing ProwJob",
			mode:        modeDelta,
			existingPJs: nil,
			wantComment: "Scheduling required tests:",
			wantNoRerun: false,
		},
		{
			name: "protected tests NOT re-triggered when ProwJob exists at same SHA (dedup)",
			mode: modeDelta,
			existingPJs: []v1.ProwJob{
				makeProwJob(protectedJobName, sha),
			},
			wantComment: "already been triggered",
			wantNoRerun: true,
		},
		{
			name: "protected tests ARE re-triggered with force command even with existing ProwJob",
			mode: modeForce,
			existingPJs: []v1.ProwJob{
				makeProwJob(protectedJobName, sha),
			},
			wantComment: "Scheduling required tests:",
			wantNoRerun: false,
		},
		{
			name: "protected tests triggered when ProwJob exists at different SHA",
			mode: modeDelta,
			existingPJs: []v1.ProwJob{
				makeProwJob(protectedJobName, "different_sha_123"),
			},
			wantComment: "Scheduling required tests:",
			wantNoRerun: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ghc := &fakeGhClient{}
			pjLister := newFakePJLister(tc.existingPJs...)

			presubmits := presubmitTests{
				protected: []config.Presubmit{protectedPresubmit},
			}

			pj := makeTriggerPJ(sha)

			deleteIdsCalled := false
			deleteIds := func() { deleteIdsCalled = true }

			err := sendCommentWithMode(presubmits, pj, ghc, deleteIds, pjLister, tc.mode, true, nil)
			if err != nil {
				t.Fatalf("sendCommentWithMode returned error: %v", err)
			}

			if len(ghc.comments) != 1 {
				t.Fatalf("expected 1 comment, got %d", len(ghc.comments))
			}
			comment := ghc.comments[0]

			if !strings.Contains(comment, tc.wantComment) {
				t.Errorf("comment %q does not contain expected substring %q", comment, tc.wantComment)
			}

			rerunPresent := strings.Contains(comment, protectedPresubmit.RerunCommand)
			if tc.wantNoRerun && rerunPresent {
				t.Errorf("expected RerunCommand NOT to appear in comment but it did: %q", comment)
			}
			if !tc.wantNoRerun && !rerunPresent {
				t.Errorf("expected RerunCommand to appear in comment but it did not: %q", comment)
			}

			// deleteIds should not be called on success
			if deleteIdsCalled {
				t.Errorf("deleteIds was called unexpectedly")
			}
		})
	}
}

// TestSendCommentWithMode_ConditionalDelta verifies the delta planner for the
// conditional (second-stage) jobs: modeDelta complements the jobs already
// present at HEAD instead of latching the whole pipeline into manual control,
// and modeForce re-fires everything. The old manual-control message is gone.
func TestSendCommentWithMode_ConditionalDelta(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	jobA := fmt.Sprintf("pull-ci-%s-%s-%s-conditional-a", org, repo, baseRef)
	jobB := fmt.Sprintf("pull-ci-%s-%s-%s-conditional-b", org, repo, baseRef)

	presubmitFor := func(name string) config.Presubmit {
		return config.Presubmit{
			JobBase: config.JobBase{
				Name:        name,
				Annotations: map[string]string{"pipeline_run_if_changed": "^cmd/"},
			},
			Reporter:     config.Reporter{Context: name},
			RerunCommand: "/test " + name,
		}
	}
	conditionalPresubmits := []config.Presubmit{presubmitFor(jobA), presubmitFor(jobB)}
	changes := []github.PullRequestChange{{Filename: "cmd/main.go"}}

	const manualControlMsg = "Tests from second stage were triggered manually"

	tests := []struct {
		name        string
		mode        scheduleMode
		existingPJs []v1.ProwJob
		wantRerun   []string // rerun commands that MUST appear
		wantNoRerun []string // rerun commands that must NOT appear
		wantComment string   // substring that must appear
	}{
		{
			name:        "no jobs exist yet schedules the full set",
			mode:        modeDelta,
			existingPJs: nil,
			wantRerun:   []string{"/test " + jobA, "/test " + jobB},
		},
		{
			name:        "one job exists schedules only the missing one (delta)",
			mode:        modeDelta,
			existingPJs: []v1.ProwJob{makeProwJob(jobA, sha)},
			wantRerun:   []string{"/test " + jobB},
			wantNoRerun: []string{"/test " + jobA},
		},
		{
			name:        "all jobs exist schedules nothing without a misleading comment",
			mode:        modeDelta,
			existingPJs: []v1.ProwJob{makeProwJob(jobA, sha), makeProwJob(jobB, sha)},
			wantNoRerun: []string{"/test " + jobA, "/test " + jobB},
			wantComment: "already been triggered",
		},
		{
			name:        "force reschedules every job even when present",
			mode:        modeForce,
			existingPJs: []v1.ProwJob{makeProwJob(jobA, sha), makeProwJob(jobB, sha)},
			wantRerun:   []string{"/test " + jobA, "/test " + jobB},
		},
		{
			name:        "job present at a different SHA does not suppress delta",
			mode:        modeDelta,
			existingPJs: []v1.ProwJob{makeProwJob(jobA, "different_sha_12345")},
			wantRerun:   []string{"/test " + jobA, "/test " + jobB},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ghc := &fakeGhClient{changes: changes}
			pjLister := newFakePJLister(tc.existingPJs...)

			presubmits := presubmitTests{pipelineConditionallyRequired: conditionalPresubmits}
			pj := makeTriggerPJ(sha)

			if err := sendCommentWithMode(presubmits, pj, ghc, func() {}, pjLister, tc.mode, true, nil); err != nil {
				t.Fatalf("sendCommentWithMode returned error: %v", err)
			}
			if len(ghc.comments) != 1 {
				t.Fatalf("expected 1 comment, got %d", len(ghc.comments))
			}
			comment := ghc.comments[0]

			if strings.Contains(comment, manualControlMsg) {
				t.Errorf("manual control message should no longer be emitted, got: %q", comment)
			}
			for _, want := range tc.wantRerun {
				if !strings.Contains(comment, want) {
					t.Errorf("expected %q in comment, got: %q", want, comment)
				}
			}
			for _, notWant := range tc.wantNoRerun {
				if strings.Contains(comment, notWant) {
					t.Errorf("did not expect %q in comment, got: %q", notWant, comment)
				}
			}
			if tc.wantComment != "" && !strings.Contains(comment, tc.wantComment) {
				t.Errorf("expected substring %q in comment, got: %q", tc.wantComment, comment)
			}
		})
	}
}

// TestSendCommentWithMode_DeltaDoesNotSuppressProtected verifies that a manually
// triggered conditional job no longer suppresses the required protected jobs:
// the protected job is still scheduled while the already-present conditional job
// is skipped.
func TestSendCommentWithMode_DeltaDoesNotSuppressProtected(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	protectedJob := fmt.Sprintf("pull-ci-%s-%s-%s-protected", org, repo, baseRef)
	conditionalJob := fmt.Sprintf("pull-ci-%s-%s-%s-conditional", org, repo, baseRef)

	presubmits := presubmitTests{
		protected: []config.Presubmit{{
			JobBase:      config.JobBase{Name: protectedJob},
			Reporter:     config.Reporter{Context: protectedJob},
			RerunCommand: "/test " + protectedJob,
		}},
		pipelineConditionallyRequired: []config.Presubmit{{
			JobBase:      config.JobBase{Name: conditionalJob, Annotations: map[string]string{"pipeline_run_if_changed": "^cmd/"}},
			Reporter:     config.Reporter{Context: conditionalJob},
			RerunCommand: "/test " + conditionalJob,
		}},
	}

	ghc := &fakeGhClient{changes: []github.PullRequestChange{{Filename: "cmd/main.go"}}}
	// The conditional job was already triggered manually; the protected job was not.
	pjLister := newFakePJLister(makeProwJob(conditionalJob, sha))
	pj := makeTriggerPJ(sha)

	if err := sendCommentWithMode(presubmits, pj, ghc, func() {}, pjLister, modeDelta, true, nil); err != nil {
		t.Fatalf("sendCommentWithMode returned error: %v", err)
	}
	if len(ghc.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(ghc.comments))
	}
	comment := ghc.comments[0]
	if !strings.Contains(comment, "/test "+protectedJob) {
		t.Errorf("protected job should be scheduled despite manual conditional trigger, got: %q", comment)
	}
	if strings.Contains(comment, "/test "+conditionalJob) {
		t.Errorf("already-triggered conditional job should be skipped, got: %q", comment)
	}
}

// errorLister is a ctrlruntimeclient.Reader whose List always fails, used to
// exercise the fail-closed behavior of modeDelta.
type errorLister struct {
	ctrlruntimeclient.Reader
}

func (errorLister) List(_ context.Context, _ ctrlruntimeclient.ObjectList, _ ...ctrlruntimeclient.ListOption) error {
	return fmt.Errorf("boom: cannot list prowjobs")
}

// TestSendCommentWithMode_DeltaListErrorFailsClosed verifies that a list error in
// modeDelta fails closed: nothing is scheduled, no comment is posted, the error
// is returned, and deleteIds is invoked so the SHA is not left with a stale
// comment (it is retried on the next event rather than double-posted).
func TestSendCommentWithMode_DeltaListErrorFailsClosed(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	conditionalJob := fmt.Sprintf("pull-ci-%s-%s-%s-conditional", org, repo, baseRef)
	presubmits := presubmitTests{
		pipelineConditionallyRequired: []config.Presubmit{{
			JobBase:      config.JobBase{Name: conditionalJob, Annotations: map[string]string{"pipeline_run_if_changed": "^cmd/"}},
			Reporter:     config.Reporter{Context: conditionalJob},
			RerunCommand: "/test " + conditionalJob,
		}},
	}

	ghc := &fakeGhClient{changes: []github.PullRequestChange{{Filename: "cmd/main.go"}}}
	pj := makeTriggerPJ(sha)

	deleteIdsCalled := false
	err := sendCommentWithMode(presubmits, pj, ghc, func() { deleteIdsCalled = true }, errorLister{}, modeDelta, true, nil)
	if err == nil {
		t.Fatal("expected an error from modeDelta list failure, got nil")
	}
	if len(ghc.comments) != 0 {
		t.Errorf("expected no comment to be posted on list error, got: %v", ghc.comments)
	}
	if !deleteIdsCalled {
		t.Error("expected deleteIds to be called on list error so the SHA is retried, not double-posted")
	}
}

// TestSendCommentWithMode_DeltaNilListerNoPanic verifies that modeDelta with a
// nil lister does not panic and schedules the full set (no cache to subtract).
func TestSendCommentWithMode_DeltaNilListerNoPanic(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	conditionalJob := fmt.Sprintf("pull-ci-%s-%s-%s-conditional", org, repo, baseRef)
	presubmits := presubmitTests{
		pipelineConditionallyRequired: []config.Presubmit{{
			JobBase:      config.JobBase{Name: conditionalJob, Annotations: map[string]string{"pipeline_run_if_changed": "^cmd/"}},
			Reporter:     config.Reporter{Context: conditionalJob},
			RerunCommand: "/test " + conditionalJob,
		}},
	}
	ghc := &fakeGhClient{changes: []github.PullRequestChange{{Filename: "cmd/main.go"}}}
	pj := makeTriggerPJ(sha)

	if err := sendCommentWithMode(presubmits, pj, ghc, func() {}, nil, modeDelta, true, nil); err != nil {
		t.Fatalf("sendCommentWithMode returned error: %v", err)
	}
	if len(ghc.comments) != 1 || !strings.Contains(ghc.comments[0], "/test "+conditionalJob) {
		t.Errorf("expected the conditional job to be scheduled with a nil lister, got: %v", ghc.comments)
	}
}

// TestSendCommentWithMode_DeltaNewHeadReEvaluates verifies that a new HEAD SHA is
// evaluated from scratch: jobs present only at the old SHA do not suppress the
// delta at the new SHA.
func TestSendCommentWithMode_DeltaNewHeadReEvaluates(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		oldSHA  = "old1234567890abc"
		newSHA  = "new1234567890abc"
		prNum   = 42
	)

	conditionalJob := fmt.Sprintf("pull-ci-%s-%s-%s-conditional", org, repo, baseRef)
	presubmits := presubmitTests{
		pipelineConditionallyRequired: []config.Presubmit{{
			JobBase:      config.JobBase{Name: conditionalJob, Annotations: map[string]string{"pipeline_run_if_changed": "^cmd/"}},
			Reporter:     config.Reporter{Context: conditionalJob},
			RerunCommand: "/test " + conditionalJob,
		}},
	}
	ghc := &fakeGhClient{changes: []github.PullRequestChange{{Filename: "cmd/main.go"}}}
	// Job exists only at the old SHA.
	pjLister := newFakePJLister(makeProwJob(conditionalJob, oldSHA))
	pj := makeTriggerPJ(newSHA)

	if err := sendCommentWithMode(presubmits, pj, ghc, func() {}, pjLister, modeDelta, true, nil); err != nil {
		t.Fatalf("sendCommentWithMode returned error: %v", err)
	}
	if len(ghc.comments) != 1 || !strings.Contains(ghc.comments[0], "/test "+conditionalJob) {
		t.Errorf("expected the conditional job to be scheduled at the new HEAD, got: %v", ghc.comments)
	}
}

// TestSendCommentWithMode_DeltaEmptyWithOnlyFirstStageJobs verifies that when the
// delta is empty and only FIRST-stage jobs exist at HEAD (no second-stage job has
// run), the controller posts the generic "No second-stage tests were triggered"
// message, not the misleading "already been triggered" one.
func TestSendCommentWithMode_DeltaEmptyWithOnlyFirstStageJobs(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	firstStageJob := fmt.Sprintf("pull-ci-%s-%s-%s-unit", org, repo, baseRef)
	conditionalJob := fmt.Sprintf("pull-ci-%s-%s-%s-conditional", org, repo, baseRef)

	presubmits := presubmitTests{
		// A first-stage always-required job (its ProwJob will exist at HEAD).
		alwaysRequired: []config.Presubmit{{
			JobBase:      config.JobBase{Name: firstStageJob},
			Reporter:     config.Reporter{Context: firstStageJob},
			RerunCommand: "/test " + firstStageJob,
		}},
		// A conditional second-stage job whose condition does NOT match the changes,
		// so the delta is empty.
		pipelineConditionallyRequired: []config.Presubmit{{
			JobBase:      config.JobBase{Name: conditionalJob, Annotations: map[string]string{"pipeline_run_if_changed": "^cmd/"}},
			Reporter:     config.Reporter{Context: conditionalJob},
			RerunCommand: "/test " + conditionalJob,
		}},
	}

	// Only the first-stage job exists at HEAD; no second-stage job has run.
	ghc := &fakeGhClient{changes: []github.PullRequestChange{{Filename: "docs/README.md"}}}
	pjLister := newFakePJLister(makeProwJob(firstStageJob, sha))
	pj := makeTriggerPJ(sha)

	if err := sendCommentWithMode(presubmits, pj, ghc, func() {}, pjLister, modeDelta, true, nil); err != nil {
		t.Fatalf("sendCommentWithMode returned error: %v", err)
	}
	if len(ghc.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d: %v", len(ghc.comments), ghc.comments)
	}
	comment := ghc.comments[0]
	if !strings.Contains(comment, "No second-stage tests were triggered") {
		t.Errorf("expected the generic no-tests message, got: %q", comment)
	}
	if strings.Contains(comment, "already been triggered") {
		t.Errorf("first-stage jobs must not trigger the already-triggered message, got: %q", comment)
	}
}

// TestSendCommentWithMode_ImplicitEmptyIsSilent verifies that an automatic
// trigger (explicit=false, as used by the reconciler auto path and LGTM) posts
// NO comment when there is nothing to schedule, while an explicit /pipeline
// command (explicit=true) still gets the "already been triggered" acknowledgment
// for the same state. This prevents fleet-wide duplicate "nothing to do" notes.
func TestSendCommentWithMode_ImplicitEmptyIsSilent(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
	)

	conditionalJob := fmt.Sprintf("pull-ci-%s-%s-%s-conditional", org, repo, baseRef)
	presubmits := presubmitTests{
		pipelineConditionallyRequired: []config.Presubmit{{
			JobBase:      config.JobBase{Name: conditionalJob, Annotations: map[string]string{"pipeline_run_if_changed": "^cmd/"}},
			Reporter:     config.Reporter{Context: conditionalJob},
			RerunCommand: "/test " + conditionalJob,
		}},
	}
	changes := []github.PullRequestChange{{Filename: "cmd/main.go"}}
	// The one applicable second-stage job already exists at HEAD -> empty delta.
	existing := []v1.ProwJob{makeProwJob(conditionalJob, sha)}

	// Implicit (automatic) trigger: must stay silent.
	ghcImplicit := &fakeGhClient{changes: changes}
	if err := sendCommentWithMode(presubmits, makeTriggerPJ(sha), ghcImplicit, func() {}, newFakePJLister(existing...), modeDelta, false, nil); err != nil {
		t.Fatalf("sendCommentWithMode(implicit) returned error: %v", err)
	}
	if len(ghcImplicit.comments) != 0 {
		t.Errorf("expected NO comment on an automatic empty-delta trigger, got %d: %v", len(ghcImplicit.comments), ghcImplicit.comments)
	}

	// Explicit command for the same state: still acknowledges.
	ghcExplicit := &fakeGhClient{changes: changes}
	if err := sendCommentWithMode(presubmits, makeTriggerPJ(sha), ghcExplicit, func() {}, newFakePJLister(existing...), modeDelta, true, nil); err != nil {
		t.Fatalf("sendCommentWithMode(explicit) returned error: %v", err)
	}
	if len(ghcExplicit.comments) != 1 || !strings.Contains(ghcExplicit.comments[0], "already been triggered") {
		t.Errorf("expected an explicit command to still post the already-triggered ack, got: %v", ghcExplicit.comments)
	}
}

// TestPlanDispatchCommentTerminalReportedJobs covers the commit-status proof
// for second-stage jobs pruned from the ProwJob cache: terminal statuses
// (success, failure and error all count as an attempt) suppress scheduling,
// pending never counts, modeForce ignores the proof, cache coverage needs no
// lookup, and lookup failures fail closed instead of mass-scheduling.
func TestPlanDispatchCommentTerminalReportedJobs(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
	)

	protectedJob := fmt.Sprintf("pull-ci-%s-%s-%s-protected", org, repo, baseRef)
	conditionalJob := fmt.Sprintf("pull-ci-%s-%s-%s-conditional", org, repo, baseRef)
	// The canonical single-base brancher and the unique context ownership
	// below keep both jobs eligible for the status fallback this test covers.
	canonical := config.Brancher{Branches: []string{"^" + baseRef + "$"}}
	presubmits := presubmitTests{
		protected: []config.Presubmit{{
			JobBase:      config.JobBase{Name: protectedJob},
			Brancher:     canonical,
			Reporter:     config.Reporter{Context: "ci/protected"},
			RerunCommand: "/test " + protectedJob,
		}},
		pipelineConditionallyRequired: []config.Presubmit{{
			JobBase:      config.JobBase{Name: conditionalJob, Annotations: map[string]string{"pipeline_run_if_changed": "^cmd/"}},
			Brancher:     canonical,
			Reporter:     config.Reporter{Context: "ci/conditional"},
			RerunCommand: "/test " + conditionalJob,
		}},
		statusContextOwners: map[string]string{"ci/protected": protectedJob, "ci/conditional": conditionalJob},
	}
	changes := []github.PullRequestChange{{Filename: "cmd/main.go"}}
	openPR := &github.PullRequest{Number: 42, State: github.PullRequestStateOpen,
		Head: github.PullRequestBranch{SHA: sha}, Base: github.PullRequestBranch{Ref: baseRef}}
	movedPR := &github.PullRequest{Number: 42, State: github.PullRequestStateOpen,
		Head: github.PullRequestBranch{SHA: "deadbeefdeadbeef"}, Base: github.PullRequestBranch{Ref: baseRef}}

	tests := []struct {
		name        string
		force       bool
		existingPJs []v1.ProwJob
		pr          *github.PullRequest
		statuses    []github.Status
		wrongSHA    bool
		statusErr   error
		wantErr     string
		wantJobs    []string
		wantComment string
		wantReads   int
		wantPRCalls int
	}{
		{
			name: "terminal success and failure suppress scheduling",
			statuses: []github.Status{{Context: "ci/protected", State: github.StatusSuccess},
				{Context: "ci/conditional", State: github.StatusFailure}},
			wantComment: "already been triggered",
			wantReads:   1,
			wantPRCalls: 1,
		},
		{
			name: "error and pending statuses filter partially",
			statuses: []github.Status{{Context: "ci/protected", State: github.StatusError},
				{Context: "ci/conditional", State: github.StatusPending}},
			wantJobs:    []string{conditionalJob},
			wantReads:   1,
			wantPRCalls: 1,
		},
		{
			name:      "pending or missing statuses schedule everything",
			statuses:  []github.Status{{Context: "ci/protected", State: github.StatusPending}},
			wantJobs:  []string{protectedJob, conditionalJob},
			wantReads: 1,
		},
		{
			name:  "force re-fires terminal jobs without a status lookup",
			force: true,
			statuses: []github.Status{{Context: "ci/protected", State: github.StatusSuccess},
				{Context: "ci/conditional", State: github.StatusSuccess}},
			wantJobs: []string{protectedJob, conditionalJob},
		},
		{
			name: "cached prowjobs in any state need no lookup",
			existingPJs: []v1.ProwJob{
				makeProwJobInState(protectedJob, sha, v1.FailureState),
				makeProwJob(conditionalJob, sha),
			},
			wantComment: "already been triggered",
		},
		{
			name:      "status read error fails closed",
			statusErr: errors.New("boom"),
			wantErr:   "getting combined status for pruned second-stage jobs of openshift/myrepo#42: boom",
			wantReads: 1,
		},
		{
			name:      "combined status for another sha fails closed",
			wrongSHA:  true,
			statuses:  []github.Status{{Context: "ci/protected", State: github.StatusSuccess}},
			wantErr:   `combined status response for pruned second-stage jobs of openshift/myrepo#42 has sha "deadbeefdeadbeef", want "abc1234567890"`,
			wantReads: 1,
		},
		{
			name:        "moved head rejects the stale proof",
			pr:          movedPR,
			statuses:    []github.Status{{Context: "ci/protected", State: github.StatusSuccess}},
			wantErr:     `stale terminal statuses for pruned second-stage jobs of openshift/myrepo#42: pull request state "open" draft false head "deadbeefdeadbeef" base "main"`,
			wantReads:   1,
			wantPRCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr := tc.pr
			if pr == nil {
				pr = openPR
			}
			combined := &github.CombinedStatus{SHA: sha, Statuses: tc.statuses}
			if tc.wrongSHA {
				combined.SHA = "deadbeefdeadbeef"
			}
			ghc := &fakeGhClient{changes: changes, pullRequest: pr, combinedStatus: combined, getCombinedStatusErr: tc.statusErr}

			mode := modeDelta
			if tc.force {
				mode = modeForce
			}
			comment, jobs, err := planDispatchComment(presubmits, makeTriggerPJ(sha), ghc, newFakePJLister(tc.existingPJs...), mode, true)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tc.wantErr)
				}
				if diff := cmp.Diff(tc.wantErr, err.Error()); diff != "" {
					t.Errorf("unexpected error (-want +got):\n%s", diff)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var names []string
			for _, job := range jobs {
				names = append(names, job.Name)
			}
			if diff := cmp.Diff(tc.wantJobs, names); diff != "" {
				t.Errorf("unexpected jobs (-want +got):\n%s", diff)
			}
			if tc.wantComment != "" && !strings.Contains(comment, tc.wantComment) {
				t.Errorf("expected substring %q in comment, got: %q", tc.wantComment, comment)
			}
			if ghc.getCombinedStatusCalls != tc.wantReads {
				t.Errorf("expected %d combined status calls, got %d", tc.wantReads, ghc.getCombinedStatusCalls)
			}
			if ghc.getPullRequestCalls != tc.wantPRCalls {
				t.Errorf("expected %d pull request calls, got %d", tc.wantPRCalls, ghc.getPullRequestCalls)
			}
		})
	}
}

func TestExistsAtSHA(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
		prNum   = 42
	)

	jobName := fmt.Sprintf("pull-ci-%s-%s-%s-e2e-test", org, repo, baseRef)

	pj := makeTriggerPJ(sha)

	tests := []struct {
		name        string
		existingPJs []v1.ProwJob
		jobName     string
		wantExists  bool
	}{
		{
			name:        "returns true when ProwJob exists at same SHA",
			existingPJs: []v1.ProwJob{makeProwJob(jobName, sha)},
			jobName:     jobName,
			wantExists:  true,
		},
		{
			name:        "returns false when no ProwJob exists",
			existingPJs: nil,
			jobName:     jobName,
			wantExists:  false,
		},
		{
			name:        "returns false when ProwJob exists at different SHA",
			existingPJs: []v1.ProwJob{makeProwJob(jobName, "differentsha1234")},
			jobName:     jobName,
			wantExists:  false,
		},
		{
			name:        "returns false when ProwJob exists for different job name",
			existingPJs: []v1.ProwJob{makeProwJob("some-other-job", sha)},
			jobName:     jobName,
			wantExists:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pjLister := newFakePJLister(tc.existingPJs...)
			got, err := existsAtSHA(context.Background(), pjLister, pj, tc.jobName)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.wantExists {
				t.Errorf("existsAtSHA() = %v, want %v", got, tc.wantExists)
			}
		})
	}
}

// makeProwJobInState returns a first-stage ProwJob like makeProwJob with an
// explicit running or terminal state.
func makeProwJobInState(name, sha string, state v1.ProwJobState) v1.ProwJob {
	pj := makeProwJob(name, sha)
	pj.Status.State = state
	return pj
}

// TestCheckFirstStageCompletePrunedJobFallback covers the commit-status
// fallback for always-required jobs deleted from the ProwJob cache by sinker:
// live ProwJobs stay authoritative and block without GitHub traffic, while
// missing jobs are verified against the statuses of the exact current HEAD on
// a still-current, open PR.
func TestCheckFirstStageCompletePrunedJobFallback(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
	)

	unitJob := fmt.Sprintf("pull-ci-%s-%s-%s-unit", org, repo, baseRef)
	buildJob := fmt.Sprintf("pull-ci-%s-%s-%s-build", org, repo, baseRef)
	imagesJob := fmt.Sprintf("pull-ci-%s-%s-%s-images", org, repo, baseRef)

	// The canonical single-base brancher and the unique context ownership
	// below keep the pruned jobs eligible for the status fallback; the PR's
	// base SHA is deliberately ahead of anything the controller could have
	// recorded, pinning that the proof is the context on the HEAD, not a
	// comparison against the base branch's latest commit.
	canonical := config.Brancher{Branches: []string{"^" + baseRef + "$"}}
	unit := config.Presubmit{JobBase: config.JobBase{Name: unitJob}, Brancher: canonical, Reporter: config.Reporter{Context: "ci/unit"}}
	build := config.Presubmit{JobBase: config.JobBase{Name: buildJob}, Brancher: canonical, Reporter: config.Reporter{Context: "ci/build"}}
	images := config.Presubmit{JobBase: config.JobBase{Name: imagesJob}, Brancher: canonical, Reporter: config.Reporter{Context: "ci/images"}}
	standard := presubmitTests{
		alwaysRequired:        []config.Presubmit{unit, build},
		conditionallyRequired: []config.Presubmit{images},
		statusContextOwners:   map[string]string{"ci/unit": unitJob, "ci/build": buildJob, "ci/images": imagesJob},
	}
	openPR := &github.PullRequest{Number: 42, State: github.PullRequestStateOpen,
		Head: github.PullRequestBranch{SHA: sha}, Base: github.PullRequestBranch{Ref: baseRef, SHA: "advanced-base-000000"}}
	// The aggregate state must be ignored; only the exact job contexts count.
	passing := &github.CombinedStatus{SHA: sha, State: github.StatusFailure, Statuses: []github.Status{
		{Context: "ci/unit", State: github.StatusSuccess},
		{Context: "ci/build", State: github.StatusSuccess},
	}}

	tests := []struct {
		name            string
		live            []v1.ProwJob
		pr              *github.PullRequest
		combined        *github.CombinedStatus
		statusErr       error
		wantComplete    bool
		wantErr         string
		wantPRCalls     int
		wantStatusCalls int
		cancelLookup    bool
	}{
		{
			name:         "all live successes need no github lookup",
			live:         []v1.ProwJob{makeProwJob(unitJob, sha), makeProwJob(buildJob, sha)},
			wantComplete: true,
		},
		{
			name:            "pruned required jobs verified from commit statuses",
			pr:              openPR,
			combined:        passing,
			wantComplete:    true, // the absent conditional job stays non-blocking
			wantPRCalls:     1,
			wantStatusCalls: 1,
		},
		{
			name: "fresh live override wins over stale pruned statuses",
			live: []v1.ProwJob{makeProwJob(unitJob, sha)},
			pr:   openPR,
			combined: &github.CombinedStatus{SHA: sha, Statuses: []github.Status{
				{Context: "ci/unit", State: github.StatusFailure},
				{Context: "ci/build", State: github.StatusSuccess},
			}},
			wantComplete:    true,
			wantPRCalls:     1,
			wantStatusCalls: 1,
		},
		{
			name: "live pending job blocks without a github lookup",
			live: []v1.ProwJob{makeProwJobInState(unitJob, sha, v1.PendingState)},
		},
		{
			name: "live failed conditional job blocks without a github lookup",
			live: []v1.ProwJob{makeProwJobInState(imagesJob, sha, v1.FailureState)},
		},
		{
			name: "missing or failing status context is incomplete",
			pr:   openPR,
			combined: &github.CombinedStatus{SHA: sha, Statuses: []github.Status{
				{Context: "ci/unit", State: github.StatusFailure},
			}},
			wantPRCalls:     1,
			wantStatusCalls: 1,
		},
		{
			name:            "combined status for another sha is rejected",
			pr:              openPR,
			combined:        &github.CombinedStatus{SHA: "deadbeefdeadbeef", Statuses: passing.Statuses},
			wantPRCalls:     1,
			wantStatusCalls: 1,
		},
		{
			name: "moved pr head rejects the fallback",
			pr: &github.PullRequest{Number: 42, State: github.PullRequestStateOpen,
				Head: github.PullRequestBranch{SHA: "deadbeefdeadbeef"}, Base: github.PullRequestBranch{Ref: baseRef}},
			wantPRCalls: 1,
		},
		{
			name:            "status lookup failure is wrapped",
			pr:              openPR,
			statusErr:       errors.New("boom"),
			wantErr:         "getting combined status for pruned first-stage jobs of trigger-pj: boom",
			wantPRCalls:     1,
			wantStatusCalls: 1,
		},
		{
			name:            "cancellation during status lookup prevents readiness",
			pr:              openPR,
			combined:        passing,
			cancelLookup:    true,
			wantErr:         "checking first-stage completion after GitHub lookups: context canceled",
			wantPRCalls:     1,
			wantStatusCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ghc := &fakeGhClient{pullRequest: tc.pr, combinedStatus: tc.combined, getCombinedStatusErr: tc.statusErr}
			if tc.cancelLookup {
				ghc.cancelStatusLookup = cancel
			}
			complete, err := checkFirstStageComplete(ctx, newFakePJLister(tc.live...), ghc, makeTriggerPJ(sha), standard)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tc.wantErr)
				}
				if diff := cmp.Diff(tc.wantErr, err.Error()); diff != "" {
					t.Errorf("unexpected error (-want +got):\n%s", diff)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.cancelLookup && !errors.Is(err, context.Canceled) {
				t.Errorf("expected wrapped context cancellation, got: %v", err)
			}
			if diff := cmp.Diff(tc.wantComplete, complete); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantPRCalls, ghc.getPullRequestCalls); diff != "" {
				t.Errorf("unexpected pull request calls (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantStatusCalls, ghc.getCombinedStatusCalls); diff != "" {
				t.Errorf("unexpected combined status calls (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFirstStageStatusFallbackRequiresAttributableContext covers the shared
// eligibility guard of the first-stage commit-status fallback: GitHub statuses
// recover a pruned always-required job only when its context is owned by
// exactly one reportable definition that runs on exactly the PR's base
// branch. Shared or unknown ownership and unrestricted, broad, multi-pattern
// or skip-branch definitions stay incomplete without spending any GitHub
// lookup, while the sole canonical owner still recovers after the base branch
// moved on: the proof is the context on the HEAD, not a base SHA comparison.
func TestFirstStageStatusFallbackRequiresAttributableContext(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
	)

	unitJob := fmt.Sprintf("pull-ci-%s-%s-%s-unit", org, repo, baseRef)
	// Every row reports success for the exact context on the exact HEAD, so
	// only the eligibility guard can keep a row incomplete.
	passing := &github.CombinedStatus{SHA: sha, Statuses: []github.Status{
		{Context: "ci/unit", State: github.StatusSuccess},
	}}
	openPR := &github.PullRequest{Number: 42, State: github.PullRequestStateOpen,
		Head: github.PullRequestBranch{SHA: sha},
		Base: github.PullRequestBranch{Ref: baseRef, SHA: "advanced-base-000000"}}

	tests := []struct {
		name         string
		branches     []string
		skipBranches []string
		owners       map[string]string
		wantComplete bool
	}{
		{
			name:     "context shared with another definition stays incomplete",
			branches: []string{"^main$"},
			owners:   map[string]string{"ci/unit": ""}, // a second claimant blanked the entry
		},
		{
			name:     "unknown ownership inventory stays incomplete",
			branches: []string{"^main$"},
		},
		{
			name:     "job running on every branch stays incomplete",
			branches: nil,
			owners:   map[string]string{"ci/unit": unitJob},
		},
		{
			name:     "prefix branch pattern stays incomplete",
			branches: []string{"^main"},
			owners:   map[string]string{"ci/unit": unitJob},
		},
		{
			name:     "multiple branch patterns stay incomplete",
			branches: []string{"^main$", "^release$"},
			owners:   map[string]string{"ci/unit": unitJob},
		},
		{
			name:         "skipped branches stay incomplete",
			branches:     []string{"^main$"},
			skipBranches: []string{"^feature$"},
			owners:       map[string]string{"ci/unit": unitJob},
		},
		{
			name:         "sole owner on the exact base recovers",
			branches:     []string{"^main$"},
			owners:       map[string]string{"ci/unit": unitJob},
			wantComplete: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			presubmits := presubmitTests{
				alwaysRequired: []config.Presubmit{{
					JobBase:  config.JobBase{Name: unitJob},
					Brancher: config.Brancher{Branches: tc.branches, SkipBranches: tc.skipBranches},
					Reporter: config.Reporter{Context: "ci/unit"},
				}},
				statusContextOwners: tc.owners,
			}
			ghc := &fakeGhClient{pullRequest: openPR, combinedStatus: passing}

			complete, err := checkFirstStageComplete(context.Background(), newFakePJLister(), ghc, makeTriggerPJ(sha), presubmits)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.wantComplete, complete); diff != "" {
				t.Errorf("unexpected result (-want +got):\n%s", diff)
			}
			// An ineligible context must not spend GitHub lookups either: the
			// stage just stays incomplete until a live ProwJob arrives.
			wantLookups := 0
			if tc.wantComplete {
				wantLookups = 1
			}
			if diff := cmp.Diff(wantLookups, ghc.getPullRequestCalls); diff != "" {
				t.Errorf("unexpected pull request calls (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(wantLookups, ghc.getCombinedStatusCalls); diff != "" {
				t.Errorf("unexpected combined status calls (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDeltaStatusFallbackRequiresAttributableContext covers the same guard on
// the second-stage side: a terminal status of a context with shared or
// unknown ownership is not proof, so the delta planner still schedules the
// job and publishes its pending placeholder (an ambiguous history blocks, it
// never fails the dispatch), while the sole owner of an exact single-base
// context is suppressed as already run despite the advanced base SHA.
func TestDeltaStatusFallbackRequiresAttributableContext(t *testing.T) {
	const (
		org     = "openshift"
		repo    = "myrepo"
		baseRef = "main"
		sha     = "abc1234567890"
	)

	protectedJob := fmt.Sprintf("pull-ci-%s-%s-%s-protected", org, repo, baseRef)
	terminal := &github.CombinedStatus{SHA: sha, Statuses: []github.Status{
		{Context: "ci/protected", State: github.StatusSuccess},
	}}
	openPR := &github.PullRequest{Number: 42, State: github.PullRequestStateOpen,
		Head: github.PullRequestBranch{SHA: sha},
		Base: github.PullRequestBranch{Ref: baseRef, SHA: "advanced-base-000000"}}

	tests := []struct {
		name         string
		owners       map[string]string
		wantSchedule bool
		wantReads    int
		wantPRCalls  int
	}{
		{
			name:         "shared context schedules and republishes pending",
			owners:       map[string]string{"ci/protected": ""},
			wantSchedule: true,
		},
		{
			name:         "unknown ownership schedules without a status lookup",
			wantSchedule: true,
		},
		{
			name:        "sole owner on the exact base is suppressed",
			owners:      map[string]string{"ci/protected": protectedJob},
			wantReads:   1,
			wantPRCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			presubmits := presubmitTests{
				protected: []config.Presubmit{{
					JobBase:      config.JobBase{Name: protectedJob},
					Brancher:     config.Brancher{Branches: []string{"^" + baseRef + "$"}},
					Reporter:     config.Reporter{Context: "ci/protected"},
					RerunCommand: "/test " + protectedJob,
				}},
				statusContextOwners: tc.owners,
			}
			gh := &fakeAgenticGitHub{pr: *openPR, combinedStatus: terminal}

			if err := sendCommentWithMode(presubmits, makeTriggerPJ(sha), gh, func() {}, newFakePJLister(), modeDelta, true, &dispatchChecks{gh: gh, appID: 101}); err != nil {
				t.Fatalf("sendCommentWithMode returned error: %v", err)
			}

			if len(gh.comments) != 1 {
				t.Fatalf("expected 1 comment, got %d", len(gh.comments))
			}
			if tc.wantSchedule {
				if !strings.Contains(gh.comments[0].Body, "/test "+protectedJob) {
					t.Errorf("expected the job to be scheduled, got: %q", gh.comments[0].Body)
				}
				want := []github.Status{{Context: "ci/protected", State: "pending", Description: PipelinePendingMessage}}
				if diff := cmp.Diff(want, gh.statusUpdates); diff != "" {
					t.Errorf("unexpected pending placeholders (-want +got):\n%s", diff)
				}
			} else {
				if !strings.Contains(gh.comments[0].Body, "already been triggered") {
					t.Errorf("expected the terminal proof to suppress scheduling, got: %q", gh.comments[0].Body)
				}
				if len(gh.statusUpdates) != 0 {
					t.Errorf("expected no pending placeholders for a proven job, got: %v", gh.statusUpdates)
				}
			}
			if diff := cmp.Diff(tc.wantReads, gh.statusReads); diff != "" {
				t.Errorf("unexpected combined status reads (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantPRCalls, gh.getPullRequestCalls); diff != "" {
				t.Errorf("unexpected pull request calls (-want +got):\n%s", diff)
			}
		})
	}
}
