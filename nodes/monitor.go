package nodes

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// NodeMonitor monitors a set of nodes, and performs checks on them
type NodeMonitor struct {
	nodes          []Node
	quitCh         chan struct{}
	backend        *blockDB
	wg             sync.WaitGroup
	reloadInterval time.Duration
}

// NewMonitor creates a new NodeMonitor
func NewMonitor(nodes []Node, db *blockDB, reload time.Duration) (*NodeMonitor, error) {
	// Do initial healthcheck
	for _, node := range nodes {
		v, err := node.Version()
		if err != nil {
			node.SetStatus(NodeStatusUnreachable)
			log.Error("Error checking version", "error", err)
		} else {
			node.SetStatus(NodeStatusOK)
		}
		log.Info("RPCNode OK", "version", v)
	}
	if reload == 0 {
		reload = 10 * time.Second
	}
	nm := &NodeMonitor{
		nodes:          nodes,
		quitCh:         make(chan struct{}),
		backend:        db,
		reloadInterval: reload,
	}
	nm.doChecks()
	return nm, nil
}

func (mon *NodeMonitor) Start() {
	mon.wg.Add(1)
	go mon.loop()
}

func (mon *NodeMonitor) Stop() {
	close(mon.quitCh)
	mon.wg.Wait()
}

func (mon *NodeMonitor) loop() {
	defer mon.wg.Done()
	for {
		select {
		case <-mon.quitCh:
			return
		case <-time.After(mon.reloadInterval):
			mon.doChecks()
		}
	}
}

func (mon *NodeMonitor) doChecks() {

	// splitSize is the max amount of blocks in any chain not accepted by all nodes.
	// If one node is simply 'behind' that does not count, since it has yet
	// to accept the canon chain
	var splitSize int64
	// We want to cross-check all 'latest' numbers. So if we have
	// node 1: x,
	// node 2: y,
	// node 3: z,
	// Then we want to check the following
	// node 1: (y, z)
	// node 2: (x, z),
	// node 3: (x, y),
	// To figure out if they are on the same chain, or have diverged

	var heads = make(map[uint64]bool)
	var activeNodes []Node
	for _, node := range mon.nodes {
		err := node.UpdateLatest()
		v, _ := node.Version()
		if err != nil {
			log.Error("Error getting latest", "node", v, "error", err)
			node.SetStatus(NodeStatusUnreachable)
		} else {
			activeNodes = append(activeNodes, node)
			node.SetStatus(NodeStatusOK)
			num := node.HeadNum()
			log.Info("Latest", "num", num, "node", v)
			heads[num] = true
		}
	}

	// Pair-wise, figure out the splitblocks (if any)
	forPairs(activeNodes,
		func(a, b Node) {
			highest := a.HeadNum()
			if b.HeadNum() < highest {
				highest = b.HeadNum()
			}
			// At the number where both nodes have blocks, check if the two
			// blocks are identical
			ha := a.BlockAt(highest, false)
			if ha == nil {
				// Yeah this actually _does_ happen, see https://github.com/NethermindEth/nethermind/issues/2306
				log.Error("Node seems to be missing blocks", "name", a.Name(), "number", highest)
				return
			}
			hb := b.BlockAt(highest, false)
			if hb == nil {
				log.Error("Node seems to be missing blocks", "name", b.Name(), "number", highest)
				return
			}
			if ha.hash == hb.hash {
				return
			}
			// They appear to have diverged
			split := findSplit(int(highest), a, b)
			splitLength := int64(int(highest) - split)
			if splitSize < splitLength {
				splitSize = splitLength
			}
			log.Info("Split found", "x", a.Name(), "y", b.Name(), "num", split)
			// Point of interest, add split-block and split-block-minus-one to heads
			heads[uint64(split)] = true
			if split > 0 {
				heads[uint64(split-1)] = true
			}
		},
	)
	metrics.GetOrRegisterGauge("chain/split", registry).Update(int64(splitSize))
	var headList []int
	for k, _ := range heads {
		headList = append(headList, int(k))
	}
	sort.Sort(sort.Reverse(sort.IntSlice(headList)))

	r := NewReport(headList)
	for _, node := range mon.nodes {
		r.AddToReport(node)
	}

	jsd, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		log.Warn("Json marshall fail", "error", err)
		return
	}
	if mon.backend == nil {
		// if there's no backend, this is probably a test.
		// Just print and return
		r.Print()
		fmt.Println(string(jsd))
		return
	}
	if err := ioutil.WriteFile("www/data.json", jsd, 0777); err != nil {
		log.Warn("Failed to write file", "error", err)
		return
	}
	// And now provide relevant hashes
	for _, hash := range r.Hashes {
		hdr := mon.backend.get(hash)
		if hdr == nil {
			log.Warn("Missing header", "hash", hash)
			continue
		}
		fname := fmt.Sprintf("www/hashes/0x%x.json", hash)
		// only write it if it isn't already there
		if _, err := os.Stat(fname); os.IsNotExist(err) {
			data, err := json.MarshalIndent(hdr, "", " ")
			if err != nil {
				log.Warn("Failed to marshall header", "error", err)
				continue
			}
			if err := ioutil.WriteFile(fname, data, 0777); err != nil {
				log.Warn("Failed to write file", "error", err)
				return
			}
		}
	}
}

// For any differences, we want to figure out the split-block.
// Let's say we have:
// node 1: (num1: x)
// node 2: (num1: y)
// Now we need to figure out which block is the first one where they disagreed.
// We do it using a binary search
//
//  Search uses binary search to find and return the smallest index i
//  in [0, n) at which f(i) is true
func findSplit(num int, a Node, b Node) int {
	splitBlock := sort.Search(num, func(i int) bool {
		return a.HashAt(uint64(i), false) != b.HashAt(uint64(i), false)
	})
	return splitBlock
}

// calls 'fn(a, b)' once for each pair in the given list of 'elems'
func forPairs(elems []Node, fn func(a, b Node)) {
	for i := 0; i < len(elems); i++ {
		for j := i + 1; j < len(elems); j++ {
			fn(elems[i], elems[j])
		}
	}
}

type blockDB struct {
	db *leveldb.DB
}

func NewBlockDB() (*blockDB, error) {
	file := "blockDB"
	db, err := leveldb.OpenFile(file, &opt.Options{
		// defaults:
		//BlockCacheCapacity:     8  * opt.MiB,
		//WriteBuffer:            4 * opt.MiB,
	})
	if _, corrupted := err.(*errors.ErrCorrupted); corrupted {
		db, err = leveldb.RecoverFile(file, nil)
	}
	if err != nil {
		return nil, err
	}
	return &blockDB{db}, nil

}

func (db *blockDB) add(key common.Hash, h *types.Header) {
	k := key[:]
	if ok, _ := db.db.Has(k, nil); ok {
		return
	}
	data, err := rlp.EncodeToBytes(h)
	if err != nil {
		panic(fmt.Sprintf("Failed encoding header: %v", err))
	}
	db.db.Put(k, data, nil)
}

func (db *blockDB) get(key common.Hash) *types.Header {
	data, err := db.db.Get(key[:], nil)
	if err != nil {
		return nil
	}
	var h types.Header
	if err = rlp.DecodeBytes(data, &h); err != nil {
		panic(fmt.Sprintf("Failed decoding our own data: %v", err))
	}
	return &h
}
