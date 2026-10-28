package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"codex-feishu/internal/bindings"
	"codex-feishu/internal/store"
)

const (
	groupNameConcurrency  = 2
	groupNameRetryInitial = 15 * time.Second
	groupNameRetryMax     = 5 * time.Minute
)

type groupNameJob struct {
	request store.GroupNameRequest
	running bool
	retryAt time.Time
	delay   time.Duration
}

// A native generation owns a fixed worker pool. Jobs outlive dormant actors,
// coalesce to the latest title, and serialize every request for the same chat.
// Desired titles are durable before enqueue; Reconcile still journals each PUT.
type groupNameSync struct {
	runtime *Runtime
	mu      sync.Mutex
	jobs    map[string]*groupNameJob
	wake    chan struct{}
}

func newGroupNameSync(r *Runtime) *groupNameSync {
	return &groupNameSync{runtime: r, jobs: map[string]*groupNameJob{}, wake: make(chan struct{}, groupNameConcurrency)}
}

func (q *groupNameSync) signal() {
	for i := 0; i < groupNameConcurrency; i++ {
		select {
		case q.wake <- struct{}{}:
		default:
		}
	}
}

func (q *groupNameSync) enqueue(request store.GroupNameRequest) {
	q.mu.Lock()
	job := q.jobs[request.Scope.Binding.ChatID]
	if job == nil {
		job = &groupNameJob{}
		q.jobs[request.Scope.Binding.ChatID] = job
	}
	job.request = request
	q.mu.Unlock()
	q.signal()
}

func (q *groupNameSync) take() (*groupNameJob, store.GroupNameRequest) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.runtime.now()
	for _, job := range q.jobs {
		if !job.running && !job.retryAt.After(now) {
			job.running = true
			return job, job.request
		}
	}
	return nil, store.GroupNameRequest{}
}

func (q *groupNameSync) finish(job *groupNameJob, request store.GroupNameRequest, err error) {
	q.mu.Lock()
	job.running = false
	current := job.request == request
	if (err == nil || errors.Is(err, bindings.ErrGroupNameRejected)) && current {
		delete(q.jobs, request.Scope.Binding.ChatID)
	} else if err == nil || job.request.Scope != request.Scope {
		job.delay, job.retryAt = 0, time.Time{}
	} else {
		job.delay *= 2
		if job.delay < groupNameRetryInitial {
			job.delay = groupNameRetryInitial
		}
		if job.delay > groupNameRetryMax {
			job.delay = groupNameRetryMax
		}
		job.retryAt = q.runtime.now().Add(job.delay)
		var deferred *bindings.GroupNameDeferredError
		if errors.As(err, &deferred) && deferred.RetryAtMS > job.retryAt.UnixMilli() {
			job.retryAt = time.UnixMilli(deferred.RetryAtMS)
		}
	}
	q.mu.Unlock()
	q.signal()
}

func (q *groupNameSync) run(ctx context.Context) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for ctx.Err() == nil {
		job, request := q.take()
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
			case <-timer.C:
			}
			continue
		}
		r := q.runtime
		nameCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		started := time.Now()
		var err error
		if !r.ready() {
			err = store.ErrConflict
		} else if r.CanDeliver != nil {
			err = r.CanDeliver(nameCtx, request.Scope.Binding)
		}
		if err == nil {
			err = r.SyncGroupName(nameCtx, request.Scope.Binding, request.Desired)
		}
		if err == nil || errors.Is(err, bindings.ErrGroupNameRejected) {
			if completeErr := r.Store.CompleteGroupNameRequest(nameCtx, request); completeErr != nil {
				err = completeErr
			}
		}
		r.observeLatency("group.name_sync", request.Scope.Binding.ThreadID, started, err)
		cancel()
		q.finish(job, request, err)
		if err != nil && ctx.Err() == nil {
			r.notice("group.name_sync_pending")
		}
	}
}
