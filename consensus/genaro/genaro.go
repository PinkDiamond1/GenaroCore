package genaro

import (
	"errors"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/GenaroNetwork/Genaro-Core/accounts"
	"github.com/GenaroNetwork/Genaro-Core/common"
	"github.com/GenaroNetwork/Genaro-Core/consensus"
	"github.com/GenaroNetwork/Genaro-Core/core/types"
	"github.com/GenaroNetwork/Genaro-Core/crypto/sha3"
	"github.com/GenaroNetwork/Genaro-Core/ethdb"
	"github.com/GenaroNetwork/Genaro-Core/log"
	"github.com/GenaroNetwork/Genaro-Core/params"
	"github.com/GenaroNetwork/Genaro-Core/rlp"
	"github.com/hashicorp/golang-lru"
	"github.com/GenaroNetwork/Genaro-Core/crypto"
	"github.com/GenaroNetwork/Genaro-Core/core/state"
	"github.com/GenaroNetwork/Genaro-Core/rpc"
	"sort"
)

const (
	wiggleTime					= 500 * time.Millisecond // Random delay (per signer) to allow concurrent signers
	inmemorySnapshots			= 128                    // Number of recent snapshots to keep in memory
	epochLength					= uint64(5000)           // Default number of blocks a turn
	epochPerYear				= uint64(12)
	minDistance					= uint64(500)
	SurplusCoinAddress			= "aaa"
	CoinActualRewardsAddress	= "bbb"
	StorageActualRewardsAddress	= "ccc"
	Pre							= "pre"
	TotalActualRewardsAddress	= "ggg"

	backStakePeriod				= uint64(2)

	IncrementDifficult			= 1
)

var (
	//totalRewards			*big.Int = big.NewInt(700000000e+18)
	coinRewardsRatio				 = 1250
	storageRewardsRatio				 = 1250
	ratioPerYear					 = 700
	base							 = 10000
)

var (
	// extra data is empty
	errEmptyExtra = errors.New("extra data is empty")
	// errUnauthorized is returned if a header is signed by a non-authorized entity.
	errUnauthorized = errors.New("unauthorized")
	// errUnauthorized is returned if epoch block has no committee list
	errInvalidEpochBlock = errors.New("epoch block has no committee list")
	errInvalidDifficulty = errors.New("invalid difficulty")
)

// Various error messages to mark blocks invalid.
var (
	// errUnknownBlock is returned when the list of signers is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")
)

// SignerFn is a signer callback function to request a hash to be signed by a
// backing account.
type SignerFn func(accounts.Account, []byte) ([]byte, error)

// sigHash returns the hash which is used as input for the proof-of-authority
// signing. It is the hash of the entire header apart from the 65 byte signature
// contained at the end of the extra data.
//
// Note, the method requires the extra data to be at least 65 bytes, otherwise it
// panics. This is done to avoid accidentally using both forms (signature present
// or not), which could be abused to produce different hashes for the same header.
func sigHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewKeccak256()

	rlp.Encode(hasher, []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra, // just hash extra
		header.MixDigest,
		header.Nonce,
	})
	hasher.Sum(hash[:0])
	return hash
}

// ecrecover extracts the Ethereum account address from a signed header.
func ecrecover(header *types.Header) (common.Address, error) {
	// If the signature's already cached, return that
	// Retrieve the signature from the header extra-data
	extraData := UnmarshalToExtra(header)
	if extraData == nil {
		return common.Address{}, errEmptyExtra
	}
	signature := extraData.Signature
	//Why reset??
	ResetHeaderSignature(header)
	// Recover the public key and the Ethereum address
	pubkey, err := crypto.Ecrecover(sigHash(header).Bytes(), signature)
	SetHeaderSignature(header, signature)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])
	return signer, nil
}

type Genaro struct {
	config     *params.GenaroConfig // genaro config
	db         ethdb.Database       // Database to store and retrieve snapshot checkpoints
	recents    *lru.ARCCache        // snapshot cache
	signer     common.Address       // Ethereum address of the signing key
	lock       sync.RWMutex         // Protects the signer fields
	signFn     SignerFn             // sign function
}

