package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// `kopicode serve --listen <socket>` is the second transport beside stdio
// (ADR-0030 decisions 1 and 2).
//
// Over stdio the process is its client's child: when stdin closes, so do its
// sessions. A listening process is not, and that is the point: a client that
// restarts reconnects and finds its sessions where it left them, and a session
// a client started keeps running with nobody attached. What changes is only who
// is on the other end:
//
//   - One client at a time. A new connection closes the one before it. The old
//     client is not told why; a supervisor that restarted does not have one.
//   - Requests the server is waiting on, consent.request and ask.request, are
//     sent again to the new client under the same ids, so the turn they hold up
//     is not stranded. One nobody answers expires as it always has.
//   - Notifications sent while no client is connected are dropped. The record is
//     the journal; a reconnecting client reads what it missed with session.events
//     and server.sessions.
//   - A response goes to the connection its request came from. A turn that ends
//     after its client was replaced answers to nobody, since the new client
//     could not tell which of its own requests an old id meant.
//
// The wire on the socket is the stdio wire, line for line.
//
// # The socket is the access control
//
// It is created mode 0600 and is reachable only by its owner, which is the same
// promise stdio makes by being the child's own pipe. There is no TCP listener
// (ADR-0030 rejected it) and nothing to authenticate.

// serveListen runs a listening serve process until ctx ends, then closes every
// session and removes the socket. A path another live process holds is refused,
// and a stale one left by a crash is replaced.
func serveListen(ctx context.Context, path string, stderr io.Writer, base engine.Options, consentTimeout time.Duration, idle time.Duration) int {
	ln, release, err := listenSocket(path)
	if err != nil {
		say(stderr, "kopicode: serve --listen %s: %v\n", path, err)
		return exitUsage
	}
	// The manager's own context is never the shutdown signal: closing a session
	// writes its SessionEnded, and a cancelled context would refuse the write.
	// Manager.Shutdown cancels what is running before it closes anything.
	s := newServer(context.Background(), stderr, base, consentTimeout)

	go func() {
		select {
		case <-ctx.Done():
		case <-s.shutdown:
		}
		_ = ln.Close()
	}()
	go s.watchIdle(ctx, idle)
	say(stderr, "kopicode: serve listening on %s\n", path)

	var readers sync.WaitGroup
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || s.stopping() || errors.Is(err, net.ErrClosed) {
				break
			}
			say(stderr, "kopicode: accepting a connection: %v\n", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		c := newConn(nc, nc)
		s.replace(c)
		readers.Add(1)
		go func() {
			defer readers.Done()
			s.readLines(nc, c)
			c.close()
		}()
	}

	s.current().close()
	readers.Wait()
	s.mgr.Shutdown()
	release()
	return exitSuccess
}

// defaultIdleTimeout is how long a listening serve waits with nobody connected
// and no session open before it exits (ADR-0030 decision 5). Long enough that a
// supervisor restarting a client does not lose the process, short enough that
// an orphan does not run for ever.
const defaultIdleTimeout = 30 * time.Minute

// requestShutdown ends the process's run loop: it is safe to call more than
// once and from any goroutine.
func (s *server) requestShutdown() { s.shutdownOnce.Do(func() { close(s.shutdown) }) }

func (s *server) stopping() bool {
	select {
	case <-s.shutdown:
		return true
	default:
		return false
	}
}

// watchIdle asks the process to exit once it has had no client and no open
// session for idle. A client that is connected but has opened nothing keeps it
// alive, and so does a session nobody is attached to: that is the point of
// listening. idle <= 0 never exits. It returns when ctx ends or a shutdown has
// been requested.
func (s *server) watchIdle(ctx context.Context, idle time.Duration) {
	if idle <= 0 {
		return
	}
	t := time.NewTicker(min(max(idle/4, 5*time.Millisecond), 5*time.Second))
	defer t.Stop()
	var since time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.shutdown:
			return
		case now := <-t.C:
			if !s.current().isClosed() || s.mgr.Open() > 0 {
				since = time.Time{}
				continue
			}
			if since.IsZero() {
				since = now
				continue
			}
			if now.Sub(since) >= idle {
				say(s.stderr, "kopicode: serve --listen: no client and no open session for %s, exiting\n", idle)
				s.requestShutdown()
				return
			}
		}
	}
}

// replace makes c the one client, closing the one before it. The old
// connection is closed first and without the server's lock, so a write stuck on
// a client that stopped reading cannot hold the replacement up.
func (s *server) replace(c *conn) {
	s.current().close()
	s.attach(c)
}

// errSocketHeld is returned when another live process owns the socket path.
var errSocketHeld = errors.New("another kopicode serve holds this socket")

func socketHeld(path string) error {
	return fmt.Errorf("%w (%s)", errSocketHeld, path)
}
