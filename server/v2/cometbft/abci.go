package cometbft

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync/atomic"

	abci "github.com/cometbft/cometbft/abci/types"
	abciproto "github.com/cometbft/cometbft/api/cometbft/abci/v1"
	gogoproto "github.com/cosmos/gogoproto/proto"

	coreappmgr "cosmossdk.io/core/app"
	"cosmossdk.io/core/comet"
	"cosmossdk.io/core/event"
	"cosmossdk.io/core/store"
	"cosmossdk.io/core/transaction"
	errorsmod "cosmossdk.io/errors"
	"cosmossdk.io/log"
	sdkmath "cosmossdk.io/math"
	"cosmossdk.io/server/v2/appmanager"
	"cosmossdk.io/server/v2/cometbft/client/grpc/cmtservice"
	"cosmossdk.io/server/v2/cometbft/handlers"
	"cosmossdk.io/server/v2/cometbft/mempool"
	"cosmossdk.io/server/v2/cometbft/types"
	cometerrors "cosmossdk.io/server/v2/cometbft/types/errors"
	"cosmossdk.io/server/v2/streaming"
	"cosmossdk.io/store/v2/snapshots"
	consensustypes "cosmossdk.io/x/consensus/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

var _ abci.Application = (*Consensus[transaction.Tx])(nil)

type Consensus[T transaction.Tx] struct {
	logger             log.Logger
	appName, version   string
	consensusAuthority string // Set by the application to grant authority to the consensus engine to send messages to the consensus module
	app                *appmanager.AppManager[T]
	txCodec            transaction.Codec[T]
	store              types.Store
	streaming          streaming.Manager
	snapshotManager    *snapshots.Manager
	mempool            mempool.Mempool[T]

	cfg           Config
	indexedEvents map[string]struct{}
	chainID       string

	initialHeight uint64
	// this is only available after this node has committed a block (in FinalizeBlock),
	// otherwise it will be empty and we will need to query the app for the last
	// committed block.
	lastCommittedHeight atomic.Int64

	prepareProposalHandler handlers.PrepareHandler[T]
	processProposalHandler handlers.ProcessHandler[T]
	verifyVoteExt          handlers.VerifyVoteExtensionhandler
	extendVote             handlers.ExtendVoteHandler

	addrPeerFilter types.PeerFilter // filter peers by address and port
	idPeerFilter   types.PeerFilter // filter peers by node ID

	grpcMethodsMap map[string]func() gogoproto.Message // maps gRPC method to message creator func
}

func NewConsensus[T transaction.Tx](
	logger log.Logger,
	appName string,
	consensusAuthority string,
	app *appmanager.AppManager[T],
	mp mempool.Mempool[T],
	indexedEvents map[string]struct{},
	gRPCMethodsMap map[string]func() gogoproto.Message,
	store types.Store,
	cfg Config,
	txCodec transaction.Codec[T],
	chainId string,
) *Consensus[T] {
	return &Consensus[T]{
		appName:                appName,
		version:                getCometBFTServerVersion(),
		consensusAuthority:     consensusAuthority,
		grpcMethodsMap:         gRPCMethodsMap,
		app:                    app,
		cfg:                    cfg,
		store:                  store,
		logger:                 logger,
		txCodec:                txCodec,
		streaming:              streaming.Manager{},
		snapshotManager:        nil,
		mempool:                mp,
		lastCommittedHeight:    atomic.Int64{},
		prepareProposalHandler: nil,
		processProposalHandler: nil,
		verifyVoteExt:          nil,
		extendVote:             nil,
		chainID:                chainId,
		indexedEvents:          indexedEvents,
		initialHeight:          0,
	}
}

// SetStreamingManager sets the streaming manager for the consensus module.
func (c *Consensus[T]) SetStreamingManager(sm streaming.Manager) {
	c.streaming = sm
}

// RegisterSnapshotExtensions registers the given extensions with the consensus module's snapshot manager.
// It allows additional snapshotter implementations to be used for creating and restoring snapshots.
func (c *Consensus[T]) RegisterSnapshotExtensions(extensions ...snapshots.ExtensionSnapshotter) error {
	if err := c.snapshotManager.RegisterExtensions(extensions...); err != nil {
		return fmt.Errorf("failed to register snapshot extensions: %w", err)
	}

	return nil
}