// New creates a Genaro consensus engine
func New(config *params.GenaroConfig, snapshotDb ethdb.Database) *Genaro {
	// Set any missing consensus parameters to their defaults
	conf := *config
	if conf.Epoch == 0 {
		conf.Epoch = epochLength
	}
	// Allocate the snapshot caches and create the engine
	recents, _ := lru.NewARC(inmemorySnapshots)

	return &Genaro{
		config:  &conf,
		db:      snapshotDb,
		recents: recents,
	}
}

// Author implements consensus.Engine, returning the Ethereum address recovered
// from the signature in the header's extra-data section.
func (g *Genaro) Author(header *types.Header) (common.Address, error) {
	log.Info("Author:" + header.Number.String())
	return ecrecover(header)
}

// Prepare implements consensus.Engine, preparing all the consensus fields of the
// header for running the transactions on top.
func (g *Genaro) Prepare(chain consensus.ChainReader, header *types.Header) error {
	log.Info("Prepare:" + header.Number.String())
	// set block author in Coinbase
	// TODO It may be modified later
	header.Coinbase = g.signer
	header.Nonce = types.BlockNonce{}
	number := header.Number.Uint64()

	snap, err := g.snapshot(chain, GetTurnOfCommiteeByBlockNumber(g.config, number))
	if err != nil {
		return err
	}

	// Set the correct difficulty
	header.Difficulty = CalcDifficulty(snap, g.signer, number)
	// if we reach the point that should get Committee written in the block
	//if number == GetLastBlockNumberOfEpoch(g.config, currEpochNumber) {
	//	// get committee rank material period
	//	materialPeriod := currEpochNumber - g.config.ElectionPeriod
	//	// load committee rank from db or generateCommittee from material period
	//	writeSnap, err := g.snapshot(chain, materialPeriod)
	//	if err != nil {
	//		return err
	//	}
	//	// write the committee rank into Block's Extra
	//	err = SetHeaderCommitteeRankList(header, writeSnap.CommitteeRank)
	//	if err != nil {
	//		return err
	//	}
	//}
	// Mix digest is reserved for now, set to empty
	header.MixDigest = common.Hash{}
	// Ensure the timestamp has the correct delay
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	header.Time = new(big.Int).SetInt64(time.Now().Unix())
	if header.Time.Int64() < parent.Time.Int64() {
		header.Time = new(big.Int).SetInt64(parent.Time.Int64() + 1)
	}
	return nil
}

// Seal implements consensus.Engine, attempting to create a sealed block using
// the local signing credentials.
func (g *Genaro) Seal(chain consensus.ChainReader, block *types.Block, stop <-chan struct{}) (*types.Block, error) {
	log.Info("Seal:" + block.Number().String())
	header := block.Header()
	// Sealing the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return nil, errUnknownBlock
	}
	// Don't hold the signer fields for the entire sealing procedure
	g.lock.RLock()
	signer, signFn := g.signer, g.signFn
	g.lock.RUnlock()

	// Sweet, wait some time if not in-turn
	snap, err := g.snapshot(chain, GetTurnOfCommiteeByBlockNumber(g.config, number))
	if err != nil {
		return nil, err
	}
	//when address is not in committee, reverseDifficult is snap.CommitteeSize + 1,
	//when address is in committee, reverseDifficult is index + 1, intrun address delay is about 1s
	reverseDifficult := snap.CommitteeSize - header.Difficulty.Uint64() + 1
	delay := time.Duration(reverseDifficult * uint64(time.Second))
	delay += time.Duration(rand.Int63n(int64(wiggleTime)))
	log.Info("delay:"+delay.String())
	select {
	case <-stop:
		return nil, nil
	case <-time.After(delay):
	}
	// Sign all the things!
	sighash, err := signFn(accounts.Account{Address: signer}, sigHash(header).Bytes())
	if err != nil {
		return nil, err
	}
	SetHeaderSignature(header, sighash)
	return block.WithSeal(header), nil
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have based on the previous blocks in the chain and the
// current signer.
func (g *Genaro) CalcDifficulty(chain consensus.ChainReader, time uint64, parent *types.Header) *big.Int {
	log.Info("CalcDifficulty:" + parent.Number.String())
	blockNumber := parent.Number.Uint64() + 1
	dependEpoch := GetTurnOfCommiteeByBlockNumber(g.config, blockNumber)

	snap, err := g.snapshot(chain, dependEpoch)
	if err != nil {
		return nil
	}
	return CalcDifficulty(snap, g.signer, blockNumber)
}

