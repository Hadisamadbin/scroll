package fetcher

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/ethclient"
	"github.com/scroll-tech/go-ethereum/log"
	"gorm.io/gorm"

	"scroll-tech/bridge-history-api/internal/config"
	"scroll-tech/bridge-history-api/internal/logic"
	"scroll-tech/bridge-history-api/internal/orm"
	"scroll-tech/bridge-history-api/internal/utils"
)

// L2MessageFetcher fetches cross message events from L2 and saves them to database.
type L2MessageFetcher struct {
	ctx                 context.Context
	cfg                 *config.LayerConfig
	db                  *gorm.DB
	client              *ethclient.Client
	syncInfo            *SyncInfo
	l2LastSyncBlockHash common.Hash

	eventUpdateLogic     *logic.EventUpdateLogic
	l2FetcherLogic       *logic.L2FetcherLogic
	l2ReorgHandlingLogic *logic.L2ReorgHandlingLogic
}

// NewL2MessageFetcher creates a new L2MessageFetcher instance.
func NewL2MessageFetcher(ctx context.Context, cfg *config.LayerConfig, db *gorm.DB, client *ethclient.Client, syncInfo *SyncInfo) (*L2MessageFetcher, error) {
	return &L2MessageFetcher{
		ctx:                  ctx,
		cfg:                  cfg,
		db:                   db,
		syncInfo:             syncInfo,
		client:               client,
		eventUpdateLogic:     logic.NewEventUpdateLogic(db),
		l2FetcherLogic:       logic.NewL2FetcherLogic(cfg, db, client),
		l2ReorgHandlingLogic: logic.NewL2ReorgHandlingLogic(client),
	}, nil
}

// Start starts the L2 message fetching process.
func (c *L2MessageFetcher) Start() {
	l2SentMessageSyncedHeight, dbErr := c.eventUpdateLogic.GetL2MessageSyncedHeightInDB(c.ctx)
	if dbErr != nil {
		log.Error("failed to get L2 cross message processed height", "err", dbErr)
		return
	}

	l2SyncHeight := l2SentMessageSyncedHeight
	// Sync from an older block to prevent reorg during restart.
	if l2SyncHeight < logic.L2ReorgSafeDepth {
		l2SyncHeight = 0
	} else {
		l2SyncHeight -= logic.L2ReorgSafeDepth
	}

	if updateErr := c.updateL2SyncHeight(l2SyncHeight); updateErr != nil {
		log.Crit("failed to update L2 sync height", "height", l2SyncHeight, "err", updateErr)
		return
	}
	log.Info("Start L2 message fetcher", "message synced height", l2SentMessageSyncedHeight, "sync start height", l2SyncHeight+1)

	tick := time.NewTicker(time.Duration(c.cfg.BlockTime) * time.Second)
	go func() {
		for {
			select {
			case <-c.ctx.Done():
				tick.Stop()
				return
			case <-tick.C:
				c.fetchAndSaveEvents(c.cfg.Confirmation)
			}
		}
	}()
}