// CheckTx implements types.Application.
// It is called by cometbft to verify transaction validity
func (c *Consensus[T]) CheckTx(ctx context.Context, req *abciproto.CheckTxRequest) (*abciproto.CheckTxResponse, error) {
	decodedTx, err := c.txCodec.Decode(req.Tx)
	if err != nil {
		return nil, err
	}

	// check tx fee with validator's minimum-gas-price config
	if err := c.checkTxFeeWithMinGasPrices(decodedTx); err != nil {
		return &abciproto.CheckTxResponse{
			Code: 1,
			Log:  err.Error(),
		}, nil
	}

	resp, err := c.app.ValidateTx(ctx, decodedTx)
	if err != nil {
		return nil, err
	}

	cometResp := &abciproto.CheckTxResponse{
		Code:      resp.Code,
		GasWanted: uint64ToInt64(resp.GasWanted),
		GasUsed:   uint64ToInt64(resp.GasUsed),
		Events:    intoABCIEvents(resp.Events, c.indexedEvents),
		Info:      resp.Info,
		Data:      resp.Data,
		Log:       resp.Log,
		Codespace: resp.Codespace,
	}
	if resp.Error != nil {
		cometResp.Code = 1
		cometResp.Log = resp.Error.Error()
	}
	return cometResp, nil
}

// checkTxFeeWithMinGasPrices ensure that the provided fees meet a minimum threshold for the validator,
// if this is a CheckTx. This is only for local mempool purposes, and thus
// is only ran on check tx.
func (c *Consensus[T]) checkTxFeeWithMinGasPrices(tx transaction.Tx) error {
	feeTx, ok := tx.(sdk.FeeTx)
	if !ok {
		return nil // don't force users to implement fee tx
	}

	feeCoins := feeTx.GetFee()
	gas := feeTx.GetGas()

	minGasPrices := c.cfg.GetMinGasPrices()
	if !minGasPrices.IsZero() {
		requiredFees := make(sdk.Coins, len(minGasPrices))

		// Determine the required fees by multiplying each required minimum gas
		// price by the gas limit, where fee = ceil(minGasPrice * gasLimit).
		glDec := sdkmath.LegacyNewDec(int64(gas))
		for i, gp := range minGasPrices {
			fee := gp.Amount.Mul(glDec)
			requiredFees[i] = sdk.NewCoin(gp.Denom, fee.Ceil().RoundInt())
		}

		if !feeCoins.IsAnyGTE(requiredFees) {
			return errorsmod.Wrapf(cometerrors.ErrInsufficientFee, "insufficient fees; got: %s required: %s", feeCoins, requiredFees)
		}
	}

	return nil
}

// Info implements types.Application.
func (c *Consensus[T]) Info(ctx context.Context, _ *abciproto.InfoRequest) (*abciproto.InfoResponse, error) {
	version, _, err := c.store.StateLatest()
	if err != nil {
		return nil, err
	}

	// cp, err := c.GetConsensusParams(ctx)
	// if err != nil {
	// 	return nil, err
	// }

	cid, err := c.store.LastCommitID()
	if err != nil {
		return nil, err
	}

	return &abciproto.InfoResponse{
		Data:             c.appName,
		Version:          c.version,
		AppVersion:       0, // TODO fetch consensus params?
		LastBlockHeight:  int64(version),
		LastBlockAppHash: cid.Hash,
	}, nil
}

// Query implements types.Application.
// It is called by cometbft to query application state.
func (c *Consensus[T]) Query(ctx context.Context, req *abciproto.QueryRequest) (resp *abciproto.QueryResponse, err error) {
	// check if it's a gRPC method
	makeGRPCRequest, isGRPC := c.grpcMethodsMap[req.Path]
	if isGRPC {
		protoRequest := makeGRPCRequest()
		err = gogoproto.Unmarshal(req.Data, protoRequest) // TODO: use codec
		if err != nil {
			return nil, fmt.Errorf("unable to decode gRPC request with path %s from ABCI.Query: %w", req.Path, err)
		}
		res, err := c.app.Query(ctx, uint64(req.Height), protoRequest)
		if err != nil {
			resp := queryResult(err)
			resp.Height = req.Height
			return resp, err

		}

		return queryResponse(res, req.Height)
	}

	// this error most probably means that we can't handle it with a proto message, so
	// it must be an app/p2p/store query
	path := splitABCIQueryPath(req.Path)
	if len(path) == 0 {
		return QueryResult(errorsmod.Wrap(cometerrors.ErrUnknownRequest, "no query path provided"), c.cfg.AppTomlConfig.Trace), nil
	}

	switch path[0] {
	case cmtservice.QueryPathApp:
		resp, err = c.handlerQueryApp(ctx, path, req)

	case cmtservice.QueryPathStore:
		resp, err = c.handleQueryStore(path, c.store, req)

	case cmtservice.QueryPathP2P:
		resp, err = c.handleQueryP2P(path)

	default:
		resp = QueryResult(errorsmod.Wrap(cometerrors.ErrUnknownRequest, "unknown query path"), c.cfg.AppTomlConfig.Trace)
	}

	if err != nil {
		return QueryResult(err, c.cfg.AppTomlConfig.Trace), nil
	}

	return resp, nil
}