func max(x uint64, y uint64) uint64 {
	if x > y {
		return x
	}else{
		return y
	}
}

// CalcDifficulty return the distance between my index and intern-index
// depend on snap
func CalcDifficulty(snap *CommitteeSnapshot, addr common.Address, blockNumber uint64) *big.Int {
	index := snap.getCurrentRankIndex(addr)
	if index < 0 {
		return new(big.Int).SetUint64(0)
	}
	difficult := snap.CommitteeSize - uint64(index)
	return new(big.Int).SetUint64(uint64(difficult))
}

// Authorize injects a private key into the consensus engine to mint new blocks
// with.
func (g *Genaro) Authorize(signer common.Address, signFn SignerFn) {
	log.Info("Authorize")
	g.lock.Lock()
	defer g.lock.Unlock()

	g.signer = signer
	g.signFn = signFn
}

// Snapshot retrieves the snapshot at "electoral materials" period.
// Snapshot func retrieves ths snapshot in order of memory, local DB, block header.
// If committeeSnapshot is empty and it is time to write, we will create a new one, otherwise return nil
func (g *Genaro) snapshot(chain consensus.ChainReader, epollNumber uint64) (*CommitteeSnapshot, error) {
	// Search for a snapshot in memory or on disk for checkpoints
	var (
		snap *CommitteeSnapshot
	)
	isCreateNew := false
	// If an in-memory snapshot was found, use that
	if s, ok := g.recents.Get(epollNumber); ok {
		snap = s.(*CommitteeSnapshot)
	}else if epollNumber < g.config.ValidPeriod + g.config.ElectionPeriod {
		// If we're at block 0 ~ ElectionPeriod + ValidPeriod - 1, make a snapshot by genesis block
		// TODO
		//committeeRank := make([]common.Address, 10)
		//committeeRank[0] = common.HexToAddress("0xe5f0b187f916eaee5c87074d5d185f3eaf527dc9")
		//committeeRank[1] = common.HexToAddress("0xed19295615336ee56D4889BcdB90563b7abA02F7")
		//committeeRank[2] = common.HexToAddress("0x4180B3a9059cb43dc93e72e641B466fEBeFEa902")
		//committeeRank[3] = common.HexToAddress("0x8d024417f284B10B1fE8f6b02533F5aeFb7C8e23")
		//committeeRank[4] = common.HexToAddress("0xCc3b246d887435490409eC9037B7320e797B195a")
		//committeeRank[5] = common.HexToAddress("0xE45815411FBE2607C7E944C2E94baFc4BD7c7163")
		//committeeRank[6] = common.HexToAddress("0x51AAddb5f44525151D3554d1876bbc9d6E9Bff1F")
		//committeeRank[7] = common.HexToAddress("0x3C3DD12E1F11d56423adF3dC204E91e78a1f1FCa")
		//committeeRank[8] = common.HexToAddress("0x53Bd332D7c34f8ca0bFCA3f51c71BC1C523F6B4A")
		//committeeRank[9] = common.HexToAddress("0xdFc387187b63af2Ed108b153187d7B2cfDD93F73")
		//proportion := []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
		//genaroConfig := &params.GenaroConfig{
		//	Epoch:				5000,
		//	BlockInterval:		10,
		//	ElectionPeriod:		1,
		//	ValidPeriod:		1,
		//	CurrencyRates:		10,
		//	CommitteeMaxSize:	5,
		//}
		//blockHash := new(common.Hash)
		//snap = newSnapshot(genaroConfig, 0, *blockHash, 0, committeeRank, proportion)

		h := chain.GetHeaderByNumber(0)
		committeeRank, proportion := GetHeaderCommitteeRankList(h)
		snap = newSnapshot(chain.Config().Genaro, 0, h.Hash(), 0, committeeRank, proportion)
		isCreateNew = true
	}else{
		// visit the blocks in epollNumber - ValidPeriod - ElectionPeriod tern
		startBlock := GetFirstBlockNumberOfEpoch(g.config, epollNumber - g.config.ValidPeriod - g.config.ElectionPeriod)
		endBlock := GetLastBlockNumberOfEpoch(g.config, epollNumber - g.config.ValidPeriod - g.config.ElectionPeriod)
		h := chain.GetHeaderByNumber(endBlock+1)
		committeeRank, proportion := GetHeaderCommitteeRankList(h)
		snap = newSnapshot(chain.Config().Genaro, h.Number.Uint64(), h.Hash(), epollNumber -
			g.config.ValidPeriod - g.config.ElectionPeriod, committeeRank, proportion)

		log.Trace("computing rank from", startBlock, "to", endBlock)
		isCreateNew = true
	}
	g.recents.Add(epollNumber, snap)
	// If we've generated a new checkpoint snapshot, save to disk
	if isCreateNew {
		if err := snap.store(g.db); err != nil {
			return nil, err
		}
		log.Trace("Stored snapshot to disk", "epollNumber", epollNumber)
	}
	return snap, nil
}