func (c *L2MessageFetcher) fetchAndSaveEvents(confirmation uint64) {
	startHeight := c.syncInfo.GetL2SyncHeight() + 1
	endHeight, rpcErr := utils.GetBlockNumber(c.ctx, c.client, confirmation)
	if rpcErr != nil {
		log.Error("failed to get L2 block number", "confirmation", confirmation, "err", rpcErr)
		return
	}
	log.Info("fetch and save missing L2 events", "start height", startHeight, "end height", endHeight)

	for from := startHeight; from <= endHeight; from += c.cfg.FetchLimit {
		to := from + c.cfg.FetchLimit - 1
		if to > endHeight {
			to = endHeight
		}

		if c.syncInfo.GetL2SyncHeight()+logic.L2ReorgSafeDepth > endHeight {
			isReorg, resyncHeight, handleErr := c.l2ReorgHandlingLogic.HandleL2Reorg(c.ctx, c.syncInfo.GetL2SyncHeight(), c.l2LastSyncBlockHash)
			if handleErr != nil {
				log.Error("failed to Handle L2 Reorg", "err", handleErr)
				return
			}

			if isReorg {
				log.Warn("L2 reorg happened, exit and re-enter fetchAndSaveEvents", "re-sync height", resyncHeight)

				if updateErr := c.updateL2SyncHeight(resyncHeight); updateErr != nil {
					log.Error("failed to update L2 re-sync height", "height", resyncHeight, "err", updateErr)
					return
				}

				return
			}
		}

		l2FilterResult, fetchErr := c.l2FetcherLogic.L2Fetcher(c.ctx, from, to)
		if fetchErr != nil {
			log.Error("failed to fetch L2 events", "from", from, "to", to, "err", fetchErr)
			return
		}

		if updateWithdrawErr := c.updateL2WithdrawMessageProofs(c.ctx, l2FilterResult.WithdrawMessages, to); updateWithdrawErr != nil {
			log.Error("failed to update L2 withdraw message", "from", from, "to", to, "err", updateWithdrawErr)
			return
		}

		if insertUpdateErr := c.eventUpdateLogic.L2InsertOrUpdate(c.ctx, l2FilterResult); insertUpdateErr != nil {
			log.Error("failed to save L2 events", "from", from, "to", to, "err", insertUpdateErr)
			return
		}

		if updateErr := c.updateL2SyncHeight(to); updateErr != nil {
			log.Error("failed to update L2 sync height", "height", to, "err", updateErr)
			return
		}
	}
}

func (c *L2MessageFetcher) updateL2WithdrawMessageProofs(ctx context.Context, l2WithdrawMessages []*orm.CrossMessage, endBlock uint64) error {
	withdrawTrie := utils.NewWithdrawTrie()
	message, err := c.eventUpdateLogic.GetL2LatestWithdrawalLEBlockHeight(ctx, c.syncInfo.GetL2SyncHeight())
	if err != nil {
		log.Error("failed to get latest L2 sent message event", "err", err)
		return err
	}

	if message != nil {
		withdrawTrie.Initialize(message.MessageNonce, common.HexToHash(message.MessageHash), message.MerkleProof)
	}

	messageHashes := make([]common.Hash, len(l2WithdrawMessages))
	for i, message := range l2WithdrawMessages {
		messageHashes[i] = common.HexToHash(message.MessageHash)
	}

	for i, messageHash := range messageHashes {
		// AppendMessages returns the proofs for the entire tree after all messages have been inserted,
		// so it is called for each message individually to obtain the correct proofs.
		proof := withdrawTrie.AppendMessages([]common.Hash{messageHash})
		if err != nil {
			log.Error("error generating proof", "messageHash", messageHash, "error", err)
			return fmt.Errorf("error generating proof for messageHash %s: %v", messageHash, err)
		}

		if len(proof) != 1 {
			log.Error("invalid proof len", "got", len(proof), "expected", 1)
			return fmt.Errorf("invalid proof len, got: %v, expected: 1", len(proof))
		}
		l2WithdrawMessages[i].MerkleProof = proof[0]
	}

	// Verify if local info is correct.
	withdrawRoot, err := c.client.StorageAt(ctx, common.HexToAddress(c.cfg.MessageQueueAddr), common.Hash{}, new(big.Int).SetUint64(endBlock))
	if err != nil {
		log.Error("failed to get withdraw root", "number", endBlock, "error", err)
		return fmt.Errorf("failed to get withdraw root: %v, number: %v", err, endBlock)
	}

	if common.BytesToHash(withdrawRoot) != withdrawTrie.MessageRoot() {
		log.Error("withdraw root mismatch", "expected", common.BytesToHash(withdrawRoot).String(), "got", withdrawTrie.MessageRoot().String())
		return fmt.Errorf("withdraw root mismatch. expected: %v, got: %v", common.BytesToHash(withdrawRoot), withdrawTrie.MessageRoot())
	}
	return nil
}

func (c *L2MessageFetcher) updateL2SyncHeight(height uint64) error {
	blockHeader, err := c.client.HeaderByNumber(c.ctx, new(big.Int).SetUint64(height))
	if err != nil {
		log.Error("failed to get L2 header by number", "block number", height, "err", err)
		return err
	}
	c.l2LastSyncBlockHash = blockHeader.Hash()
	c.syncInfo.SetL2SyncHeight(height)
	return nil
}