// InitChain implements types.Application.
func (c *Consensus[T]) InitChain(ctx context.Context, req *abciproto.InitChainRequest) (*abciproto.InitChainResponse, error) {
	c.logger.Info("InitChain", "initialHeight", req.InitialHeight, "chainID", req.ChainId)

	// store chainID to be used later on in execution
	c.chainID = req.ChainId

	// TODO: check if we need to load the config from genesis.json or config.toml
	c.initialHeight = uint64(req.InitialHeight)
	if c.initialHeight == 0 { // If initial height is 0, set it to 1
		c.initialHeight = 1
	}

	var consMessages []transaction.Msg
	if req.ConsensusParams != nil {
		consMessages = append(consMessages, &consensustypes.MsgUpdateParams{
			Authority: c.consensusAuthority,
			Block:     req.ConsensusParams.Block,
			Evidence:  req.ConsensusParams.Evidence,
			Validator: req.ConsensusParams.Validator,
			Abci:      req.ConsensusParams.Abci,
		})
	}

	ci, err := c.store.LastCommitID()
	if err != nil {
		return nil, err
	}

	// populate hash with empty byte slice instead of nil
	bz := sha256.Sum256([]byte{})

	br := &coreappmgr.BlockRequest[T]{
		Height:            uint64(req.InitialHeight - 1),
		Time:              req.Time,
		Hash:              bz[:],
		AppHash:           ci.Hash,
		ChainId:           req.ChainId,
		ConsensusMessages: consMessages,
		IsGenesis:         true,
	}

	blockresponse, genesisState, err := c.app.InitGenesis(
		ctx,
		br,
		req.AppStateBytes,
		c.txCodec)
	if err != nil {
		return nil, fmt.Errorf("genesis state init failure: %w", err)
	}

	// TODO necessary? where should this WARN live if it all. helpful for testing
	for _, txRes := range blockresponse.TxResults {
		if txRes.Error != nil {
			c.logger.Warn("genesis tx failed", "code", txRes.Code, "log", txRes.Log, "error", txRes.Error)
		}
	}

	validatorUpdates := intoABCIValidatorUpdates(blockresponse.ValidatorUpdates)

	// set the initial version of the store
	if err := c.store.SetInitialVersion(uint64(req.InitialHeight)); err != nil {
		return nil, fmt.Errorf("failed to set initial version: %w", err)
	}

	stateChanges, err := genesisState.GetStateChanges()
	if err != nil {
		return nil, err
	}
	cs := &store.Changeset{
		Changes: stateChanges,
	}
	stateRoot, err := c.store.WorkingHash(cs)
	if err != nil {
		return nil, fmt.Errorf("unable to write the changeset: %w", err)
	}

	return &abciproto.InitChainResponse{
		ConsensusParams: req.ConsensusParams,
		Validators:      validatorUpdates,
		AppHash:         stateRoot,
	}, nil
}

// PrepareProposal implements types.Application.
// It is called by cometbft to prepare a proposal block.
func (c *Consensus[T]) PrepareProposal(
	ctx context.Context,
	req *abciproto.PrepareProposalRequest,
) (resp *abciproto.PrepareProposalResponse, err error) {
	if req.Height < 1 {
		return nil, errors.New("PrepareProposal called with invalid height")
	}

	decodedTxs := make([]T, len(req.Txs))
	for i, tx := range req.Txs {
		decTx, err := c.txCodec.Decode(tx)
		if err != nil {
			// TODO: vote extension meta data as a custom type to avoid possibly accepting invalid txs
			// continue even if tx decoding fails
			c.logger.Error("failed to decode tx", "err", err)
			continue
		}

		decodedTxs[i] = decTx
	}

	ciCtx := contextWithCometInfo(ctx, comet.Info{
		Evidence:        toCoreEvidence(req.Misbehavior),
		ValidatorsHash:  req.NextValidatorsHash,
		ProposerAddress: req.ProposerAddress,
		LastCommit:      toCoreExtendedCommitInfo(req.LocalLastCommit),
	})

	txs, err := c.prepareProposalHandler(ciCtx, c.app, decodedTxs, req)
	if err != nil {
		return nil, err
	}

	encodedTxs := make([][]byte, len(txs))
	for i, tx := range txs {
		encodedTxs[i] = tx.Bytes()
	}

	return &abciproto.PrepareProposalResponse{
		Txs: encodedTxs,
	}, nil
}

