package tools

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minhquan2904/loki-via-grafana-mcp/internal/grafana"
)

// blockingGrafana holds every ListDatasources call until release is closed.
type blockingGrafana struct {
	fakeGrafana
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (b *blockingGrafana) ListDatasources(ctx context.Context) ([]grafana.Datasource, error) {
	if b.calls.Add(1) == 1 {
		close(b.started)
	}
	select {
	case <-b.release:
		return lokiList(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestDSCache_ExpiresWithInjectedClock(t *testing.T) {
	now := testNow
	fake := &fakeGrafana{list: lokiList()}
	cache := &dsCache{ttl: time.Minute, now: func() time.Time { return now }}

	for _, step := range []time.Duration{0, 59 * time.Second, 2 * time.Second} {
		now = now.Add(step)
		if _, err := cache.datasources(context.Background(), fake); err != nil {
			t.Fatal(err)
		}
	}
	// t0 fetch, t0+59s cached, t0+61s expired -> second fetch.
	if calls := fake.listCallCount(); calls != 2 {
		t.Fatalf("list calls = %d, want 2", calls)
	}
}

func TestDSCache_ConcurrentCallersShareOneFetch(t *testing.T) {
	g := &blockingGrafana{started: make(chan struct{}), release: make(chan struct{})}
	cache := &dsCache{ttl: time.Minute, now: time.Now}

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.datasources(context.Background(), g)
			errs <- err
		}()
	}
	<-g.started
	time.Sleep(50 * time.Millisecond) // let the others queue behind the fetch
	close(g.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls := g.calls.Load(); calls != 1 {
		t.Fatalf("ListDatasources ran %d times, want 1", calls)
	}
}

func TestDSCache_WaiterHonoursItsContext(t *testing.T) {
	g := &blockingGrafana{started: make(chan struct{}), release: make(chan struct{})}
	defer close(g.release)
	cache := &dsCache{ttl: time.Minute, now: time.Now}

	go func() { _, _ = cache.datasources(context.Background(), g) }()
	<-g.started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := cache.datasources(ctx, g)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the waiter's deadline", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waiter was stuck for %s behind the fetch", waited)
	}
}
