package main

import (
	"context"
	"time"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/prow/pkg/github"
)

type agenticWork struct {
	org, repo string
	number    int
}

type agenticInput struct {
	generation uint64
	comments   []github.IssueComment
	retries    int
	retryAt    time.Time
}

func (a *agenticController) enqueue(org, repo string, number int, comment *github.IssueComment) {
	if number <= 0 {
		return
	}
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if a.queue != nil && a.queue.ShuttingDown() {
		return
	}
	work := agenticWork{org: org, repo: repo, number: number}
	if a.inputs == nil {
		a.inputs = map[agenticWork]*agenticInput{}
	}
	if a.inputs[work] == nil {
		a.inputs[work] = &agenticInput{}
	}
	input := a.inputs[work]
	input.generation++
	if comment != nil {
		duplicate := false
		for _, queued := range input.comments {
			duplicate = duplicate || queued.ID == comment.ID
		}
		if !duplicate {
			input.comments = append(input.comments, *comment)
		}
	}
	if a.queue != nil {
		a.queue.Add(work)
	}
}

func (a *agenticController) processNext(ctx context.Context) bool {
	work, shutdown := a.queue.Get()
	if shutdown {
		return false
	}
	defer a.queue.Done(work)
	a.queueMu.Lock()
	input := a.inputs[work]
	var generation uint64
	var comment *github.IssueComment
	if input != nil {
		if input.retryAt.After(a.currentTime()) {
			a.queue.AddAfter(work, input.retryAt.Sub(a.currentTime()))
			a.queueMu.Unlock()
			return true
		}
		generation = input.generation
		if len(input.comments) != 0 {
			copy := input.comments[0]
			comment = &copy
		}
	}
	a.queueMu.Unlock()
	a.mu.Lock()
	if input == nil {
		s := a.states[work]
		if s == nil || s.waitingUntil.IsZero() {
			a.mu.Unlock()
			return true // A cancelled/stale deadline makes no GitHub calls.
		}
		if s.waitingUntil.After(a.currentTime()) {
			a.queue.AddAfter(work, s.waitingUntil.Sub(a.currentTime()))
			a.mu.Unlock()
			return true
		}
	}
	err := a.reconcilePull(ctx, work, comment)
	var deadline time.Time
	if s := a.states[work]; s != nil {
		deadline = s.waitingUntil
	}
	a.mu.Unlock()
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	input = a.inputs[work]
	retry := agenticRetryFor(err)
	if retry.transient && (input == nil || input.retries < 5) {
		if a.inputs == nil {
			a.inputs = map[agenticWork]*agenticInput{}
		}
		if input == nil {
			input = &agenticInput{}
			a.inputs[work] = input
		}
		input.retries++
		if retry.after > 0 {
			input.retryAt = a.currentTime().Add(retry.after)
			a.queue.AddAfter(work, retry.after)
		} else {
			a.queue.AddRateLimited(work)
		}
		return true
	}
	a.queue.Forget(work)
	if input != nil {
		input.retries, input.retryAt = 0, time.Time{}
		if comment != nil && len(input.comments) != 0 && input.comments[0].ID == comment.ID {
			input.comments = input.comments[1:]
		}
		if len(input.comments) == 0 && input.generation == generation {
			delete(a.inputs, work)
		} else {
			a.queue.Add(work)
		}
	}
	if err == nil && !deadline.IsZero() {
		a.queue.AddAfter(work, max(deadline.Sub(a.currentTime()), 0))
	}
	if err != nil {
		a.logger.WithError(err).WithField("pr", work.number).Warn("Pipeline handoff failed; waiting for another relevant event")
	}
	return true
}

func (a *agenticController) Run(ctx context.Context) error {
	a.queueMu.Lock()
	a.queue = workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedItemExponentialFailureRateLimiter[agenticWork](5*time.Second, 5*time.Minute))
	for work := range a.inputs {
		a.queue.Add(work)
	}
	a.queueMu.Unlock()
	defer a.queue.ShutDown()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			a.queue.ShutDown()
		case <-done:
		}
	}()
	for a.processNext(ctx) {
	}
	return nil
}
