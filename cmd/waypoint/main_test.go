package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

func TestForkCheckpointsConcurrentlyStartsTogetherAndPreservesOrder(t *testing.T) {
	const count = 3
	started := make(chan int, count)
	completed := make(chan int, count)
	release := make([]chan struct{}, count)
	for i := range release {
		release[i] = make(chan struct{})
	}

	done := make(chan []forkResult, 1)
	go func() {
		done <- forkCheckpointsConcurrently(count, func(i int) (*waypoint.Fork, error) {
			started <- i
			<-release[i]
			completed <- i
			return &waypoint.Fork{ID: fmt.Sprintf("fork-%d", i)}, nil
		})
	}()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for i := 0; i < count; i++ {
		select {
		case <-started:
		case <-timer.C:
			for _, gate := range release {
				close(gate)
			}
			<-done
			t.Fatalf("only %d/%d forks started before one was allowed to finish", i, count)
		}
	}

	for i := count - 1; i >= 0; i-- {
		close(release[i])
		if got := <-completed; got != i {
			t.Fatalf("fork completion order = %d, want %d", got, i)
		}
	}
	results := <-done
	for i, result := range results {
		if result.err != nil {
			t.Fatalf("result %d error = %v, want nil", i, result.err)
		}
		wantID := fmt.Sprintf("fork-%d", i)
		if result.fork.ID != wantID {
			t.Fatalf("result %d fork ID = %q, want %q", i, result.fork.ID, wantID)
		}
	}
}

func TestForkCheckpointsConcurrentlyCollectsPartialFailures(t *testing.T) {
	wantErr := errors.New("restore failed")
	results := forkCheckpointsConcurrently(3, func(i int) (*waypoint.Fork, error) {
		if i == 1 {
			return nil, wantErr
		}
		return &waypoint.Fork{ID: fmt.Sprintf("fork-%d", i)}, nil
	})

	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	if results[0].fork == nil || results[2].fork == nil {
		t.Fatal("successful results were not preserved")
	}
	if !errors.Is(results[1].err, wantErr) {
		t.Fatalf("result 1 error = %v, want %v", results[1].err, wantErr)
	}
}

func TestForkCheckpointsConcurrentlyHandlesSmallCounts(t *testing.T) {
	// count==1 is the default `fork` path, and count==0 must return rather
	// than block on a WaitGroup nothing will ever release.
	for _, count := range []int{0, 1} {
		t.Run(fmt.Sprintf("count=%d", count), func(t *testing.T) {
			calls := 0
			results := forkCheckpointsConcurrently(count, func(i int) (*waypoint.Fork, error) {
				calls++
				return &waypoint.Fork{ID: fmt.Sprintf("fork-%d", i)}, nil
			})
			if len(results) != count {
				t.Fatalf("len(results) = %d, want %d", len(results), count)
			}
			if calls != count {
				t.Fatalf("create called %d times, want %d", calls, count)
			}
		})
	}
}
