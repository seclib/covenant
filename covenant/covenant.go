package covenant

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/btcsuite/btcd/btcec/v2"

	"go.uber.org/zap"

	covcfg "github.com/babylonchain/covenant-emulator/config"
	"github.com/babylonchain/covenant-emulator/keyring"

	"github.com/babylonchain/babylon/btcstaking"
	asig "github.com/babylonchain/babylon/crypto/schnorr-adaptor-signature"
	bbntypes "github.com/babylonchain/babylon/types"
	bstypes "github.com/babylonchain/babylon/x/btcstaking/types"
	"github.com/btcsuite/btcd/btcutil"

	"github.com/babylonchain/covenant-emulator/clientcontroller"
	"github.com/babylonchain/covenant-emulator/types"
)

var (
	// TODO: Maybe configurable?
	RtyAttNum = uint(5)
	RtyAtt    = retry.Attempts(RtyAttNum)
	RtyDel    = retry.Delay(time.Millisecond * 400)
	RtyErr    = retry.LastErrorOnly(true)
)

type CovenantEmulator struct {
	startOnce sync.Once
	stopOnce  sync.Once

	wg   sync.WaitGroup
	quit chan struct{}

	pk *btcec.PublicKey

	cc clientcontroller.ClientController
	kc *keyring.ChainKeyringController

	config *covcfg.Config
	params *types.StakingParams
	logger *zap.Logger

	// input is used to pass passphrase to the keyring
	input      *strings.Reader
	passphrase string
}

func NewCovenantEmulator(
	config *covcfg.Config,
	cc clientcontroller.ClientController,
	passphrase string,
	logger *zap.Logger,
) (*CovenantEmulator, error) {
	input := strings.NewReader("")
	kr, err := keyring.CreateKeyring(
		config.BabylonConfig.KeyDirectory,
		config.BabylonConfig.ChainID,
		config.BabylonConfig.KeyringBackend,
		input,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create keyring: %w", err)
	}

	kc, err := keyring.NewChainKeyringControllerWithKeyring(kr, config.BabylonConfig.Key, input)
	if err != nil {
		return nil, err
	}

	sk, err := kc.GetChainPrivKey(passphrase)
	if err != nil {
		return nil, fmt.Errorf("covenant key %s is not found: %w", config.BabylonConfig.Key, err)
	}

	pk, err := btcec.ParsePubKey(sk.PubKey().Bytes())
	if err != nil {
		return nil, err
	}

	return &CovenantEmulator{
		cc:         cc,
		kc:         kc,
		config:     config,
		logger:     logger,
		input:      input,
		passphrase: passphrase,
		pk:         pk,
		quit:       make(chan struct{}),
	}, nil
}

func (ce *CovenantEmulator) UpdateParams() error {
	params, err := ce.getParamsWithRetry()
	if err != nil {
		return err
	}
	ce.params = params

	return nil
}

