package stakerservice

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/babylonchain/btc-staker/babylonclient"
	str "github.com/babylonchain/btc-staker/staker"
	scfg "github.com/babylonchain/btc-staker/stakercfg"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/cometbft/cometbft/libs/log"
	rpc "github.com/cometbft/cometbft/rpc/jsonrpc/server"
	rpctypes "github.com/cometbft/cometbft/rpc/jsonrpc/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/sirupsen/logrus"
)

const (
	defaultOffset = 0
	defaultLimit  = 50
	maxLimit      = 100
)

type RoutesMap map[string]*rpc.RPCFunc

type StakerService struct {
	started int32

	config      *scfg.Config
	staker      *str.StakerApp
	logger      *logrus.Logger
	db          kvdb.Backend
	interceptor signal.Interceptor
}

func NewStakerService(
	c *scfg.Config,
	s *str.StakerApp,
	l *logrus.Logger,
	sig signal.Interceptor,
	db kvdb.Backend,
) *StakerService {
	return &StakerService{
		config:      c,
		staker:      s,
		logger:      l,
		interceptor: sig,
		db:          db,
	}
}

func (s *StakerService) health(_ *rpctypes.Context) (*ResultHealth, error) {
	return &ResultHealth{}, nil
}

func (s *StakerService) stake(_ *rpctypes.Context,
	stakerAddress string,
	stakingAmount int64,
	validatorPk string,
	stakingTimeBlocks int64,
) (*ResultStake, error) {

	if stakingAmount <= 0 {
		return nil, fmt.Errorf("staking amount must be positive")
	}

	amount := btcutil.Amount(stakingAmount)

	address, err := btcutil.DecodeAddress(stakerAddress, &s.config.ActiveNetParams)

	if err != nil {
		return nil, err
	}

	validatorPkBytes, err := hex.DecodeString(validatorPk)

	if err != nil {
		return nil, err
	}

	valSchnorrKey, err := schnorr.ParsePubKey(validatorPkBytes)

	if err != nil {
		return nil, err
	}

	if stakingTimeBlocks <= 0 || stakingTimeBlocks > math.MaxUint16 {
		return nil, fmt.Errorf("staking time must be positive and lower than %d", math.MaxUint16)
	}

	stakingTimeUint16 := uint16(stakingTimeBlocks)

	stakingTxHash, err := s.staker.StakeFunds(address, amount, valSchnorrKey, stakingTimeUint16)

	if err != nil {
		return nil, err
	}

	return &ResultStake{
		TxHash: stakingTxHash.String(),
	}, nil
}

func (s *StakerService) stakingDetails(_ *rpctypes.Context,
	stakingTxHash string) (*StakingDetails, error) {

	txHash, err := chainhash.NewHashFromStr(stakingTxHash)

	if err != nil {
		return nil, err
	}

	storedTx, err := s.staker.GetStoredTransaction(txHash)

	if err != nil {
		return nil, err
	}

	return &StakingDetails{
		StakingTxHash: storedTx.BtcTx.TxHash().String(),
		StakerAddress: storedTx.StakerAddress,
		StakingScript: hex.EncodeToString(storedTx.TxScript),
		StakingState:  storedTx.State.String(),
	}, nil
}

func (s *StakerService) spendStake(_ *rpctypes.Context,
	stakingTxHash string) (*SpendTxDetails, error) {
	txHash, err := chainhash.NewHashFromStr(stakingTxHash)

	if err != nil {
		return nil, err
	}

	spendTxHash, value, err := s.staker.SpendStake(txHash)

	if err != nil {
		return nil, err
	}

	txValue := strconv.FormatInt(int64(*value), 10)

	return &SpendTxDetails{
		TxHash:  spendTxHash.String(),
		TxValue: txValue,
	}, nil
}

func (s *StakerService) listOutputs(_ *rpctypes.Context) (*OutputsResponse, error) {

	outputs, err := s.staker.ListUnspentOutputs()

	if err != nil {
		return nil, err
	}

	var outputDetails []OutputDetail

	for _, output := range outputs {
		outputDetails = append(outputDetails, OutputDetail{
			Address: output.Address,
			Amount:  output.Amount.String(),
		})
	}

	return &OutputsResponse{
		Outputs: outputDetails,
	}, nil
}

type PageParams struct {
	Offset uint64
	Limit  uint64
}

func getPageParams(offsetPtr *int, limitPtr *int) PageParams {
	var limit uint64

	if limitPtr == nil {
		limit = defaultLimit
	} else {
		limit = uint64(*limitPtr)
	}

	if limit > maxLimit {
		limit = maxLimit
	}

	var offset uint64

	if offsetPtr == nil {
		offset = defaultOffset
	} else {
		offset = uint64(*offsetPtr)
	}

	return PageParams{
		Offset: offset,
		Limit:  limit,
	}
}