// ProcessProposal implements types.Application.
// It is called by cometbft to process/verify a proposal block.
func (c *Consensus[T]) ProcessProposal(
	ctx context.Context,
	req *abciproto.ProcessProposalRequest,
) (*abciproto.ProcessProposalResponse, error) {
	decodedTxs := make([]T, len(req.Txs))
	for _, tx := range req.Txs {
		decTx, err := c.txCodec.Decode(tx)
		if err != nil {
			// TODO: vote extension meta data as a custom type to avoid possibly accepting invalid txs
			// continue even if tx decoding fails
			c.logger.Error("failed to decode tx", "err", err)
			continue
		}
		decodedTxs = append(decodedTxs, decTx)
	}

	ciCtx := contextWithCometInfo(ctx, comet.Info{
		Evidence:        toCoreEvidence(req.Misbehavior),
		ValidatorsHash:  req.NextValidatorsHash,
		ProposerAddress: req.ProposerAddress,
		LastCommit:      toCoreCommitInfo(req.ProposedLastCommit),
	})

	err := c.processProposalHandler(ciCtx, c.app, decodedTxs, req)
	if err != nil {
		c.logger.Error("failed to process proposal", "height", req.Height, "time", req.Time, "hash", fmt.Sprintf("%X", req.Hash), "err", err)
		return &abciproto.ProcessProposalResponse{
			Status: abciproto.PROCESS_PROPOSAL_STATUS_REJECT,
		}, nil
	}

	return &abciproto.ProcessProposalResponse{
		Status: abciproto.PROCESS_PROPOSAL_STATUS_ACCEPT,
	}, nil
}

// FinalizeBlock implements types.Application.
// It is called by cometbft to finalize a block.
func (c *Consensus[T]) FinalizeBlock(
	ctx context.Context,
	req *abciproto.FinalizeBlockRequest,
) (*abciproto.FinalizeBlockResponse, error) {
	if err := c.validateFinalizeBlockHeight(req); err != nil {
		return nil, err
	}

	if err := c.checkHalt(req.Height, req.Time); err != nil {
		return nil, err
	}

	// we don't need to deliver the block in the genesis block
	if req.Height == int64(c.initialHeight) {
		appHash, err := c.store.Commit(store.NewChangeset())
		if err != nil {
			return nil, fmt.Errorf("unable to commit the changeset: %w", err)
		}
		c.lastCommittedHeight.Store(req.Height)
		return &abciproto.FinalizeBlockResponse{
			AppHash: appHash,
		}, nil
	}

	// TODO(tip): can we expect some txs to not decode? if so, what we do in this case? this does not seem to be the case,
	// considering that prepare and process always decode txs, assuming they're the ones providing txs we should never
	// have a tx that fails decoding.
	decodedTxs, err := decodeTxs(req.Txs, c.txCodec)
	if err != nil {
		return nil, err
	}

	cid, err := c.store.LastCommitID()
	if err != nil {
		return nil, err
	}

	blockReq := &coreappmgr.BlockRequest[T]{
		Height:  uint64(req.Height),
		Time:    req.Time,
		Hash:    req.Hash,
		AppHash: cid.Hash,
		ChainId: c.chainID,
		Txs:     decodedTxs,
	}

	ciCtx := contextWithCometInfo(ctx, comet.Info{
		Evidence:        toCoreEvidence(req.Misbehavior),
		ValidatorsHash:  req.NextValidatorsHash,
		ProposerAddress: req.ProposerAddress,
		LastCommit:      toCoreCommitInfo(req.DecidedLastCommit),
	})

	resp, newState, err := c.app.DeliverBlock(ciCtx, blockReq)
	if err != nil {
		return nil, err
	}

	// after we get the changeset we can produce the commit hash,
	// from the store.
	stateChanges, err := newState.GetStateChanges()
	if err != nil {
		return nil, err
	}
	appHash, err := c.store.Commit(&store.Changeset{Changes: stateChanges})
	if err != nil {
		return nil, fmt.Errorf("unable to commit the changeset: %w", err)
	}

	var events []event.Event
	events = append(events, resp.PreBlockEvents...)
	events = append(events, resp.BeginBlockEvents...)
	for _, tx := range resp.TxResults {
		events = append(events, tx.Events...)
	}
	events = append(events, resp.EndBlockEvents...)

	// listen to state streaming changes in accordance with the block
	err = c.streamDeliverBlockChanges(ctx, req.Height, req.Txs, resp.TxResults, events, stateChanges)
	if err != nil {
		return nil, err
	}

	// remove txs from the mempool
	err = c.mempool.Remove(decodedTxs)
	if err != nil {
		return nil, fmt.Errorf("unable to remove txs: %w", err)
	}

	c.lastCommittedHeight.Store(req.Height)

	cp, err := c.GetConsensusParams(ctx) // we get the consensus params from the latest state because we committed state above
	if err != nil {
		return nil, err
	}

	return finalizeBlockResponse(resp, cp, appHash, c.indexedEvents)
}

