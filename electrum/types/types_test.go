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

package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatus(t *testing.T) {
	history := TxHistory{}
	require.Equal(t, "", history.Status())

	tx1 := &TxInfo{
		Height: 10,
		TxHash: "5feceb66ffc86f38d952786c6d696c79c2dbc239dd4e91b46729d73a27fb57e9",
	}
	tx2 := &TxInfo{
		Height: 12,
		TxHash: "6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b",
	}

	history = []*TxInfo{tx1}
	require.Equal(t,
		"e3c92cc699a99df1e781e36a17910f7915e9d7d428740ba4467011c6ec53f56a",
		history.Status())

	history = []*TxInfo{tx1, tx2}
	require.Equal(t,
		"45b0f24a02f6ae363fe8cc71875f6358bc5ff911b0b33e99219f694c8b762b58",
		history.Status())
}
