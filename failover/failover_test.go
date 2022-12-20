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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type atomicVal[T any] struct {
	val T
	mu  sync.RWMutex
}

func (a *atomicVal[T]) set(val T) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.val = val
}

func (a *atomicVal[T]) get() T {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.val
}

type client struct {
	serverName string
	onError    func(error)
	close      func()
}

func (c *client) SetOnError(f func(error)) {
	c.onError = f
}

func (c *client) Close() {
	c.close()
}

type testSuite struct {
	suite.Suite
	failover          *Failover[*client]
	connectFail       atomic.Bool
	onConnectCount    atomic.Uint32
	onConnect         atomicVal[func(counter uint32, serverName string)]
	onDisconnectCount atomic.Uint32
	onDisconnect      atomicVal[func(counter uint32, serverName string)]
	onRetry           atomicVal[func(error)]
}

func (s *testSuite) wait(ch chan struct{}) {
	s.T().Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		require.Fail(s.T(), "timeout")
	}
}

func (s *testSuite) SetupTest() {
	s.onConnectCount.Store(0)
	s.onConnect.set(nil)
	s.onDisconnectCount.Store(0)
	s.onDisconnect.set(nil)
	s.onRetry.set(nil)

	mkServer := func(serverName string) *Server[*client] {
		return &Server[*client]{
			Name: serverName,
			Connect: func() (*client, error) {
				if s.connectFail.Load() {
					return nil, fmt.Errorf("%s fails", serverName)
				}

				// We fire this here instead of in OnConnect() as OnConnect() is called in a
				// goroutine, making exact unit testing of the order of connections difficult.
				count := s.onConnectCount.Add(1)
				onConnect := s.onConnect.get()
				if onConnect != nil {
					onConnect(count, serverName)
				}
				return &client{
					serverName: serverName,
					close: func() {
						// We fire this here instead of in OnDisConnect() as OnDisConnect() is
						// called in a goroutine, making exact unit testing of the order of
						// connections difficult.

						count := s.onDisconnectCount.Add(1)
						onDisconnect := s.onDisconnect.get()
						if onDisconnect != nil {
							onDisconnect(count, serverName)
						}
					},
				}, nil
			},
		}
	}
	failover, err := New(&Options[*client]{
		Servers: []*Server[*client]{
			mkServer("server1"),
			mkServer("server2"),
		},
		// For deterministic tests, always start at the first server.
		StartIndex:   func() int { return 0 },
		RetryTimeout: time.Millisecond,
		OnConnect: func(server *Server[*client]) {

		},
		OnDisconnect: func(server *Server[*client], err error) {
			require.Error(s.T(), err)
		},
		OnRetry: func(err error) {
			onRetry := s.onRetry.get()
			if onRetry != nil {
				onRetry(err)
			}
		},
	})
	require.NoError(s.T(), err)
	s.failover = failover
}

func (s *testSuite) TearDownTest() {
	s.failover.Close()
}

func TestSuite(t *testing.T) {
	suite.Run(t, &testSuite{})
}

func (s *testSuite) TestSuccessWithoutFailover() {
	s.onConnect.set(func(counter uint32, serverName string) {
		if counter == 1 {
			require.Equal(s.T(), "server1", serverName)
		}
	})
	var callCount atomic.Uint32
	f := func(client *client) (int, error) {
		switch callCount.Add(1) {
		case 1:
			require.Equal(s.T(), client.serverName, "server1")
			return 42, nil
		case 2:
			require.Equal(s.T(), client.serverName, "server1")
			// Call can return an error that does not trigger failover.
			return 0, errors.New("non-failover error")
		default:
			panic("too many calls")
		}
	}
	result, err := Call(s.failover, f)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)

	_, err = Call(s.failover, f)
	require.EqualError(s.T(), err, "non-failover error")

	require.Equal(s.T(), uint32(1), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(0), s.onDisconnectCount.Load())
}

