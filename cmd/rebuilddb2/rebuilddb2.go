// Copyright (c) 2018-2019, The Decred-Next developers
// Copyright (c) 2017, The dcrdata developers
// See LICENSE for details.

package main

import (
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/decred/dcrd/rpcclient/v5"
	"github.com/decred/dcrdata/db/dcrpg/v5"
	"github.com/decred/dcrdata/rpcutils/v3"
	"github.com/decred/dcrdata/stakedb/v3"
	"github.com/decred/slog"
	"github.com/dmigwi/go-piparser/proposals"
)

var (
	backendLog      *slog.Backend
	rpcclientLogger slog.Logger
	pgLogger        slog.Logger
	stakedbLogger   slog.Logger
)

const (
	rescanLogBlockChunk = 250
)

func init() {
	err := InitLogger()
	if err != nil {
		fmt.Printf("Unable to start logger: %v", err)
		os.Exit(1)
	}
	backendLog = slog.NewBackend(log.Writer())
	rpcclientLogger = backendLog.Logger("RPC")
	rpcclient.UseLogger(rpcclientLogger)
	pgLogger = backendLog.Logger("PSQL")
	dcrpg.UseLogger(pgLogger)
	stakedbLogger = backendLog.Logger("SKDB")
	stakedb.UseLogger(stakedbLogger)
}