// VerifySeal implements consensus.Engine, checking whether the signature contained
// in the header satisfies the consensus protocol requirements.
func (g *Genaro) VerifySeal(chain consensus.ChainReader, header *types.Header) error {
	log.Info("VerifySeal:" + header.Number.String())
	blockNumber := header.Number.Uint64()
	if blockNumber == 0 {
		return errUnknownBlock
	}
	// Don't waste time checking blocks from the future
	if header.Time.Cmp(big.NewInt(time.Now().Unix())) > 0 {
		return consensus.ErrFutureBlock
	}
	// check epoch point
	epochPoint := (blockNumber % g.config.Epoch) == 0
	if epochPoint {
		extraData := UnmarshalToExtra(header)
		committeeSize := uint64(len(extraData.CommitteeRank) / common.AddressLength)
		if committeeSize == 0 || committeeSize > g.config.CommitteeMaxSize {
			return errInvalidEpochBlock
		}
	}
	// get current committee snapshot
	snap, err := g.snapshot(chain, GetTurnOfCommiteeByBlockNumber(g.config, blockNumber))
	if err != nil {
		return err
	}
	// get signer from header
	signer, err := ecrecover(header)
	if err != nil {
		return err
	}
	// check signer
	if _, ok := snap.Committee[signer]; !ok {
		return errUnauthorized
	}

	// Ensure the timestamp has the correct delay
	parent := chain.GetHeader(header.ParentHash, blockNumber-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	if header.Time.Uint64() < parent.Time.Uint64() {
		return errUnknownBlock
	}
	// Ensure that difficulty corresponds to the turn of the signer
	inturn := snap.inturn(blockNumber, signer)
	if !inturn {
		bias := header.Difficulty.Uint64()
		delay := uint64(time.Duration(bias * uint64(time.Second)))
		if parent.Time.Uint64()+delay/uint64(time.Second) > header.Time.Uint64() {
			return errInvalidDifficulty
		}
	}
	return nil
}

// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (g *Genaro) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	log.Info("VerifyUncles:" + block.Number().String())
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed")
	}
	return nil
}

func Rank(candidateInfos state.CandidateInfos) ([]common.Address, []uint64){
	candidateInfos.Apply()
	sort.Sort(candidateInfos)
	committeeRank := make([]common.Address, len(candidateInfos))
	proportion := make([]uint64, len(candidateInfos))
	total := uint64(0)
	for _, c := range candidateInfos{
		total += c.Stake
	}
	if total == 0 {
		return committeeRank, proportion
	}
	for i, c := range candidateInfos{
		committeeRank[i] = c.Signer
		proportion[i] = c.Stake*uint64(base)/total
	}

	return committeeRank, proportion
}

