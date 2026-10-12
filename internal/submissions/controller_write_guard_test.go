package submissions

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"codex-feishu/internal/store"
)

func TestControllerWriteGuardNilKeepsOrdinaryAdd(t *testing.T) {
	c, f, _ := fixture(t, false)
	r := claim(t, c, "nil-guard")
	tick(t, c)

	n := view(t, c, r.ID)
	if c.BeginWrite != nil || f.addCalls != 1 || n.State != "consumed" {
		t.Fatalf("nil guard changed ordinary add: guard=%v adds=%d state=%s", c.BeginWrite != nil, f.addCalls, n.State)
	}
}

func TestControllerWriteGuardRefusalLeavesReceivedWithoutAttempt(t *testing.T) {
	c, f, _ := fixture(t, false)
	r := claim(t, c, "lease-refused")
	const secret = "sensitive lease refusal body"
	c.BeginWrite = func(context.Context) (context.Context, func(), error) {
		return nil, nil, errors.New(secret)
	}

	err := c.Tick(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("lease refusal was not safely reported: %v", err)
	}
	n := view(t, c, r.ID)
	attempts, attemptsErr := c.Store.SubmissionAttempts(context.Background(), c.scope())
	if attemptsErr != nil {
		t.Fatal(attemptsErr)
	}
	if n.State != "received" || len(attempts) != 0 || f.addCalls != 0 {
		t.Fatalf("refused write changed durable/native state: state=%s attempts=%d adds=%d", n.State, len(attempts), f.addCalls)
	}
}

func TestControllerWriteGuardReleasesFailedGrantWithoutClaim(t *testing.T) {
	c, f, _ := fixture(t, false)
	r := claim(t, c, "lease-failed-grant")
	var releases atomic.Int32
	const secret = "sensitive failed grant body"
	c.BeginWrite = func(parent context.Context) (context.Context, func(), error) {
		return parent, func() { releases.Add(1) }, errors.New(secret)
	}

	err := c.Tick(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("failed grant was not safely reported: %v", err)
	}
	n := view(t, c, r.ID)
	attempts, attemptsErr := c.Store.SubmissionAttempts(context.Background(), c.scope())
	if attemptsErr != nil {
		t.Fatal(attemptsErr)
	}
	if n.State != "received" || len(attempts) != 0 || f.addCalls != 0 || releases.Load() != 1 {
		t.Fatalf("failed grant crossed claim/native/release boundary: state=%s attempts=%d adds=%d releases=%d", n.State, len(attempts), f.addCalls, releases.Load())
	}
}

func TestControllerWriteGuardCanceledBeforeClaim(t *testing.T) {
	c, f, _ := fixture(t, false)
	r := claim(t, c, "lease-canceled-before-claim")
	var releases atomic.Int32
	c.BeginWrite = func(context.Context) (context.Context, func(), error) {
		lease, cancel := context.WithCancel(context.Background())
		cancel()
		return lease, func() { releases.Add(1) }, nil
	}

	err := c.Tick(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lease was not returned: %v", err)
	}
	n := view(t, c, r.ID)
	attempts, attemptsErr := c.Store.SubmissionAttempts(context.Background(), c.scope())
	if attemptsErr != nil {
		t.Fatal(attemptsErr)
	}
	if n.State != "received" || len(attempts) != 0 || f.addCalls != 0 || releases.Load() != 1 {
		t.Fatalf("canceled lease crossed claim/release boundary: state=%s attempts=%d adds=%d releases=%d", n.State, len(attempts), f.addCalls, releases.Load())
	}
}

func TestControllerWriteGuardCanceledAfterClaimNeverCallsNative(t *testing.T) {
	c, f, _ := fixture(t, false)
	r := claim(t, c, "lease-canceled-after-claim")
	ctx := context.Background()
	if err := c.Store.StageSubmissions(ctx, c.scope()); err != nil {
		t.Fatal(err)
	}
	n := view(t, c, r.ID)
	lease, cancel := context.WithCancel(ctx)
	a, claimed, err := c.Store.ClaimSubmissionAttempt(lease, c.scope(), n.ID, "", "add")
	if err != nil || !claimed {
		t.Fatalf("claim setup failed: claimed=%v err=%v", claimed, err)
	}
	c.recovered = true
	cancel()
	if err = c.dispatch(lease, a, n); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lease reached unexpected dispatch result: %v", err)
	}
	attempts, attemptsErr := c.Store.SubmissionAttempts(ctx, c.scope())
	if attemptsErr != nil {
		t.Fatal(attemptsErr)
	}
	if f.addCalls != 0 || len(attempts) != 1 || attempts[0].State != "dispatching" || c.recovered {
		t.Fatalf("canceled lease escaped native guard: adds=%d attempts=%+v recovered=%v", f.addCalls, attempts, c.recovered)
	}
}

