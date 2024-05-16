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

package electrum

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BitBoxSwiss/block-client-go/electrum/types"
	"github.com/BitBoxSwiss/block-client-go/jsonrpc/test"
	jsonrpctypes "github.com/BitBoxSwiss/block-client-go/jsonrpc/types"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

func TestConnectFail(t *testing.T) {
	// We launch a server so we are sure that the port we try to connect to is unused and not
	// listened on.
	server := test.NewServer(t)
	server.Close()
	_, err := Connect(&Options{
		Dial: func() (net.Conn, error) {
			return net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", server.Port), time.Second)
		},
	})
	require.Error(t, err)
}

func TestPing(t *testing.T) {
	ch := make(chan struct{})
	server := test.NewServer(t)
	var counter atomic.Uint32
	server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		switch request.Method {
		case "server.version":
			require.Equal(t, []interface{}{"testclient", "1.4"}, request.Params)
			return &jsonrpctypes.Response{
				JSONRPC: jsonrpctypes.JSONRPC,
				ID:      &request.ID,
				Result:  test.ToResult([]string{"testserver", "1.5"}),
			}
		case "server.ping":
			if counter.Add(1) == 10 {
				close(ch)
			}
			require.Equal(t, []interface{}{}, request.Params)
			return &jsonrpctypes.Response{
				JSONRPC: jsonrpctypes.JSONRPC,
				ID:      &request.ID,
				Result:  test.ToResult(nil),
			}
		default:
			require.Fail(t, "unexpected method call: %s", request.Method)
			return nil
		}
	})

	client, err := Connect(&Options{
		Dial: func() (net.Conn, error) {
			return net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", server.Port), time.Second)
		},
		PingInterval:    time.Millisecond,
		SoftwareVersion: "testclient",
	})
	require.NoError(t, err)
	defer client.Close()
	select {
	case <-ch:
	case <-time.After(time.Second):
		require.Fail(t, "timeout")
	}
}

type clientTestsuite struct {
	suite.Suite

	server *test.Server
	client *Client
}

func (s *clientTestsuite) SetupTest() {
	s.server = test.NewServer(s.T())
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "server.version", request.Method)
		require.Equal(s.T(), []interface{}{"testclient", "1.4"}, request.Params)
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult([]string{"testserver", "1.5"}),
		}
	})
	client, err := Connect(&Options{
		Dial: func() (net.Conn, error) {
			return net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.server.Port), time.Second)
		},
		MethodTimeout:   time.Second,
		PingInterval:    -1,
		SoftwareVersion: "testclient",
	})
	require.NoError(s.T(), err)
	s.server.OnRequest.Set(nil)
	s.client = client
}

func (s *clientTestsuite) TearDownTest() {
	s.server.Close()
}

func TestClient(t *testing.T) {
	suite.Run(t, &clientTestsuite{})
}

func (s *clientTestsuite) TestScriptHashGetHistory() {
	expectedResponse := types.TxHistory([]*types.TxInfo{
		{Height: 0, TxHash: "testhash"},
		{Height: 1, TxHash: "testhash2"},
	})
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.scripthash.get_history", request.Method)
		require.Equal(s.T(), []interface{}{"testscripthash"}, request.Params)
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(expectedResponse),
		}
	})
	response, err := s.client.ScriptHashGetHistory(context.Background(), "testscripthash")
	require.NoError(s.T(), err)
	require.Equal(s.T(), expectedResponse, response)
}

func (s *clientTestsuite) TestTransactionGet() {
	expectedResponse := []byte("\xaa\xbb\xcc")
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.transaction.get", request.Method)
		require.Equal(s.T(), []interface{}{"testtxhash"}, request.Params)
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(hex.EncodeToString(expectedResponse)),
		}
	})
	response, err := s.client.TransactionGet(context.Background(), "testtxhash")
	require.NoError(s.T(), err)
	require.Equal(s.T(), expectedResponse, response)
}

func (s *clientTestsuite) TestTransactionGetCancel() {
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error)
	go func() {
		_, err := s.client.TransactionGet(ctx, "testtxhash")
		errCh <- err
	}()
	cancel()
	select {
	case err := <-errCh:
		require.ErrorIs(s.T(), err, context.Canceled)
	case <-time.After(2 * time.Second):
		require.Fail(s.T(), "timeoout")
	}
}