// First server fails, second server works.
func (s *testSuite) TestFailoverOnce() {
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
	s.onRetry.set(func(err error) {
		require.Fail(s.T(), "unexpected retry")
	})

	var callCount atomic.Uint32
	f := func(client *client) (int, error) {
		switch callCount.Add(1) {
		case 1:
			require.Equal(s.T(), client.serverName, "server1")
			// Triggers failover.
			return 0, NewFailoverError(errors.New("fail"))
		case 2, 3:
			require.Equal(s.T(), client.serverName, "server2")
		default:
			panic("too many calls")
		}
		return 42, nil

	}
	result, err := Call(s.failover, f)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)

	result, err = Call(s.failover, f)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)

	require.Equal(s.T(), uint32(2), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(1), s.onDisconnectCount.Load())
}

// Tests that all servers fail and we try again from the start. The errors happen due method call errors,
// not connection errors.
func (s *testSuite) TestRetryDueToCallError() {
	s.onConnect.set(func(counter uint32, serverName string) {
		switch counter {
		case 1:
			require.Equal(s.T(), "server1", serverName)
		case 2:
			require.Equal(s.T(), "server2", serverName)
		case 3:
			require.Equal(s.T(), "server1", serverName)
		default:
			require.Fail(s.T(), "too many onConnects")
		}
	})
	s.onDisconnect.set(func(counter uint32, serverName string) {
		switch counter {
		case 1:
			require.Equal(s.T(), "server1", serverName)
		case 2:
			require.Equal(s.T(), "server2", serverName)
		case 3:
			require.Equal(s.T(), "server1", serverName)
		}
	})
	var retryCalled atomic.Bool
	s.onRetry.set(func(err error) {
		require.EqualError(s.T(), err, "fail2")
		retryCalled.Store(true)
	})
	var callCount atomic.Uint32
	f := func(client *client) (int, error) {
		switch callCount.Add(1) {
		case 1:
			require.Equal(s.T(), client.serverName, "server1")
			// Triggers failover.
			return 0, NewFailoverError(errors.New("fail1"))
		case 2:
			require.Equal(s.T(), client.serverName, "server2")
			// Triggers failover and retry.
			return 0, NewFailoverError(errors.New("fail2"))
		case 3, 4:
			require.Equal(s.T(), client.serverName, "server1")
			return 42, nil
		default:
			panic("too many calls")
		}
	}
	result, err := Call(s.failover, f)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)

	result, err = Call(s.failover, f)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)

	require.Equal(s.T(), uint32(3), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(2), s.onDisconnectCount.Load())
	require.True(s.T(), retryCalled.Load())
}

// Tests that all servers fail and we try again from the start. The errors happen due to connection
// errors.
func (s *testSuite) TestRetryDueToConnectionErrors() {
	s.connectFail.Store(true)
	s.onConnect.set(func(counter uint32, serverName string) {
		switch counter {
		case 1:
			require.Equal(s.T(), "server1", serverName)
		case 2:
			require.Equal(s.T(), "server2", serverName)
		case 3:
			require.Equal(s.T(), "server1", serverName)
		default:
			require.Fail(s.T(), "too many onConnects")
		}
	})
	var retryCalled atomic.Bool
	s.onRetry.set(func(err error) {
		require.EqualError(s.T(), err, "server2 fails")
		retryCalled.Store(true)
		// Make first client connect again, ending the failover loop.
		s.connectFail.Store(false)
	})
	var callCount atomic.Uint32
	f := func(client *client) (int, error) {
		switch callCount.Add(1) {
		case 1:
			require.Equal(s.T(), client.serverName, "server1")
		case 2:
			require.Equal(s.T(), client.serverName, "server1")
		default:
			panic("too many calls")
		}

		return 42, nil
	}
	result, err := Call(s.failover, f)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)

	result, err = Call(s.failover, f)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)

	require.Equal(s.T(), uint32(1), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(0), s.onDisconnectCount.Load())
	require.Equal(s.T(), uint32(2), callCount.Load())
	require.True(s.T(), retryCalled.Load())
}