func TestControllerWriteGuardReleasesAfterEachFinishedStage(t *testing.T) {
	c, f, _ := fixture(t, true)
	r := claim(t, c, "lease-each-stage")
	var releases atomic.Int32
	var unfinished atomic.Int32
	c.BeginWrite = func(parent context.Context) (context.Context, func(), error) {
		return parent, func() {
			attempts, err := c.Store.SubmissionAttempts(context.Background(), c.scope())
			if err != nil {
				unfinished.Add(1)
				return
			}
			for _, a := range attempts {
				if a.State == "dispatching" {
					unfinished.Add(1)
				}
			}
			releases.Add(1)
		}, nil
	}
	tick(t, c)
	action(t, c, r.ID, "steer")
	tick(t, c)

	attempts, err := c.Store.SubmissionAttempts(context.Background(), c.scope())
	if err != nil {
		t.Fatal(err)
	}
	if f.addCalls != 1 || f.deleteCalls != 1 || f.steerCalls != 1 || releases.Load() != 3 || unfinished.Load() != 0 || len(attempts) != 3 {
		t.Fatalf("lease stages were not independently released: adds=%d deletes=%d steers=%d releases=%d unfinished=%d attempts=%d", f.addCalls, f.deleteCalls, f.steerCalls, releases.Load(), unfinished.Load(), len(attempts))
	}
	for _, a := range attempts {
		if a.State != "confirmed" {
			t.Fatalf("lease released before attempt finish: %+v", a)
		}
	}
}

func TestControllerWriteGuardRefusalAfterDeleteKeepsActionForLaterStage(t *testing.T) {
	c, f, _ := fixture(t, true)
	r := claim(t, c, "lease-stage-refusal")
	tick(t, c)
	var grants atomic.Int32
	var releases atomic.Int32
	c.BeginWrite = func(parent context.Context) (context.Context, func(), error) {
		if grants.Add(1) == 1 {
			return parent, func() { releases.Add(1) }, nil
		}
		return nil, nil, errors.New("sensitive next-stage refusal")
	}
	op := action(t, c, r.ID, "steer")

	err := c.Tick(context.Background())
	if err == nil || strings.Contains(err.Error(), "sensitive next-stage refusal") {
		t.Fatalf("next-stage lease refusal was not safely reported: %v", err)
	}
	attempts, attemptsErr := c.Store.SubmissionAttempts(context.Background(), c.scope())
	if attemptsErr != nil {
		t.Fatal(attemptsErr)
	}
	got, actionErr := c.Store.SubmissionAction(context.Background(), c.scope(), op.ID)
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	if f.deleteCalls != 1 || f.steerCalls != 0 || f.addCalls != 1 || releases.Load() != 1 || len(attempts) != 2 || got.State != "ready" || got.Stage != "steer" {
		t.Fatalf("delete stage was not preserved when next lease refused: delete=%d steer=%d add=%d releases=%d attempts=%d action=%+v", f.deleteCalls, f.steerCalls, f.addCalls, releases.Load(), len(attempts), got)
	}
}

func TestControllerWriteGuardReleasesOnExistingAttemptAndDispatchError(t *testing.T) {
	c, f, _ := fixture(t, false)
	r := claim(t, c, "lease-release-error")
	var releases atomic.Int32
	c.BeginWrite = func(parent context.Context) (context.Context, func(), error) {
		return parent, func() { releases.Add(1) }, nil
	}
	f.addReplyErr = errors.New("sensitive native reply body")
	if err := c.Tick(context.Background()); err == nil || strings.Contains(err.Error(), "sensitive native reply body") {
		t.Fatalf("native error was not safely classified: %v", err)
	}
	if releases.Load() != 1 || f.addCalls != 1 {
		t.Fatalf("dispatch error leaked write lease: releases=%d adds=%d", releases.Load(), f.addCalls)
	}

	// The completed add attempt is an unclaimed replay. It must still acquire
	// and release a lease, without issuing a second native call.
	n := view(t, c, r.ID)
	claimed, err := c.claimAndDispatch(context.Background(), n, "", "add")
	if err != nil || claimed || releases.Load() != 2 || f.addCalls != 1 {
		t.Fatalf("unclaimed replay did not release exactly once: claimed=%v err=%v releases=%d adds=%d", claimed, err, releases.Load(), f.addCalls)
	}
}

func TestControllerWriteGuardStaleClaimReleasesWithoutNativeWrite(t *testing.T) {
	c, f, _ := fixture(t, true)
	r := claim(t, c, "lease-stale")
	tick(t, c)
	op := action(t, c, r.ID, "steer")
	var releases atomic.Int32
	c.BeginWrite = func(parent context.Context) (context.Context, func(), error) {
		return parent, func() { releases.Add(1) }, nil
	}
	n := view(t, c, r.ID)
	claimed, err := c.claimAndDispatch(context.Background(), n, op.ID, "restore")
	if !errors.Is(err, store.ErrSubmissionStale) {
		t.Fatalf("stale claim was not rejected: claimed=%v err=%v", claimed, err)
	}
	if claimed || releases.Load() != 1 || f.steerCalls != 0 || f.deleteCalls != 0 {
		t.Fatalf("stale claim crossed native/release boundary: claimed=%v releases=%d steer=%d delete=%d", claimed, releases.Load(), f.steerCalls, f.deleteCalls)
	}
}
