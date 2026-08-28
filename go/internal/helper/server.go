package helper

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
)

// Server is the helper process itself: listens on a local Unix
// socket, accepts one connection per relayed op (mirroring how a
// shim invocation makes exactly one call before exiting — no need
// for the server to support multiple requests per client connection),
// and dispatches to a Pool of already-handshaken daemon connections.
type Server struct {
	pool *Pool
	ln   net.Listener
}

// Listen creates the helper's own socket at path (removing a stale
// one from a previous run first — a leftover socket file from a
// killed helper would otherwise make bind fail with "address already
// in use" on the next build) and starts accepting connections.
// remote is the real Nix daemon's own NIX_REMOTE value the pool
// dials; poolSize should track NIX_BUILD_CORES (see Pool's own
// docstring for why).
func Listen(path, remote string, poolSize int) (*Server, error) {
	_ = os.Remove(path) // best-effort; bind below is the real check
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("helper: listen on %s: %w", path, err)
	}
	return &Server{pool: NewPool(remote, poolSize), ln: ln}, nil
}

// Serve accepts connections until the listener is closed (via
// Shutdown), handling each on its own goroutine. Blocks; callers run
// it in a goroutine or as the helper process's main loop.
func (s *Server) Serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return // listener closed — normal shutdown path
		}
		go s.handleOne(c)
	}
}

// Shutdown closes the listener (causing Serve to return) and every
// pooled daemon connection. Does not wait for in-flight handleOne
// goroutines — callers that need that should stop calling Listen's
// socket first (nothing new can connect) and give in-flight ops a
// moment to finish before the process exits; nixgg's own postBuild
// hook does this by design (see doc.go), not this package.
func (s *Server) Shutdown() {
	s.ln.Close()
	s.pool.CloseAll()
}

// handleOne reads exactly one request off c, dispatches it, writes
// exactly one response, and closes c. One request per connection,
// not a persistent client session — matches every real caller (a
// shim process makes one call then exits).
func (s *Server) handleOne(c net.Conn) {
	defer c.Close()

	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)

	var req request
	if err := readFrame(r, &req); err != nil {
		log.Printf("nixgg-helper: read request: %v", err)
		return
	}

	resp := s.dispatch(&req)

	if err := writeFrame(w, &resp); err != nil {
		log.Printf("nixgg-helper: write response: %v", err)
	}
}

func (s *Server) dispatch(req *request) response {
	conn, err := s.pool.Get()
	if err != nil {
		return response{Err: err.Error()}
	}

	switch req.Op {
	case "AddDerivation":
		path, err := conn.AddDerivation(req.Name, req.Contents, req.Refs)
		if err != nil {
			s.pool.Drop(conn)
			return response{Err: err.Error()}
		}
		s.pool.Put(conn)
		return response{StorePath: path}

	case "AddToStoreScanning":
		path, err := conn.AddToStoreScanning(req.Name, req.NarDump)
		if err != nil {
			s.pool.Drop(conn)
			return response{Err: err.Error()}
		}
		s.pool.Put(conn)
		return response{StorePath: path}

	case "SubmitOutput":
		err := conn.SubmitOutput(req.DrvPath, req.Output)
		if err != nil {
			s.pool.Drop(conn)
			return response{Err: err.Error()}
		}
		s.pool.Put(conn)
		return response{}

	default:
		s.pool.Put(conn) // never touched; op was rejected before any daemon call
		return response{Err: fmt.Sprintf("helper: unknown op %q", req.Op)}
	}
}