func updateEpochRewards(state *state.StateDB)  {
	//reset CoinActualRewards and StorageActualRewards, add TotalActualRewards
	coinrewards := state.GetBalance(common.BytesToAddress([]byte(CoinActualRewardsAddress)))
	storagerewards := state.GetBalance(common.BytesToAddress([]byte(StorageActualRewardsAddress)))

	state.SetBalance(common.BytesToAddress([]byte(Pre + CoinActualRewardsAddress)), coinrewards)
	state.SetBalance(common.BytesToAddress([]byte(Pre + StorageActualRewardsAddress)), storagerewards)

	state.SetBalance(common.BytesToAddress([]byte(CoinActualRewardsAddress)), big.NewInt(0))
	state.SetBalance(common.BytesToAddress([]byte(StorageActualRewardsAddress)), big.NewInt(0))

	state.AddBalance(common.BytesToAddress([]byte(TotalActualRewardsAddress)), coinrewards)
	state.AddBalance(common.BytesToAddress([]byte(TotalActualRewardsAddress)), storagerewards)
}

func updateEpochYearRewards(state *state.StateDB)  {
	surplusrewards := state.GetBalance(common.BytesToAddress([]byte(SurplusCoinAddress)))
	state.SetBalance(common.BytesToAddress([]byte(Pre + SurplusCoinAddress)), surplusrewards)

	totalRewards := state.GetBalance(common.BytesToAddress([]byte(TotalActualRewardsAddress)))
	state.SubBalance(common.BytesToAddress([]byte(SurplusCoinAddress)), totalRewards)
	state.SetBalance(common.BytesToAddress([]byte(TotalActualRewardsAddress)), big.NewInt(0))
}

func updateSpecialBlock(config *params.GenaroConfig, header *types.Header, state *state.StateDB)  {
	blockNumber := header.Number.Uint64()
	if blockNumber%config.Epoch == 0 {
		//rank
		epochStartBlockNumber := blockNumber - config.Epoch
		epochEndBlockNumber := blockNumber
		candidateInfos := state.GetCandidatesInfoInRange(epochStartBlockNumber, epochEndBlockNumber)
		commiteeRank, proportion := Rank(candidateInfos)
		if uint64(len(candidateInfos)) <= config.CommitteeMaxSize {
			SetHeaderCommitteeRankList(header, commiteeRank, proportion)
		}else{
			SetHeaderCommitteeRankList(header, commiteeRank[:config.CommitteeMaxSize],proportion[:config.CommitteeMaxSize])
		}
		//CoinActualRewards and StorageActualRewards should update per epoch
		updateEpochRewards(state)
	}
	if blockNumber%(epochPerYear*config.Epoch) == 0 {
		//CoinActualRewards and StorageActualRewards should update per epoch, surplusCoin should update per year
		updateEpochYearRewards(state)
	}
}

func handleAlreadyBackStakeList(config *params.GenaroConfig, header *types.Header, state *state.StateDB)  {
	blockNumber := header.Number.Uint64()
	backlist := state.GetAlreadyBackStakeList()
	for i := 0; i < len(backlist); i++ {
		back := backlist[i]
		if IsBackStakeBlockNumber(config, back.BackBlockNumber, blockNumber) {
			backlist = append(backlist[:i], backlist[i+1:]...)
			i--
		}
	}
	state.SetAlreadyBackStakeList(backlist)
}

func handleApplyBackStakeList(config *params.GenaroConfig, header *types.Header, state *state.StateDB)  {
	blockNumber := header.Number.Uint64()
	backlist := state.GetAlreadyBackStakeList()
	for i := 0; i < len(backlist); i++ {
		back := backlist[i]
		if IsBackStakeBlockNumber(config, back.BackBlockNumber, blockNumber) {
			backlist = append(backlist[:i], backlist[i+1:]...)
			i--
		}
	}
	state.SetAlreadyBackStakeList(backlist)
}

