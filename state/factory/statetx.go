// Copyright (c) 2019 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package factory

import (
	"context"

	"github.com/iotexproject/iotex-core/address"
	"github.com/iotexproject/iotex-core/pkg/keypair"
	"github.com/iotexproject/iotex-core/pkg/log"

	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/action/protocol"
	"github.com/iotexproject/iotex-core/db"
	"github.com/iotexproject/iotex-core/pkg/hash"
	"github.com/iotexproject/iotex-core/pkg/util/byteutil"
	"github.com/iotexproject/iotex-core/state"
)

// stateTX implements stateTX interface, tracks pending changes to account/contract in local cache
type stateTX struct {
	ver            uint64
	blkHeight      uint64
	cb             db.CachedBatch // cached batch for pending writes
	dao            db.KVStore     // the underlying DB for account/contract storage
	actionHandlers []protocol.ActionHandler
}

// newStateTX creates a new state tx
func newStateTX(
	version uint64,
	kv db.KVStore,
	actionHandlers []protocol.ActionHandler,
) *stateTX {
	return &stateTX{
		ver:            version,
		cb:             db.NewCachedBatch(),
		dao:            kv,
		actionHandlers: actionHandlers,
	}
}

// RootHash returns the hash of the root node of the accountTrie
func (stx *stateTX) RootHash() hash.Hash256 { return hash.ZeroHash256 }

// Digest returns the delta state digest
func (stx *stateTX) Digest() hash.Hash256 { return stx.GetCachedBatch().Digest() }

// Version returns the Version of this working set
func (stx *stateTX) Version() uint64 { return stx.ver }

// Height returns the Height of the block being worked on
func (stx *stateTX) Height() uint64 { return stx.blkHeight }

// RunActions runs actions in the block and track pending changes in working set
func (stx *stateTX) RunActions(
	ctx context.Context,
	blockHeight uint64,
	elps []action.SealedEnvelope,
) (hash.Hash256, []*action.Receipt, error) {
	stx.blkHeight = blockHeight
	// Handle actions
	receipts := make([]*action.Receipt, 0)
	for _, elp := range elps {
		// Add caller address into the run action context
		raCtx, ok := protocol.GetRunActionsCtx(ctx)
		if !ok {
			log.S().Panic("Miss context to run action")
		}
		callerPKHash := keypair.HashPubKey(elp.SrcPubkey())
		callerAddr, err := address.FromBytes(callerPKHash[:])
		if err != nil {
			return hash.ZeroHash256, nil, err
		}
		raCtx.Caller = callerAddr
		raCtx.ActionHash = elp.Hash()
		raCtx.Nonce = elp.Nonce()
		ctx = protocol.WithRunActionsCtx(ctx, raCtx)
		for _, actionHandler := range stx.actionHandlers {
			receipt, err := actionHandler.Handle(ctx, elp.Action(), stx)
			if err != nil {
				return hash.ZeroHash256, nil, errors.Wrapf(
					err,
					"error when action %x (nonce: %d) from %s mutates states",
					elp.Hash(),
					elp.Nonce(),
					callerAddr.String(),
				)
			}
			if receipt != nil {
				receipts = append(receipts, receipt)
			}
		}
	}

	// Persist current chain Height
	h := byteutil.Uint64ToBytes(blockHeight)
	stx.cb.Put(AccountKVNameSpace, []byte(CurrentHeightKey), h, "failed to store accountTrie's current Height")
	return hash.ZeroHash256, receipts, nil
}

func (stx *stateTX) Snapshot() int { return stx.cb.Snapshot() }

func (stx *stateTX) Revert(snapshot int) error { return stx.cb.Revert(snapshot) }

// Commit persists all changes in RunActions() into the DB
func (stx *stateTX) Commit() error {
	// Commit all changes in a batch
	dbBatchSizelMtc.WithLabelValues().Set(float64(stx.cb.Size()))
	if err := stx.dao.Commit(stx.cb); err != nil {
		return errors.Wrap(err, "failed to Commit all changes to underlying DB in a batch")
	}
	return nil
}

// GetDB returns the underlying DB for account/contract storage
func (stx *stateTX) GetDB() db.KVStore {
	return stx.dao
}

// GetCachedBatch returns the cached batch for pending writes
func (stx *stateTX) GetCachedBatch() db.CachedBatch {
	return stx.cb
}

// State pulls a state from DB
func (stx *stateTX) State(hash hash.Hash160, s interface{}) error {
	stateDBMtc.WithLabelValues("get").Inc()
	mstate, err := stx.cb.Get(AccountKVNameSpace, hash[:])
	if errors.Cause(err) == db.ErrNotExist {
		if mstate, err = stx.dao.Get(AccountKVNameSpace, hash[:]); errors.Cause(err) == db.ErrNotExist {
			return errors.Wrapf(state.ErrStateNotExist, "k = %x doesn't exist", hash)
		}
	}
	if err != nil {
		return errors.Wrapf(err, "failed to get account of %x", hash)
	}
	return state.Deserialize(s, mstate)
}

// PutState puts a state into DB
func (stx *stateTX) PutState(pkHash hash.Hash160, s interface{}) error {
	stateDBMtc.WithLabelValues("put").Inc()
	ss, err := state.Serialize(s)
	if err != nil {
		return errors.Wrapf(err, "failed to convert account %v to bytes", s)
	}
	stx.cb.Put(AccountKVNameSpace, pkHash[:], ss, "error when putting k = %x", pkHash)
	return nil
}

// DelState deletes a state from DB
func (stx *stateTX) DelState(pkHash hash.Hash160) error {
	stx.cb.Delete(AccountKVNameSpace, pkHash[:], "error when deleting k = %x", pkHash)
	return nil
}
