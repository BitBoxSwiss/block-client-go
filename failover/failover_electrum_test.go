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

package failover

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitalbitbox/block-client-go/electrum"
	"github.com/digitalbitbox/block-client-go/electrum/types"
	"github.com/digitalbitbox/block-client-go/jsonrpc/test"
	jsonrpctypes "github.com/digitalbitbox/block-client-go/jsonrpc/types"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// failoverClient is an Electrum client that is backed by multiple servers. If a server fails, there
// is an automatic failover to another server. If all servers fail, there is a retry timeout and all
// servers are tried again. Subscriptions are automatically re-subscribed on new servers.
type failoverClient struct {
	failover *Failover[*electrum.Client]
}

// newFailoverClient creates a new failover client.
func newFailoverClient(opts *Options[*electrum.Client]) *failoverClient {
	return &failoverClient{
		failover: New[*electrum.Client](opts),
	}
}

func (c *failoverClient) Close() {
	c.failover.Close()
}

func (c *failoverClient) ScriptHashGetHistory(scriptHashHex string) (types.TxHistory, error) {
	return CallAlwaysFailover(c.failover, func(client *electrum.Client) (types.TxHistory, error) {
		return client.ScriptHashGetHistory(context.TODO(), scriptHashHex)
	})
}

func (c *failoverClient) TransactionGet(txHash string) ([]byte, error) {
	return CallAlwaysFailover(c.failover, func(client *electrum.Client) ([]byte, error) {
		return client.TransactionGet(context.Background(), txHash)
	})
}

func (c *failoverClient) HeadersSubscribe(result func(header *types.Header, err error)) {
	SubscribeAlwaysFailover(
		c.failover,
		func(client *electrum.Client, result func(*types.Header, error)) {
			client.HeadersSubscribe(context.Background(), result)
		},
		result)
}

type electrumTestsuite struct {
	suite.Suite

	testCounter int

	server1 atomicVal[*test.Server]
	server2 atomicVal[*test.Server]
	// Handling responses from server1.
	onRequest1 atomicVal[func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response]
	// Handling responses from server2.
	onRequest2        atomicVal[func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response]
	client            *failoverClient
	onConnectCount    atomic.Uint32
	onConnect         atomicVal[func(counter uint32, serverName string)]
	onDisconnectCount atomic.Uint32
	onDisconnect      atomicVal[func(counter uint32, serverName string)]
	onRetry           atomicVal[func()]
}

func (s *electrumTestsuite) SetupTest() {
	s.testCounter++
	s.onConnectCount.Store(0)
	s.onConnect.set(nil)
	s.onDisconnectCount.Store(0)
	s.onDisconnect.set(nil)
	s.onRetry.set(nil)

	s.server1.set(test.NewServer(s.T()))
	s.server2.set(test.NewServer(s.T()))

	s.server1.get().OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		if request.Method == "server.version" {
			require.Equal(s.T(), []interface{}{"testclient for server1", "1.4"}, request.Params)
			return &jsonrpctypes.Response{
				JSONRPC: jsonrpctypes.JSONRPC,
				ID:      &request.ID,
				Result:  test.ToResult([]string{"testserver", "1.5"}),
			}
		}
		onRequest1 := s.onRequest1.get()
		return onRequest1(conn, request)
	})

	s.server2.get().OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		if request.Method == "server.version" {
			require.Equal(s.T(), []interface{}{"testclient for server2", "1.4"}, request.Params)
			return &jsonrpctypes.Response{
				JSONRPC: jsonrpctypes.JSONRPC,
				ID:      &request.ID,
				Result:  test.ToResult([]string{"testserver", "1.5"}),
			}
		}
		onRequest2 := s.onRequest2.get()
		return onRequest2(conn, request)
	})

	mkServer := func(name string, port int) *Server[*electrum.Client] {
		return &Server[*electrum.Client]{
			Name: name,
			Connect: func() (*electrum.Client, error) {
				c, err := electrum.Connect(&electrum.Options{
					Dial: func() (net.Conn, error) {
						return net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
					},
					MethodTimeout:   100 * time.Millisecond,
					PingInterval:    -1,
					SoftwareVersion: fmt.Sprintf("testclient for %s", name),
				})
				if err != nil {
					return nil, err
				}

				// We fire this here instead of in OnConnect() as OnConnect() is called in a
				// goroutine, making exact unit testing of the order of connections difficult.
				count := s.onConnectCount.Add(1)
				onConnect := s.onConnect.get()
				if onConnect != nil {
					onConnect(count, name)
				}

				return c, nil
			},
		}
	}

	currentTestCounter := s.testCounter
	s.client = newFailoverClient(&Options[*electrum.Client]{
		Servers: []*Server[*electrum.Client]{
			mkServer("server1", s.server1.get().Port),
			mkServer("server2", s.server2.get().Port),
		},
		// For deterministic tests, always start at the first server.
		StartIndex:   func() int { return 0 },
		RetryTimeout: time.Millisecond,
		OnConnect:    func(server *Server[*electrum.Client]) {},
		OnDisconnect: func(server *Server[*electrum.Client], err error) {
			if s.testCounter != currentTestCounter {
				return
			}
			require.Error(s.T(), err)
			count := s.onDisconnectCount.Add(1)
			onDisconnect := s.onDisconnect.get()
			if onDisconnect != nil {
				onDisconnect(count, server.Name)
			}
		},
		OnRetry: func(err error) {
			if s.testCounter != currentTestCounter {
				return
			}
			onRetry := s.onRetry.get()
			if onRetry != nil {
				onRetry()
			}
		},
	})
}

