// Copyright 2022 Shift Crypto AG
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/BitBoxSwiss/block-client-go/jsonrpc/types"
	"github.com/stretchr/testify/require"
)

type AtomicVal[T any] struct {
	val T
	mu  sync.RWMutex
}

func (a *AtomicVal[T]) Set(val T) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.val = val
}

func (a *AtomicVal[T]) Get() T {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.val
}

// Server is a TCP server useful for mocking a JSON RPC server.
type Server struct {
	listener net.Listener
	closed   bool
	closedMu sync.RWMutex

	OnRequest     AtomicVal[func(conn net.Conn, request *types.Request) *types.Response]
	onNewClient   func(net.Conn)
	onNewClientMu sync.RWMutex
	Port          int
	closeWait     sync.WaitGroup
}

// NewServerUsingPort creates a mock server on the given port.
func NewServerUsingPort(t *testing.T, port int) *Server {
	t.Helper()
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	require.NoError(t, err)
	s := &Server{
		listener: listener,
		Port:     listener.Addr().(*net.TCPAddr).Port,
	}
	go func() {
		for !s.isClosed() {
			conn, err := listener.Accept()
			if err == nil {
				s.closeWait.Add(1)
				go s.readLoop(t, conn)

				s.onNewClientMu.RLock()
				onNewClient := s.onNewClient
				s.onNewClientMu.RUnlock()
				if onNewClient != nil {
					go onNewClient(conn)
				}
			}
		}
	}()
	return s
}

// NewServer creates a test server on an arbitrary unused port.
func NewServer(t *testing.T) *Server {
	return NewServerUsingPort(t, 0)
}

func (s *Server) isClosed() bool {
	s.closedMu.RLock()
	defer s.closedMu.RUnlock()
	return s.closed
}

func (s *Server) readLoop(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	defer s.closeWait.Done()
	reader := bufio.NewReader(conn)
	for !s.isClosed() {
		// Use a timeout to give a chance to shut down the client after Close() is called.
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
		line, err := reader.ReadBytes(byte('\n'))
		if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
			continue
		}
		if err != nil {
			return
		}
		go func() {
			var req types.Request
			require.NoError(t, json.Unmarshal(line, &req))
			resp := s.OnRequest.Get()(conn, &req)
			if resp != nil {
				Write(t, conn, resp)
			}
		}()
	}
}

// Write writes the `response` to the socket.
func Write(t *testing.T, conn net.Conn, response *types.Response) {
	t.Helper()

	respBytes, err := json.Marshal(response)
	require.NoError(t, err)
	_, _ = conn.Write(append(respBytes, byte('\n')))
}

// SetOnNewClient defines a callback that is called when a new client connects to this server.
func (s *Server) SetOnNewClient(f func(net.Conn)) {
	s.onNewClientMu.Lock()
	defer s.onNewClientMu.Unlock()
	s.onNewClient = f
}

func (s *Server) close() bool {
	s.closedMu.Lock()
	defer s.closedMu.Unlock()
	if s.closed {
		return false
	}
	s.closed = true

	_ = s.listener.Close()
	return true
}

// Close closes the server. It blocks until all client connections are closed.
func (s *Server) Close() {
	if s.close() {
		s.closeWait.Wait()
	}
}

// ToResult JSON-marshals the resul. Panics on error.
func ToResult(result interface{}) json.RawMessage {
	bytes, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	return bytes
}
