package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
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

// Once the context is done, a worker must not start the jobs that were
// already queued for it.
func TestOrderedWorkersStopAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	newWorker := func() (func(int) (int, error), error) {
		return func(v int) (int, error) {
			if calls.Add(1) == 1 {
				time.Sleep(50 * time.Millisecond) // let the queue fill
				cancel()
			}
			return v, nil
		}, nil
	}
	err := Ordered(ctx, 1, countSource(100), newWorker, func(int) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("worker ran %d jobs, want 1: the rest were queued when the context ended", n)
	}
}

// Results that are ready when the context ends must not reach the sink.
func TestOrderedSinkStopsAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newWorker := func() (func(int) (int, error), error) {
		return func(v int) (int, error) {
			if v == 0 {
				time.Sleep(50 * time.Millisecond) // the others are ready first
			}
			return v, nil
		}, nil
	}
	var sunk int
	err := Ordered(ctx, 4, countSource(4), newWorker, func(int) error {
		sunk++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if sunk != 1 {
		t.Errorf("sink ran %d times, want 1: the rest were ready when the context ended", sunk)
	}
}

// pipelineGoroutines returns the stacks of live goroutines that run code from
// this package, apart from the test's own.
func pipelineGoroutines() []string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var found []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "internal/pipeline.Ordered") && !strings.Contains(g, "testing.tRunner") {
			found = append(found, g)
		}
	}
	return found
}

// assertNoPipelineGoroutines fails if any goroutine started by Ordered is
// still running once it had time to wind down.
func assertNoPipelineGoroutines(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		left := pipelineGoroutines()
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d pipeline goroutines leaked:\n%s", len(left), strings.Join(left, "\n\n"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// run calls Ordered and returns what it returned, or the value that it
// panicked with in the caller.
func run(ctx context.Context, workers int, in func(func(int) bool) error,
	newWorker func() (func(int) (int, error), error), sink func(int) error) (err error, panicked any) {
	defer func() { panicked = recover() }()
	return Ordered(ctx, workers, in, newWorker, sink), nil
}

func TestOrderedNoLeaks(t *testing.T) {
	boom := errors.New("boom")
	nop := func(int) error { return nil }
	modes := map[string]func(ctx context.Context, cancel func()) (error, any){
		"success": func(ctx context.Context, _ func()) (error, any) {
			return run(ctx, 4, countSource(100), identityWorker, nop)
		},
		"empty": func(ctx context.Context, _ func()) (error, any) {
			return run(ctx, 4, countSource(0), identityWorker, nop)
		},
		"source error": func(ctx context.Context, _ func()) (error, any) {
			src := func(yield func(int) bool) error {
				for i := 0; i < 10 && yield(i); i++ {
				}
				return boom
			}
			return run(ctx, 4, src, identityWorker, nop)
		},
		"worker error": func(ctx context.Context, _ func()) (error, any) {
			w := func() (func(int) (int, error), error) {
				return func(v int) (int, error) {
					if v == 7 {
						return 0, boom
					}
					return v, nil
				}, nil
			}
			return run(ctx, 4, countSource(1000), w, nop)
		},
		"newWorker error": func(ctx context.Context, _ func()) (error, any) {
			w := func() (func(int) (int, error), error) { return nil, boom }
			return run(ctx, 4, countSource(1000), w, nop)
		},
		"sink error": func(ctx context.Context, _ func()) (error, any) {
			return run(ctx, 4, countSource(1000), identityWorker, func(v int) error {
				if v == 3 {
					return boom
				}
				return nil
			})
		},
		"cancel": func(ctx context.Context, cancel func()) (error, any) {
			return run(ctx, 4, countSource(1_000_000), identityWorker, func(v int) error {
				if v == 50 {
					cancel()
				}
				return nil
			})
		},
		"source panic": func(ctx context.Context, _ func()) (error, any) {
			src := func(yield func(int) bool) error {
				yield(0)
				panic(boom)
			}
			return run(ctx, 4, src, identityWorker, nop)
		},
		"worker panic": func(ctx context.Context, _ func()) (error, any) {
			w := func() (func(int) (int, error), error) {
				return func(v int) (int, error) {
					if v == 7 {
						panic(boom)
					}
					return v, nil
				}, nil
			}
			return run(ctx, 4, countSource(1000), w, nop)
		},
		"newWorker panic": func(ctx context.Context, _ func()) (error, any) {
			w := func() (func(int) (int, error), error) { panic(boom) }
			return run(ctx, 4, countSource(1000), w, nop)
		},
		"sink panic": func(ctx context.Context, _ func()) (error, any) {
			return run(ctx, 4, countSource(1000), identityWorker, func(v int) error {
				if v == 3 {
					panic(boom)
				}
				return nil
			})
		},
	}
	wantPanic := map[string]bool{"source panic": true, "worker panic": true, "newWorker panic": true, "sink panic": true}
	wantErr := map[string]error{
		"success": nil, "empty": nil, "source error": boom, "worker error": boom, "newWorker error": boom,
		"sink error": boom, "cancel": context.Canceled, "source panic": nil, "worker panic": nil,
		"newWorker panic": nil, "sink panic": nil,
	}
	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			for range 20 {
				ctx, cancel := context.WithCancel(context.Background())
				err, panicked := mode(ctx, cancel)
				cancel()
				if wantPanic[name] {
					if panicked != boom {
						t.Fatalf("panic value = %v, want the original %v", panicked, boom)
					}
				} else if panicked != nil {
					t.Fatalf("unexpected panic %v", panicked)
				} else if !errors.Is(err, wantErr[name]) {
					t.Fatalf("err = %v, want %v", err, wantErr[name])
				}
				// Ordered must not return while a goroutine of its own runs.
				if left := pipelineGoroutines(); len(left) != 0 {
					t.Fatalf("%d pipeline goroutines running after return:\n%s", len(left), strings.Join(left, "\n\n"))
				}
			}
			assertNoPipelineGoroutines(t)
		})
	}
}
