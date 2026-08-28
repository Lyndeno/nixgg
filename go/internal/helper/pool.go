package helper

import (
	"fmt"
	"sync"

	"github.com/tbereknyei/nixgg/internal/rpc"
)

// Pool holds a small, fixed-size set of already-handshaken
// internal/rpc.Conns to the real Nix daemon, checked out one at a
// time and returned when done.
//
// A single shared Conn would serialize every op across a `make -j`
// build: the Nix worker protocol is strictly request/response per
// connection (confirmed against the real Nix C++ client's own
// RemoteStore, which hands out connections from a Pool<Connection>
// rather than multiplexing ops over one socket — see
// src/libstore/include/nix/store/remote-store.hh). A pool sized to
// the build's own parallelism lets N concurrent shim calls proceed
// concurrently, each still paying the daemon handshake only once
// (at whichever connection's first checkout) rather than once per
// call.
type Pool struct {
	size int
	dial func() (*rpc.Conn, error)

	mu   sync.Mutex
	cond *sync.Cond
	idle []*rpc.Conn
	// outstanding counts every connection currently open, whether
	// idle or checked out — bounds total daemon connections, not just
	// idle capacity.
	outstanding int
}

// NewPool creates a pool that opens connections to remote (a
// "unix://<path>" NIX_REMOTE value, same as internal/rpc.Dial takes)
// lazily, up to size total. size should track the build's own
// parallelism (NIX_BUILD_CORES) — oversizing wastes daemon-side
// handshake work that no concurrent caller will use; undersizing
// serializes calls beyond what the daemon itself would bottleneck on.
func NewPool(remote string, size int) *Pool {
	return newPoolWithDialer(size, func() (*rpc.Conn, error) { return rpc.Dial(remote) })
}

// newPoolWithDialer is NewPool's real constructor, parameterized over
// the dial function so tests can inject a fake instead of a real
// daemon handshake (see pool_test.go).
func newPoolWithDialer(size int, dial func() (*rpc.Conn, error)) *Pool {
	if size < 1 {
		size = 1
	}
	p := &Pool{size: size, dial: dial}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Get checks out a connection: reuses an idle one if available, opens
// a new one (paying its handshake once) if the pool has spare
// capacity, or blocks until another caller Puts/Drops one otherwise.
func (p *Pool) Get() (*rpc.Conn, error) {
	p.mu.Lock()
	for {
		if n := len(p.idle); n > 0 {
			c := p.idle[n-1]
			p.idle = p.idle[:n-1]
			p.mu.Unlock()
			return c, nil
		}
		if p.outstanding < p.size {
			p.outstanding++
			p.mu.Unlock()
			c, err := p.dial()
			if err != nil {
				p.mu.Lock()
				p.outstanding--
				p.mu.Unlock()
				p.cond.Signal() // wake another waiter to try opening instead
				return nil, fmt.Errorf("helper: pool dial: %w", err)
			}
			return c, nil
		}
		p.cond.Wait() // releases p.mu while blocked, re-acquires on wake
	}
}

// Put returns a healthy connection to the pool for reuse. Callers
// must not use c again after calling Put.
func (p *Pool) Put(c *rpc.Conn) {
	p.mu.Lock()
	p.idle = append(p.idle, c)
	p.mu.Unlock()
	p.cond.Signal()
}

// Drop discards a connection instead of returning it — for a
// connection whose call failed and whose wire state is no longer
// trustworthy for reuse (a daemon error mid-op can leave subsequent
// reads desynchronised).
func (p *Pool) Drop(c *rpc.Conn) {
	c.Close()
	p.mu.Lock()
	p.outstanding--
	p.mu.Unlock()
	p.cond.Signal() // frees capacity for a new Dial
}

// CloseAll closes every idle connection. Checked-out connections that
// are never returned via Put/Drop leak — callers are expected to
// always do one or the other before the helper process exits.
func (p *Pool) CloseAll() {
	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, c := range idle {
		c.Close()
	}
}