func (s *electrumTestsuite) TearDownTest() {
	s.client.Close()
	s.server1.get().Close()
	s.server2.get().Close()
}

func TestElectrumFailover(t *testing.T) {
	suite.Run(t, &electrumTestsuite{})
}

// A request works on the first try, no failover needed.
func (s *electrumTestsuite) TestSuccessWithoutfailover() {
	expectedResponse := []byte("\xaa\xbb\xcc")
	s.onRequest1.set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.transaction.get", request.Method)
		require.Equal(s.T(), []interface{}{"testtxhash"}, request.Params)
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(hex.EncodeToString(expectedResponse)),
		}
	})
	s.onConnect.set(func(counter uint32, serverName string) {
		require.Equal(s.T(), "server1", serverName)
	})
	s.onRetry.set(func() { require.Fail(s.T(), "unexpected retry") })
	response, err := s.client.TransactionGet("testtxhash")
	require.NoError(s.T(), err)
	require.Equal(s.T(), expectedResponse, response)
	require.Equal(s.T(), uint32(1), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(0), s.onDisconnectCount.Load())
}

// First server fails, second server works.
func (s *electrumTestsuite) TestFailoverOnce() {
	expectedResponse := []byte("\xaa\xbb\xcc")
	s.onRequest1.set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		return nil
	})
	s.onRequest2.set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.transaction.get", request.Method)
		require.Equal(s.T(), []interface{}{"testtxhash"}, request.Params)
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(hex.EncodeToString(expectedResponse)),
		}
	})

	s.onConnect.set(func(counter uint32, serverName string) {
		switch counter {
		case 1:
			require.Equal(s.T(), "server1", serverName)
		case 2:
			require.Equal(s.T(), "server2", serverName)
		default:
			require.Fail(s.T(), "too many onConnects")
		}
	})
	s.onDisconnect.set(func(counter uint32, serverName string) {
		switch counter {
		case 1:
			require.Equal(s.T(), "server1", serverName)
		default:
		}
	})
	s.onRetry.set(func() { require.Fail(s.T(), "unexpected retry") })
	response, err := s.client.TransactionGet("testtxhash")
	require.NoError(s.T(), err)
	require.Equal(s.T(), expectedResponse, response)
	require.Equal(s.T(), uint32(2), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(1), s.onDisconnectCount.Load())
}

