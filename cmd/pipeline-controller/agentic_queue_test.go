package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
)

func TestAgenticQueueCancelsDeadlinesAndBoundsRetries(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.a.queue = workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedItemExponentialFailureRateLimiter[agenticWork](time.Second, time.Minute))
	defer f.a.queue.ShutDown()
	f.passFirstStage()
	f.reconcile(t, nil)
	f.reconcile(t, f.command(100, "mark-pipeline-gate"))
	calls := f.gh.getPullRequestCalls
	f.a.queue.Add(f.work) // Stale timer delivery after completion.
	require.True(t, f.a.processNext(context.Background()))
	require.Equal(t, calls, f.gh.getPullRequestCalls)
	f.gh.getPullRequestError = io.ErrUnexpectedEOF
	f.a.enqueue(f.work.org, f.work.repo, f.work.number, nil)
	for range 6 {
		f.a.queue.Add(f.work)
		require.True(t, f.a.processNext(context.Background()))
	}
	require.Equal(t, calls+6, f.gh.getPullRequestCalls)
	require.Empty(t, f.a.inputs, "initial attempt plus five retries must stop until another event")
}

func TestAgenticProwJobEventsAvoidReplayAndNoise(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	r := &reconciler{agentic: f.a}
	pj := makeProwJob("pull-ci-org-repo-main-unit", f.gh.pr.Head.SHA)
	pj.Spec.Refs = pullRefs(f.work.org, f.work.repo, &f.gh.pr)
	require.False(t, r.shouldReconcileProwJobCreate(event.CreateEvent{Object: &pj, IsInInitialList: true}))
	require.True(t, r.shouldReconcileProwJobCreate(event.CreateEvent{Object: &pj}))
	pj.ResourceVersion = "1"
	current := pj.DeepCopy()
	current.ResourceVersion, current.Status.PodName = "2", "pod"
	require.False(t, r.shouldReconcileProwJobUpdate(event.UpdateEvent{ObjectOld: &pj, ObjectNew: current}))
	current.Status.State = v1.FailureState
	require.True(t, r.shouldReconcileProwJobUpdate(event.UpdateEvent{ObjectOld: &pj, ObjectNew: current}))
	r.agentic = nil
	require.True(t, r.shouldReconcileProwJobCreate(event.CreateEvent{Object: &pj, IsInInitialList: true}))
}