func (s *clientTestsuite) TestHeadersSubscribe() {
	latestHeader := &types.Header{Height: 10}
	numNotifiedHeaders := 1000
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.headers.subscribe", request.Method)
		require.Equal(s.T(), []interface{}{}, request.Params)
		// Mock some notifications.
		go func() {
			for i := 0; i < numNotifiedHeaders; i++ {
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
			Result:  test.ToResult(latestHeader),
		}
	})
	receivedHeadersCh := make(chan *types.Header)
	onHeader := func(header *types.Header, err error) {
		require.NoError(s.T(), err)
		receivedHeadersCh <- header
	}
	s.client.HeadersSubscribe(context.Background(), onHeader)

	// A set structure as the notifications can come out of order.
	receivedHeaders := map[int]struct{}{}
	expectedHeaders := map[int]struct{}{}
	for i := 0; i < numNotifiedHeaders+1; i++ {
		expectedHeaders[latestHeader.Height+i] = struct{}{}
		select {
		case header := <-receivedHeadersCh:
			receivedHeaders[header.Height] = struct{}{}
		case <-time.After(time.Second):
			require.Fail(s.T(), "timeout")
		}
	}
	require.Equal(s.T(), expectedHeaders, receivedHeaders)
}

func (s *clientTestsuite) TestScriptHashSubscribe() {
	numNotifications := 1000
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.scripthash.subscribe", request.Method)
		scriptHashHex := request.Params[0].(string)
		// Send a few notifications.
		go func() {
			for i := 0; i < numNotifications; i++ {
				test.Write(s.T(), conn, &jsonrpctypes.Response{
					JSONRPC: jsonrpctypes.JSONRPC,
					Method:  &request.Method,
					Params:  test.ToResult([]interface{}{scriptHashHex, fmt.Sprintf("status-%s-%d", scriptHashHex, i+1)}),
				})
			}
		}()
		// Method response.
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(fmt.Sprintf("status-%s-0", scriptHashHex)),
		}
	})

	numTests := 10
	ch := make(chan string)
	for testCnt := 1; testCnt <= numTests; testCnt++ {
		s.client.ScriptHashSubscribe(
			context.Background(),
			fmt.Sprintf("hash-%d", testCnt),
			func(status string, err error) {
				require.NoError(s.T(), err)
				ch <- status
			},
		)
	}
	receivedStatuses := map[string]struct{}{}
	for i := 0; i < (numNotifications+1)*numTests; i++ {
		select {
		case status := <-ch:
			receivedStatuses[status] = struct{}{}
		case <-time.After(time.Second):
			require.Fail(s.T(), "timeout")
		}
	}
	for testCnt := 1; testCnt <= numTests; testCnt++ {
		for i := 0; i < numNotifications+1; i++ {
			expectedStatus := fmt.Sprintf("status-hash-%d-%d", testCnt, i)
			_, ok := receivedStatuses[expectedStatus]
			require.True(s.T(), ok, "notification missing: %s", expectedStatus)
		}
	}
}

// Tests that the script hash subscription method response can fail.
func (s *clientTestsuite) TestScriptHashSubscribeFailMethod() {
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.scripthash.subscribe", request.Method)
		// Invalid method response.
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			// Invalid result, must be a string.
			Result: test.ToResult(123),
		}
	})

	ch := make(chan struct{})
	s.client.ScriptHashSubscribe(context.Background(), "hash", func(status string, err error) {
		require.Error(s.T(), err)
		close(ch)
	})
	select {
	case <-ch:
	case <-time.After(time.Second):
		require.Fail(s.T(), "timeout")
	}
}