// AddCovenantSignature adds a Covenant signature on the given Bitcoin delegation and submits it to Babylon
// TODO: break this function into smaller components
func (ce *CovenantEmulator) AddCovenantSignature(btcDel *types.Delegation) (*types.TxResponse, error) {
	// 0. nil checks
	if btcDel == nil {
		return nil, fmt.Errorf("empty delegation")
	}

	if btcDel.BtcUndelegation == nil {
		return nil, fmt.Errorf("empty undelegation")
	}

	// 1. the quorum is already achieved, skip sending more sigs
	if btcDel.HasCovenantQuorum(ce.params.CovenantQuorum) {
		return nil, nil
	}

	// 2. check staking tx and slashing tx are valid
	stakingMsgTx, _, err := bbntypes.NewBTCTxFromHex(btcDel.StakingTxHex)
	if err != nil {
		return nil, err
	}

	slashingTx, err := bstypes.NewBTCSlashingTxFromHex(btcDel.SlashingTxHex)
	if err != nil {
		return nil, err
	}

	slashingMsgTx, err := slashingTx.ToMsgTx()
	if err != nil {
		return nil, err
	}

	if err := btcstaking.CheckTransactions(
		slashingMsgTx,
		stakingMsgTx,
		btcDel.StakingOutputIdx,
		int64(ce.params.MinSlashingTxFeeSat),
		ce.params.SlashingRate,
		ce.params.SlashingAddress,
		&ce.config.BTCNetParams,
	); err != nil {
		return nil, fmt.Errorf("invalid txs in the delegation: %w", err)
	}

	// 3. Check unbonding transaction
	unbondingSlashingMsgTx, _, err := bbntypes.NewBTCTxFromHex(btcDel.BtcUndelegation.SlashingTxHex)
	if err != nil {
		return nil, err
	}

	unbondingMsgTx, _, err := bbntypes.NewBTCTxFromHex(btcDel.BtcUndelegation.UnbondingTxHex)
	if err != nil {
		return nil, err
	}

	unbondingInfo, err := btcstaking.BuildUnbondingInfo(
		btcDel.BtcPk,
		btcDel.FpBtcPks,
		ce.params.CovenantPks,
		ce.params.CovenantQuorum,
		uint16(btcDel.BtcUndelegation.UnbondingTime),
		btcutil.Amount(unbondingMsgTx.TxOut[0].Value),
		&ce.config.BTCNetParams,
	)
	if err != nil {
		return nil, err
	}

	err = btcstaking.CheckTransactions(
		unbondingSlashingMsgTx,
		unbondingMsgTx,
		0,
		int64(ce.params.MinSlashingTxFeeSat),
		ce.params.SlashingRate,
		ce.params.SlashingAddress,
		&ce.config.BTCNetParams,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid txs in the undelegation: %w", err)
	}

	// 4. sign covenant staking sigs
	covenantPrivKey, err := ce.getPrivKey()
	if err != nil {
		return nil, fmt.Errorf("failed to get Covenant private key: %w", err)
	}

	stakingInfo, err := btcstaking.BuildStakingInfo(
		btcDel.BtcPk,
		btcDel.FpBtcPks,
		ce.params.CovenantPks,
		ce.params.CovenantQuorum,
		btcDel.GetStakingTime(),
		btcutil.Amount(btcDel.TotalSat),
		&ce.config.BTCNetParams,
	)
	if err != nil {
		return nil, err
	}

	slashingPathInfo, err := stakingInfo.SlashingPathSpendInfo()
	if err != nil {
		return nil, err
	}

	covSigs := make([][]byte, 0, len(btcDel.FpBtcPks))
	for _, valPk := range btcDel.FpBtcPks {
		encKey, err := asig.NewEncryptionKeyFromBTCPK(valPk)
		if err != nil {
			return nil, err
		}
		covenantSig, err := slashingTx.EncSign(
			stakingMsgTx,
			btcDel.StakingOutputIdx,
			slashingPathInfo.GetPkScriptPath(),
			covenantPrivKey,
			encKey,
		)
		if err != nil {
			return nil, err
		}
		covSigs = append(covSigs, covenantSig.MustMarshal())
	}

	// 5. sign covenant unbonding sig
	stakingTxUnbondingPathInfo, err := stakingInfo.UnbondingPathSpendInfo()
	if err != nil {
		return nil, err
	}
	covenantUnbondingSignature, err := btcstaking.SignTxWithOneScriptSpendInputStrict(
		unbondingMsgTx,
		stakingMsgTx,
		btcDel.StakingOutputIdx,
		stakingTxUnbondingPathInfo.GetPkScriptPath(),
		covenantPrivKey,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to sign unbonding tx: %w", err)
	}

	// 6. sign covenant unbonding slashing sig
	slashUnbondingTx, err := bstypes.NewBTCSlashingTxFromHex(btcDel.BtcUndelegation.SlashingTxHex)
	if err != nil {
		return nil, err
	}

	unbondingTxSlashingPath, err := unbondingInfo.SlashingPathSpendInfo()
	if err != nil {
		return nil, err
	}

	covSlashingSigs := make([][]byte, 0, len(btcDel.FpBtcPks))
	for _, fpPk := range btcDel.FpBtcPks {
		encKey, err := asig.NewEncryptionKeyFromBTCPK(fpPk)
		if err != nil {
			return nil, err
		}
		covenantSig, err := slashUnbondingTx.EncSign(
			unbondingMsgTx,
			0, // 0th output is always the unbonding script output
			unbondingTxSlashingPath.GetPkScriptPath(),
			covenantPrivKey,
			encKey,
		)
		if err != nil {
			return nil, err
		}
		covSlashingSigs = append(covSlashingSigs, covenantSig.MustMarshal())
	}

	// 7. submit covenant sigs
	res, err := ce.cc.SubmitCovenantSigs(ce.pk, stakingMsgTx.TxHash().String(), covSigs, covenantUnbondingSignature, covSlashingSigs)

	if err != nil {
		return nil, err
	}

	return &types.TxResponse{TxHash: res.TxHash}, nil
}

