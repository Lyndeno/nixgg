package helper

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tbereknyei/nixgg/internal/rpc"
)

// TestPoolGetPutReuse verifies Put makes a connection available to a
// subsequent Get without opening a new one — Pool's whole point.
func TestPoolGetPutReuse(t *testing.T) {
	sock, stop := fakeDaemon(t)
	defer stop()

	var dials int32
	p := newPoolWithDialer(2, func() (*rpc.Conn, error) {
		atomic.AddInt32(&dials, 1)
		return rpc.Dial("unix://" + sock)
	})

	c1, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	p.Put(c1)

	c2, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	p.Put(c2)

	if got := atomic.LoadInt32(&dials); got != 1 {
		t.Fatalf("dials = %d, want 1 (second Get should have reused the idle connection)", got)
	}
}

// TestPoolOpensUpToSize verifies the pool opens a fresh connection
// per concurrent Get up to size, not fewer (undersized = needless
// serialization) and not more (oversized = wasted daemon handshakes —
// see Pool's own docstring on why NIX_BUILD_CORES-sized is correct).
func TestPoolOpensUpToSize(t *testing.T) {
	sock, stop := fakeDaemon(t)
	defer stop()

	var dials int32
	const size = 3
	p := newPoolWithDialer(size, func() (*rpc.Conn, error) {
		atomic.AddInt32(&dials, 1)
		return rpc.Dial("unix://" + sock)
	})

	conns := make([]*rpc.Conn, size)
	var wg sync.WaitGroup
	for i := 0; i < size; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := p.Get()
			if err != nil {
				t.Errorf("Get %d: %v", i, err)
				return
			}
			conns[i] = c
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&dials); got != size {
		t.Fatalf("dials = %d, want %d", got, size)
	}
	for _, c := range conns {
		if c != nil {
			p.Put(c)
		}
	}
}

// TestPoolBlocksAtCapacity verifies a Get beyond size blocks until a
// Put frees capacity, rather than opening an (size+1)th connection —
// the property that prevents a helper from opening unbounded daemon
// connections under a heavily parallel `make -j` build.
func TestPoolBlocksAtCapacity(t *testing.T) {
	sock, stop := fakeDaemon(t)
	defer stop()

	var dials int32
	p := newPoolWithDialer(1, func() (*rpc.Conn, error) {
		atomic.AddInt32(&dials, 1)
		return rpc.Dial("unix://" + sock)
	})

	c1, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	blockedReturned := make(chan struct{})
	go func() {
		c2, err := p.Get()
		if err != nil {
			t.Errorf("blocked Get: %v", err)
			return
		}
		p.Put(c2)
		close(blockedReturned)
	}()

	select {
	case <-blockedReturned:
		t.Fatal("second Get returned before capacity was freed — pool did not block at size=1")
	case <-time.After(50 * time.Millisecond):
		// Expected: still blocked.
	}

	p.Put(c1) // frees capacity

	select {
	case <-blockedReturned:
		// Expected: unblocks once Put runs.
	case <-time.After(2 * time.Second):
		t.Fatal("second Get never returned after Put freed capacity")
	}

	if got := atomic.LoadInt32(&dials); got != 1 {
		t.Fatalf("dials = %d, want 1 (blocked Get should reuse the Put connection, not open a second)", got)
	}
}

// TestPoolDropReleasesCapacityForNewDial verifies Drop (used when a
// call on a connection failed) frees capacity for a genuinely NEW
// dial rather than leaving the slot permanently consumed — a
// connection whose wire state is desynchronised must not be reused
// via Put, but the capacity it held must still become available.
func TestPoolDropReleasesCapacityForNewDial(t *testing.T) {
	sock, stop := fakeDaemon(t)
	defer stop()

	var dials int32
	p := newPoolWithDialer(1, func() (*rpc.Conn, error) {
		atomic.AddInt32(&dials, 1)
		return rpc.Dial("unix://" + sock)
	})

	c1, err := p.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	p.Drop(c1)

	c2, err := p.Get()
	if err != nil {
		t.Fatalf("Get after Drop: %v", err)
	}
	p.Put(c2)

	if got := atomic.LoadInt32(&dials); got != 2 {
		t.Fatalf("dials = %d, want 2 (Drop must free capacity for a fresh dial, not reuse the dropped connection)", got)
	}
}

// TestPoolDialFailureReleasesCapacity verifies a failed Dial doesn't
// permanently consume a capacity slot — otherwise a transient daemon
// hiccup would degrade the pool's effective size forever.
func TestPoolDialFailureReleasesCapacity(t *testing.T) {
	p := newPoolWithDialer(1, func() (*rpc.Conn, error) {
		return nil, errDialAlwaysFails
	})

	if _, err := p.Get(); err == nil {
		t.Fatal("expected dial failure")
	}
	if _, err := p.Get(); err == nil {
		t.Fatal("expected second dial failure — capacity should have been released after the first")
	}

	p.mu.Lock()
	outstanding := p.outstanding
	p.mu.Unlock()
	if outstanding != 0 {
		t.Fatalf("outstanding = %d, want 0 after two failed dials", outstanding)
	}
}

type dialError string

func (e dialError) Error() string { return string(e) }

const errDialAlwaysFails = dialError("dial always fails")
