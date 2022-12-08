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

package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/digitalbitbox/block-client-go/jsonrpc/test"
	"github.com/digitalbitbox/block-client-go/jsonrpc/types"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type testSuite struct {
	suite.Suite

	server *test.Server
	dial   func() (net.Conn, error)
}

func (s *testSuite) SetupTest() {
	s.server = test.NewServer(s.T())
	s.dial = func() (net.Conn, error) {
		return net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.server.Port), time.Second)
	}
}

func (s *testSuite) TearDownTest() {
	s.server.Close()
}

func TestSuite(t *testing.T) {
	suite.Run(t, &testSuite{})
}

func (s *testSuite) TestConnectFail() {
	// We launch a server so we are sure that the port we try to connect to is unused and not
	// listened on.
	s.server.Close()
	_, err := Connect(&Options{
		Dial: s.dial,
	})
	require.Error(s.T(), err)
}

func (s *testSuite) TestMethodSync() {
	client, err := Connect(&Options{
		Dial: s.dial,
	})
	require.NoError(s.T(), err)
	s.server.OnRequest.Set(func(conn net.Conn, req *types.Request) *types.Response {
		require.Equal(s.T(), "method", req.Method)
		require.Equal(s.T(), []interface{}{"param1", 42.}, req.Params)
		return &types.Response{
			JSONRPC: types.JSONRPC,
			ID:      &req.ID,
			Result:  test.ToResult("response"),
		}
	})

	var response string
	err = client.MethodBlocking(
		context.Background(),
		&response,
		"method", "param1", 42.)
	require.NoError(s.T(), err)
	require.Equal(s.T(), "response", response)
	require.Empty(s.T(), client.pendingRequests)
}

// Server returns an error nested in a `{ message: ...}` object.
func (s *testSuite) TestMethodBlockingNestedErrByServer() {
	client, err := Connect(&Options{
		Dial: s.dial,
	})
	require.NoError(s.T(), err)
	s.server.OnRequest.Set(func(conn net.Conn, req *types.Request) *types.Response {
		errJSONBytes, err := json.Marshal(map[string]string{
			"message": "some error",
		})
		errJSONBytesRaw := json.RawMessage(errJSONBytes)
		require.NoError(s.T(), err)
		return &types.Response{
			JSONRPC: types.JSONRPC,
			ID:      &req.ID,
			Error:   &errJSONBytesRaw,
		}
	})

	var response string
	err = client.MethodBlocking(
		context.Background(),
		&response,
		"method", "param1", 42.)
	require.EqualError(s.T(), err, "some error")
}

// Server returns a error not nested in a `{ message: ... }` object, but as a direct string.
func (s *testSuite) TestMethodBlockingUnnestedErrorByServer() {
	client, err := Connect(&Options{
		Dial: s.dial,
	})
	require.NoError(s.T(), err)
	s.server.OnRequest.Set(func(conn net.Conn, req *types.Request) *types.Response {
		errJSONBytesRaw := json.RawMessage(`"not a valid error object"`)
		return &types.Response{
			JSONRPC: types.JSONRPC,
			ID:      &req.ID,
			Error:   &errJSONBytesRaw,
		}
	})

	var response string
	err = client.MethodBlocking(
		context.Background(),
		&response,
		"method", "param1", 42.)
	require.Error(s.T(), err)
	require.Equal(s.T(), "not a valid error object", err.Error())
}

func (s *testSuite) TestMethodBlockingTimeout() {
	client, err := Connect(&Options{
		Dial: s.dial,
	})
	require.NoError(s.T(), err)
	s.server.OnRequest.Set(func(conn net.Conn, req *types.Request) *types.Response {
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	var response string
	err = client.MethodBlocking(
		ctx,
		&response,
		"method", "param1", "params2")
	require.ErrorIs(s.T(), err, context.DeadlineExceeded)
	require.Empty(s.T(), client.pendingRequests)
}

func (s *testSuite) TestMethodBlockingClientCancelledCTX() {
	client, err := Connect(&Options{
		Dial: s.dial,
	})
	require.NoError(s.T(), err)
	s.server.OnRequest.Set(func(conn net.Conn, req *types.Request) *types.Response {
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var response string
	err = client.MethodBlocking(
		ctx,
		&response,
		"method", "param1", "params2")
	require.ErrorIs(s.T(), err, context.Canceled)
	require.Empty(s.T(), client.pendingRequests)
}

// MethodBlocking() called on a closed client.
func (s *testSuite) TestMethodBlockingClientClosedBefore() {
	client, err := Connect(&Options{
		Dial: s.dial,
	})
	require.NoError(s.T(), err)
	client.Close()
	s.server.OnRequest.Set(func(conn net.Conn, req *types.Request) *types.Response {
		return nil
	})
	var response string
	err = client.MethodBlocking(
		context.Background(),
		&response,
		"method", "param1", "params2")
	require.Error(s.T(), err)
	require.NotErrorIs(s.T(), err, context.Canceled)
}

// Client closed after sending request while waiting for response.
func (s *testSuite) TestMethodBlockingClientClosedDuring() {
	client, err := Connect(&Options{
		Dial: s.dial,
	})
	require.NoError(s.T(), err)
	s.server.OnRequest.Set(func(conn net.Conn, req *types.Request) *types.Response {
		client.Close()
		return nil
	})
	var response string
	err = client.MethodBlocking(
		context.Background(),
		&response,
		"method", "param1", "params2")
	require.Error(s.T(), err)
}

// server.Closed after the client is connected, before sending request.
func (s *testSuite) TestMethodBlockingServerClosedBefore() {
	client, err := Connect(&Options{
		Dial: s.dial,
	})
	require.NoError(s.T(), err)
	s.server.Close()
	s.server.OnRequest.Set(func(conn net.Conn, req *types.Request) *types.Response {
		return nil
	})
	var response string
	err = client.MethodBlocking(
		context.Background(),
		&response,
		"method", "param1", "params2")
	require.Error(s.T(), err)
}

func (s *testSuite) TestNotification() {
	expectedMethod := "notification-method"
	expectedParams := test.ToResult([]interface{}{"param1", 42})

	s.server.SetOnNewClient(func(conn net.Conn) {
		test.Write(s.T(), conn, &types.Response{
			JSONRPC: types.JSONRPC,
			Method:  &expectedMethod,
			Params:  expectedParams,
		})
	})

	checked := make(chan struct{})
	client, err := Connect(&Options{
		Dial: s.dial,
	})
	require.NoError(s.T(), err)
	client.OnNotification(func(method string, params json.RawMessage) {
		require.Equal(s.T(), expectedMethod, method)
		require.Equal(s.T(), expectedParams, params)
		close(checked)
	})

	select {
	case <-checked:
	case <-time.After(1 * time.Second):
		s.T().Fatal("timeout")
	}
}