// Tests that the script hash subscription notification can fail.
func (s *clientTestsuite) TestScriptHashSubscribeFailNotification() {
	numNotifications := 1000
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.scripthash.subscribe", request.Method)
		scriptHashHex := request.Params[0].(string)
		// Send a few invalid notifications.
		go func() {
			for i := 0; i < numNotifications; i++ {
				test.Write(s.T(), conn, &jsonrpctypes.Response{
					JSONRPC: jsonrpctypes.JSONRPC,
					Method:  &request.Method,
					// Invalid params, status must be a string.
					Params: test.ToResult([]interface{}{scriptHashHex, 123}),
				})
			}
		}()
		// Valid method response.
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult("status"),
		}
	})

	errCh := make(chan error)
	s.client.SetOnError(func(err error) {
		errCh <- err
	})

	ch := make(chan struct{})
	var counter atomic.Uint32
	s.client.ScriptHashSubscribe(context.Background(), "hash", func(status string, err error) {
		require.NoError(s.T(), err)
		switch counter.Add(1) {
		case 1:
			close(ch)
		default:
			require.Fail(s.T(), "too many notifications")
		}
	})

	select {
	case <-ch:
	case <-time.After(time.Second):
		require.Fail(s.T(), "timeout")
	}

	for i := 0; i < numNotifications; i++ {
		select {
		case err := <-errCh:
			require.Error(s.T(), err)
		case <-time.After(time.Second):
			require.Fail(s.T(), "timeout")
		}
	}
}

func (s *clientTestsuite) TestEstimateFee() {
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.estimatefee", request.Method)
		require.Equal(s.T(), []interface{}{24.}, request.Params)
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(0.0123),
		}
	})
	fee, err := s.client.EstimateFee(context.Background(), 24)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 0.0123, fee)
}

func (s *clientTestsuite) TestEstimateFeeFail() {
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.estimatefee", request.Method)
		require.Equal(s.T(), []interface{}{24.}, request.Params)
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(-1),
		}
	})
	_, err := s.client.EstimateFee(context.Background(), 24)
	require.Error(s.T(), err)
}

func (s *clientTestsuite) TestRelayFee() {
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.relayfee", request.Method)
		require.Equal(s.T(), []interface{}{}, request.Params)
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(0.0123),
		}
	})
	fee, err := s.client.RelayFee(context.Background())
	require.NoError(s.T(), err)
	require.Equal(s.T(), 0.0123, fee)
}

func (s *clientTestsuite) TestTransactionBroadcast() {
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.transaction.broadcast", request.Method)
		require.Equal(s.T(), []interface{}{"rawTxHex"}, request.Params)
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult("txID"),
		}
	})
	txID, err := s.client.TransactionBroadcast(context.Background(), "rawTxHex")
	require.NoError(s.T(), err)
	require.Equal(s.T(), "txID", txID)
}

