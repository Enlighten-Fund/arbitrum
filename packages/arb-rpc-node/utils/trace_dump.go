/*
* Copyright 2020, Offchain Labs, Inc.
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*
*    http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
 */

package utils

import (
	"context"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/pkg/errors"

	"github.com/offchainlabs/arbitrum/packages/arb-evm/evm"
	"github.com/offchainlabs/arbitrum/packages/arb-evm/message"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/snapshot"
	arbcommon "github.com/offchainlabs/arbitrum/packages/arb-util/common"
	"github.com/offchainlabs/arbitrum/packages/arb-util/core"
	"github.com/offchainlabs/arbitrum/packages/arb-util/machine"
	"github.com/offchainlabs/arbitrum/packages/arb-util/value"
)

type TraceAction struct {
	CallType string          `json:"callType,omitempty"`
	From     common.Address  `json:"from"`
	Gas      hexutil.Uint64  `json:"gas"`
	Input    *hexutil.Bytes  `json:"input,omitempty"`
	Init     hexutil.Bytes   `json:"init,omitempty"`
	To       *common.Address `json:"to,omitempty"`
	Value    *hexutil.Big    `json:"value"`
}

type TraceCallResult struct {
	Address *common.Address `json:"address,omitempty"`
	Code    *hexutil.Bytes  `json:"code,omitempty"`
	GasUsed hexutil.Uint64  `json:"gasUsed"`
	Output  *hexutil.Bytes  `json:"output,omitempty"`
}

type TraceFrame struct {
	Action              TraceAction      `json:"action"`
	BlockHash           *hexutil.Bytes   `json:"blockHash,omitempty"`
	BlockNumber         *uint64          `json:"blockNumber,omitempty"`
	Result              *TraceCallResult `json:"result,omitempty"`
	Error               *string          `json:"error,omitempty"`
	Subtraces           int              `json:"subtraces"`
	TraceAddress        []int            `json:"traceAddress"`
	TransactionHash     *hexutil.Bytes   `json:"transactionHash,omitempty"`
	TransactionPosition *uint64          `json:"transactionPosition,omitempty"`
	Type                string           `json:"type"`
}

func ExtractTrace(debugPrints []value.Value) (*evm.EVMTrace, error) {
	var trace *evm.EVMTrace
	for _, debugPrint := range debugPrints {
		parsedLog, err := evm.NewLogLineFromValue(debugPrint)
		if err != nil {
			return nil, err
		}
		foundTrace, ok := parsedLog.(*evm.EVMTrace)
		if ok {
			if trace != nil {
				return nil, errors.New("found multiple traces")
			}
			trace = foundTrace
		}
	}
	if trace == nil {
		return nil, errors.New("found no trace")
	}
	return trace, nil
}

func ExtractValuesFromEmissions(emissions []core.MachineEmission) []value.Value {
	values := make([]value.Value, 0, len(emissions))
	for _, emission := range emissions {
		values = append(values, emission.Value)
	}
	return values
}

func NeedsTopLevelCreate(frames []TraceFrame) bool {
	return len(frames) > 0 &&
		frames[0].Type == "create" &&
		frames[0].Result != nil &&
		frames[0].Result.Address != nil
}

func FillInTopLevelCreate(ctx context.Context, frames []TraceFrame, snap *snapshot.Snapshot) {
	createdCode, err := snap.GetCode(ctx, arbcommon.NewAddressFromEth(*frames[0].Result.Address))
	if err != nil {
		logger.Warn().Msg("failed to retrieve code for contract")
	} else {
		frames[0].Result.Code = (*hexutil.Bytes)(&createdCode)
	}
}