// Tests that there is a failover if there is an asynchronous error (outside of a call).
func (s *testSuite) TestFailOverDueToAsyncErr() {
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
	s.onRetry.set(func(err error) { require.Fail(s.T(), "unexpected retry") })

	var callCount atomic.Uint32
	f := func(client *client) (int, error) {
		switch callCount.Add(1) {
		case 1:
			require.Equal(s.T(), client.serverName, "server1")
			// Simulate async error
			client.onError(errors.New("asynchronous error"))
		case 2:
			require.Equal(s.T(), client.serverName, "server2")
		default:
			panic("too many calls")
		}
		return 42, nil

	}
	result, err := Call(s.failover, f)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)

	result, err = Call(s.failover, f)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)

	require.Equal(s.T(), uint32(2), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(1), s.onDisconnectCount.Load())
	require.Equal(s.T(), uint32(2), callCount.Load())
}

// Tests that there is a failover if there is an asynchronous error (outside of a call) at the same
// time as a call error. This makes sure that in this case, we only failover once to another server,
// not twice.
func (s *testSuite) TestFailOverDueToAsyncErrAndCallErr() {
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
	s.onRetry.set(func(err error) { require.Fail(s.T(), "unexpected retry") })

	var callCount atomic.Uint32
	f := func(client *client) (int, error) {
		switch callCount.Add(1) {
		case 1:
			require.Equal(s.T(), client.serverName, "server1")
			client.onError(errors.New("asynchronous error"))
			return 0, NewFailoverError(errors.New("call error"))
		case 2, 3:
			require.Equal(s.T(), client.serverName, "server2")
			return 42, nil
		default:
			panic("too many calls")
		}

	}
	result, err := Call(s.failover, f)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)

	result, err = Call(s.failover, f)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)

	require.Equal(s.T(), uint32(2), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(1), s.onDisconnectCount.Load())
	require.Equal(s.T(), uint32(3), callCount.Load())
}

// Test subscribing to a method normally without failover.
func (s *testSuite) TestSubscribe() {
	counter := 0
	onResult := func(result string, err error) {
		counter++
		switch counter {
		case 1:
			require.Equal(s.T(), "response from server1", result)
			require.NoError(s.T(), err)
		case 2:
			require.EqualError(s.T(), err, "non-failover error")
		}
	}
	Subscribe(
		s.failover,
		func(client *client, result func(string, error)) {
			result(fmt.Sprintf("response from %s", client.serverName), nil)
			result("", errors.New("non-failover error"))
		},
		onResult,
	)
	require.Equal(s.T(), 2, counter)
}

// Tests that we failover to another server and resubscribe, as the first server errors.
func (s *testSuite) TestSubscribeFailoverDueToCallError() {
	ch := make(chan struct{})
	onResult := func(result string, err error) {
		require.Equal(s.T(), "response from server2", result)
		require.NoError(s.T(), err)
		close(ch)
	}
	counter := 0
	Subscribe(
		s.failover,
		func(client *client, result func(string, error)) {
			counter += 1
			switch counter {
			case 1:
				require.Equal(s.T(), "server1", client.serverName)
				result("", NewFailoverError(errors.New("server1 error")))
			case 2:
				require.Equal(s.T(), "server2", client.serverName)
				result("response from server2", nil)
			default:
				require.Fail(s.T(), "too many calls")
			}

		},
		onResult,
	)
	s.wait(ch)
}