// Tests that all servers fail and we try again from the start. The errors happen due to timeouts in
// method calls, not connection errors.
func (s *electrumTestsuite) TestRetryDueToTimeouts() {
	expectedResponse := []byte("\xaa\xbb\xcc")
	onRequest := func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.transaction.get", request.Method)
		require.Equal(s.T(), []interface{}{"testtxhash"}, request.Params)
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(hex.EncodeToString(expectedResponse)),
		}
	}
	s.onRequest1.set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		return nil
	})
	s.onRequest2.set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		return nil
	})
	s.onRetry.set(func() {
		// When retrying, we make the second server respond normally again.
		s.onRequest2.set(onRequest)
	})

	s.onConnect.set(func(counter uint32, serverName string) {
		switch counter {
		case 1:
			require.Equal(s.T(), "server1", serverName)
		case 2:
			require.Equal(s.T(), "server2", serverName)
		case 3:
			require.Equal(s.T(), "server1", serverName)
		case 4:
			require.Equal(s.T(), "server2", serverName)
		default:
			require.Fail(s.T(), "too many onConnects")
		}
	})
	var server1DisconnectCount, server2DisconnectCount atomic.Uint32
	s.onDisconnect.set(func(counter uint32, serverName string) {
		switch serverName {
		case "server1":
			server1DisconnectCount.Add(1)
		case "server2":
			server2DisconnectCount.Add(1)
		default:
			require.Failf(s.T(), "unexpected server", "%s", serverName)
		}
	})

	response, err := s.client.TransactionGet("testtxhash")
	require.NoError(s.T(), err)
	require.Equal(s.T(), expectedResponse, response)
	require.Equal(s.T(), uint32(4), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(2), server1DisconnectCount.Load())
	require.Equal(s.T(), uint32(1), server2DisconnectCount.Load())
	require.Equal(s.T(), uint32(3), s.onDisconnectCount.Load())
}

// Tests that all servers fail and we try again from the start. The errors happen due to connection
// errors.
func (s *electrumTestsuite) TestRetryDueToConnectionErrors() {
	s.server1.get().Close()
	s.server2.get().Close()
	expectedResponse := []byte("\xaa\xbb\xcc")
	onRequest := func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.transaction.get", request.Method)
		require.Equal(s.T(), []interface{}{"testtxhash"}, request.Params)
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(hex.EncodeToString(expectedResponse)),
		}
	}
	s.onRequest1.set(onRequest)
	s.onRequest2.set(onRequest)
	s.onRetry.set(func() {
		// When retrying, we make the second server respond normally again.
		newServer2 := test.NewServerUsingPort(s.T(), s.server2.get().Port)
		newServer2.OnRequest.Set(s.server2.get().OnRequest.Get())
		s.server2.set(newServer2)
	})

	s.onConnect.set(func(counter uint32, serverName string) {
		switch counter {
		case 1:
			require.Equal(s.T(), "server2", serverName)
		default:
			require.Fail(s.T(), "too many onConnects")
		}
	})

	response, err := s.client.TransactionGet("testtxhash")
	require.NoError(s.T(), err)
	require.Equal(s.T(), expectedResponse, response)
	require.Equal(s.T(), uint32(1), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(0), s.onDisconnectCount.Load())
}

// Tests that if one request fails, the connection is closed and that another concurrent requests
// also fails. They all succeed by moving to the next server. This test also makes sure that both
// requests run concurrently to make sure that the first request failing closes the connection,
// triggering a failure of the second request, and both use the next server to recover. The second
// request does not trigger moving to another server because the first request failure already did.
func (s *electrumTestsuite) TestTwoRequestsFailover() {
	expectedHistory := types.TxHistory{{Height: 1, TxHash: "hash"}}

	waitForSecondRequest := sync.WaitGroup{}
	waitForSecondRequest.Add(1)

	waitForFirstRequestFailure := sync.WaitGroup{}
	waitForFirstRequestFailure.Add(1)

	// We disconnect once after the first request fails.
	s.onDisconnect.set(func(counter uint32, serverName string) {
		switch counter {
		case 1:
			require.Equal(s.T(), "server1", serverName)
			waitForFirstRequestFailure.Done()
		default:
		}
	})
	s.onRetry.set(func() { require.Fail(s.T(), "unexpected retry") })

	counterTransactionGet := 0
	counterGetHistory := 0
	onRequest := func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		switch request.Method {
		case "blockchain.transaction.get":
			// This is called twice. The first time we return an invalid result triggering a failure
			// and
			counterTransactionGet++
			switch counterTransactionGet {
			case 1:
				// We wait for the second request to have been sent but not yet handled, to make
				// sure both requests run concurrently and the first request failing triggers
				// failure of the second request (as a result of closing the current server
				// connection upong failure).
				waitForSecondRequest.Wait()
				return &jsonrpctypes.Response{
					JSONRPC: jsonrpctypes.JSONRPC,
					ID:      &request.ID,
					Result:  test.ToResult(123),
				}
			case 2:
				return &jsonrpctypes.Response{
					JSONRPC: jsonrpctypes.JSONRPC,
					ID:      &request.ID,
					Result:  test.ToResult("aabbcc"),
				}
			default:
				require.Fail(s.T(), "too many requests")
			}

		case "blockchain.scripthash.get_history":
			// This is called twice. The first time we respond normally but the client is already
			// disconnected, so it will fail triggering a second failover request.
			counterGetHistory++
			switch counterGetHistory {
			case 1:
				// Allow the first request to proceed, making sure that the second request has been
				// sent and is waiting for a response.
				waitForSecondRequest.Done()
				// Only go on to respond after the first request failed.
				waitForFirstRequestFailure.Wait()
			case 2:
			default:
				require.Fail(s.T(), "too many requests")
			}
			return &jsonrpctypes.Response{
				JSONRPC: jsonrpctypes.JSONRPC,
				ID:      &request.ID,
				Result:  test.ToResult(expectedHistory),
			}
		}
		panic("unexpected request")
	}
	s.onRequest1.set(onRequest)
	s.onRequest2.set(onRequest)

	ch := make(chan struct{})
	go func() {
		defer close(ch)
		history, err := s.client.TransactionGet("testtxhash")
		require.NoError(s.T(), err)
		require.Equal(s.T(), []byte("\xaa\xbb\xcc"), history)
	}()
	history, err := s.client.ScriptHashGetHistory("testScriptHashhex")
	require.NoError(s.T(), err)
	require.Equal(s.T(), expectedHistory, history)
	select {
	case <-ch:
	case <-time.After(time.Second):
		require.Fail(s.T(), "timeout")
	}
}