func mainCore() error {
	// Parse the configuration file, and setup logger.
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("Failed to load dcrdata config: %s\n", err.Error())
		return err
	}

	if cfg.HTTPProfile {
		go func() {
			log.Infoln(http.ListenAndServe("localhost:6060", nil))
		}()
	}

	if cfg.CPUProfile != "" {
		var f *os.File
		f, err = os.Create(cfg.CPUProfile)
		if err != nil {
			log.Fatal(err)
			return err
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if cfg.MemProfile != "" {
		var f *os.File
		f, err = os.Create(cfg.MemProfile)
		if err != nil {
			log.Fatal(err)
			return err
		}
		timer := time.NewTimer(time.Second * 15)
		go func() {
			<-timer.C
			pprof.WriteHeapProfile(f)
			f.Close()
		}()
	}

	// Connect to node RPC server
	client, _, err := rpcutils.ConnectNodeRPC(cfg.DcrdServ, cfg.DcrdUser,
		cfg.DcrdPass, cfg.DcrdCert, cfg.DisableDaemonTLS, false)
	if err != nil {
		log.Fatalf("Unable to connect to RPC server: %v", err)
		return err
	}

	infoResult, err := client.GetInfo()
	if err != nil {
		log.Errorf("GetInfo failed: %v", err)
		return err
	}
	log.Info("Node connection count: ", infoResult.Connections)

	host, port := cfg.DBHostPort, ""
	if !strings.HasPrefix(host, "/") {
		host, port, err = net.SplitHostPort(cfg.DBHostPort)
		if err != nil {
			log.Errorf("SplitHostPort failed: %v", err)
			return err
		}
	}

	// Configure PostgreSQL ChainDB
	dbi := dcrpg.DBInfo{
		Host:   host,
		Port:   port,
		User:   cfg.DBUser,
		Pass:   cfg.DBPass,
		DBName: cfg.DBName,
	}

	log.Infof("Setting up the Politeia's proposals clone repository. Please wait...")

	// repoName and repoOwner are set to empty string so that the defaults can be used.
	parser, err := proposals.NewParser("", "", cfg.LogDir)
	if err != nil {
		return err
	}

	var piParser dcrpg.ProposalsFetcher
	if parser != nil {
		piParser = parser
	}

	// Construct a ChainDB without a stakeDB to allow quick dropping of tables.
	dbCfg := &dcrpg.ChainDBCfg{
		DBi:                  &dbi,
		Params:               activeChain,
		DevPrefetch:          true,
		HidePGConfig:         false,
		AddrCacheAddrCap:     2,
		AddrCacheRowCap:      2,
		AddrCacheUTXOByteCap: 1 << 5,
	}
	mpChecker := rpcutils.NewMempoolAddressChecker(client, activeChain)
	db, err := dcrpg.NewChainDB(dbCfg, nil, mpChecker, piParser, client, func() {})
	if db != nil {
		defer db.Close()
	}
	if err != nil || db == nil {
		return err
	}

	if cfg.DropDBTables {
		db.DropTables()
		return nil
	}

	// Create/load stake database (which includes the separate ticket pool DB).
	sdbDir := "rebuild_data"
	stakeDB, stakeDBHeight, err := stakedb.NewStakeDatabase(client, activeChain, sdbDir)
	if err != nil {
		log.Errorf("Unable to create stake DB: %v", err)
		if stakeDBHeight >= 0 {
			log.Infof("Attempting to recover stake DB...")
			stakeDB, err = stakedb.LoadAndRecover(client, activeChain, sdbDir, stakeDBHeight-288)
			stakeDBHeight = int64(stakeDB.Height())
		}
		if err != nil {
			if stakeDB != nil {
				_ = stakeDB.Close()
			}
			return fmt.Errorf("StakeDatabase recovery failed: %v", err)
		}
	}
	defer stakeDB.Close()

	log.Infof("Loaded StakeDatabase at height %d", stakeDBHeight)

	// Provide the stake database to the ChainDB for all of it's ticket tracking
	// needs.
	db.UseStakeDB(stakeDB)

	if cfg.DuplicateEntryRecovery {
		return db.DeleteDuplicatesRecovery(nil)
	}

	// Ctrl-C to shut down.
	// Nothing should be sent the quit channel.  It should only be closed.
	quit := make(chan struct{})
	// Only accept a single CTRL+C
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	// Check current height of DB
	lastBlock, err := db.HeightDB()
	if err != nil {
		log.Errorln("RetrieveBestBlockHeight:", err)
		return err
	}
	if lastBlock == -1 {
		log.Info("tables are empty, starting fresh.")
	}

	// Start waiting for the interrupt signal
	go func() {
		<-c
		signal.Stop(c)
		// Close the channel so multiple goroutines can get the message
		log.Infof("CTRL+C hit.  Closing goroutines. Please wait.")
		close(quit)
	}()

	// Get stakedb at PG DB height
	var rewindTo int64
	if lastBlock > 0 {
		// Rewind one extra block to ensure previous winning tickets (validators
		// for current block) get stored in the cache by advancing one block.
		rewindTo = lastBlock - 1
	}
	if stakeDBHeight > rewindTo {
		log.Infof("Rewinding stake db from %d to %d...", stakeDBHeight, rewindTo)
	}
	for stakeDBHeight > rewindTo {
		// check for quit signal
		select {
		case <-quit:
			log.Infof("Rewind cancelled at height %d.", stakeDBHeight)
			return nil
		default:
		}
		if err = stakeDB.DisconnectBlock(false); err != nil {
			return err
		}
		stakeDBHeight = int64(stakeDB.Height())
	}

	// Advance to last block, but don't log if it's just one block to connect
	if stakeDBHeight+1 < lastBlock {
		log.Infof("Advancing stake db from %d to %d...", stakeDBHeight, lastBlock)
	}
	for stakeDBHeight < lastBlock {
		// check for quit signal
		select {
		case <-quit:
			log.Infof("Rescan cancelled at height %d.", stakeDBHeight)
			return nil
		default:
		}

		block, blockHash, err := rpcutils.GetBlock(stakeDBHeight+1, client)
		if err != nil {
			return fmt.Errorf("GetBlock failed (%s): %v", blockHash, err)
		}

		if err = stakeDB.ConnectBlock(block); err != nil {
			return err
		}
		stakeDBHeight = int64(stakeDB.Height())
		if stakeDBHeight%1000 == 0 {
			log.Infof("Stake DB at height %d.", stakeDBHeight)
		}
	}

	// Note that we are doing a batch blockchain sync
	db.InBatchSync = true
	defer func() { db.InBatchSync = false }()

	var totalTxs, totalVins, totalVouts int64
	var lastTxs, lastVins, lastVouts int64
	tickTime := 10 * time.Second
	ticker := time.NewTicker(tickTime)
	startTime := time.Now()
	o := sync.Once{}
	speedReporter := func() {
		ticker.Stop()
		totalElapsed := time.Since(startTime).Seconds()
		if int64(totalElapsed) == 0 {
			return
		}
		totalVoutPerSec := totalVouts / int64(totalElapsed)
		totalTxPerSec := totalTxs / int64(totalElapsed)
		log.Infof("Avg. speed: %d tx/s, %d vout/s", totalTxPerSec, totalVoutPerSec)
	}
	speedReport := func() { o.Do(speedReporter) }
	defer speedReport()

	// Get chain servers's best block
	_, height, err := client.GetBestBlock()
	if err != nil {
		return fmt.Errorf("GetBestBlock failed: %v", err)
	}

	// Remove indexes/constraints before bulk import
	blocksToSync := height - lastBlock
	reindexing := blocksToSync > height/2
	if reindexing || cfg.ForceReindex {
		log.Info("Large bulk load: Removing indexes and disabling duplicate checks.")
		err = db.DeindexAll()
		if err != nil && !strings.Contains(err.Error(), "does not exist") {
			return err
		}
		db.EnableDuplicateCheckOnInsert(false)
	} else {
		db.EnableDuplicateCheckOnInsert(true)
	}

	startHeight := lastBlock + 1
	for ib := startHeight; ib <= height; ib++ {
		// check for quit signal
		select {
		case <-quit:
			log.Infof("Rescan cancelled at height %d.", ib)
			return nil
		default:
		}

		if (ib-1)%rescanLogBlockChunk == 0 || ib == startHeight {
			if ib == 0 {
				log.Infof("Scanning genesis block.")
			} else {
				endRangeBlock := rescanLogBlockChunk * (1 + (ib-1)/rescanLogBlockChunk)
				if endRangeBlock > height {
					endRangeBlock = height
				}
				log.Infof("Processing blocks %d to %d...", ib, endRangeBlock)
			}
		}
		select {
		case <-ticker.C:
			blocksPerSec := float64(ib-lastBlock) / tickTime.Seconds()
			txPerSec := float64(totalTxs-lastTxs) / tickTime.Seconds()
			vinsPerSec := float64(totalVins-lastVins) / tickTime.Seconds()
			voutPerSec := float64(totalVouts-lastVouts) / tickTime.Seconds()
			log.Infof("(%3d blk/s,%5d tx/s,%5d vin/sec,%5d vout/s)", int64(blocksPerSec),
				int64(txPerSec), int64(vinsPerSec), int64(voutPerSec))
			lastBlock, lastTxs = ib, totalTxs
			lastVins, lastVouts = totalVins, totalVouts
		default:
		}

		block, blockHash, err := rpcutils.GetBlock(ib, client)
		if err != nil {
			return fmt.Errorf("GetBlock failed (%s): %v", blockHash, err)
		}

		// Grab the chainwork.
		chainWork, err := rpcutils.GetChainWork(client, blockHash)
		if err != nil {
			return fmt.Errorf("GetChainWork failed (%s): %v", blockHash, err)
		}

		var numVins, numVouts int64
		isValid, isMainchain, updateExistingRecords := true, true, true
		numVins, numVouts, _, err = db.StoreBlock(block.MsgBlock(), isValid,
			isMainchain, updateExistingRecords, cfg.AddrSpendInfoOnline,
			!cfg.TicketSpendInfoBatch, chainWork)
		if err != nil {
			return fmt.Errorf("StoreBlock failed: %v", err)
		}
		totalVins += numVins
		totalVouts += numVouts

		numSTx := int64(len(block.STransactions()))
		numRTx := int64(len(block.Transactions()))
		totalTxs += numRTx + numSTx
		// totalRTxs += numRTx
		// totalSTxs += numSTx

		// update height, the end condition for the loop
		if _, height, err = client.GetBestBlock(); err != nil {
			return fmt.Errorf("GetBestBlock failed: %v", err)
		}
	}

	speedReport()

	if reindexing || cfg.ForceReindex {
		if err = db.DeleteDuplicates(nil); err != nil {
			return err
		}

		// Create indexes
		if err = db.IndexAll(nil); err != nil {
			return fmt.Errorf("IndexAll failed: %v", err)
		}
		// Only reindex address table here if we do not do it below
		if cfg.AddrSpendInfoOnline {
			err = db.IndexAddressTable(nil)
		}
		if !cfg.TicketSpendInfoBatch {
			err = db.IndexTicketsTable(nil)
		}
	}

	if !cfg.AddrSpendInfoOnline {
		// Remove indexes not on funding txns (remove on address table indexes)
		_ = db.DeindexAddressTable() // ignore errors for non-existent indexes
		db.EnableDuplicateCheckOnInsert(false)
		log.Infof("Populating spending tx info in address table...")
		numAddresses, err := db.UpdateSpendingInfoInAllAddresses(nil)
		if err != nil {
			log.Errorf("UpdateSpendingInfoInAllAddresses FAILED: %v", err)
		}
		// Index address table
		log.Infof("Updated %d rows of address table", numAddresses)
		if err = db.IndexAddressTable(nil); err != nil {
			log.Errorf("IndexAddressTable FAILED: %v", err)
		}
	}

	if cfg.TicketSpendInfoBatch {
		// Remove indexes not on funding txns (remove on address table indexes)
		_ = db.DeindexTicketsTable() // ignore errors for non-existent indexes
		db.EnableDuplicateCheckOnInsert(false)
		log.Infof("Populating spending tx info in tickets table...")
		numTicketsUpdated, err := db.UpdateSpendingInfoInAllTickets()
		if err != nil {
			log.Errorf("UpdateSpendingInfoInAllTickets FAILED: %v", err)
		}
		// Index tickets table
		log.Infof("Updated %d rows of address table", numTicketsUpdated)
		if err = db.IndexTicketsTable(nil); err != nil {
			log.Errorf("IndexTicketsTable FAILED: %v", err)
		}
	}

	log.Infof("Rebuild finished at height %d. Delta: %d blocks, %d transactions, %d ins, %d outs",
		height, height-startHeight+1, totalTxs, totalVins, totalVouts)

	return err
}

func main() {
	if err := mainCore(); err != nil {
		log.Error(err)
		os.Exit(1)
	}
	os.Exit(0)
}