// Finalize implements consensus.Engine, ensuring no uncles are set, nor block
// rewards given, and returns the final block.
func (g *Genaro) Finalize(chain consensus.ChainReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	log.Info("Finalize:" + header.Number.String())
	//commit rank
	blockNumber := header.Number.Uint64()
	updateSpecialBlock(g.config, header, state)

	snap, err := g.snapshot(chain, GetTurnOfCommiteeByBlockNumber(g.config, header.Number.Uint64()))
	if err != nil {
		return nil, err
	}
	proportion := snap.Committee[header.Coinbase]
	//  coin interest reward
	accumulateInterestRewards(g.config, state, header, proportion, blockNumber)
	// storage reward
	accumulateStorageRewards(g.config, state, blockNumber)

	//handle apply back stake list

	//handle already back stake list
	handleAlreadyBackStakeList(g.config, header, state)

	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)

	// Assemble and return the final block for sealing
	return types.NewBlock(header, txs, nil, receipts), nil
}

func getCoinCofficient(config *params.GenaroConfig, coinrewards, surplusRewards *big.Int) uint64 {
	if coinrewards.Cmp(big.NewInt(0)) == 0 {
		return uint64(base)
	}
	planrewards := big.NewInt(0)
	//get total coinReward
	planrewards.Mul(surplusRewards, big.NewInt(int64(coinRewardsRatio)))
	planrewards.Div(planrewards, big.NewInt(int64(base)))
	//get coinReward perYear
	planrewards.Div(planrewards, big.NewInt(int64(ratioPerYear)))
	planrewards.Mul(planrewards, big.NewInt(int64(base)))
	//get coinReward perEpoch
	planrewards.Div(planrewards, big.NewInt(int64(config.Epoch)))
	//get coefficient
	planrewards.Mul(planrewards, big.NewInt(int64(base)))
	coinRatio := planrewards.Div(planrewards, coinrewards).Uint64()
	return coinRatio
}

func getStorageCoefficient(config *params.GenaroConfig, storagerewards, surplusRewards *big.Int) uint64 {
	if storagerewards.Cmp(big.NewInt(0)) == 0 {
		return uint64(base)
	}
	planrewards := big.NewInt(0)
	//get total storageReward
	planrewards.Mul(surplusRewards, big.NewInt(int64(storageRewardsRatio)))
	planrewards.Div(planrewards, big.NewInt(int64(base)))
	//get storageReward perYear
	planrewards.Div(planrewards, big.NewInt(int64(ratioPerYear)))
	planrewards.Mul(planrewards, big.NewInt(int64(base)))
	//get storageReward perEpoch
	planrewards.Div(planrewards, big.NewInt(int64(config.Epoch)))
	//get coefficient
	planrewards.Mul(planrewards, big.NewInt(int64(base)))
	storageRatio := planrewards.Div(planrewards, storagerewards).Uint64()
	return storageRatio
}

// AccumulateInterestRewards credits the reward to the block author by coin  interest
func accumulateInterestRewards(config *params.GenaroConfig, state *state.StateDB, header *types.Header, proportion uint64, blockNumber uint64) error {
	preCoinRewards := state.GetBalance(common.BytesToAddress([]byte(Pre + CoinActualRewardsAddress)))
	preSurplusRewards := big.NewInt(0)
	//when now is the start of year, preSurplusRewards should get "Pre + SurplusCoinAddress"
	if blockNumber%(config.Epoch*epochPerYear) == 0 {
		preSurplusRewards = state.GetBalance(common.BytesToAddress([]byte(Pre + SurplusCoinAddress)))
	}else{
		preSurplusRewards = state.GetBalance(common.BytesToAddress([]byte(SurplusCoinAddress)))
	}
	coefficient := getCoinCofficient(config, preCoinRewards, preSurplusRewards)

	surplusRewards := state.GetBalance(common.BytesToAddress([]byte(SurplusCoinAddress)))
	//plan rewards per year
	planRewards := surplusRewards.Mul(surplusRewards, big.NewInt(int64(coinRewardsRatio)))
	planRewards.Div(planRewards, big.NewInt(int64(base)))
	//plan rewards per epoch
	planRewards.Div(planRewards, big.NewInt(int64(epochPerYear)))
	//Coefficient adjustment
	planRewards.Mul(planRewards, big.NewInt(int64(coefficient)))
	planRewards.Div(planRewards, big.NewInt(int64(base)))
	//this addr should get
	planRewards.Mul(planRewards, big.NewInt(int64(proportion)))
	planRewards.Div(planRewards, big.NewInt(int64(base)))

	blockReward := big.NewInt(0)
	blockReward = planRewards.Div(planRewards, big.NewInt(int64(config.Epoch)))

	reward := blockReward
	state.AddBalance(header.Coinbase, reward)
	state.AddBalance(common.BytesToAddress([]byte(CoinActualRewardsAddress)), reward)
	return nil
}

