package main

import (
	nativeerrors "errors"
	"fmt"
	"github.com/Kash-Protocol/kashd/version"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/Kash-Protocol/kashd/app/appmessage"
	"github.com/Kash-Protocol/kashd/cmd/kashminer/templatemanager"
	"github.com/Kash-Protocol/kashd/domain/consensus/model/externalapi"
	"github.com/Kash-Protocol/kashd/domain/consensus/utils/consensushashing"
	"github.com/Kash-Protocol/kashd/domain/consensus/utils/pow"
	"github.com/Kash-Protocol/kashd/infrastructure/network/netadapter/router"
	"github.com/Kash-Protocol/kashd/util"
	"github.com/pkg/errors"
)

var hashesTried uint64

const logHashRateInterval = 10 * time.Second

func mineLoop(client *minerClient, numberOfBlocks uint64, targetBlocksPerSecond float64, mineWhenNotSynced bool,
	workers int, miningAddr util.Address) error {
	rand.Seed(time.Now().UnixNano()) // Seed the global concurrent-safe random source.

	errChan := make(chan error)
	doneChan := make(chan struct{})

	// We don't want to send router.DefaultMaxMessages blocks at once because there's
	// a high chance we'll get disconnected from the node, so we make the channel
	// capacity router.DefaultMaxMessages/2 (we give some slack for getBlockTemplate
	// requests)
	foundBlockChan := make(chan *externalapi.DomainBlock, router.DefaultMaxMessages/2)

	spawn("templatesLoop", func() {
		templatesLoop(client, miningAddr, errChan)
	})

	for i := 0; i < workers; i++ {
		spawn(fmt.Sprintf("mineWorker%d", i), func() {
			const windowSize = 10
			hasBlockRateTarget := targetBlocksPerSecond != 0
			var windowTicker, blockTicker *time.Ticker

			if hasBlockRateTarget {
				windowRate := time.Duration(float64(time.Second) / (targetBlocksPerSecond / windowSize))
				blockRate := time.Duration(float64(time.Second) / (targetBlocksPerSecond * windowSize))
				windowTicker = time.NewTicker(windowRate)
				blockTicker = time.NewTicker(blockRate)
				defer windowTicker.Stop()
				defer blockTicker.Stop()
			}

			windowStart := time.Now()
			for blockIndex := 1; ; blockIndex++ {
				foundBlockChan <- mineNextBlock(mineWhenNotSynced)

				if hasBlockRateTarget {
					<-blockTicker.C

					if (blockIndex % windowSize) == 0 {
						tickerStart := time.Now()
						<-windowTicker.C
						log.Infof("Worker %d finished mining %d blocks in: %s. Slept for: %s", i,
							windowSize, time.Since(windowStart), time.Since(tickerStart))
						windowStart = time.Now()
					}
				}
			}
		})
	}

	spawn("handleFoundBlock", func() {
		for i := uint64(0); numberOfBlocks == 0 || i < numberOfBlocks; i++ {
			block := <-foundBlockChan
			err := handleFoundBlock(client, block)
			if err != nil {
				errChan <- err
				return
			}
		}
		doneChan <- struct{}{}
	})

	logHashRate()

	select {
	case err := <-errChan:
		return err
	case <-doneChan:
		return nil
	}
}

func logHashRate() {
	spawn("logHashRate", func() {
		lastCheck := time.Now()
		for range time.Tick(logHashRateInterval) {
			currentHashesTried := atomic.LoadUint64(&hashesTried)
			currentTime := time.Now()
			kiloHashesTried := float64(currentHashesTried)
			hashRate := kiloHashesTried / currentTime.Sub(lastCheck).Seconds()
			log.Infof("Current hash rate is %.2f H/s", hashRate)
			lastCheck = currentTime
			// subtract from hashesTried the hashes we already sampled
			atomic.AddUint64(&hashesTried, -currentHashesTried)
		}
	})
}

