// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package state

import (
	"bytes"
	"fmt"
	"math/big"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/core/types/accounts"
	"github.com/ledgerwatch/turbo-geth/crypto"
	"github.com/ledgerwatch/turbo-geth/trie"
)

var emptyCodeHash = crypto.Keccak256(nil)
var emptyCodeHashH = common.BytesToHash(emptyCodeHash)

type Code []byte

func (self Code) String() string {
	return string(self) //strings.Join(Disassemble(self), " ")
}

type Storage map[common.Hash]common.Hash

func (self Storage) String() (str string) {
	for key, value := range self {
		str += fmt.Sprintf("%X : %X\n", key, value)
	}

	return
}

func (self Storage) Copy() Storage {
	cpy := make(Storage)
	for key, value := range self {
		cpy[key] = value
	}

	return cpy
}

// stateObject represents an Ethereum account which is being modified.
//
// The usage pattern is as follows:
// First you need to obtain a state object.
// Account values can be accessed and modified through the object.
// Finally, call CommitTrie to write the modified storage trie into a database.
type stateObject struct {
	address  common.Address
	data     accounts.Account
	original accounts.Account
	db       *IntraBlockState

	// DB error.
	// State objects are used by the consensus core and VM which are
	// unable to deal with database-level errors. Any error that occurs
	// during a database read is memoized here and will eventually be returned
	// by IntraBlockState.Commit.
	dbErr error

	// Write caches.
	//trie Trie // storage trie, which becomes non-nil on first access
	code Code // contract bytecode, which gets set when code is loaded

	originStorage      Storage // Storage cache of original entries to dedup rewrites
	blockOriginStorage Storage
	dirtyStorage       Storage // Storage entries that need to be flushed to disk

	// Cache flags.
	// When an object is marked suicided it will be delete from the trie
	// during the "update" phase of the state transition.
	dirtyCode bool // true if the code was updated
	suicided  bool
	deleted   bool
}

// empty returns whether the account is considered empty.
func (so *stateObject) empty() bool {
	return so.data.Nonce == 0 && so.data.Balance.Sign() == 0 && bytes.Equal(so.data.CodeHash[:], emptyCodeHash)
}

// huge number stub. see https://eips.ethereum.org/EIPS/eip-2027
const HugeNumber = uint64(1 << 63)

// newObject creates a state object.
func newObject(db *IntraBlockState, address common.Address, data, original *accounts.Account) *stateObject {
	var so = stateObject{
		db:                 db,
		address:            address,
		originStorage:      make(Storage),
		blockOriginStorage: make(Storage),
		dirtyStorage:       make(Storage),
	}
	so.data.Copy(data)
	if !so.data.Initialised {
		so.data.Balance.SetUint64(0)
		so.data.Initialised = true
	}
	if so.data.CodeHash == (common.Hash{}) {
		so.data.CodeHash = emptyCodeHashH
	}
	if so.data.Root == (common.Hash{}) {
		so.data.Root = trie.EmptyRoot
	}
	so.original.Copy(original)

	return &so
}

// setError remembers the first non-nil error it is called with.
func (so *stateObject) setError(err error) {
	if so.dbErr == nil {
		so.dbErr = err
	}
}

func (so *stateObject) markSuicided() {
	so.suicided = true
}

func (so *stateObject) touch() {
	so.db.journal.append(touchChange{
		account: &so.address,
	})
	if so.address == ripemd {
		// Explicitly put it in the dirty-cache, which is otherwise generated from
		// flattened journals.
		so.db.journal.dirty(so.address)
	}
}

// GetState returns a value from account storage.
func (so *stateObject) GetState(key common.Hash) common.Hash {
	value, dirty := so.dirtyStorage[key]
	if dirty {
		return value
	}
	// Otherwise return the entry's original value
	return so.GetCommittedState(key)
}

// GetCommittedState retrieves a value from the committed account storage trie.
func (so *stateObject) GetCommittedState(key common.Hash) common.Hash {
	// If we have the original value cached, return that
	{
		value, cached := so.originStorage[key]
		if cached {
			return value
		}
	}
	// Load from DB in case it is missing.
	enc, err := so.db.stateReader.ReadAccountStorage(so.address, &key)
	if err != nil {
		so.setError(err)
		return common.Hash{}
	}
	var value common.Hash
	if enc != nil {
		value.SetBytes(enc)
	}
	so.originStorage[key] = value
	so.blockOriginStorage[key] = value
	return value
}

// SetState updates a value in account storage.
func (so *stateObject) SetState(key, value common.Hash) {
	// If the new value is the same as old, don't set
	prev := so.GetState(key)
	if prev == value {
		return
	}
	// New value is different, update and journal the change
	so.db.journal.append(storageChange{
		account:  &so.address,
		key:      key,
		prevalue: prev,
	})
	so.setState(key, value)
}