// Tests that we failover to another server and resubscribe all subscriptions.
func (s *testSuite) TestResubscribe() {
	ch := make(chan struct{})
	notificationCounter1 := 0
	onResult1 := func(result string, err error) {
		// Called twice, once on first server and once when resubscribed on the second server after
		// failover.
		notificationCounter1++
		switch notificationCounter1 {
		case 1:
			require.Equal(s.T(), "response from server1", result)
			require.NoError(s.T(), err)
		case 2:
			require.Equal(s.T(), "response from server2", result)
			require.NoError(s.T(), err)
			ch <- struct{}{}
		default:
			require.Fail(s.T(), "too many notifications")
		}

	}
	notificationCounter2 := 0
	onResult2 := func(result string, err error) {
		// Called only once after failover to the 2nd server (first server errored).
		notificationCounter2++
		switch notificationCounter2 {
		case 1:
			require.Equal(s.T(), "response from server2", result)
			require.NoError(s.T(), err)
			ch <- struct{}{}
		default:
			require.Fail(s.T(), "too many notifications")
		}

	}
	var subscribeCounter1 atomic.Uint32
	subscribe1 := func(client *client, result func(string, error)) {
		c := subscribeCounter1.Add(1)
		switch c {
		case 1:
			require.Equal(s.T(), "server1", client.serverName)
			result("response from server1", nil)
		case 2:
			require.Equal(s.T(), "server2", client.serverName)
			result("response from server2", nil)
		case 3:
			require.Equal(s.T(), "server1", client.serverName)
			result("response from server1", nil)
		default:
			require.Fail(s.T(), "too many calls")
		}
	}
	var subscribeCounter2 atomic.Uint32
	subscribe2 := func(client *client, result func(string, error)) {
		switch subscribeCounter2.Add(1) {
		case 1:
			require.Equal(s.T(), "server1", client.serverName)
			result("", NewFailoverError(errors.New("server1 error")))
		case 2:
			result(fmt.Sprintf("response from %s", client.serverName), nil)
		default:
			require.Fail(s.T(), "too many calls")
		}
	}
	Subscribe(
		s.failover,
		subscribe1,
		onResult1,
	)
	Subscribe(
		s.failover,
		subscribe2,
		onResult2,
	)

	for i := 0; i < 2; i++ {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			require.Fail(s.T(), "timeout")
		}
	}
}

// Tests that all servers fail and we try again from the start. The errors happen due to connection
// errors.
func (s *testSuite) TestSubscribeRetryDueToConnectionErrors() {
	s.connectFail.Store(true)
	s.onConnect.set(func(counter uint32, serverName string) {
		switch counter {
		case 1:
			require.Equal(s.T(), "server1", serverName)
		default:
			require.Fail(s.T(), "too many onConnects")
		}
	})
	var retryCalled atomic.Bool
	s.onRetry.set(func(err error) {
		require.EqualError(s.T(), err, "server2 fails")
		retryCalled.Store(true)
		// Make first client connect again, ending the failover loop.
		s.connectFail.Store(false)
	})
	callCount := 0
	resultCounter := 0
	Subscribe(
		s.failover,
		func(client *client, result func(int, error)) {
			callCount++
			switch callCount {
			case 1:
				require.Equal(s.T(), client.serverName, "server1")
			case 2:
				require.Equal(s.T(), "server2", client.serverName)
			default:
				require.Fail(s.T(), "too many calls")
			}
			result(42, nil)
		},
		func(result int, err error) {
			resultCounter++
			require.Equal(s.T(), 42, result)
			require.NoError(s.T(), err)
		},
	)

	require.Equal(s.T(), uint32(1), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(0), s.onDisconnectCount.Load())
	require.Equal(s.T(), 1, callCount)
	require.Equal(s.T(), 1, resultCounter)
	require.True(s.T(), retryCalled.Load())
}

// Tests that all servers fail and we try again from the start. The errors happen due to call errors
func (s *testSuite) TestSubscribeRetryDueToCallError() {
	ch := make(chan struct{})
	s.onConnect.set(func(counter uint32, serverName string) {
		switch counter {
		case 1:
			require.Equal(s.T(), "server1", serverName)
		case 2:
			require.Equal(s.T(), "server2", serverName)
		case 3:
			require.Equal(s.T(), "server1", serverName)
		default:
			require.Fail(s.T(), "too many onConnects")
		}
	})
	s.onDisconnect.set(func(counter uint32, serverName string) {
		switch counter {
		case 1:
			require.Equal(s.T(), "server1", serverName)
		case 2:
			require.Equal(s.T(), "server2", serverName)
		case 3:
			require.Equal(s.T(), "server1", serverName)
		}
	})
	var retryCalled atomic.Bool
	s.onRetry.set(func(err error) {
		require.EqualError(s.T(), err, "server2 error")
		retryCalled.Store(true)
	})
	var callCount atomic.Uint32
	resultCounter := 0
	Subscribe(
		s.failover,
		func(client *client, result func(int, error)) {
			switch callCount.Add(1) {
			case 1:
				require.Equal(s.T(), client.serverName, "server1")
				// Triggers failover.
				result(0, NewFailoverError(errors.New("server1 error")))
			case 2:
				require.Equal(s.T(), "server2", client.serverName)
				// Triggers failover and retry.
				result(0, NewFailoverError(errors.New("server2 error")))
			case 3:
				require.Equal(s.T(), client.serverName, "server1")
				result(42, nil)
				close(ch)
			default:
				require.Fail(s.T(), "too many calls")
			}
		},
		func(result int, err error) {
			resultCounter++
			require.Equal(s.T(), 42, result)
			require.NoError(s.T(), err)
		},
	)
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		require.Fail(s.T(), "timeout")
	}
	require.Equal(s.T(), uint32(3), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(2), s.onDisconnectCount.Load())
	require.Equal(s.T(), uint32(3), callCount.Load())
	require.Equal(s.T(), 1, resultCounter)
	require.True(s.T(), retryCalled.Load())
}

