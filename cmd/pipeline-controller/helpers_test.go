package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
	comments    []string
	changes     []github.PullRequestChange
	pullRequest *github.PullRequest
	addedLabels []string
}

func (f *fakeGhClient) GetPullRequest(org, repo string, number int) (*github.PullRequest, error) {
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

			err := sendCommentWithMode(presubmits, pj, ghc, deleteIds, pjLister, tc.mode, true)
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

			if err := sendCommentWithMode(presubmits, pj, ghc, func() {}, pjLister, tc.mode, true); err != nil {
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

	if err := sendCommentWithMode(presubmits, pj, ghc, func() {}, pjLister, modeDelta, true); err != nil {
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
	err := sendCommentWithMode(presubmits, pj, ghc, func() { deleteIdsCalled = true }, errorLister{}, modeDelta, true)
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

	if err := sendCommentWithMode(presubmits, pj, ghc, func() {}, nil, modeDelta, true); err != nil {
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

	if err := sendCommentWithMode(presubmits, pj, ghc, func() {}, pjLister, modeDelta, true); err != nil {
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

	if err := sendCommentWithMode(presubmits, pj, ghc, func() {}, pjLister, modeDelta, true); err != nil {
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
	if err := sendCommentWithMode(presubmits, makeTriggerPJ(sha), ghcImplicit, func() {}, newFakePJLister(existing...), modeDelta, false); err != nil {
		t.Fatalf("sendCommentWithMode(implicit) returned error: %v", err)
	}
	if len(ghcImplicit.comments) != 0 {
		t.Errorf("expected NO comment on an automatic empty-delta trigger, got %d: %v", len(ghcImplicit.comments), ghcImplicit.comments)
	}

	// Explicit command for the same state: still acknowledges.
	ghcExplicit := &fakeGhClient{changes: changes}
	if err := sendCommentWithMode(presubmits, makeTriggerPJ(sha), ghcExplicit, func() {}, newFakePJLister(existing...), modeDelta, true); err != nil {
		t.Fatalf("sendCommentWithMode(explicit) returned error: %v", err)
	}
	if len(ghcExplicit.comments) != 1 || !strings.Contains(ghcExplicit.comments[0], "already been triggered") {
		t.Errorf("expected an explicit command to still post the already-triggered ack, got: %v", ghcExplicit.comments)
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
			got := existsAtSHA(context.Background(), pjLister, pj, tc.jobName)
			if got != tc.wantExists {
				t.Errorf("existsAtSHA() = %v, want %v", got, tc.wantExists)
			}
		})
	}
}