func (ce *CovenantEmulator) getPrivKey() (*btcec.PrivateKey, error) {
	sdkPrivKey, err := ce.kc.GetChainPrivKey(ce.passphrase)
	if err != nil {
		return nil, err
	}

	privKey, _ := btcec.PrivKeyFromBytes(sdkPrivKey.Key)

	return privKey, nil
}

// covenantSigSubmissionLoop is the reactor to submit Covenant signature for BTC delegations
func (ce *CovenantEmulator) covenantSigSubmissionLoop() {
	defer ce.wg.Done()

	interval := ce.config.QueryInterval
	limit := ce.config.DelegationLimit
	covenantSigTicker := time.NewTicker(interval)

	for {
		select {
		case <-covenantSigTicker.C:
			// 0. Update slashing address in case it is changed upon governance proposal
			if err := ce.UpdateParams(); err != nil {
				ce.logger.Debug("failed to get staking params", zap.Error(err))
				continue
			}

			// 1. Get all pending delegations
			dels, err := ce.cc.QueryPendingDelegations(limit)
			if err != nil {
				ce.logger.Debug("failed to get pending delegations", zap.Error(err))
				continue
			}
			if len(dels) == 0 {
				ce.logger.Debug("no pending delegations are found")
			}

			for _, d := range dels {
				_, err := ce.AddCovenantSignature(d)
				if err != nil {
					delPkHex := bbntypes.NewBIP340PubKeyFromBTCPK(d.BtcPk).MarshalHex()
					ce.logger.Error(
						"failed to submit covenant signatures to the BTC delegation",
						zap.String("del_btc_pk", delPkHex),
						zap.Error(err),
					)
				}
			}

		case <-ce.quit:
			ce.logger.Debug("exiting covenant signature submission loop")
			return
		}
	}

}

func CreateCovenantKey(keyringDir, chainID, keyName, backend, passphrase, hdPath string) (*types.ChainKeyInfo, error) {
	sdkCtx, err := keyring.CreateClientCtx(
		keyringDir, chainID,
	)
	if err != nil {
		return nil, err
	}

	krController, err := keyring.NewChainKeyringController(
		sdkCtx,
		keyName,
		backend,
	)
	if err != nil {
		return nil, err
	}

	return krController.CreateChainKey(passphrase, hdPath)
}

func (ce *CovenantEmulator) getParamsWithRetry() (*types.StakingParams, error) {
	var (
		params *types.StakingParams
		err    error
	)

	if err := retry.Do(func() error {
		params, err = ce.cc.QueryStakingParams()
		if err != nil {
			return err
		}
		return nil
	}, RtyAtt, RtyDel, RtyErr, retry.OnRetry(func(n uint, err error) {
		ce.logger.Debug(
			"failed to query the consumer chain for the staking params",
			zap.Uint("attempt", n+1),
			zap.Uint("max_attempts", RtyAttNum),
			zap.Error(err),
		)
	})); err != nil {
		return nil, err
	}

	return params, nil
}

func (ce *CovenantEmulator) Start() error {
	var startErr error
	ce.startOnce.Do(func() {
		ce.logger.Info("Starting Covenant Emulator")

		ce.wg.Add(1)
		go ce.covenantSigSubmissionLoop()
	})

	return startErr
}

func (ce *CovenantEmulator) Stop() error {
	var stopErr error
	ce.stopOnce.Do(func() {
		ce.logger.Info("Stopping Covenant Emulator")

		// Always stop the submission loop first to not generate additional events and actions
		ce.logger.Debug("Stopping submission loop")
		close(ce.quit)
		ce.wg.Wait()

		ce.logger.Debug("Covenant Emulator successfully stopped")
	})
	return stopErr
}