package indexers

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mit-dci/utreexo/accumulator"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/chaincfg"
	"github.com/utreexo/utreexod/database"
	_ "github.com/utreexo/utreexod/database/ffldb"
	"github.com/utreexo/utreexod/txscript"
	"github.com/utreexo/utreexod/wire"
)

const (
	// testDbType is the database backend type to use for the tests.
	testDbType = "ffldb"

	// testDbRoot is the root directory used to create all test databases.
	testDbRoot = "testdbs"

	// blockDataNet is the expected network in the test block data.
	blockDataNet = wire.MainNet
)

func createDB(dbName string) (database.DB, string, error) {
	if !blockchain.IsSupportedDbType(testDbType) {
		return nil, "", fmt.Errorf("unsupported db type %v", testDbType)
	}

	// Handle memory database specially since it doesn't need the disk
	// specific handling.
	var db database.DB
	var dbPath string
	if testDbType == "memdb" {
		ndb, err := database.Create(testDbType)
		if err != nil {
			return nil, "", fmt.Errorf("error creating db: %v", err)
		}
		db = ndb
	} else {
		// Create the root directory for test databases.
		if !blockchain.FileExists(testDbRoot) {
			if err := os.MkdirAll(testDbRoot, 0700); err != nil {
				err := fmt.Errorf("unable to create test db "+
					"root: %v", err)
				return nil, "", err
			}
		}

		// Create a new database to store the accepted blocks into.
		dbPath = filepath.Join(testDbRoot, dbName)
		_ = os.RemoveAll(dbPath)
		ndb, err := database.Create(testDbType, dbPath, blockDataNet)
		if err != nil {
			return nil, "", fmt.Errorf("error creating db: %v", err)
		}
		db = ndb
	}

	return db, dbPath, nil
}

func initIndexes(dbPath string, db *database.DB, params *chaincfg.Params) (
	*Manager, []Indexer, error) {

	flatUtreexoProofIndex, err := NewFlatUtreexoProofIndex(dbPath, params)
	if err != nil {
		return nil, nil, err
	}

	utreexoProofIndex, err := NewUtreexoProofIndex(*db, dbPath, params)
	if err != nil {
		return nil, nil, err
	}

	indexes := []Indexer{
		utreexoProofIndex,
		flatUtreexoProofIndex,
	}
	indexManager := NewManager(*db, indexes)
	return indexManager, indexes, nil
}

func indexersTestChain(testName string) (*blockchain.BlockChain, []Indexer, *chaincfg.Params, func()) {
	params := chaincfg.RegressionNetParams
	params.CoinbaseMaturity = 1

	db, dbPath, err := createDB(testName)
	tearDown := func() {
		db.Close()
		os.RemoveAll(dbPath)
	}
	if err != nil {
		tearDown()
		os.RemoveAll(testDbRoot)
		panic(fmt.Errorf("error creating database: %v", err))
	}

	// Create the indexes to be used in the chain.
	indexManager, indexes, err := initIndexes(dbPath, &db, &params)
	if err != nil {
		tearDown()
		os.RemoveAll(testDbRoot)
		panic(fmt.Errorf("error creating indexes: %v", err))
	}

	// Create the main chain instance.
	chain, err := blockchain.New(&blockchain.Config{
		DB:               db,
		ChainParams:      &params,
		Checkpoints:      nil,
		TimeSource:       blockchain.NewMedianTime(),
		SigCache:         txscript.NewSigCache(1000),
		UtxoCacheMaxSize: 10 * 1024 * 1024,
		IndexManager:     indexManager,
	})
	if err != nil {
		tearDown()
		os.RemoveAll(testDbRoot)
		panic(fmt.Errorf("failed to create chain instance: %v", err))
	}

	// Init the indexes.
	err = indexManager.Init(chain, nil)
	if err != nil {
		tearDown()
		os.RemoveAll(testDbRoot)
		panic(fmt.Errorf("failed to init indexs: %v", err))
	}

	return chain, indexes, &params, tearDown
}