func (s *clientTestsuite) TestHeaders() {
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.block.headers", request.Method)
		require.Equal(s.T(), []interface{}{0., 10.}, request.Params)
		response := struct {
			Hex   string `json:"hex"`
			Count int    `json:"count"`
			Max   int    `json:"max"`
		}{
			// Ten first block headers of Bitcoin.
			Hex:   "0100000000000000000000000000000000000000000000000000000000000000000000003ba3edfd7a7b12b27ac72c3e67768f617fc81bc3888a51323a9fb8aa4b1e5e4a29ab5f49ffff001d1dac2b7c010000006fe28c0ab6f1b372c1a6a246ae63f74f931e8365e15a089c68d6190000000000982051fd1e4ba744bbbe680e1fee14677ba1a3c3540bf7b1cdb606e857233e0e61bc6649ffff001d01e36299010000004860eb18bf1b1620e37e9490fc8a427514416fd75159ab86688e9a8300000000d5fdcc541e25de1c7a5addedf24858b8bb665c9f36ef744ee42c316022c90f9bb0bc6649ffff001d08d2bd6101000000bddd99ccfda39da1b108ce1a5d70038d0a967bacb68b6b63065f626a0000000044f672226090d85db9a9f2fbfe5f0f9609b387af7be5b7fbb7a1767c831c9e995dbe6649ffff001d05e0ed6d010000004944469562ae1c2c74d9a535e00b6f3e40ffbad4f2fda3895501b582000000007a06ea98cd40ba2e3288262b28638cec5337c1456aaf5eedc8e9e5a20f062bdf8cc16649ffff001d2bfee0a90100000085144a84488ea88d221c8bd6c059da090e88f8a2c99690ee55dbba4e00000000e11c48fecdd9e72510ca84f023370c9a38bf91ac5cae88019bee94d24528526344c36649ffff001d1d03e47701000000fc33f596f822a0a1951ffdbf2a897b095636ad871707bf5d3162729b00000000379dfb96a5ea8c81700ea4ac6b97ae9a9312b2d4301a29580e924ee6761a2520adc46649ffff001d189c4c97010000008d778fdc15a2d3fb76b7122a3b5582bea4f21f5a0c693537e7a03130000000003f674005103b42f984169c7d008370967e91920a6a5d64fd51282f75bc73a68af1c66649ffff001d39a59c86010000004494c8cf4154bdcc0720cd4a59d9c9b285e4b146d45f061d2b6c967100000000e3855ed886605b6d4a99d5fa2ef2e9b0b164e63df3c4136bebf2d0dac0f1f7a667c86649ffff001d1c4b566601000000c60ddef1b7618ca2348a46e868afc26e3efc68226c78aa47f8488c4000000000c997a5e56e104102fa209c6a852dd90660a20b2d9c352423edce25857fcd37047fca6649ffff001d28404f53",
			Count: 10,
			Max:   2016,
		}
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(response),
		}
	})
	result, err := s.client.Headers(context.Background(), 0, 10)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 2016, result.Max)

	unhex := func(str string) []byte {
		b, err := hex.DecodeString(str)
		require.NoError(s.T(), err)
		return b
	}
	require.Equal(s.T(),
		[][]byte{
			unhex("0100000000000000000000000000000000000000000000000000000000000000000000003ba3edfd7a7b12b27ac72c3e67768f617fc81bc3888a51323a9fb8aa4b1e5e4a29ab5f49ffff001d1dac2b7c"),
			unhex("010000006fe28c0ab6f1b372c1a6a246ae63f74f931e8365e15a089c68d6190000000000982051fd1e4ba744bbbe680e1fee14677ba1a3c3540bf7b1cdb606e857233e0e61bc6649ffff001d01e36299"),
			unhex("010000004860eb18bf1b1620e37e9490fc8a427514416fd75159ab86688e9a8300000000d5fdcc541e25de1c7a5addedf24858b8bb665c9f36ef744ee42c316022c90f9bb0bc6649ffff001d08d2bd61"),
			unhex("01000000bddd99ccfda39da1b108ce1a5d70038d0a967bacb68b6b63065f626a0000000044f672226090d85db9a9f2fbfe5f0f9609b387af7be5b7fbb7a1767c831c9e995dbe6649ffff001d05e0ed6d"),
			unhex("010000004944469562ae1c2c74d9a535e00b6f3e40ffbad4f2fda3895501b582000000007a06ea98cd40ba2e3288262b28638cec5337c1456aaf5eedc8e9e5a20f062bdf8cc16649ffff001d2bfee0a9"),
			unhex("0100000085144a84488ea88d221c8bd6c059da090e88f8a2c99690ee55dbba4e00000000e11c48fecdd9e72510ca84f023370c9a38bf91ac5cae88019bee94d24528526344c36649ffff001d1d03e477"),
			unhex("01000000fc33f596f822a0a1951ffdbf2a897b095636ad871707bf5d3162729b00000000379dfb96a5ea8c81700ea4ac6b97ae9a9312b2d4301a29580e924ee6761a2520adc46649ffff001d189c4c97"),
			unhex("010000008d778fdc15a2d3fb76b7122a3b5582bea4f21f5a0c693537e7a03130000000003f674005103b42f984169c7d008370967e91920a6a5d64fd51282f75bc73a68af1c66649ffff001d39a59c86"),
			unhex("010000004494c8cf4154bdcc0720cd4a59d9c9b285e4b146d45f061d2b6c967100000000e3855ed886605b6d4a99d5fa2ef2e9b0b164e63df3c4136bebf2d0dac0f1f7a667c86649ffff001d1c4b5666"),
			unhex("01000000c60ddef1b7618ca2348a46e868afc26e3efc68226c78aa47f8488c4000000000c997a5e56e104102fa209c6a852dd90660a20b2d9c352423edce25857fcd37047fca6649ffff001d28404f53"),
		},
		result.Headers)
}

