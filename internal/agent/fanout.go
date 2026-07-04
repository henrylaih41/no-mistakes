package agent

import (
	"context"
	"fmt"
	"sync"
)

// FanOutResult pairs a reviewer agent with the outcome of running it. For a
// completed agent exactly one of Result or Err is non-nil; Agent is always the
// input agent for that slot so callers can attribute the outcome.
type FanOutResult struct {
	Agent  Agent
	Result *Result
	Err    error
}

// FanOut runs the same RunOpts against every agent concurrently and returns one
// result slot per input agent, in input order. maxParallel bounds the number of
// agents running at once; maxParallel <= 0 means unbounded. A per-agent failure
// is captured in that agent's slot (Err) and never aborts the others - FanOut
// itself returns no top-level error. Each goroutine writes only its own
// preallocated slot, so the results slice needs no additional synchronization
// beyond the WaitGroup.
func FanOut(ctx context.Context, agents []Agent, opts RunOpts, maxParallel int) []FanOutResult {
	return fanOut(ctx, agents, opts, maxParallel, nil)
}

// FanOutCancelOnError is like FanOut, but cancels remaining work after the
// first slot records a non-nil Err. The per-slot Result/Err contract is
// unchanged; siblings that observe the cancellation record their context error.
func FanOutCancelOnError(ctx context.Context, agents []Agent, opts RunOpts, maxParallel int) []FanOutResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return fanOut(ctx, agents, opts, maxParallel, cancel)
}

func fanOut(ctx context.Context, agents []Agent, opts RunOpts, maxParallel int, cancelOnError context.CancelFunc) []FanOutResult {
	results := make([]FanOutResult, len(agents))
	if len(agents) == 0 {
		return results
	}

	// A buffered semaphore caps concurrency. nil means unbounded.
	var sem chan struct{}
	if maxParallel > 0 {
		sem = make(chan struct{}, maxParallel)
	}

	var wg sync.WaitGroup
	var cancelOnce sync.Once
	recordErr := func(i int, err error) {
		results[i].Err = err
		if err != nil && cancelOnError != nil {
			cancelOnce.Do(cancelOnError)
		}
	}
	for i, ag := range agents {
		wg.Add(1)
		go func(i int, ag Agent) {
			defer wg.Done()
			results[i].Agent = ag
			defer func() {
				if r := recover(); r != nil {
					// A panic on this child goroutine bypasses the pipeline
					// goroutine's recover and would kill the shared daemon.
					recordErr(i, fmt.Errorf("reviewer %s panicked: %v", ag.Name(), r))
				}
			}()
			// Honor cancellation while queued on the semaphore...
			if sem != nil {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					recordErr(i, ctx.Err())
					return
				}
			}
			// ...and again before invoking Run, so an already-cancelled ctx
			// short-circuits instead of spawning the agent.
			if err := ctx.Err(); err != nil {
				recordErr(i, err)
				return
			}
			res, err := ag.Run(ctx, opts)
			results[i].Result = res
			recordErr(i, err)
		}(i, ag)
	}
	wg.Wait()
	return results
}
