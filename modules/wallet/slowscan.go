package wallet

import (
	"github.com/HyperspaceApp/Hyperspace/build"
	"github.com/HyperspaceApp/Hyperspace/modules"
	"github.com/HyperspaceApp/Hyperspace/persist"
	"github.com/HyperspaceApp/Hyperspace/types"

	siasync "github.com/HyperspaceApp/Hyperspace/sync"
)

const scanMultiplier = 4 // how many more keys to generate after each scan iteration

// This is legacy code from the bad old days of terrible seed scanning and improper
// management of pubkey generation. It will be useful for grabbing addresses made by
// wallets not behaving in accordance with the addressGapLimit specified by BIP 44.

// numInitialKeys is the number of keys generated by the seedScanner before
// scanning the blockchain for the first time.
var numInitialKeys = func() uint64 {
	switch build.Release {
	case "dev":
		return 10e3
	case "standard":
		return 10e6
	case "testing":
		return 10e3
	default:
		panic("unrecognized build.Release")
	}
}()

// A slowSeedScanner scans the blockchain for addresses that belong to a given
// seed. This is for legacy scanning.
type slowSeedScanner struct {
	dustThreshold        types.Currency              // minimum value of outputs to be included
	keys                 map[types.UnlockHash]uint64 // map address to seed index
	keysArray            [][]byte
	maximumExternalIndex uint64
	seed                 modules.Seed
	addressGapLimit      uint64
	siacoinOutputs       map[types.SiacoinOutputID]scannedOutput
	cs                   modules.ConsensusSet
	gapScanner           *seedScanner
	lastConsensusChange  modules.ConsensusChangeID
	cancel               chan struct{}

	log *persist.Logger
}

func (s slowSeedScanner) getMaximumExternalIndex() uint64 {
	return s.maximumExternalIndex
}

func (s slowSeedScanner) getMaximumInternalIndex() uint64 {
	return s.gapScanner.maximumInternalIndex
}

func (s *slowSeedScanner) setDustThreshold(d types.Currency) {
	s.dustThreshold = d
	s.gapScanner.dustThreshold = d
}

func (s slowSeedScanner) getSiacoinOutputs() map[types.SiacoinOutputID]scannedOutput {
	return s.siacoinOutputs
}

func (s slowSeedScanner) numKeys() uint64 {
	return uint64(len(s.keys))
}

// generateKeys generates n additional keys from the slowSeedScanner's seed.
func (s *slowSeedScanner) generateKeys(n uint64) {
	initialProgress := s.numKeys()
	for i, k := range generateKeys(s.seed, initialProgress, n) {
		s.keys[k.UnlockConditions.UnlockHash()] = initialProgress + uint64(i)
		u := k.UnlockConditions.UnlockHash()
		s.keysArray = append(s.keysArray, u[:])
	}
}

func isAirdrop(h types.BlockHeight) bool {
	return h <= 7
}

func (s *slowSeedScanner) adjustMinimumIndex(siacoinOutputDiffs []modules.SiacoinOutputDiff) {
	for _, diff := range siacoinOutputDiffs {
		index, exists := s.keys[diff.SiacoinOutput.UnlockHash]
		if exists {
			s.log.Debugln("Seed scanner adjustMinimumIndex at index", index)
			if index > s.maximumExternalIndex {
				s.maximumExternalIndex = index
			}
		}
	}
}