// Response hex does not divide into 80 byte headers.
func (s *clientTestsuite) TestHeadersFailUnevenBytes() {
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.block.headers", request.Method)
		require.Equal(s.T(), []interface{}{0., 10.}, request.Params)
		response := struct {
			Hex   string `json:"hex"`
			Count int    `json:"count"`
			Max   int    `json:"max"`
		}{
			Hex:   "0100000000000000000000000000000000000000000000000000000000000000000000003ba3edfd7a7b12b27ac72c3e67768f617fc81bc3888a51323a9fb8aa4b1e5e4a29ab5f49ffff001d1dac2b7caabbcc",
			Count: 10,
			Max:   2016,
		}
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(response),
		}
	})
	_, err := s.client.Headers(context.Background(), 0, 10)
	require.Error(s.T(), err)
}

// Response hex does not contain `count` headers.
func (s *clientTestsuite) TestHeadersFailIncorrectNumber() {
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.block.headers", request.Method)
		require.Equal(s.T(), []interface{}{0., 10.}, request.Params)
		response := struct {
			Hex   string `json:"hex"`
			Count int    `json:"count"`
			Max   int    `json:"max"`
		}{
			Hex:   "0100000000000000000000000000000000000000000000000000000000000000000000003ba3edfd7a7b12b27ac72c3e67768f617fc81bc3888a51323a9fb8aa4b1e5e4a29ab5f49ffff001d1dac2b7c",
			Count: 10,
			Max:   2016,
		}
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(response),
		}
	})
	_, err := s.client.Headers(context.Background(), 0, 10)
	require.Error(s.T(), err)
}

func (s *clientTestsuite) TestGetMerkle() {
	txHashHex := "93cb4509402d1c1363dceb1953d8ff6763d0ca9e08d336958df22be9c3a5ca1c"
	height := 764978
	expectedMerkle := []string{
		"f0df2796114c3347e59d2d30d4183360562922e13a7f3f2069cd789f032e07ad",
		"672f847bebfeeeb6706486e92a92c106a5a4ab15adc7caa668017630ae0653d0",
		"4248250df74772b46c2a7216c37ba8e3dc68382b3ff5d9c8e67881bded572c00",
		"936592e882855fee5287e93e9179e8210b17591a2cc8037ef65aaad0343392fa",
		"40153ec9d71707cf5efedee620e0b347da66ab30512fc75648565022a4a924df",
		"a17073e1b29956fb67abbc7012a2a0c5f0da6558e7daf660e46d4ac27139f989",
		"d7ff8fd1cabec8f335e797a3b9d20677b610aaeb8134f46ffdff274e37e69686",
		"70abbdf47d040f4cf19d5a917513a51bc9bad51b44968a806be296b6f1fe908a",
		"5489b496f5fee59386164a98edb6d1b7068bc8d1e425f31d0b2869006af81cf8",
		"9840243ab143864e6b09689a3a2c369fd4f226821c4fac3b708e5eac2eb236ff",
		"6108c29b45a12473b4dff4757ea7b2ebdacdc9b73c9a75cfa7d439e6f0713427",
	}
	s.server.OnRequest.Set(func(conn net.Conn, request *jsonrpctypes.Request) *jsonrpctypes.Response {
		require.Equal(s.T(), "blockchain.transaction.get_merkle", request.Method)
		require.Equal(s.T(), []interface{}{txHashHex, float64(height)}, request.Params)
		response := struct {
			Merkle      []string `json:"merkle"`
			Pos         int      `json:"pos"`
			BlockHeight int      `json:"block_height"`
		}{
			Merkle:      expectedMerkle,
			Pos:         1,
			BlockHeight: height,
		}
		return &jsonrpctypes.Response{
			JSONRPC: jsonrpctypes.JSONRPC,
			ID:      &request.ID,
			Result:  test.ToResult(response),
		}
	})
	merkle, err := s.client.GetMerkle(context.Background(), txHashHex, height)
	require.NoError(s.T(), err)
	require.Equal(s.T(), expectedMerkle, merkle.Merkle)
	require.Equal(s.T(), 1, merkle.Pos)
}