func (s *StakerService) validators(_ *rpctypes.Context, offset, limit *int) (*ValidatorsResponse, error) {

	pageParams := getPageParams(offset, limit)

	validatorsResp, err := s.staker.ListActiveValidators(pageParams.Limit, pageParams.Offset)

	if err != nil {
		return nil, err
	}

	var validatorInfos []ValidatorInfoResponse

	for _, validator := range validatorsResp.Validators {
		v := ValidatorInfoResponse{
			BabylonPublicKey: hex.EncodeToString(validator.BabylonPk.Key),
			BtcPublicKey:     hex.EncodeToString(schnorr.SerializePubKey(&validator.BtcPk)),
		}

		validatorInfos = append(validatorInfos, v)
	}

	totalCount := strconv.FormatUint(validatorsResp.Total, 10)

	return &ValidatorsResponse{
		Validators:          validatorInfos,
		TotalValidatorCount: totalCount,
	}, nil
}

func (s *StakerService) listStakingTransactions(_ *rpctypes.Context, offset, limit *int) (*ListStakingTransactionsResponse, error) {
	pageParams := getPageParams(offset, limit)

	txResult, err := s.staker.StoredTransactions(pageParams.Limit, pageParams.Offset)

	if err != nil {
		return nil, err
	}

	var stakingDetails []StakingDetails

	for _, tx := range txResult.Transactions {
		stakingDetails = append(stakingDetails, StakingDetails{
			StakingTxHash: tx.BtcTx.TxHash().String(),
			StakerAddress: tx.StakerAddress,
			StakingScript: hex.EncodeToString(tx.TxScript),
			StakingState:  tx.State.String(),
		})
	}

	totalCount := strconv.FormatUint(txResult.Total, 10)

	return &ListStakingTransactionsResponse{
		Transactions:          stakingDetails,
		TotalTransactionCount: totalCount,
	}, nil
}

func decodeBtcTx(txHex string) (*wire.MsgTx, error) {
	txBytes, err := hex.DecodeString(txHex)

	if err != nil {
		return nil, err
	}

	var txMsg wire.MsgTx

	err = txMsg.Deserialize(bytes.NewReader(txBytes))

	if err != nil {
		return nil, err
	}

	return &txMsg, nil
}

func (s *StakerService) watchStaking(
	_ *rpctypes.Context,
	stakingTx string,
	stakingScript string,
	slashingTx string,
	slashingTxSig string,
	stakerBabylonPk string,
	stakerAddress string,
	stakerBabylonSig string,
	stakerBtcSig string,
	popType int,
) (*ResultStake, error) {

	stkTx, err := decodeBtcTx(stakingTx)

	if err != nil {
		return nil, err
	}

	slshTx, err := decodeBtcTx(slashingTx)

	if err != nil {
		return nil, err
	}

	stkScript, err := hex.DecodeString(stakingScript)

	if err != nil {
		return nil, err
	}

	address, err := btcutil.DecodeAddress(stakerAddress, &s.config.ActiveNetParams)

	if err != nil {
		return nil, err
	}

	slashTxSigBytes, err := hex.DecodeString(slashingTxSig)

	if err != nil {
		return nil, err
	}

	slashingTxSchnorSig, err := schnorr.ParseSignature(slashTxSigBytes)

	if err != nil {
		return nil, err
	}

	stakerBabylonPubkeyBytes, err := hex.DecodeString(stakerBabylonPk)

	if err != nil {
		return nil, err
	}

	if len(stakerBabylonPubkeyBytes) != secp256k1.PubKeySize {
		return nil, fmt.Errorf("babylon public key must have %d bytes", secp256k1.PubKeySize)
	}

	stakerBabylonPubKey := secp256k1.PubKey{
		Key: stakerBabylonPubkeyBytes,
	}

	stakerBabylonSigBytes, err := hex.DecodeString(stakerBabylonSig)

	if err != nil {
		return nil, err
	}

	stakerBtcSigBytes, err := hex.DecodeString(stakerBtcSig)

	if err != nil {
		return nil, err
	}

	btcPopType, err := babylonclient.IntToPopType(popType)

	if err != nil {
		return nil, err
	}

	proofOfPossesion, err := babylonclient.NewBabylonPop(btcPopType, stakerBabylonSigBytes, stakerBtcSigBytes)

	if err != nil {
		return nil, err
	}

	hash, err := s.staker.WatchStaking(
		stkTx,
		stkScript,
		slshTx,
		slashingTxSchnorSig,
		&stakerBabylonPubKey,
		address,
		proofOfPossesion,
	)

	if err != nil {
		return nil, err
	}

	return &ResultStake{
		TxHash: hash.String(),
	}, nil
}