// ProcessHeaderConsensusChange match consensus change headers with generated seeds
// It needs to look for two types new outputs:
//
// 1) Delayed outputs that have matured during this block. These outputs come
// attached to the HeaderConsensusChange via the output diff.
//
// 2) Fresh outputs that were created and activated during this block. If the
// current block contains these outputs, the header filter will match the wallet's
// keys.
//
// In a full node, we read the block directly from the consensus db and grab the
// outputs from the block output diff.
func (s *slowSeedScanner) ProcessHeaderConsensusChange(hcc modules.HeaderConsensusChange,
	getSiacoinOutputDiff func(types.BlockID, modules.DiffDirection) ([]modules.SiacoinOutputDiff, error)) {
	var siacoinOutputDiffs []modules.SiacoinOutputDiff

	// grab matured outputs
	siacoinOutputDiffs = append(siacoinOutputDiffs, hcc.MaturedSiacoinOutputDiffs...)

	// grab applied active outputs from full blocks
	for _, pbh := range hcc.AppliedBlockHeaders {
		if !isAirdrop(pbh.Height) {
			close(s.cancel)
			return
		}
		blockID := pbh.BlockHeader.ID()
		if pbh.GCSFilter.MatchUnlockHash(blockID[:], s.keysArray) {
			// log.Printf("apply block: %d", pbh.Height)
			// read the block, process the output
			blockSiacoinOutputDiffs, err := getSiacoinOutputDiff(blockID, modules.DiffApply)
			if err != nil {
				panic(err)
			}
			s.adjustMinimumIndex(blockSiacoinOutputDiffs)
			siacoinOutputDiffs = append(siacoinOutputDiffs, blockSiacoinOutputDiffs...)
		}
	}

	// grab reverted active outputs from full blocks
	for _, pbh := range hcc.RevertedBlockHeaders {
		blockID := pbh.BlockHeader.ID()
		if pbh.GCSFilter.MatchUnlockHash(blockID[:], s.keysArray) {
			// log.Printf("revert block: %d", pbh.Height)
			blockSiacoinOutputDiffs, err := getSiacoinOutputDiff(blockID, modules.DiffRevert)
			if err != nil {
				panic(err)
			}
			s.adjustMinimumIndex(blockSiacoinOutputDiffs)
			siacoinOutputDiffs = append(siacoinOutputDiffs, blockSiacoinOutputDiffs...)
		}
	}

	// apply the aggregated output diffs
	for _, diff := range siacoinOutputDiffs {
		if diff.Direction == modules.DiffApply {
			if index, exists := s.keys[diff.SiacoinOutput.UnlockHash]; exists && diff.SiacoinOutput.Value.Cmp(s.dustThreshold) > 0 {
				// log.Printf("slow DiffApply %d: %s\n", index, diff.SiacoinOutput.Value.String())
				s.siacoinOutputs[diff.ID] = scannedOutput{
					id:        types.OutputID(diff.ID),
					value:     diff.SiacoinOutput.Value,
					seedIndex: index,
				}
			}
		} else if diff.Direction == modules.DiffRevert {
			// NOTE: DiffRevert means the output was either spent or was in a
			// block that was reverted.
			if _, exists := s.keys[diff.SiacoinOutput.UnlockHash]; exists {
				// log.Printf("slow DiffRevert %d: %s\n", index, diff.SiacoinOutput.Value.String())
				delete(s.siacoinOutputs, diff.ID)
			}
		}
	}

	s.lastConsensusChange = hcc.ID
}

// scan subscribes s to cs and scans the blockchain for addresses that belong
// to s's seed. If scan returns errMaxKeys, additional keys may need to be
// generated to find all the addresses.
func (s *slowSeedScanner) scan(cancel <-chan struct{}) error {
	// generate a bunch of keys and scan the blockchain looking for them. If
	// none of the 'upper' half of the generated keys are found, we are done;
	// otherwise, generate more keys and try again (bounded by a sane
	// default).
	//
	// NOTE: since scanning is very slow, we aim to only scan once, which
	// means generating many keys.
	s.gapScanner = newFastSeedScanner(s.seed, s.addressGapLimit, s.cs, s.log)

	s.generateKeys(numInitialKeys)
	s.cancel = make(chan struct{}) // this will disturbe thread stop to stop scan
	err := s.cs.HeaderConsensusSetSubscribe(s, modules.ConsensusChangeBeginning, s.cancel)
	if err != siasync.ErrStopped {
		return err
	}
	s.cs.HeaderUnsubscribe(s)

	// log.Printf("end fist part slow scan s.maximumExternalIndex %d\n", s.maximumExternalIndex)
	s.gapScanner.minimumIndex = s.maximumExternalIndex
	s.gapScanner.maximumInternalIndex = s.maximumExternalIndex
	s.gapScanner.maximumExternalIndex = s.maximumExternalIndex
	s.gapScanner.siacoinOutputs = s.siacoinOutputs
	s.gapScanner.generateKeys(uint64(s.addressGapLimit))

	if err := s.gapScanner.cs.HeaderConsensusSetSubscribe(s.gapScanner, s.lastConsensusChange, cancel); err != nil {
		return err
	}
	s.gapScanner.cs.HeaderUnsubscribe(s.gapScanner)

	s.maximumExternalIndex = s.gapScanner.maximumExternalIndex
	// log.Printf("slow scan s.maximumExternalIndex %d\n", s.maximumExternalIndex)
	// for id, sco := range s.gapScanner.siacoinOutputs {
	// 	log.Printf("siacoinOutputs: %d %s", sco.seedIndex, sco.value.String())
	// 	s.siacoinOutputs[id] = sco
	// }

	return nil
}

// newSlowSeedScanner returns a new slowSeedScanner.
func newSlowSeedScanner(seed modules.Seed, addressGapLimit uint64,
	cs modules.ConsensusSet, log *persist.Logger) *slowSeedScanner {
	return &slowSeedScanner{
		seed:                 seed,
		addressGapLimit:      addressGapLimit,
		maximumExternalIndex: 0,
		keys:                 make(map[types.UnlockHash]uint64, numInitialKeys),
		siacoinOutputs:       make(map[types.SiacoinOutputID]scannedOutput),
		cs:                   cs,
		log:                  log,
	}
}