func (so *stateObject) setState(key, value common.Hash) {
	so.dirtyStorage[key] = value
}

// updateTrie writes cached storage modifications into the object's storage trie.
func (so *stateObject) updateTrie(stateWriter StateWriter, noHistory bool) error {
	for key, value := range so.dirtyStorage {
		key := key
		value := value

		original := so.blockOriginStorage[key]
		so.originStorage[key] = value
		if err := stateWriter.WriteAccountStorage(so.address, &key, &original, &value, noHistory); err != nil {
			return err
		}
	}
	return nil
}

// AddBalance adds amount to so's balance.
// It is used to add funds to the destination account of a transfer.
func (so *stateObject) AddBalance(amount *big.Int) {
	// EIP158: We must check emptiness for the objects such that the account
	// clearing (0,0,0 objects) can take effect.
	if amount.Sign() == 0 {
		if so.empty() {
			so.touch()
		}

		return
	}
	so.SetBalance(new(big.Int).Add(so.Balance(), amount))
}

// SubBalance removes amount from so's balance.
// It is used to remove funds from the origin account of a transfer.
func (so *stateObject) SubBalance(amount *big.Int) {
	if amount.Sign() == 0 {
		return
	}
	so.SetBalance(new(big.Int).Sub(so.Balance(), amount))
}

func (so *stateObject) SetBalance(amount *big.Int) {
	so.db.journal.append(balanceChange{
		account: &so.address,
		prev:    new(big.Int).Set(&so.data.Balance),
	})
	so.setBalance(amount)
}

func (so *stateObject) setBalance(amount *big.Int) {
	so.data.Balance.Set(amount)
	so.data.Initialised = true
}

// Return the gas back to the origin. Used by the Virtual machine or Closures
func (so *stateObject) ReturnGas(gas *big.Int) {}

func (so *stateObject) deepCopy(db *IntraBlockState) *stateObject {
	stateObject := newObject(db, so.address, &so.data, &so.original)
	stateObject.code = so.code
	stateObject.dirtyStorage = so.dirtyStorage.Copy()
	stateObject.originStorage = so.originStorage.Copy()
	stateObject.blockOriginStorage = so.blockOriginStorage.Copy()
	stateObject.suicided = so.suicided
	stateObject.dirtyCode = so.dirtyCode
	stateObject.deleted = so.deleted
	return stateObject
}

//
// Attribute accessors
//

// Returns the address of the contract/account
func (so *stateObject) Address() common.Address {
	return so.address
}

// Code returns the contract code associated with this object, if any.
func (so *stateObject) Code() []byte {
	if so.code != nil {
		return so.code
	}
	if bytes.Equal(so.CodeHash(), emptyCodeHash) {
		return nil
	}
	code, err := so.db.stateReader.ReadAccountCode(common.BytesToHash(so.CodeHash()))
	if err != nil {
		so.setError(fmt.Errorf("can't load code hash %x: %v", so.CodeHash(), err))
	}
	so.code = code
	return code
}

func (so *stateObject) SetCode(codeHash common.Hash, code []byte) {
	prevcode := so.Code()
	so.db.journal.append(codeChange{
		account:  &so.address,
		prevhash: so.data.CodeHash,
		prevcode: prevcode,
	})
	so.setCode(codeHash, code)
}

func (so *stateObject) setCode(codeHash common.Hash, code []byte) {
	so.code = code
	so.data.CodeHash = codeHash
	so.dirtyCode = true
}

func (so *stateObject) SetNonce(nonce uint64) {
	so.db.journal.append(nonceChange{
		account: &so.address,
		prev:    so.data.Nonce,
	})
	so.setNonce(nonce)
}

func (so *stateObject) setNonce(nonce uint64) {
	so.data.Nonce = nonce
}

func (so *stateObject) StorageSize() (bool, uint64) {
	return so.data.HasStorageSize, so.data.StorageSize
}

func (so *stateObject) SetStorageSize(size uint64) {
	so.db.journal.append(storageSizeChange{
		account:  &so.address,
		prevsize: so.data.StorageSize,
	})
	so.setStorageSize(true, size)
}

func (so *stateObject) setStorageSize(has bool, size uint64) {
	so.data.HasStorageSize = has
	so.data.StorageSize = size
}

func (so *stateObject) CodeHash() []byte {
	return so.data.CodeHash[:]
}

func (so *stateObject) Balance() *big.Int {
	return &so.data.Balance
}

func (so *stateObject) Nonce() uint64 {
	return so.data.Nonce
}

// Never called, but must be present to allow stateObject to be used
// as a vm.Account interface that also satisfies the vm.ContractRef
// interface. Interfaces are awesome.
func (so *stateObject) Value() *big.Int {
	panic("Value on stateObject should never be called")
}