func handleFoundBlock(client *minerClient, block *externalapi.DomainBlock) error {
	blockHash := consensushashing.BlockHash(block)
	log.Infof("Submitting block %s to %s", blockHash, client.Address())

	rejectReason, err := client.SubmitBlock(block)
	if err != nil {
		if nativeerrors.Is(err, router.ErrTimeout) {
			log.Warnf("Got timeout while submitting block %s to %s: %s", blockHash, client.Address(), err)
			return client.Reconnect()
		}
		if nativeerrors.Is(err, router.ErrRouteClosed) {
			log.Debugf("Got route is closed while requesting block template from %s. "+
				"The client is most likely reconnecting", client.Address())
			return nil
		}
		if rejectReason == appmessage.RejectReasonIsInIBD {
			const waitTime = 1 * time.Second
			log.Warnf("Block %s was rejected because the node is in IBD. Waiting for %s", blockHash, waitTime)
			time.Sleep(waitTime)
			return nil
		}
		return errors.Wrapf(err, "Error submitting block %s to %s", blockHash, client.Address())
	}
	return nil
}

func mineNextBlock(mineWhenNotSynced bool) *externalapi.DomainBlock {
	nonce := rand.Uint64() // Use the global concurrent-safe random source.
	for {
		nonce++
		// For each nonce we try to build a block from the most up to date
		// block template.
		// In the rare case where the nonce space is exhausted for a specific
		// block, it'll keep looping the nonce until a new block template
		// is discovered.
		block, state := getBlockForMining(mineWhenNotSynced)
		state.Nonce = nonce
		atomic.AddUint64(&hashesTried, 1)
		if state.CheckProofOfWork() {
			mutHeader := block.Header.ToMutable()
			mutHeader.SetNonce(nonce)
			block.Header = mutHeader.ToImmutable()
			log.Infof("Found block %s with parents %s", consensushashing.BlockHash(block), block.Header.DirectParents())
			return block
		}
	}
}

func getBlockForMining(mineWhenNotSynced bool) (*externalapi.DomainBlock, *pow.State) {
	tryCount := 0

	const sleepTime = 500 * time.Millisecond
	const sleepTimeWhenNotSynced = 5 * time.Second

	for {
		tryCount++

		shouldLog := (tryCount-1)%10 == 0
		template, state, isSynced := templatemanager.Get()
		if template == nil {
			if shouldLog {
				log.Info("Waiting for the initial template")
			}
			time.Sleep(sleepTime)
			continue
		}
		if !isSynced && !mineWhenNotSynced {
			if shouldLog {
				log.Warnf("Kashd is not synced. Skipping current block template")
			}
			time.Sleep(sleepTimeWhenNotSynced)
			continue
		}

		return template, state
	}
}

func templatesLoop(client *minerClient, miningAddr util.Address, errChan chan error) {
	getBlockTemplate := func() {
		template, err := client.GetBlockTemplate(miningAddr.String(), "kashminer-"+version.Version())
		if nativeerrors.Is(err, router.ErrTimeout) {
			log.Warnf("Got timeout while requesting block template from %s: %s", client.Address(), err)
			reconnectErr := client.Reconnect()
			if reconnectErr != nil {
				errChan <- reconnectErr
			}
			return
		}
		if nativeerrors.Is(err, router.ErrRouteClosed) {
			log.Debugf("Got route is closed while requesting block template from %s. "+
				"The client is most likely reconnecting", client.Address())
			return
		}
		if err != nil {
			errChan <- errors.Wrapf(err, "Error getting block template from %s", client.Address())
			return
		}
		err = templatemanager.Set(template)
		if err != nil {
			errChan <- errors.Wrapf(err, "Error setting block template from %s", client.Address())
			return
		}
	}

	getBlockTemplate()
	const tickerTime = 500 * time.Millisecond
	ticker := time.NewTicker(tickerTime)
	for {
		select {
		case <-client.newBlockTemplateNotificationChan:
			getBlockTemplate()
			ticker.Reset(tickerTime)
		case <-ticker.C:
			getBlockTemplate()
		}
	}
}