// csnTestChain creates a chain using the compact utreexo state.
func csnTestChain(testName string) (*blockchain.BlockChain, *chaincfg.Params, func(), error) {
	params := chaincfg.RegressionNetParams
	params.CoinbaseMaturity = 1

	db, dbPath, err := createDB(testName)
	tearDown := func() {
		db.Close()
		os.RemoveAll(dbPath)
	}
	if err != nil {
		return nil, nil, tearDown, err
	}

	// Create the main csn chain instance.
	chain, err := blockchain.New(&blockchain.Config{
		DB:          db,
		ChainParams: &params,
		Checkpoints: nil,
		TimeSource:  blockchain.NewMedianTime(),
		SigCache:    txscript.NewSigCache(1000),
		UtreexoView: blockchain.NewUtreexoViewpoint(),
	})
	if err != nil {
		err := fmt.Errorf("failed to create csn chain instance: %v", err)
		return nil, nil, tearDown, err
	}

	return chain, &params, tearDown, nil
}

// compareUtreexoIdx compares the indexed proof and the undo blocks from start
// to end.
func compareUtreexoIdx(start, end int32, chain *blockchain.BlockChain, indexes []Indexer) error {
	// Check that the newly added data to both of the indexes are equal.
	for b := start; b < end; b++ {
		// Declare the utreexo data and the undo blocks that we'll be
		// comparing.
		var utreexoUD, flatUD *wire.UData
		var undo, flatUndo *accumulator.UndoBlock

		for _, indexer := range indexes {
			switch idxType := indexer.(type) {
			case *UtreexoProofIndex:
				block, err := chain.BlockByHeight(b)
				utreexoUD, err = idxType.FetchUtreexoProof(block.Hash())
				if err != nil {
					return err
				}

				err = idxType.db.View(func(dbTx database.Tx) error {
					undoBytes, err := dbFetchUndoBlockEntry(dbTx, block.Hash())
					r := bytes.NewReader(undoBytes)
					undo = new(accumulator.UndoBlock)
					err = undo.Deserialize(r)
					if err != nil {
						return err
					}
					return nil
				})
				if err != nil {
					return err
				}

			case *FlatUtreexoProofIndex:
				var err error
				flatUD, err = idxType.FetchUtreexoProof(b)
				if err != nil {
					return err
				}

				flatUndo, err = idxType.fetchUndoBlock(b)
				if err != nil {
					return err
				}
			}
		}

		if !reflect.DeepEqual(utreexoUD, flatUD) {
			err := fmt.Errorf("Fetched utreexo data differ for "+
				"utreexo proof index and flat utreexo proof index at height %d", b)
			return err
		}

		if !reflect.DeepEqual(undo, flatUndo) {
			err := fmt.Errorf("Fetched undo data differ for "+
				"utreexo proof index and flat utreexo proof index at height %d", b)
			return err
		}
	}

	return nil
}

// syncCsnChain will take in two chains: one to sync from, one to sync.  Sync will
// be done from start to end.
func syncCsnChain(start, end int32, chainToSyncFrom, csnChain *blockchain.BlockChain,
	indexes []Indexer) error {

	for b := start; b < end; b++ {
		// Fetch the raw block bytes from the database.
		block, err := chainToSyncFrom.BlockByHeight(b)
		if err != nil {
			str := fmt.Errorf("Fail at block height %d err:%s\n", b, err)
			return str
		}

		var flatUD, ud *wire.UData
		for _, indexer := range indexes {
			switch idxType := indexer.(type) {
			case *FlatUtreexoProofIndex:
				flatUD, err = idxType.FetchUtreexoProof(b)
				if err != nil {
					return err
				}
			case *UtreexoProofIndex:
				ud, err = idxType.FetchUtreexoProof(block.Hash())
				if err != nil {
					return err
				}
			}
		}

		if !reflect.DeepEqual(ud, flatUD) {
			err := fmt.Errorf("Fetched utreexo data differ for "+
				"utreexo proof index and flat utreexo proof index at height %d", b)
			return err
		}

		block.MsgBlock().UData = ud

		_, _, err = csnChain.ProcessBlock(block, blockchain.BFNone)
		if err != nil {
			str := fmt.Errorf("ProcessBlock fail at block height %d err: %s\n", b, err)
			return str
		}
	}

	return nil
}

