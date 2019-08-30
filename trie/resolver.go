package trie

import (
	"bytes"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"

	. "github.com/ledgerwatch/turbo-geth/common/bucket"
	"github.com/ledgerwatch/turbo-geth/common/pool"
	"github.com/ledgerwatch/turbo-geth/core/types/accounts"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/valyala/bytebufferpool"
)

var emptyHash [32]byte

func (t *Trie) Rebuild(db ethdb.Database, blockNr uint64) error {
	if t.root == nil {
		return nil
	}
	n, ok := t.root.(hashNode)
	if !ok {
		return fmt.Errorf("Rebuild: Expected hashNode, got %T", t.root)
	}
	if err := t.rebuildHashes(db, nil, 0, blockNr, true, n); err != nil {
		return err
	}
	log.Info("Rebuilt top of account trie and verified", "root hash", n)
	return nil
}

/* One resolver per trie (prefix) */
type TrieResolver struct {
	accounts   bool // Is this a resolver for accounts or for storage
	topLevels  int  // How many top levels of the trie to keep (not roll into hashes)
	requests   []*ResolveRequest
	reqIndices []int // Indices pointing back to request slice from slices returned by PrepareResolveParams
	keyIdx     int
	currentReq *ResolveRequest // Request currently being handled
	currentRs  *ResolveSet     // ResolveSet currently being used
	historical bool
	blockNr    uint64
	hb         *HashBuilder2
	rss        []*ResolveSet
	prec       bytes.Buffer
	curr       bytes.Buffer
	succ       bytes.Buffer
	groups     []uint32
	prefix     []byte
	a          accounts.Account
}

func NewResolver(topLevels int, accounts bool, blockNr uint64) *TrieResolver {
	tr := TrieResolver{
		accounts:   accounts,
		topLevels:  topLevels,
		requests:   []*ResolveRequest{},
		reqIndices: []int{},
		blockNr:    blockNr,
		hb:         NewHashBuilder2(),
	}
	return &tr
}

func (tr *TrieResolver) SetHistorical(h bool) {
	tr.historical = h
}

