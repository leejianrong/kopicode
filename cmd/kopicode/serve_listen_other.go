//go:build !unix

package main

import (
	"errors"
	"net"
)

// listenSocket is unix-only: the socket's mode and the lock that tells a live
// holder from a stale file are unix mechanisms. Stdio works everywhere.
func listenSocket(string) (net.Listener, func(), error) {
	return nil, nil, errors.New("--listen needs a unix platform; use stdio")
}