func (s *StakerService) unbondStaking(_ *rpctypes.Context, stakingTxHash string, feeRate *int) (*UnbondingResponse, error) {
	txHash, err := chainhash.NewHashFromStr(stakingTxHash)

	if err != nil {
		return nil, err
	}

	var feeRateBtc *btcutil.Amount = nil

	if feeRate != nil {
		amt := btcutil.Amount(*feeRate)
		feeRateBtc = &amt
	}

	unbondingTxHash, err := s.staker.UnbondStaking(*txHash, feeRateBtc)

	if err != nil {
		return nil, err
	}

	return &UnbondingResponse{
		UnbondingTxHash: unbondingTxHash.String(),
	}, nil
}

func (s *StakerService) GetRoutes() RoutesMap {
	return RoutesMap{
		// info AP
		"health": rpc.NewRPCFunc(s.health, ""),
		// staking API
		"stake":                     rpc.NewRPCFunc(s.stake, "stakerAddress,stakingAmount,validatorPk,stakingTimeBlocks"),
		"staking_details":           rpc.NewRPCFunc(s.stakingDetails, "stakingTxHash"),
		"spend_stake":               rpc.NewRPCFunc(s.spendStake, "stakingTxHash"),
		"list_staking_transactions": rpc.NewRPCFunc(s.listStakingTransactions, "offset,limit"),
		"unbond_staking":            rpc.NewRPCFunc(s.unbondStaking, "stakingTxHash,feeRate"),

		// watch api
		"watch_staking_tx": rpc.NewRPCFunc(s.watchStaking, "stakingTx,stakingScript,slashingTx,slashingTxSig,stakerBabylonPk,stakerAddress,stakerBabylonSig,stakerBtcSig,popType"),

		// Wallet api
		"list_outputs": rpc.NewRPCFunc(s.listOutputs, ""),

		// Babylon api
		"babylon_validators": rpc.NewRPCFunc(s.validators, "offset,limit"),
	}
}

func (s *StakerService) RunUntilShutdown() error {
	if atomic.AddInt32(&s.started, 1) != 1 {
		return nil
	}

	defer func() {
		s.logger.Info("Shutdown complete")
	}()

	defer func() {
		s.logger.Info("Closing database...")
		s.db.Close()
		s.logger.Info("Database closed")
	}()

	mkErr := func(format string, args ...interface{}) error {
		logFormat := strings.ReplaceAll(format, "%w", "%v")
		s.logger.Errorf("Shutting down because error in main "+
			"method: "+logFormat, args...)
		return fmt.Errorf(format, args...)
	}

	err := s.staker.Start()
	if err != nil {
		return mkErr("error starting staker: %w", err)
	}

	defer func() {
		_ = s.staker.Stop()
		s.logger.Info("staker stop complete")
	}()

	routes := s.GetRoutes()
	// TODO: Add staker service dedicated config to define those values
	config := rpc.DefaultConfig()
	// This way logger will log to stdout and file
	// TODO: investigate if we can use logrus directly to pass it to rpcserver
	rpcLogger := log.NewTMLogger(s.logger.Writer())

	listeners := make([]net.Listener, len(s.config.RpcListeners))
	for i, listenAddr := range s.config.RpcListeners {
		listenAddressStr := listenAddr.Network() + "://" + listenAddr.String()
		mux := http.NewServeMux()
		rpc.RegisterRPCFuncs(mux, routes, rpcLogger)

		listener, err := rpc.Listen(
			listenAddressStr,
			config,
		)

		if err != nil {
			return mkErr("unable to listen on %s: %v",
				listenAddressStr, err)
		}

		defer func() {
			err := listener.Close()
			if err != nil {
				s.logger.Error("Error closing listener", "err", err)
			}
		}()

		// Start standard HTTP server serving json-rpc
		// TODO: Add additional middleware, like CORS, TLS, etc.
		// TODO: Consider we need some websockets for some notications
		go func() {
			s.logger.Debug("Starting Json RPC HTTP server ", "address", listenAddressStr)

			err := rpc.Serve(
				listener,
				mux,
				rpcLogger,
				config,
			)

			s.logger.Error("Json RPC HTTP server stopped ", "err", err)
		}()

		listeners[i] = listener
	}

	s.logger.Info("Staker Service fully started")

	// Wait for shutdown signal from either a graceful service stop or from
	// the interrupt handler.
	<-s.interceptor.ShutdownChannel()

	s.logger.Info("Received shutdown signal. Stopping...")

	return nil
}