// TrieResolver implements sort.Interface
// and sorts by resolve requests
// (more general requests come first)
func (tr *TrieResolver) Len() int {
	return len(tr.requests)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (tr *TrieResolver) Less(i, j int) bool {
	ci := tr.requests[i]
	cj := tr.requests[j]
	m := min(ci.resolvePos, cj.resolvePos)
	c := bytes.Compare(ci.contract, cj.contract)
	if c != 0 {
		return c < 0
	}
	c = bytes.Compare(ci.resolveHex[:m], cj.resolveHex[:m])
	if c != 0 {
		return c < 0
	}
	return ci.resolvePos < cj.resolvePos
}

func (tr *TrieResolver) Swap(i, j int) {
	tr.requests[i], tr.requests[j] = tr.requests[j], tr.requests[i]
}

func (tr *TrieResolver) AddRequest(req *ResolveRequest) {
	tr.requests = append(tr.requests, req)
}

func (tr *TrieResolver) Print() {
	for _, req := range tr.requests {
		fmt.Printf("%s\n", req.String())
	}
}

// PrepareResolveParams prepares information for the MultiWalk
func (tr *TrieResolver) PrepareResolveParams() ([][]byte, []uint) {
	// Remove requests strictly contained in the preceding ones
	startkeys := [][]byte{}
	fixedbits := []uint{}
	tr.rss = nil
	if len(tr.requests) == 0 {
		return startkeys, fixedbits
	}
	sort.Stable(tr)
	var prevReq *ResolveRequest
	for i, req := range tr.requests {
		if prevReq == nil ||
			!bytes.Equal(req.contract, prevReq.contract) ||
			!bytes.Equal(req.resolveHex[:req.resolvePos], prevReq.resolveHex[:prevReq.resolvePos]) {

			tr.reqIndices = append(tr.reqIndices, i)
			pLen := len(req.contract)
			key := make([]byte, pLen+32)
			copy(key[:], req.contract)
			decodeNibbles(req.resolveHex[:req.resolvePos], key[pLen:])
			startkeys = append(startkeys, key)
			req.extResolvePos = req.resolvePos + 2*pLen
			fixedbits = append(fixedbits, uint(4*req.extResolvePos))
			prevReq = req
			var minLength int
			if req.resolvePos >= tr.topLevels {
				minLength = 0
			} else {
				minLength = tr.topLevels - req.resolvePos
			}
			rs := NewResolveSet(minLength)
			tr.rss = append(tr.rss, rs)
			rs.AddHex(req.resolveHex[req.resolvePos:])
		} else {
			rs := tr.rss[len(tr.rss)-1]
			rs.AddHex(req.resolveHex[req.resolvePos:])
		}
	}
	tr.currentReq = tr.requests[tr.reqIndices[0]]
	tr.currentRs = tr.rss[0]
	return startkeys, fixedbits
}

// Walker - k, v - shouldn't be reused in the caller's code
func (tr *TrieResolver) Walker(keyIdx int, k []byte, v []byte) (bool, error) {
	//fmt.Printf("keyIdx: %d key:%x  value:%x\n", keyIdx, k, v)
	if keyIdx != tr.keyIdx {
		tr.prec.Reset()
		tr.prec.Write(tr.curr.Bytes())
		tr.curr.Reset()
		tr.curr.Write(tr.succ.Bytes())
		tr.succ.Reset()
		if tr.curr.Len() > 0 {
			tr.prefix, tr.groups = step2(tr.currentRs.HashOnly, false, tr.prec.Bytes(), tr.curr.Bytes(), tr.succ.Bytes(), tr.hb, tr.prefix, tr.groups)
		}
		if tr.hb.hasRoot() {
			hbRoot := tr.hb.root()
			hbHash := tr.hb.rootHash()
			if len(tr.currentReq.resolveHash) > 0 && !bytes.Equal(tr.currentReq.resolveHash, hbHash[:]) {
				return false, fmt.Errorf("mismatching hash: %s %x", tr.currentReq.resolveHash, hbHash)
			}

			if tr.currentReq.RequiresRLP {
				hasher := newHasher(false)
				defer returnHasherToPool(hasher)
				tr.currentReq.NodeRLP = hasher.hashChildren(hbRoot, 0)
			}

			tr.currentReq.t.hook(tr.currentReq.resolveHex[:tr.currentReq.resolvePos], hbRoot)
		}
		tr.hb.Reset()
		tr.groups = nil
		tr.prefix = nil
		tr.keyIdx = keyIdx
		tr.currentReq = tr.requests[tr.reqIndices[keyIdx]]
		tr.currentRs = tr.rss[keyIdx]
		tr.curr.Reset()
		tr.prec.Reset()
	}
	if len(v) > 0 {
		tr.prec.Reset()
		tr.prec.Write(tr.curr.Bytes())
		tr.curr.Reset()
		tr.curr.Write(tr.succ.Bytes())
		tr.succ.Reset()
		skip := tr.currentReq.extResolvePos // how many first nibbles to skip
		i := 0
		for _, b := range k {
			if i >= skip {
				tr.succ.WriteByte(b / 16)
			}
			i++
			if i >= skip {
				tr.succ.WriteByte(b % 16)
			}
			i++
		}
		tr.succ.WriteByte(16)
		if tr.curr.Len() > 0 {
			tr.prefix, tr.groups = step2(tr.currentRs.HashOnly, false, tr.prec.Bytes(), tr.curr.Bytes(), tr.succ.Bytes(), tr.hb, tr.prefix, tr.groups)
		}
		// Remember the current key and value
		if tr.accounts {
			if err := tr.a.Decode(v); err != nil {
				return false, err
			}

			encodeLen := tr.a.EncodingLengthForHashing()
			buf := pool.GetBuffer(encodeLen)

			tr.a.EncodeForHashing(buf.B)
			tr.hb.setKeyValue(skip, k, buf)
		} else {
			var vv *bytebufferpool.ByteBuffer
			if len(v) > 1 || v[0] >= 128 {
				vv = pool.GetBuffer(uint(len(v) + 1))
				vv.B[0] = byte(128 + len(v))
				copy(vv.B[1:], v)
			} else {
				vv = pool.GetBuffer(1)
				vv.B[0] = v[0]
			}
			tr.hb.setKeyValue(skip, k, vv)
		}
	}
	return true, nil
}

func (tr *TrieResolver) ResolveWithDb(db ethdb.Database, blockNr uint64) error {
	startkeys, fixedbits := tr.PrepareResolveParams()
	var err error
	if db == nil {
		var b strings.Builder
		fmt.Fprintf(&b, "ResolveWithDb(db=nil), tr.accounts: %t\n", tr.accounts)
		for i, sk := range startkeys {
			fmt.Fprintf(&b, "sk %x, bits: %d\n", sk, fixedbits[i])
		}
		return fmt.Errorf("Unexpected resolution: %s at %s", b.String(), debug.Stack())
	}
	if tr.accounts {
		if tr.historical {
			err = db.MultiWalkAsOf(AccountsBucket, AccountsHistoryBucket, startkeys, fixedbits, blockNr+1, tr.Walker)
		} else {
			err = db.MultiWalk(AccountsBucket, startkeys, fixedbits, tr.Walker)
		}
	} else {
		if tr.historical {
			err = db.MultiWalkAsOf(StorageBucket, StorageHistoryBucket, startkeys, fixedbits, blockNr+1, tr.Walker)
		} else {
			err = db.MultiWalk(StorageBucket, startkeys, fixedbits, tr.Walker)
		}
	}
	tr.prec.Reset()
	tr.prec.Write(tr.curr.Bytes())
	tr.curr.Reset()
	tr.curr.Write(tr.succ.Bytes())
	tr.succ.Reset()
	if tr.curr.Len() > 0 {
		tr.prefix, tr.groups = step2(tr.currentRs.HashOnly, false, tr.prec.Bytes(), tr.curr.Bytes(), tr.succ.Bytes(), tr.hb, tr.prefix, tr.groups)
	}
	if tr.hb.hasRoot() {
		hbRoot := tr.hb.root()
		hbHash := tr.hb.rootHash()
		if len(tr.currentReq.resolveHash) > 0 && !bytes.Equal(tr.currentReq.resolveHash, hbHash[:]) {
			return fmt.Errorf("mismatching hash: %s %x", tr.currentReq.resolveHash, hbHash)
		}

		if tr.currentReq.RequiresRLP {
			hasher := newHasher(false)
			defer returnHasherToPool(hasher)
			tr.currentReq.NodeRLP = hasher.hashChildren(hbRoot, 0)
		}

		tr.currentReq.t.touchAll(hbRoot, tr.currentReq.resolveHex[:tr.currentReq.resolvePos], false)
		tr.currentReq.t.hook(tr.currentReq.resolveHex[:tr.currentReq.resolvePos], hbRoot)
	}
	return err
}

func (t *Trie) rebuildHashes(db ethdb.Database, key []byte, pos int, blockNr uint64, accounts bool, expected hashNode) error {
	req := t.NewResolveRequest(nil, key, pos, expected)
	r := NewResolver(5, accounts, blockNr)
	r.AddRequest(req)
	return r.ResolveWithDb(db, blockNr)
}
