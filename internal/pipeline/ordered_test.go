package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func countSource(n int) func(yield func(int) bool) error {
	return func(yield func(int) bool) error {
		for i := 0; i < n; i++ {
			if !yield(i) {
				return nil
			}
		}
		return nil
	}
}

func identityWorker() (func(int) (int, error), error) {
	return func(v int) (int, error) { return v, nil }, nil
}

// assertNoLeaks waits for the goroutine count to return to baseline.
func assertNoLeaks(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d > baseline %d", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOrderedPreservesOrder(t *testing.T) {
	for _, workers := range []int{1, 4, 32} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			const n = 500
			newWorker := func() (func(int) (int, error), error) {
				rng := rand.New(rand.NewSource(rand.Int63()))
				return func(v int) (int, error) {
					time.Sleep(time.Duration(rng.Intn(300)) * time.Microsecond)
					return v * 2, nil
				}, nil
			}
			next := 0
			err := Ordered(context.Background(), workers, countSource(n), newWorker, func(out int) error {
				if out != next*2 {
					return fmt.Errorf("got %d, want %d", out, next*2)
				}
				next++
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if next != n {
				t.Fatalf("sink saw %d items, want %d", next, n)
			}
		})
	}
}

func TestOrderedStopsOnWorkerError(t *testing.T) {
	baseline := runtime.NumGoroutine()
	boom := errors.New("worker boom")
	newWorker := func() (func(int) (int, error), error) {
		return func(v int) (int, error) {
			if v == 10 {
				return 0, boom
			}
			return v, nil
		}, nil
	}
	err := Ordered(context.Background(), 4, countSource(1000), newWorker, func(int) error { return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	assertNoLeaks(t, baseline)
}

func TestOrderedStopsOnNewWorkerError(t *testing.T) {
	baseline := runtime.NumGoroutine()
	boom := errors.New("factory boom")
	newWorker := func() (func(int) (int, error), error) { return nil, boom }
	err := Ordered(context.Background(), 4, countSource(1000), newWorker, func(int) error { return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	assertNoLeaks(t, baseline)
}

func TestOrderedStopsOnSinkError(t *testing.T) {
	baseline := runtime.NumGoroutine()
	boom := errors.New("sink boom")
	count := 0
	err := Ordered(context.Background(), 4, countSource(1000), identityWorker, func(int) error {
		count++
		if count == 5 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	assertNoLeaks(t, baseline)
}

func TestOrderedStopsOnSourceError(t *testing.T) {
	baseline := runtime.NumGoroutine()
	boom := errors.New("source boom")
	src := func(yield func(int) bool) error {
		for i := 0; i < 20; i++ {
			if !yield(i) {
				return nil
			}
		}
		return boom
	}
	err := Ordered(context.Background(), 4, src, identityWorker, func(int) error { return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
	assertNoLeaks(t, baseline)
}

func TestOrderedCancel(t *testing.T) {
	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	count := 0
	err := Ordered(ctx, 4, countSource(1_000_000), identityWorker, func(int) error {
		count++
		if count == 10 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	assertNoLeaks(t, baseline)

	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	err = Ordered(ctx, 4, countSource(10), identityWorker, func(int) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled: got %v, want context.Canceled", err)
	}
	assertNoLeaks(t, baseline)
}

func TestOrderedBounded(t *testing.T) {
	for _, workers := range []int{1, 4, 8} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			var produced, consumed, maxLead atomic.Int64
			src := func(yield func(int) bool) error {
				for i := 0; i < 300; i++ {
					p := produced.Add(1)
					if lead := p - consumed.Load(); lead > maxLead.Load() {
						maxLead.Store(lead)
					}
					if !yield(i) {
						return nil
					}
				}
				return nil
			}
			err := Ordered(context.Background(), workers, src, identityWorker, func(int) error {
				time.Sleep(200 * time.Microsecond) // slow sink
				consumed.Add(1)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// Channel capacities, items held by workers, and the one the
			// source is blocked on yielding.
			limit := int64(2*workers + workers + 1)
			if got := maxLead.Load(); got > limit {
				t.Fatalf("source lead %d exceeds %d", got, limit)
			}
		})
	}
}
