// Package pipeline provides concurrency helpers shared by compression,
// verification and extraction.
package pipeline

import (
	"context"
	"sync"

	"golang.org/x/sync/errgroup"
)

type job[In any] struct {
	seq int
	in  In
}

type result[Out any] struct {
	seq int
	out Out
}

// Ordered streams the items produced by in through workers goroutines and
// hands the results to sink in input order, whatever the worker count.
//
// Each worker gets its own state from newWorker, which is called once per
// worker from the worker's goroutine. Sink is only ever called from a single
// goroutine. At most 3×workers items are in flight at once, which bounds the
// memory held by the pipeline and by the reorder buffer.
//
// The first error from the source, a worker, newWorker or sink cancels the
// pipeline and is returned. If ctx is cancelled first, its error is returned.
// All goroutines have exited when Ordered returns.
func Ordered[In, Out any](
	ctx context.Context,
	workers int,
	in func(yield func(In) bool) error,
	newWorker func() (func(In) (Out, error), error),
	sink func(Out) error,
) error {
	if workers < 1 {
		workers = 1
	}
	g, ctx := errgroup.WithContext(ctx)
	jobs := make(chan job[In], 2*workers)
	results := make(chan result[Out], 2*workers)
	// tokens caps the items between the source and the sink, so the reorder
	// buffer cannot grow while a slow item holds up the sequence.
	tokens := make(chan struct{}, 3*workers)

	g.Go(func() error {
		defer close(jobs)
		seq := 0
		err := in(func(v In) bool {
			select {
			case tokens <- struct{}{}:
			case <-ctx.Done():
				return false
			}
			select {
			case jobs <- job[In]{seq, v}:
				seq++
				return true
			case <-ctx.Done():
				return false
			}
		})
		if err != nil {
			return err
		}
		return ctx.Err()
	})

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		g.Go(func() error {
			defer wg.Done()
			work, err := newWorker()
			if err != nil {
				return err
			}
			for j := range jobs {
				// Jobs that were queued before the cancel are not started.
				if err := ctx.Err(); err != nil {
					return err
				}
				out, err := work(j.in)
				if err != nil {
					return err
				}
				select {
				case results <- result[Out]{j.seq, out}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}
	g.Go(func() error {
		wg.Wait()
		close(results)
		return nil
	})

	g.Go(func() error {
		pending := make(map[int]Out)
		next := 0
		for {
			select {
			case r, ok := <-results:
				if !ok {
					return ctx.Err()
				}
				pending[r.seq] = r.out
				for {
					out, ok := pending[next]
					if !ok {
						break
					}
					// Results that were ready before the cancel are not written.
					if err := ctx.Err(); err != nil {
						return err
					}
					delete(pending, next)
					next++
					err := sink(out)
					<-tokens
					if err != nil {
						return err
					}
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})

	return g.Wait()
}