// Failover closes before making a call.
func (s *testSuite) TestGetCloseErrorBefore() {
	s.failover.Close()
	_, err := Call(s.failover, func(client *client) (int, error) { return 42, nil })
	require.ErrorIs(s.T(), err, ErrClosed)
}

// Failover closes during a call, but result still arrives from the server.
func (s *testSuite) TestGetCloseErrorDuringSuccess() {
	result, err := Call(s.failover, func(client *client) (int, error) {
		s.failover.Close()
		return 42, nil
	})
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)
	require.Equal(s.T(), uint32(1), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(1), s.onDisconnectCount.Load())
}

// Failover closes during a call, but server returns error (presumably because the client closed).
func (s *testSuite) TestGetCloseErrorDuringFailure() {
	_, err := Call(s.failover, func(client *client) (int, error) {
		s.failover.Close()
		return 0, errors.New("foo")
	})
	require.EqualError(s.T(), err, "foo")
	require.Equal(s.T(), uint32(1), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(1), s.onDisconnectCount.Load())
}

// Failover closes before subscribing.
func (s *testSuite) TestSubscribeCloseErrorBefore() {
	s.failover.Close()
	errCh := make(chan error)
	Subscribe(
		s.failover,
		func(client *client, result func(int, error)) {
			result(42, nil)
		},
		func(result int, err error) {
			go func() {
				errCh <- err
			}()
		},
	)
	select {
	case err := <-errCh:
		require.ErrorIs(s.T(), err, ErrClosed)
	case <-time.After(2 * time.Second):
		require.Fail(s.T(), "timeout")
	}
	require.Equal(s.T(), uint32(0), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(0), s.onDisconnectCount.Load())
}

// Failover closes during subscribing, but result still arrives.
func (s *testSuite) TestSubscribeCloseDuringSuccess() {
	ch := make(chan struct{})
	Subscribe(
		s.failover,
		func(client *client, result func(int, error)) {
			s.failover.Close()
			result(42, nil)
		},
		func(result int, err error) {
			require.Equal(s.T(), 42, result)
			close(ch)
		},
	)
	s.wait(ch)
	require.Equal(s.T(), uint32(1), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(1), s.onDisconnectCount.Load())
}

// Failover closes during subscribing, but server returns error (presumably because the client
// closed).
func (s *testSuite) TestSubscribeCloseDuringFailure() {
	ch := make(chan struct{})
	Subscribe(
		s.failover,
		func(client *client, result func(int, error)) {
			s.failover.Close()
			result(42, errors.New("foo"))
		},
		func(result int, err error) {
			require.EqualError(s.T(), err, "foo")
			close(ch)
		},
	)
	s.wait(ch)
	require.Equal(s.T(), uint32(1), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(1), s.onDisconnectCount.Load())
}

// Asynchronous error causes failover, but failover client already closed.
func (s *testSuite) TestAsyncFailoverWhileClosed() {
	result, err := Call(s.failover,
		func(client *client) (int, error) {
			s.failover.Close()
			// Simulate async error -> does not trigger a failover because the failover client is closed.
			client.onError(errors.New("asynchronous error"))
			return 42, nil
		})
	require.NoError(s.T(), err)
	require.Equal(s.T(), 42, result)
	require.Equal(s.T(), uint32(1), s.onConnectCount.Load())
	require.Equal(s.T(), uint32(1), s.onDisconnectCount.Load())
}
