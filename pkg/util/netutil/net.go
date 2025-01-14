// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL.txt and at www.mariadb.com/bsl11.
//
// Change Date: 2022-10-01
//
// On the date above, in accordance with the Business Source License, use
// of this software will be governed by the Apache License, Version 2.0,
// included in the file licenses/APL.txt and at
// https://www.apache.org/licenses/LICENSE-2.0

package netutil

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cockroachdb/cmux"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
)

// ListenAndServeGRPC creates a listener and serves the specified grpc Server
// on it, closing the listener when signaled by the stopper.
func ListenAndServeGRPC(
	stopper *stop.Stopper, server *grpc.Server, addr net.Addr,
) (net.Listener, error) {
	ln, err := net.Listen(addr.Network(), addr.String())
	if err != nil {
		return ln, err
	}

	ctx := context.TODO()

	stopper.RunWorker(ctx, func(context.Context) {
		<-stopper.ShouldQuiesce()
		FatalIfUnexpected(ln.Close())
		<-stopper.ShouldStop()
		server.Stop()
	})

	stopper.RunWorker(ctx, func(context.Context) {
		FatalIfUnexpected(server.Serve(ln))
	})
	return ln, nil
}

var httpLogger = log.NewStdLogger(log.Severity_ERROR, "httpLogger")

// Server is a thin wrapper around http.Server. See MakeServer for more detail.
type Server struct {
	*http.Server
}

// MakeServer constructs a Server that tracks active connections, closing them
// when signaled by stopper.
func MakeServer(stopper *stop.Stopper, tlsConfig *tls.Config, handler http.Handler) Server {
	var mu syncutil.Mutex
	activeConns := make(map[net.Conn]struct{})
	server := Server{
		Server: &http.Server{
			Handler:   handler,
			TLSConfig: tlsConfig,
			ConnState: func(conn net.Conn, state http.ConnState) {
				mu.Lock()
				switch state {
				case http.StateNew:
					activeConns[conn] = struct{}{}
				case http.StateClosed:
					delete(activeConns, conn)
				}
				mu.Unlock()
			},
			ErrorLog: httpLogger,
		},
	}

	ctx := context.TODO()

	// net/http.(*Server).Serve/http2.ConfigureServer are not thread safe with
	// respect to net/http.(*Server).TLSConfig, so we call it synchronously here.
	if err := http2.ConfigureServer(server.Server, nil); err != nil {
		log.Fatal(ctx, err)
	}

	stopper.RunWorker(ctx, func(context.Context) {
		<-stopper.ShouldStop()

		mu.Lock()
		for conn := range activeConns {
			conn.Close()
		}
		mu.Unlock()
	})

	return server
}

// ServeWith accepts connections on ln and serves them using serveConn.
func (s *Server) ServeWith(
	ctx context.Context, stopper *stop.Stopper, l net.Listener, serveConn func(net.Conn),
) error {
	// Inspired by net/http.(*Server).Serve
	var tempDelay time.Duration // how long to sleep on accept failure
	for {
		rw, e := l.Accept()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				httpLogger.Printf("http: Accept error: %v; retrying in %v", e, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return e
		}
		tempDelay = 0
		go func() {
			defer stopper.Recover(ctx)
			s.Server.ConnState(rw, http.StateNew) // before Serve can return
			serveConn(rw)
			s.Server.ConnState(rw, http.StateClosed)
		}()
	}
}

// IsClosedConnection returns true if err is cmux.ErrListenerClosed,
// grpc.ErrServerStopped, io.EOF, or the net package's errClosed.
func IsClosedConnection(err error) bool {
	return err == cmux.ErrListenerClosed ||
		err == grpc.ErrServerStopped ||
		err == io.EOF ||
		strings.Contains(err.Error(), "use of closed network connection")
}

// FatalIfUnexpected calls Log.Fatal(err) unless err is nil,
// cmux.ErrListenerClosed, or the net package's errClosed.
func FatalIfUnexpected(err error) {
	if err != nil && !IsClosedConnection(err) {
		log.Fatal(context.TODO(), err)
	}
}

// InitialHeartbeatFailedError indicates that while attempting a GRPC
// connection to a node, we aren't successful and have never seen a
// heartbeat over that connection before.
type InitialHeartbeatFailedError struct {
	WrappedErr error
}

func (e InitialHeartbeatFailedError) Error() string {
	return fmt.Sprintf("initial connection heartbeat failed: %s", e.WrappedErr)
}

// NewInitialHeartBeatFailedError creates a new InitialHeartbeatFailedError.
func NewInitialHeartBeatFailedError(cause error) *InitialHeartbeatFailedError {
	return &InitialHeartbeatFailedError{
		WrappedErr: cause,
	}
}