// AccumulateStorageRewards credits the reward to the sentinel owner
func accumulateStorageRewards(config *params.GenaroConfig, state *state.StateDB, blockNumber uint64) error {
	preStorageRewards := state.GetBalance(common.BytesToAddress([]byte(Pre + StorageActualRewardsAddress)))
	preSurplusRewards := big.NewInt(0)
	//when now is the start of year, preSurplusRewards should get "Pre + SurplusCoinAddress"
	if blockNumber%(config.Epoch*epochPerYear) == 0 {
		preSurplusRewards = state.GetBalance(common.BytesToAddress([]byte(Pre + SurplusCoinAddress)))
	}else{
		preSurplusRewards = state.GetBalance(common.BytesToAddress([]byte(SurplusCoinAddress)))
	}
	coefficient := getStorageCoefficient(config, preStorageRewards, preSurplusRewards)

	surplusRewards := state.GetBalance(common.BytesToAddress([]byte(SurplusCoinAddress)))
	//plan rewards per year
	planRewards := surplusRewards.Mul(surplusRewards, big.NewInt(int64(storageRewardsRatio)))
	planRewards.Div(planRewards, big.NewInt(int64(base)))
	//plan rewards per epoch
	planRewards.Div(planRewards, big.NewInt(int64(epochPerYear)))
	//Coefficient adjustment
	planRewards.Mul(planRewards, big.NewInt(int64(coefficient)))
	planRewards.Div(planRewards, big.NewInt(int64(base)))
	//plan rewards per block
	blockReward := big.NewInt(0)
	blockReward = planRewards.Div(planRewards, big.NewInt(int64(config.Epoch)))

	//allocate blockReward
	cs := state.GetCandidates()
	total := uint64(0)
	contributes := make([]uint64, len(cs))
	for i, c := range cs{
		contributes[i] = state.GetHeftLastDiff(c, blockNumber)
		total += contributes[i]
	}
	if total == 0 {
		return nil
	}

	for i, c := range cs{
		reward := big.NewInt(0)
		reward.Mul(blockReward, big.NewInt(int64(contributes[i])))
		reward.Div(blockReward, big.NewInt(int64(total)))
		state.AddBalance(c, reward)
		state.AddBalance(common.BytesToAddress([]byte(StorageActualRewardsAddress)), reward)
	}
	return nil
}

// VerifyHeader checks whether a header conforms to the consensus rules of a
// given engine. Verifying the seal may be done optionally here, or explicitly
// via the VerifySeal method.
func (g *Genaro) VerifyHeader(chain consensus.ChainReader, header *types.Header, seal bool) error {
	log.Info("VerifyHeader:" + header.Number.String())
	return g.VerifySeal(chain, header)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications (the order is that of
// the input slice).
func (g *Genaro) VerifyHeaders(chain consensus.ChainReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	log.Info("VerifyHeaders")
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for _, header := range headers {
			err := g.VerifySeal(chain, header)

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

// APIs implements consensus.Engine, returning the user facing RPC API
func (g *Genaro) APIs(chain consensus.ChainReader) []rpc.API {
	return []rpc.API{{
		Namespace: "genaro",
		Version:   "1.0",
		Service:   &API{chain: chain, genaro: g},
		Public:    false,
	}}
}