func RenderTraceFrames(txRes *evm.TxResult, trace *evm.EVMTrace) ([]TraceFrame, error) {
	frame, err := trace.FrameTree()
	if err != nil || frame == nil {
		return nil, err
	}

	type trackedFrame struct {
		f            evm.Frame
		traceAddress []int
	}

	frames := []trackedFrame{{f: frame, traceAddress: make([]int, 0)}}
	resFrames := make([]TraceFrame, 0)
	for len(frames) > 0 {
		frame := frames[0]
		frames = frames[1:]

		callFrame := frame.f.GetCallFrame()
		action := TraceAction{
			From:  callFrame.Call.From.ToEthAddress(),
			Gas:   hexutil.Uint64(callFrame.Call.Gas.Uint64()),
			Value: (*hexutil.Big)(callFrame.Call.Value),
		}

		var result *TraceCallResult
		var callErr *string
		if callFrame.Return.Result == evm.ReturnCode {
			result = &TraceCallResult{
				GasUsed: hexutil.Uint64(callFrame.Return.GasUsed.Uint64()),
			}
		} else {
			tmp := callFrame.Return.Result.String()
			callErr = &tmp
		}

		var frameType string
		switch frame := frame.f.(type) {
		case *evm.CallFrame:
			// Top level call could actually be contract creation
			if len(resFrames) == 0 && txRes.IsContractCreation() {
				frameType = "create"
				// Call frame has no input for contract construction
				if txRes.IncomingRequest.Kind == message.L2Type || txRes.IncomingRequest.Kind == message.EthDepositTxType {
					abstractMessage, err := message.L2Message{Data: txRes.IncomingRequest.Data}.AbstractMessage()
					if err == nil {
						if msg, ok := abstractMessage.(message.EthConvertable); ok {
							action.Init = msg.AsEthTx().Data()
						}
					}
				}

				if result != nil {
					topLevelContractAddress, gotAddress := txRes.GetCreatedContractAddress()
					if gotAddress {
						result.Address = &topLevelContractAddress
					}
					// Return data contains the created contract address so we can't get the created code from that
					// We'll get it by querying the code instead
				}
			} else {
				frameType = "call"
				tmpInput := hexutil.Bytes(callFrame.Call.Data)
				action.Input = &tmpInput
				var toTmp common.Address
				if callFrame.Call.To != nil {
					toTmp = callFrame.Call.To.ToEthAddress()
				}
				action.To = &toTmp
				action.CallType = callFrame.Call.Type.RPCString()
				if result != nil {
					result.Output = (*hexutil.Bytes)(&callFrame.Return.ReturnData)
				}
			}
		case *evm.CreateFrame:
			frameType = "create"
			action.Init = frame.Create.Code
			if result != nil {
				tmp := frame.Create.ContractAddress.ToEthAddress()
				result.Address = &tmp
				result.Code = (*hexutil.Bytes)(&callFrame.Return.ReturnData)
			}
		case *evm.Create2Frame:
			frameType = "create"
			action.Init = frame.Create.Code
			if result != nil {
				tmp := frame.Create.ContractAddress.ToEthAddress()
				result.Address = &tmp
				result.Code = (*hexutil.Bytes)(&callFrame.Return.ReturnData)
			}
		}

		resFrames = append(resFrames, TraceFrame{
			Action:       action,
			Result:       result,
			Error:        callErr,
			Subtraces:    len(callFrame.Nested),
			TraceAddress: frame.traceAddress,
			Type:         frameType,
		})
		for i, nested := range callFrame.Nested {
			nestedTrace := make([]int, 0)
			nestedTrace = append(nestedTrace, frame.traceAddress...)
			nestedTrace = append(nestedTrace, i)
			frames = append(frames, trackedFrame{f: nested, traceAddress: nestedTrace})
		}
	}
	sort.SliceStable(resFrames, func(i, j int) bool {
		addr1 := resFrames[i].TraceAddress
		addr2 := resFrames[j].TraceAddress
		maxLength := len(addr1)
		if len(addr2) > maxLength {
			maxLength = len(addr2)
		}
		for i := 0; i < maxLength; i++ {
			if i >= len(addr1) {
				return true
			}
			if i >= len(addr2) {
				return false
			}
			if addr1[i] < addr2[i] {
				return true
			}
		}
		return false
	})
	return resFrames, nil
}

type ChainContext struct {
	blockHash           *hexutil.Bytes
	blockNumber         *uint64
	transactionHash     *hexutil.Bytes
	transactionPosition *uint64
}

func NewChainContext(res *evm.TxResult, blockInfo *machine.BlockInfo) *ChainContext {
	blockHash := hexutil.Bytes(blockInfo.Header.Hash().Bytes())
	blockNumber := res.IncomingRequest.L2BlockNumber.Uint64()
	txIndex := res.TxIndex.Uint64()
	txHash := hexutil.Bytes(res.IncomingRequest.MessageID.Bytes())
	return &ChainContext{
		blockHash:           &blockHash,
		blockNumber:         &blockNumber,
		transactionHash:     &txHash,
		transactionPosition: &txIndex,
	}
}

func AddChainContext(frame *TraceFrame, context *ChainContext) {
	frame.TransactionHash = context.transactionHash
	frame.TransactionPosition = context.transactionPosition
	frame.BlockNumber = context.blockNumber
	frame.BlockHash = context.blockHash
}