// Tests that subscriptions are recreated on a new server after a failover.
func (s *electrumTestsuite) TestSubscriptionFailover() {
	latestHeaderServer1 := &types.Header{Height: 10}
	latestHeaderServer2 := &types.Header{Height: 100}
	numNotifiedHeadersServer1 := 5
	numNotifiedHeadersServer2 := 100
	s.onRequest1.set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		// This is called on the first normal subscription.
		require.Equal(s.T(), "blockchain.headers.subscribe", request.Method)
		require.Equal(s.T(), []interface{}{}, request.Params)
		// Mock some notifications.
		go func() {
			for i := 0; i < numNotifiedHeadersServer1; i++ {
				test.Write(s.T(), conn, &jsonrpctypes.Response{
					JSONRPC: jsonrpctypes.JSONRPC,
					Method:  &request.Method,
					Params:  test.ToResult([]interface{}{&types.Header{Height: 11 + i}}),
				})
			}
		}()
		// Method response.
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(latestHeaderServer1),
		}
	})
	s.onRequest2.set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		// This is called as a failover subscription after the first server shuts down.
		require.Equal(s.T(), "blockchain.headers.subscribe", request.Method)
		require.Equal(s.T(), []interface{}{}, request.Params)
		// Mock some notifications.
		go func() {
			for i := 0; i < numNotifiedHeadersServer2; i++ {
				test.Write(s.T(), conn, &jsonrpctypes.Response{
					JSONRPC: jsonrpctypes.JSONRPC,
					Method:  &request.Method,
					Params:  test.ToResult([]interface{}{&types.Header{Height: 111 + i}}),
				})
			}
		}()
		// Method response.
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(latestHeaderServer2),
		}
	})

	waitNotificationsServer1 := make(chan struct{})
	waitNotificationsServer2 := make(chan struct{})
	var counter atomic.Uint32
	s.client.HeadersSubscribe(func(header *types.Header, err error) {
		require.NoError(s.T(), err)
		switch counter.Add(1) {
		// +1 because the method call itself returns the latest header.
		case uint32(numNotifiedHeadersServer1 + 1):
			close(waitNotificationsServer1)
		case uint32(numNotifiedHeadersServer1 + 1 + numNotifiedHeadersServer2 + 1):
			close(waitNotificationsServer2)
		}
	})

	select {
	case <-waitNotificationsServer1:
	case <-time.After(time.Second):
		require.Fail(s.T(), "timeout")
	}

	s.server1.get().Close() // triggers failover to the second server

	select {
	case <-waitNotificationsServer2:
	case <-time.After(time.Second):
		require.Fail(s.T(), "timeout")
	}
}
