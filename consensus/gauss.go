// Copyright 2021 The PlatON Network Authors
// This file is part of the PlatON-Go library.

package consensus

import (
	"math/big"

	"github.com/PlatONnetwork/PlatON-Go/common"
	"github.com/PlatONnetwork/PlatON-Go/core/snapshotdb"
	"github.com/PlatONnetwork/PlatON-Go/core/types"
	"github.com/PlatONnetwork/PlatON-Go/params"
	"github.com/PlatONnetwork/PlatON-Go/x/gov"
)

// IsGaussEnabled reports whether Gauss (FORKVERSION_1_7_0) rules apply to the
// block identified by number and parent header. Gauss covers the Ethereum
// features that are not part of Hawking: Cancun opcodes, EIP-4844, EIP-3651
// and EIP-3860.
//
// certain is false when the caller cannot load parent state (e.g. HeaderChain
// during snap header sync). Callers must not treat uncertain results as a
// definitive "before Gauss" when rejecting post-fork header fields.
func IsGaussEnabled(config *params.ChainConfig, number *big.Int, parent *types.Header, stateAt StateAtFunc) (enabled, certain bool) {
	if !config.IsPauli(number) {
		return false, true
	}
	if config.IsGauss(number) {
		return true, true
	}
	if number == nil || number.Sign() == 0 {
		return false, true
	}
	if parent == nil || stateAt == nil {
		return false, false
	}
	statedb, err := stateAt(parent.Root)
	if err != nil {
		return false, false
	}
	govInst := gov.NewGov(snapshotdb.Instance())
	if govInst.Gte170VersionState(statedb) {
		return true, true
	}
	gdb := gov.NewGovDB(snapshotdb.Instance())
	if gdb.GetPreActiveVersion(parent.Hash()) < params.FORKVERSION_1_7_0 {
		return false, true
	}
	preActiveID, err := gdb.GetPreActiveProposalID(parent.Hash())
	if err != nil || preActiveID == common.ZeroHash {
		return false, true
	}
	prop, err := gdb.GetExistProposal(preActiveID, statedb)
	if err != nil {
		return false, true
	}
	vp, ok := prop.(*gov.VersionProposal)
	return ok && vp.NewVersion >= params.FORKVERSION_1_7_0 && vp.GetActiveBlock() == number.Uint64(), true
}