func TestUtreexoProofIndex(t *testing.T) {
	// Always remove the root on return.
	defer os.RemoveAll(testDbRoot)

	source := rand.NewSource(time.Now().UnixNano())
	rand := rand.New(source)

	chain, indexes, params, tearDown := indexersTestChain("TestUtreexoProofIndex")
	defer tearDown()

	tip := btcutil.NewBlock(params.GenesisBlock)

	// Create block at height 1.
	var emptySpendableOuts []*blockchain.SpendableOut
	b1, spendableOuts1 := blockchain.AddBlock(chain, tip, emptySpendableOuts)

	var allSpends []*blockchain.SpendableOut
	nextBlock := b1
	nextSpends := spendableOuts1

	// Create a chain with 101 blocks.
	for b := 0; b < 100; b++ {
		newBlock, newSpendableOuts := blockchain.AddBlock(chain, nextBlock, nextSpends)
		nextBlock = newBlock

		allSpends = append(allSpends, newSpendableOuts...)

		var nextSpendsTmp []*blockchain.SpendableOut
		for i := 0; i < len(allSpends); i++ {
			randIdx := rand.Intn(len(allSpends))

			spend := allSpends[randIdx]                                       // get
			allSpends = append(allSpends[:randIdx], allSpends[randIdx+1:]...) // delete
			nextSpendsTmp = append(nextSpendsTmp, spend)
		}
		nextSpends = nextSpendsTmp

		if b%10 == 0 {
			// Commit the two base blocks to DB
			if err := chain.FlushCachedState(blockchain.FlushRequired); err != nil {
				t.Fatalf("unexpected error while flushing cache: %v", err)
			}
		}
	}

	// Check that the added 100 blocks are equal for both indexes.
	err := compareUtreexoIdx(1, 100, chain, indexes)
	if err != nil {
		t.Fatal(err)
	}

	// Create a chain that consumes the data from the indexes and test that this
	// chain is able to consume the data properly.
	csnChain, _, csnTearDown, err := csnTestChain("TestUtreexoProofIndex-CsnChain")
	defer csnTearDown()
	if err != nil {
		t.Fatal(err)
	}

	// Sync the csn chain to the tip from block 1.
	err = syncCsnChain(1, 100, chain, csnChain, indexes)
	if err != nil {
		t.Fatal(err)
	}

	// We'll start adding a different chain starting from block 1. Once we reach block 102,
	// we'll switch over to this chain.
	altBlocks := make([]*btcutil.Block, 110)
	var altSpends []*blockchain.SpendableOut
	altNextSpends := spendableOuts1
	altNextBlock := b1
	for i := range altBlocks {
		var newSpends []*blockchain.SpendableOut
		altBlocks[i], newSpends = blockchain.AddBlock(chain, altNextBlock, altNextSpends)
		altNextBlock = altBlocks[i]

		altSpends = append(altSpends, newSpends...)

		var nextSpendsTmp []*blockchain.SpendableOut
		for i := 0; i < len(altSpends); i++ {
			randIdx := rand.Intn(len(altSpends))

			spend := altSpends[randIdx]                                       // get
			altSpends = append(altSpends[:randIdx], altSpends[randIdx+1:]...) // delete
			nextSpendsTmp = append(nextSpendsTmp, spend)
		}
		altNextSpends = nextSpendsTmp
	}

	// Check that the newly added data to both of the indexes are equal.
	err = compareUtreexoIdx(1, 100, chain, indexes)
	if err != nil {
		t.Fatal(err)
	}

	// Reorg the csn chain as well.
	err = syncCsnChain(2, 100, chain, csnChain, indexes)
	if err != nil {
		t.Fatal(err)
	}
}