// Commit implements types.Application.
// It is called by cometbft to notify the application that a block was committed.
func (c *Consensus[T]) Commit(ctx context.Context, _ *abciproto.CommitRequest) (*abciproto.CommitResponse, error) {
	lastCommittedHeight := c.lastCommittedHeight.Load()

	c.snapshotManager.SnapshotIfApplicable(lastCommittedHeight)

	cp, err := c.GetConsensusParams(ctx)
	if err != nil {
		return nil, err
	}

	return &abci.CommitResponse{
		RetainHeight: c.GetBlockRetentionHeight(cp, lastCommittedHeight),
	}, nil
}

// Vote extensions

// VerifyVoteExtension implements types.Application.
func (c *Consensus[T]) VerifyVoteExtension(
	ctx context.Context,
	req *abciproto.VerifyVoteExtensionRequest,
) (*abciproto.VerifyVoteExtensionResponse, error) {
	// If vote extensions are not enabled, as a safety precaution, we return an
	// error.
	cp, err := c.GetConsensusParams(ctx)
	if err != nil {
		return nil, err
	}

	// Note: we verify votes extensions on VoteExtensionsEnableHeight+1. Check
	// comment in ExtendVote and ValidateVoteExtensions for more details.
	extsEnabled := cp.Abci != nil && req.Height >= cp.Abci.VoteExtensionsEnableHeight && cp.Abci.VoteExtensionsEnableHeight != 0
	if !extsEnabled {
		return nil, fmt.Errorf("vote extensions are not enabled; unexpected call to VerifyVoteExtension at height %d", req.Height)
	}

	if c.verifyVoteExt == nil {
		return nil, errors.New("vote extensions are enabled but no verify function was set")
	}

	_, latestStore, err := c.store.StateLatest()
	if err != nil {
		return nil, err
	}

	resp, err := c.verifyVoteExt(ctx, latestStore, req)
	if err != nil {
		c.logger.Error("failed to verify vote extension", "height", req.Height, "err", err)
		return &abciproto.VerifyVoteExtensionResponse{Status: abciproto.VERIFY_VOTE_EXTENSION_STATUS_REJECT}, nil
	}

	return resp, err
}

// ExtendVote implements types.Application.
func (c *Consensus[T]) ExtendVote(ctx context.Context, req *abciproto.ExtendVoteRequest) (*abciproto.ExtendVoteResponse, error) {
	// If vote extensions are not enabled, as a safety precaution, we return an
	// error.
	cp, err := c.GetConsensusParams(ctx)
	if err != nil {
		return nil, err
	}

	// Note: In this case, we do want to extend vote if the height is equal or
	// greater than VoteExtensionsEnableHeight. This defers from the check done
	// in ValidateVoteExtensions and PrepareProposal in which we'll check for
	// vote extensions on VoteExtensionsEnableHeight+1.
	extsEnabled := cp.Abci != nil && req.Height >= cp.Abci.VoteExtensionsEnableHeight && cp.Abci.VoteExtensionsEnableHeight != 0
	if !extsEnabled {
		return nil, fmt.Errorf("vote extensions are not enabled; unexpected call to ExtendVote at height %d", req.Height)
	}

	if c.verifyVoteExt == nil {
		return nil, errors.New("vote extensions are enabled but no verify function was set")
	}

	_, latestStore, err := c.store.StateLatest()
	if err != nil {
		return nil, err
	}

	resp, err := c.extendVote(ctx, latestStore, req)
	if err != nil {
		c.logger.Error("failed to verify vote extension", "height", req.Height, "err", err)
		return &abciproto.ExtendVoteResponse{}, nil
	}

	return resp, err
}

func decodeTxs[T transaction.Tx](rawTxs [][]byte, codec transaction.Codec[T]) ([]T, error) {
	txs := make([]T, len(rawTxs))
	for i, rawTx := range rawTxs {
		tx, err := codec.Decode(rawTx)
		if err != nil {
			return nil, fmt.Errorf("unable to decode tx: %d: %w", i, err)
		}
		txs[i] = tx
	}
	return txs, nil
}
