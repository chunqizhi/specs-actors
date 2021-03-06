// nolint:unused // 20200716 until tests are restored from miner state refactor
package miner_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	addr "github.com/filecoin-project/go-address"
	bitfield "github.com/filecoin-project/go-bitfield"
	cid "github.com/ipfs/go-cid"
	"github.com/minio/blake2b-simd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/abi/big"
	"github.com/filecoin-project/specs-actors/actors/builtin"
	"github.com/filecoin-project/specs-actors/actors/builtin/market"
	"github.com/filecoin-project/specs-actors/actors/builtin/miner"
	"github.com/filecoin-project/specs-actors/actors/builtin/power"
	"github.com/filecoin-project/specs-actors/actors/builtin/reward"
	"github.com/filecoin-project/specs-actors/actors/crypto"
	"github.com/filecoin-project/specs-actors/actors/runtime"
	"github.com/filecoin-project/specs-actors/actors/runtime/exitcode"
	"github.com/filecoin-project/specs-actors/actors/util/adt"
	"github.com/filecoin-project/specs-actors/support/mock"
	tutil "github.com/filecoin-project/specs-actors/support/testing"
)

var testPid abi.PeerID
var testMultiaddrs []abi.Multiaddrs

// A balance for use in tests where the miner's low balance is not interesting.
var bigBalance = big.Mul(big.NewInt(10000), big.NewInt(1e18))

func init() {
	testPid = abi.PeerID("peerID")

	testMultiaddrs = []abi.Multiaddrs{
		{1},
		{2},
	}

	// permit 2KiB sectors in tests
	miner.SupportedProofTypes[abi.RegisteredSealProof_StackedDrg2KiBV1] = struct{}{}
}

func TestExports(t *testing.T) {
	mock.CheckActorExports(t, miner.Actor{})
}

func TestConstruction(t *testing.T) {
	actor := miner.Actor{}
	owner := tutil.NewIDAddr(t, 100)
	worker := tutil.NewIDAddr(t, 101)
	workerKey := tutil.NewBLSAddr(t, 0)
	receiver := tutil.NewIDAddr(t, 1000)
	builder := mock.NewBuilder(context.Background(), receiver).
		WithActorType(owner, builtin.AccountActorCodeID).
		WithActorType(worker, builtin.AccountActorCodeID).
		WithHasher(blake2b.Sum256).
		WithCaller(builtin.InitActorAddr, builtin.InitActorCodeID)

	t.Run("simple construction", func(t *testing.T) {
		rt := builder.Build(t)
		params := miner.ConstructorParams{
			OwnerAddr:     owner,
			WorkerAddr:    worker,
			SealProofType: abi.RegisteredSealProof_StackedDrg32GiBV1,
			PeerId:        testPid,
			Multiaddrs:    testMultiaddrs,
		}

		provingPeriodStart := abi.ChainEpoch(2386) // This is just set from running the code.
		rt.ExpectValidateCallerAddr(builtin.InitActorAddr)
		// Fetch worker pubkey.
		rt.ExpectSend(worker, builtin.MethodsAccount.PubkeyAddress, nil, big.Zero(), &workerKey, exitcode.Ok)
		// Register proving period cron.
		rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.EnrollCronEvent,
			makeDeadlineCronEventParams(t, provingPeriodStart-1), big.Zero(), nil, exitcode.Ok)
		ret := rt.Call(actor.Constructor, &params)

		assert.Nil(t, ret)
		rt.Verify()

		var st miner.State
		rt.GetState(&st)
		info, err := st.GetInfo(adt.AsStore(rt))
		require.NoError(t, err)
		assert.Equal(t, params.OwnerAddr, info.Owner)
		assert.Equal(t, params.WorkerAddr, info.Worker)
		assert.Equal(t, params.PeerId, info.PeerId)
		assert.Equal(t, params.Multiaddrs, info.Multiaddrs)
		assert.Equal(t, abi.RegisteredSealProof_StackedDrg32GiBV1, info.SealProofType)
		assert.Equal(t, abi.SectorSize(1<<35), info.SectorSize)
		assert.Equal(t, uint64(2349), info.WindowPoStPartitionSectors)

		assert.Equal(t, big.Zero(), st.PreCommitDeposits)
		assert.Equal(t, big.Zero(), st.LockedFunds)
		assert.True(t, st.VestingFunds.Defined())
		assert.True(t, st.PreCommittedSectors.Defined())

		assert.True(t, st.Sectors.Defined())
		assert.Equal(t, provingPeriodStart, st.ProvingPeriodStart)
		assert.Equal(t, uint64(0), st.CurrentDeadline)

		var deadlines miner.Deadlines
		rt.Get(st.Deadlines, &deadlines)
		for i := uint64(0); i < miner.WPoStPeriodDeadlines; i++ {
			var deadline miner.Deadline
			rt.Get(deadlines.Due[i], &deadline)
			assert.True(t, deadline.Partitions.Defined())
			assert.True(t, deadline.ExpirationsEpochs.Defined())
			assertEmptyBitfield(t, deadline.PostSubmissions)
			assertEmptyBitfield(t, deadline.EarlyTerminations)
			assert.Equal(t, uint64(0), deadline.LiveSectors)
		}

		assertEmptyBitfield(t, st.EarlyTerminations)
		assert.Equal(t, miner.NewPowerPairZero(), st.FaultyPower)
	})
}

// Tests for fetching and manipulating miner addresses.
func TestControlAddresses(t *testing.T) {
	actor := newHarness(t, 0)
	builder := builderForHarness(actor)

	t.Run("get addresses", func(t *testing.T) {
		rt := builder.Build(t)
		actor.constructAndVerify(rt)

		o, w := actor.controlAddresses(rt)
		assert.Equal(t, actor.owner, o)
		assert.Equal(t, actor.worker, w)
	})

	// TODO: test changing worker (with delay), changing peer id
	// https://github.com/filecoin-project/specs-actors/issues/479
}

// Test for sector precommitment and proving.
func TestCommitments(t *testing.T) {
	periodOffset := abi.ChainEpoch(100)

	// TODO more tests
	// - Concurrent attempts to upgrade the same CC sector (one should succeed)
	// - Insufficient funds for pre-commit, for prove-commit
	// - CC sector targeted for upgrade expires naturally before the upgrade is proven

	t.Run("valid precommit then provecommit", func(t *testing.T) {
		actor := newHarness(t, periodOffset)
		rt := builderForHarness(actor).
			WithBalance(bigBalance, big.Zero()).
			Build(t)
		precommitEpoch := periodOffset + 1
		rt.SetEpoch(precommitEpoch)
		actor.constructAndVerify(rt)
		dlInfo := actor.deadline(rt)

		// Make a good commitment for the proof to target.
		// Use the max sector number to make sure everything works.
		sectorNo := abi.SectorNumber(abi.MaxSectorNumber)
		expiration := dlInfo.PeriodEnd() + 181*miner.WPoStProvingPeriod // something on deadline boundary but > 180 days
		precommit := actor.makePreCommit(sectorNo, precommitEpoch-1, expiration, nil)
		actor.preCommitSector(rt, precommit)

		// assert precommit exists and meets expectations
		onChainPrecommit := actor.getPreCommit(rt, sectorNo)

		// expect precommit deposit to be initial pledge calculated at precommit time
		sectorSize, err := precommit.SealProof.SectorSize()
		require.NoError(t, err)

		// deal weights mocked by actor harness for market actor must be set in precommit onchain info
		assert.Equal(t, big.NewInt(int64(sectorSize/2)), onChainPrecommit.DealWeight)
		assert.Equal(t, big.NewInt(int64(sectorSize/2)), onChainPrecommit.VerifiedDealWeight)

		qaPower := miner.QAPowerForWeight(sectorSize, precommit.Expiration-precommitEpoch, onChainPrecommit.DealWeight, onChainPrecommit.VerifiedDealWeight)
		expectedDeposit := miner.InitialPledgeForPower(qaPower, actor.networkQAPower, actor.baselinePower, actor.networkPledge, actor.epochReward, rt.TotalFilCircSupply())
		assert.Equal(t, expectedDeposit, onChainPrecommit.PreCommitDeposit)

		// expect total precommit deposit to equal our new deposit
		st := getState(rt)
		assert.Equal(t, expectedDeposit, st.PreCommitDeposits)

		// run prove commit logic
		rt.SetEpoch(precommitEpoch + miner.PreCommitChallengeDelay + 1)
		rt.SetBalance(big.Mul(big.NewInt(1000), big.NewInt(1e18)))
		actor.proveCommitSectorAndConfirm(rt, precommit, precommitEpoch, makeProveCommit(sectorNo), proveCommitConf{})

		// expect precommit to have been removed
		st = getState(rt)
		_, found, err := st.GetPrecommittedSector(rt.AdtStore(), sectorNo)
		require.NoError(t, err)
		require.False(t, found)

		// expect deposit to have been transferred to initial pledges
		assert.Equal(t, big.Zero(), st.PreCommitDeposits)

		qaPower = miner.QAPowerForWeight(sectorSize, precommit.Expiration-rt.Epoch(), onChainPrecommit.DealWeight,
			onChainPrecommit.VerifiedDealWeight)
		expectedInitialPledge := miner.InitialPledgeForPower(qaPower, actor.networkQAPower, actor.baselinePower,
			actor.networkPledge, actor.epochReward, rt.TotalFilCircSupply())
		assert.Equal(t, expectedInitialPledge, st.InitialPledgeRequirement)

		// expect new onchain sector
		sector := actor.getSector(rt, sectorNo)
		sectorPower := miner.PowerForSector(sectorSize, sector)

		// expect deal weights to be transfered to on chain info
		assert.Equal(t, onChainPrecommit.DealWeight, sector.DealWeight)
		assert.Equal(t, onChainPrecommit.VerifiedDealWeight, sector.VerifiedDealWeight)

		// expect activation epoch to be current epoch
		assert.Equal(t, rt.Epoch(), sector.Activation)

		// expect initial plege of sector to be set
		assert.Equal(t, expectedInitialPledge, sector.InitialPledge)

		// expect locked initial pledge of sector to be the same as pledge requirement
		assert.Equal(t, expectedInitialPledge, st.InitialPledgeRequirement)

		// expect sector to be assigned a deadline/partition
		dlIdx, pIdx, err := st.FindSector(rt.AdtStore(), sectorNo)
		require.NoError(t, err)
		deadline, partition := actor.getDeadlineAndPartition(rt, dlIdx, pIdx)
		assert.Equal(t, uint64(1), deadline.LiveSectors)
		assertEmptyBitfield(t, deadline.PostSubmissions)
		assertEmptyBitfield(t, deadline.EarlyTerminations)

		dQueue := actor.collectDeadlineExpirations(rt, deadline)
		assert.Equal(t, map[abi.ChainEpoch][]uint64{
			precommit.Expiration: {pIdx},
		}, dQueue)

		assertBitfieldEquals(t, partition.Sectors, uint64(sectorNo))
		assertEmptyBitfield(t, partition.Faults)
		assertEmptyBitfield(t, partition.Recoveries)
		assertEmptyBitfield(t, partition.Terminated)
		assert.Equal(t, sectorPower, partition.LivePower)
		assert.Equal(t, miner.NewPowerPairZero(), partition.FaultyPower)
		assert.Equal(t, miner.NewPowerPairZero(), partition.RecoveringPower)

		pQueue := actor.collectPartitionExpirations(rt, partition)
		entry, ok := pQueue[precommit.Expiration]
		require.True(t, ok)
		assertBitfieldEquals(t, entry.OnTimeSectors, uint64(sectorNo))
		assertEmptyBitfield(t, entry.EarlySectors)
		assert.Equal(t, expectedInitialPledge, entry.OnTimePledge)
		assert.Equal(t, sectorPower, entry.ActivePower)
		assert.Equal(t, miner.NewPowerPairZero(), entry.FaultyPower)
	})

	t.Run("invalid pre-commit rejected", func(t *testing.T) {
		actor := newHarness(t, periodOffset)
		rt := builderForHarness(actor).
			WithBalance(bigBalance, big.Zero()).
			Build(t)
		precommitEpoch := periodOffset + 1
		rt.SetEpoch(precommitEpoch)
		actor.constructAndVerify(rt)
		deadline := actor.deadline(rt)
		challengeEpoch := precommitEpoch - 1

		oldSector := actor.commitAndProveSectors(rt, 1, 181, nil)[0]

		// Good commitment.
		expiration := deadline.PeriodEnd() + 181*miner.WPoStProvingPeriod
		actor.preCommitSector(rt, actor.makePreCommit(101, challengeEpoch, expiration, nil))

		// Duplicate pre-commit sector ID
		rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
			actor.preCommitSector(rt, actor.makePreCommit(101, challengeEpoch, expiration, nil))
		})
		rt.Reset()

		// Sector ID already committed
		rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
			actor.preCommitSector(rt, actor.makePreCommit(oldSector.SectorNumber, challengeEpoch, expiration, nil))
		})
		rt.Reset()

		// Bad sealed CID
		rt.ExpectAbortConstainsMessage(exitcode.ErrIllegalArgument, "sealed CID had wrong prefix", func() {
			pc := actor.makePreCommit(102, challengeEpoch, deadline.PeriodEnd(), nil)
			pc.SealedCID = tutil.MakeCID("Random Data", nil)
			actor.preCommitSector(rt, pc)
		})
		rt.Reset()

		// Bad seal proof type
		rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
			pc := actor.makePreCommit(102, challengeEpoch, deadline.PeriodEnd(), nil)
			pc.SealProof = abi.RegisteredSealProof_StackedDrg8MiBV1
			actor.preCommitSector(rt, pc)
		})
		rt.Reset()

		// Expires at current epoch
		rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
			actor.preCommitSector(rt, actor.makePreCommit(102, challengeEpoch, rt.Epoch(), nil))
		})
		rt.Reset()

		// Expires before current epoch
		rt.SetEpoch(expiration + 1)
		rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
			actor.preCommitSector(rt, actor.makePreCommit(102, challengeEpoch, expiration, nil))
		})
		rt.Reset()

		// Expires not on period end
		rt.SetEpoch(precommitEpoch)
		rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
			actor.preCommitSector(rt, actor.makePreCommit(102, challengeEpoch, deadline.PeriodEnd()-1, nil))
		})
		rt.Reset()

		// Expires too early
		rt.ExpectAbortConstainsMessage(exitcode.ErrIllegalArgument, "must exceed", func() {
			actor.preCommitSector(rt, actor.makePreCommit(102, challengeEpoch, expiration-20*builtin.EpochsInDay, nil))
		})
		rt.Reset()

		// Errors when expiry too far in the future
		rt.SetEpoch(precommitEpoch)
		expiration = deadline.PeriodEnd() + miner.WPoStProvingPeriod*(miner.MaxSectorExpirationExtension/miner.WPoStProvingPeriod+1)
		rt.ExpectAbortConstainsMessage(exitcode.ErrIllegalArgument, "invalid expiration", func() {
			actor.preCommitSector(rt, actor.makePreCommit(102, challengeEpoch, deadline.PeriodEnd()-1, nil))
		})

		// Sector ID out of range
		rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
			actor.preCommitSector(rt, actor.makePreCommit(abi.MaxSectorNumber+1, challengeEpoch, expiration, nil))
		})
		rt.Reset()
	})

	t.Run("valid committed capacity upgrade", func(t *testing.T) {
		actor := newHarness(t, periodOffset)
		rt := builderForHarness(actor).
			WithBalance(bigBalance, big.Zero()).
			Build(t)
		actor.constructAndVerify(rt)

		// Move the current epoch forward so that the first deadline is a stable candidate for both sectors
		rt.SetEpoch(periodOffset + miner.WPoStChallengeWindow)

		// Commit a sector to upgrade
		// Use the max sector number to make sure everything works.
		oldSector := actor.commitAndProveSector(rt, abi.MaxSectorNumber, 181, nil)
		st := getState(rt)
		dlIdx, partIdx, err := st.FindSector(rt.AdtStore(), oldSector.SectorNumber)
		require.NoError(t, err)

		// Reduce the epoch reward so that a new sector's initial pledge would otherwise be lesser.
		actor.epochReward = big.Div(actor.epochReward, big.NewInt(2))

		challengeEpoch := rt.Epoch() - 1
		upgradeParams := actor.makePreCommit(200, challengeEpoch, oldSector.Expiration, []abi.DealID{1})
		upgradeParams.ReplaceCapacity = true
		upgradeParams.ReplaceSectorDeadline = dlIdx
		upgradeParams.ReplaceSectorPartition = partIdx
		upgradeParams.ReplaceSectorNumber = oldSector.SectorNumber
		upgrade := actor.preCommitSector(rt, upgradeParams)

		// Check new pre-commit in state
		assert.True(t, upgrade.Info.ReplaceCapacity)
		assert.Equal(t, upgradeParams.ReplaceSectorNumber, upgrade.Info.ReplaceSectorNumber)
		// Require new sector's pledge to be at least that of the old sector.
		assert.Equal(t, oldSector.InitialPledge, upgrade.PreCommitDeposit)

		// Old sector is unchanged
		oldSectorAgain := actor.getSector(rt, oldSector.SectorNumber)
		assert.Equal(t, oldSector, oldSectorAgain)

		// Deposit and pledge as expected
		st = getState(rt)
		assert.Equal(t, st.PreCommitDeposits, upgrade.PreCommitDeposit)
		assert.Equal(t, st.InitialPledgeRequirement, oldSector.InitialPledge)

		// Prove new sector
		rt.SetEpoch(upgrade.PreCommitEpoch + miner.PreCommitChallengeDelay + 1)
		newSector := actor.proveCommitSectorAndConfirm(rt, &upgrade.Info, upgrade.PreCommitEpoch,
			makeProveCommit(upgrade.Info.SectorNumber), proveCommitConf{})

		// Both sectors have pledge
		st = getState(rt)
		assert.Equal(t, big.Zero(), st.PreCommitDeposits)
		assert.Equal(t, st.InitialPledgeRequirement, big.Add(oldSector.InitialPledge, newSector.InitialPledge))

		// Both sectors are present (in the same deadline/partition).
		deadline, partition := actor.getDeadlineAndPartition(rt, dlIdx, partIdx)
		assert.Equal(t, uint64(2), deadline.TotalSectors)
		assert.Equal(t, uint64(2), deadline.LiveSectors)
		assertEmptyBitfield(t, deadline.EarlyTerminations)

		assertBitfieldEquals(t, partition.Sectors, uint64(newSector.SectorNumber), uint64(oldSector.SectorNumber))
		assertEmptyBitfield(t, partition.Faults)
		assertEmptyBitfield(t, partition.Recoveries)
		assertEmptyBitfield(t, partition.Terminated)

		// The old sector's expiration has changed to the end of this proving deadline.
		// The new one expires when the old one used to.
		// The partition is registered with an expiry at both epochs.
		dQueue := actor.collectDeadlineExpirations(rt, deadline)
		dlInfo := miner.NewDeadlineInfo(st.ProvingPeriodStart, dlIdx, rt.Epoch())
		assert.Equal(t, map[abi.ChainEpoch][]uint64{
			dlInfo.NextNotElapsed().Last(): {uint64(0)},
			oldSector.Expiration:           {uint64(0)},
		}, dQueue)

		pQueue := actor.collectPartitionExpirations(rt, partition)
		assertBitfieldEquals(t, pQueue[dlInfo.NextNotElapsed().Last()].OnTimeSectors, uint64(oldSector.SectorNumber))
		assertBitfieldEquals(t, pQueue[oldSector.Expiration].OnTimeSectors, uint64(newSector.SectorNumber))

		// Roll forward to the beginning of the next iteration of this deadline
		advanceToEpochWithCron(rt, actor, dlInfo.NextNotElapsed().Open)

		// Fail to submit PoSt. This means that both sectors will be detected faulty.
		// Expect the old sector to be marked as terminated.
		bothSectors := []*miner.SectorOnChainInfo{oldSector, newSector}
		lostPower := actor.powerPairForSectors(bothSectors).Neg()
		faultPenalty := actor.undeclaredFaultPenalty(bothSectors)

		actor.addLockedFund(rt, big.Mul(big.NewInt(5), faultPenalty))

		advanceDeadline(rt, actor, &cronConfig{
			detectedFaultsPowerDelta:  &lostPower,
			detectedFaultsPenalty:     faultPenalty,
			expiredSectorsPledgeDelta: oldSector.InitialPledge.Neg(),
		})

		// The old sector is marked as terminated
		st = getState(rt)
		deadline, partition = actor.getDeadlineAndPartition(rt, dlIdx, partIdx)
		assert.Equal(t, uint64(2), deadline.TotalSectors)
		assert.Equal(t, uint64(1), deadline.LiveSectors)
		assertBitfieldEquals(t, partition.Sectors, uint64(newSector.SectorNumber), uint64(oldSector.SectorNumber))
		assertBitfieldEquals(t, partition.Terminated, uint64(oldSector.SectorNumber))
		assertBitfieldEquals(t, partition.Faults, uint64(newSector.SectorNumber))
		newSectorPower := miner.PowerForSector(actor.sectorSize, newSector)
		assert.True(t, newSectorPower.Equals(partition.LivePower))
		assert.True(t, newSectorPower.Equals(partition.FaultyPower))

		dQueue = actor.collectDeadlineExpirations(rt, deadline)
		assert.Equal(t, map[abi.ChainEpoch][]uint64{
			newSector.Expiration: {uint64(0)},
		}, dQueue)

		// Old sector gone from pledge requirement and deposit
		assert.Equal(t, st.InitialPledgeRequirement, newSector.InitialPledge)
		assert.Equal(t, st.LockedFunds, big.Mul(big.NewInt(4), faultPenalty)) // from manual fund addition above - 1 fault penalty
	})

	t.Run("invalid committed capacity upgrade rejected", func(t *testing.T) {
		//t.Skip("Disabled in miner state refactor #648, restore soon")
		actor := newHarness(t, periodOffset)
		rt := builderForHarness(actor).
			WithBalance(bigBalance, big.Zero()).
			Build(t)
		actor.constructAndVerify(rt)

		// Commit sectors to target upgrade. The first has no deals, the second has a deal.
		oldSectors := actor.commitAndProveSectors(rt, 2, 181, [][]abi.DealID{nil, {10}})

		st := getState(rt)
		dlIdx, partIdx, err := st.FindSector(rt.AdtStore(), oldSectors[0].SectorNumber)
		require.NoError(t, err)

		challengeEpoch := rt.Epoch() - 1
		upgradeParams := actor.makePreCommit(200, challengeEpoch, oldSectors[0].Expiration, []abi.DealID{20})
		upgradeParams.ReplaceCapacity = true
		upgradeParams.ReplaceSectorDeadline = dlIdx
		upgradeParams.ReplaceSectorPartition = partIdx
		upgradeParams.ReplaceSectorNumber = oldSectors[0].SectorNumber

		{ // Must have deals
			params := *upgradeParams
			params.DealIDs = nil
			rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
				actor.preCommitSector(rt, &params)
			})
			rt.Reset()
		}
		{ // Old sector cannot have deals
			params := *upgradeParams
			params.ReplaceSectorNumber = oldSectors[1].SectorNumber
			rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
				actor.preCommitSector(rt, &params)
			})
			rt.Reset()
		}
		{ // Target sector must exist
			params := *upgradeParams
			params.ReplaceSectorNumber = 999
			rt.ExpectAbort(exitcode.ErrNotFound, func() {
				actor.preCommitSector(rt, &params)
			})
			rt.Reset()
		}
		{ // Expiration must not be sooner than target
			params := *upgradeParams
			params.Expiration = params.Expiration - miner.WPoStProvingPeriod
			rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
				actor.preCommitSector(rt, &params)
			})
			rt.Reset()
		}
		{ // Target must not be faulty
			params := *upgradeParams
			st := getState(rt)
			prevState := *st
			deadlines, err := st.LoadDeadlines(rt.AdtStore())
			require.NoError(t, err)
			deadline, err := deadlines.LoadDeadline(rt.AdtStore(), dlIdx)
			require.NoError(t, err)
			partitions, err := deadline.PartitionsArray(rt.AdtStore())
			require.NoError(t, err)
			var partition miner.Partition
			found, err := partitions.Get(partIdx, &partition)
			require.True(t, found)
			require.NoError(t, err)
			_, err = partition.AddFaults(rt.AdtStore(), bf(uint64(oldSectors[0].SectorNumber)), oldSectors[0:1], 100000,
				actor.sectorSize, st.QuantEndOfDeadline())
			require.NoError(t, err)
			require.NoError(t, partitions.Set(partIdx, &partition))
			deadline.Partitions, err = partitions.Root()
			require.NoError(t, err)
			deadlines.Due[dlIdx] = rt.Put(deadline)
			require.NoError(t, st.SaveDeadlines(rt.AdtStore(), deadlines))
			// Phew!

			rt.ReplaceState(st)
			rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
				actor.preCommitSector(rt, &params)
			})
			rt.ReplaceState(&prevState)
			rt.Reset()
		}

		// Demonstrate that the params are otherwise ok
		actor.preCommitSector(rt, upgradeParams)
		rt.Verify()
	})

	t.Run("faulty committed capacity sector not replaced", func(t *testing.T) {
		t.Skip("Disabled in miner state refactor #648, restore soon")
		actor := newHarness(t, periodOffset)
		rt := builderForHarness(actor).
			WithBalance(bigBalance, big.Zero()).
			Build(t)
		actor.constructAndVerify(rt)

		// Commit a sector to target upgrade
		oldSector := actor.commitAndProveSectors(rt, 1, 100, nil)[0]

		// Complete proving period
		// June 2020: it is impossible to declare fault for a sector not yet assigned to a deadline
		completeProvingPeriod(rt, actor, &cronConfig{})

		// Pre-commit a sector to replace the existing one
		challengeEpoch := rt.Epoch() - 1
		upgradeParams := actor.makePreCommit(200, challengeEpoch, oldSector.Expiration, []abi.DealID{20})
		upgradeParams.ReplaceCapacity = true
		// TODO minerstate sector location
		upgradeParams.ReplaceSectorNumber = oldSector.SectorNumber

		upgrade := actor.preCommitSector(rt, upgradeParams)

		// Declare the old sector faulty
		_, qaPower := powerForSectors(actor.sectorSize, []*miner.SectorOnChainInfo{oldSector})
		fee := miner.PledgePenaltyForDeclaredFault(actor.epochReward, actor.networkQAPower, qaPower)
		actor.declareFaults(rt, fee, oldSector)

		rt.SetEpoch(upgrade.PreCommitEpoch + miner.PreCommitChallengeDelay + 1)
		// Proof is initially denied because the fault fee has reduced locked funds.
		rt.ExpectAbort(exitcode.ErrInsufficientFunds, func() {
			actor.proveCommitSectorAndConfirm(rt, &upgrade.Info, upgrade.PreCommitEpoch,
				makeProveCommit(upgrade.Info.SectorNumber), proveCommitConf{})
		})
		rt.Reset()

		// Prove the new sector
		actor.addLockedFund(rt, fee)
		newSector := actor.proveCommitSectorAndConfirm(rt, &upgrade.Info, upgrade.PreCommitEpoch,
			makeProveCommit(upgrade.Info.SectorNumber), proveCommitConf{})

		// The old sector's expiration has *not* changed
		oldSectorAgain := actor.getSector(rt, oldSector.SectorNumber)
		assert.Equal(t, oldSector.Expiration, oldSectorAgain.Expiration)

		// Roll forward to PP cron. The faulty old sector pays a fee, but is not terminated.
		penalty := miner.PledgePenaltyForDeclaredFault(actor.epochReward, actor.networkQAPower,
			miner.QAPowerForSector(actor.sectorSize, oldSector))
		completeProvingPeriod(rt, actor, &cronConfig{
			ongoingFaultsPenalty: penalty,
		})

		// Both sectors remain
		sectors := actor.collectSectors(rt)
		assert.Equal(t, 2, len(sectors))
		assert.Equal(t, oldSector, sectors[oldSector.SectorNumber])
		assert.Equal(t, newSector, sectors[newSector.SectorNumber])
		//expirations := actor.collectExpirations(rt)
		//assert.Equal(t, 1, len(expirations))
		//assert.Equal(t, []uint64{100, 200}, expirations[newSector.Expiration])
	})

	t.Run("invalid proof rejected", func(t *testing.T) {
		actor := newHarness(t, periodOffset)
		rt := builderForHarness(actor).
			WithBalance(bigBalance, big.Zero()).
			Build(t)
		precommitEpoch := periodOffset + 1
		rt.SetEpoch(precommitEpoch)
		actor.constructAndVerify(rt)
		deadline := actor.deadline(rt)

		// Make a good commitment for the proof to target.
		sectorNo := abi.SectorNumber(100)
		precommit := actor.makePreCommit(sectorNo, precommitEpoch-1, deadline.PeriodEnd()+181*miner.WPoStProvingPeriod, nil)
		actor.preCommitSector(rt, precommit)

		// Sector pre-commitment missing.
		rt.SetEpoch(precommitEpoch + miner.PreCommitChallengeDelay + 1)
		rt.ExpectAbort(exitcode.ErrNotFound, func() {
			actor.proveCommitSectorAndConfirm(rt, precommit, precommitEpoch, makeProveCommit(sectorNo+1), proveCommitConf{})
		})
		rt.Reset()

		// Too late.
		rt.SetEpoch(precommitEpoch + miner.MaxSealDuration[precommit.SealProof] + 1)
		rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
			actor.proveCommitSectorAndConfirm(rt, precommit, precommitEpoch, makeProveCommit(sectorNo), proveCommitConf{})
		})
		rt.Reset()

		// TODO: too early to prove sector
		// TODO: seal rand epoch too old
		// TODO: commitment expires before proof
		// https://github.com/filecoin-project/specs-actors/issues/479

		// Set the right epoch for all following tests
		rt.SetEpoch(precommitEpoch + miner.PreCommitChallengeDelay + 1)

		// Invalid deals (market ActivateDeals aborts)
		verifyDealsExit := make(map[abi.SectorNumber]exitcode.ExitCode)
		verifyDealsExit[precommit.SectorNumber] = exitcode.ErrIllegalArgument
		rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
			actor.proveCommitSectorAndConfirm(rt, precommit, precommitEpoch, makeProveCommit(sectorNo), proveCommitConf{
				verifyDealsExit: verifyDealsExit,
			})
		})
		rt.Reset()

		// Invalid seal proof
		/* TODO: how should this test work?
		// https://github.com/filecoin-project/specs-actors/issues/479
		rt.ExpectAbort(exitcode.ErrIllegalState, func() {
			actor.proveCommitSectorAndConfirm(rt, precommit, precommitEpoch, makeProveCommit(sectorNo), proveCommitConf{
				verifySealErr: fmt.Errorf("for testing"),
			})
		})
		rt.Reset()
		*/

		// Good proof
		rt.SetBalance(big.Mul(big.NewInt(1000), big.NewInt(1e18)))
		actor.proveCommitSectorAndConfirm(rt, precommit, precommitEpoch, makeProveCommit(sectorNo), proveCommitConf{})
		st := getState(rt)
		// Verify new sectors
		// TODO minerstate
		//newSectors, err := st.NewSectors.All(miner.SectorsMax)
		//require.NoError(t, err)
		//assert.Equal(t, []uint64{uint64(sectorNo)}, newSectors)
		// Verify pledge lock-up
		assert.True(t, st.InitialPledgeRequirement.GreaterThan(big.Zero()))
		rt.Reset()

		// Duplicate proof (sector no-longer pre-committed)
		rt.ExpectAbort(exitcode.ErrNotFound, func() {
			actor.proveCommitSectorAndConfirm(rt, precommit, precommitEpoch, makeProveCommit(sectorNo), proveCommitConf{})
		})
		rt.Reset()
	})

	t.Run("fails with too many deals", func(t *testing.T) {
		setup := func(proof abi.RegisteredSealProof) (*mock.Runtime, *actorHarness, *miner.DeadlineInfo) {
			actor := newHarness(t, periodOffset)
			actor.setProofType(proof)
			rt := builderForHarness(actor).
				WithBalance(bigBalance, big.Zero()).
				Build(t)
			rt.SetEpoch(periodOffset + 1)
			actor.constructAndVerify(rt)
			deadline := actor.deadline(rt)
			return rt, actor, deadline
		}

		makeDealIDs := func(n int) []abi.DealID {
			ids := make([]abi.DealID, n)
			for i := range ids {
				ids[i] = abi.DealID(i)
			}
			return ids
		}

		// Make a good commitment for the proof to target.
		sectorNo := abi.SectorNumber(100)

		dealLimits := map[abi.RegisteredSealProof]int{
			abi.RegisteredSealProof_StackedDrg2KiBV1:  256,
			abi.RegisteredSealProof_StackedDrg32GiBV1: 256,
			abi.RegisteredSealProof_StackedDrg64GiBV1: 512,
		}

		for proof, limit := range dealLimits {
			// attempt to pre-commmit a sector with too many sectors
			rt, actor, deadline := setup(proof)
			expiration := deadline.PeriodEnd() + 181*miner.WPoStProvingPeriod
			precommit := actor.makePreCommit(sectorNo, rt.Epoch()-1, expiration, makeDealIDs(limit+1))
			rt.ExpectAbortConstainsMessage(exitcode.ErrIllegalArgument, "too many deals for sector", func() {
				actor.preCommitSector(rt, precommit)
			})

			// sector at or below limit succeeds
			rt, actor, _ = setup(proof)
			precommit = actor.makePreCommit(sectorNo, rt.Epoch()-1, expiration, makeDealIDs(limit))
			actor.preCommitSector(rt, precommit)
		}

	})
}

func TestWindowPost(t *testing.T) {
	periodOffset := abi.ChainEpoch(100)
	actor := newHarness(t, periodOffset)
	actor.setProofType(abi.RegisteredSealProof_StackedDrg2KiBV1)
	precommitEpoch := abi.ChainEpoch(1)
	builder := builderForHarness(actor).
		WithEpoch(precommitEpoch).
		WithBalance(bigBalance, big.Zero())

	t.Run("test proof", func(t *testing.T) {
		rt := builder.Build(t)
		actor.constructAndVerify(rt)
		store := rt.AdtStore()
		sector := actor.commitAndProveSectors(rt, 1, 181, nil)[0]

		st := getState(rt)
		dlIdx, pIdx, err := st.FindSector(store, sector.SectorNumber)
		require.NoError(t, err)

		// Skip over deadlines until the beginning of the one with the new sector
		dlinfo := actor.deadline(rt)
		for dlinfo.Index != dlIdx {
			advanceDeadline(rt, actor, &cronConfig{})
			dlinfo = actor.deadline(rt)
		}

		// Submit PoSt
		partitions := []miner.PoStPartition{
			{Index: pIdx, Skipped: abi.NewBitField()},
		}
		actor.submitWindowPoSt(rt, dlinfo, partitions, []*miner.SectorOnChainInfo{sector}, nil)

		// Verify proof recorded
		deadline := actor.getDeadline(rt, dlIdx)
		empty, err := deadline.PostSubmissions.IsEmpty()
		require.NoError(t, err)
		assert.False(t, empty, "no post submission")

		// Advance to end-of-deadline cron to verify no penalties.
		advanceDeadline(rt, actor, &cronConfig{})
	})

	//runTillNextDeadline := func(rt *mock.Runtime) (*miner.DeadlineInfo, []*miner.SectorOnChainInfo, []uint64) {
	//	st := getState(rt)
	//	deadlines, err := st.LoadDeadlines(rt.AdtStore())
	//	require.NoError(t, err)
	//	deadline := actor.deadline(rt)
	//
	//	// advance to next deadline where we expect the first sectors to appear
	//	rt.SetEpoch(deadline.NextOpen())
	//	deadline = st.DeadlineInfo(rt.Epoch())
	//
	//	infos, partitions := actor.computePartitions(rt, deadlines, deadline.Index)
	//	return deadline, infos, partitions
	//}

	//runTillFirstDeadline := func(rt *mock.Runtime) (*miner.DeadlineInfo, []*miner.SectorOnChainInfo, []uint64) {
	//	actor.constructAndVerify(rt)
	//
	//	_ = actor.commitAndProveSectors(rt, 6, 100, nil)
	//
	//	// Skip to end of proving period, cron adds sectors to proving set.
	//	actor.advancePastProvingPeriodWithCron(rt)
	//
	//	return runTillNextDeadline(rt)
	//}

	t.Run("successful recoveries recover power", func(t *testing.T) {
		t.Skip("Disabled in miner state refactor #648, restore soon")
		// TODO minerstate
		//rt := builder.Build(t)
		//deadline, infos, partitions := runTillFirstDeadline(rt)
		//st := getState(rt)

		// mark all sectors as recovered faults
		//sectors := bitfield.New()
		//for _, info := range infos {
		//	sectors.Set(uint64(info.SectorNumber))
		//}
		//err := st.AddFaults(rt.AdtStore(), &sectors, rt.Epoch())
		//require.NoError(t, err)
		//err = st.AddRecoveries(&sectors)
		//require.NoError(t, err)
		//rt.ReplaceState(st)

		//pwr := miner.PowerForSectors(actor.sectorSize, infos)
		//
		//cfg := &poStConfig{
		//	expectedRawPowerDelta: pwr.Raw,
		//	expectedQAPowerDelta:  pwr.QA,
		//	expectedPenalty:       big.Zero(),
		//}
		//
		//actor.submitWindowPoSt(rt, deadline, partitions, infos, cfg)
	})

	t.Run("skipped faults are penalized and adjust power adjusted", func(t *testing.T) {
		t.Skip("Disabled in miner state refactor #648, restore soon")
		//rt := builder.Build(t)
		//deadline, infos, partitions := runTillFirstDeadline(rt)
		//
		//// skip the first sector in the partition
		//skipped := bitfield.NewFromSet([]uint64{uint64(infos[0].SectorNumber)})
		//
		//pwr := miner.PowerForSectors(actor.sectorSize, infos[:1])
		//
		//// expected penalty is the fee for an undeclared fault
		//expectedPenalty := miner.PledgePenaltyForUndeclaredFault(actor.epochReward, actor.networkQAPower, pwr.QA)
		//
		//cfg := &poStConfig{
		//	expectedRawPowerDelta: pwr.Raw.Neg(),
		//	expectedQAPowerDelta:  pwr.QA.Neg(),
		//	expectedPenalty:       expectedPenalty,
		//}
		//
		//actor.submitWindowPoSt(rt, deadline, partitions, infos, cfg)
	})

	// TODO minerstate
	//t.Run("skipped all sectors in a deadline may be skipped", func(t *testing.T) {
	//	rt := builder.Build(t)
	//	deadline, infos, partitions := runTillFirstDeadline(rt)
	//
	//	// skip all sectors in deadline
	//	st := getState(rt)
	//	deadlines, err := st.LoadDeadlines(rt.AdtStore())
	//	require.NoError(t, err)
	//	skipped := deadlines.Due[deadline.Index]
	//	count, err := skipped.Count()
	//	require.NoError(t, err)
	//	assert.Greater(t, count, uint64(0))
	//
	//	pwr := miner.PowerForSectors(actor.sectorSize, infos)
	//
	//	// expected penalty is the fee for an undeclared fault
	//	expectedPenalty := miner.PledgePenaltyForUndeclaredFault(actor.epochReward, actor.networkQAPower, pwr.QA)
	//
	//	cfg := &poStConfig{
	//		skipped:               skipped,
	//		expectedRawPowerDelta: pwr.Raw.Neg(),
	//		expectedQAPowerDelta:  pwr.QA.Neg(),
	//		expectedPenalty:       expectedPenalty,
	//	}
	//
	//	actor.submitWindowPoSt(rt, deadline, partitions, infos, cfg)
	//})

	// TODO minerstate
	//t.Run("skipped recoveries are penalized and do not recover power", func(t *testing.T) {
	//	rt := builder.Build(t)
	//	deadline, infos, partitions := runTillFirstDeadline(rt)
	//	st := getState(rt)
	//
	//	// mark all sectors as recovered faults
	//	sectors := bitfield.NewFromSet([]uint64{uint64(infos[0].SectorNumber)})
	//	err := st.AddFaults(rt.AdtStore(), sectors, rt.Epoch())
	//	require.NoError(t, err)
	//	err = st.AddRecoveries(sectors)
	//	require.NoError(t, err)
	//	rt.ReplaceState(st)
	//
	//	pwr := miner.PowerForSectors(actor.sectorSize, infos[:1])
	//
	//	// skip the first sector in the partition
	//	skipped := bitfield.NewFromSet([]uint64{uint64(infos[0].SectorNumber)})
	//	// expected penalty is the fee for an undeclared fault
	//	expectedPenalty := miner.PledgePenaltyForUndeclaredFault(actor.epochReward, actor.networkQAPower, pwr.QA)
	//
	//	cfg := &poStConfig{
	//		expectedRawPowerDelta: big.Zero(),
	//		expectedQAPowerDelta:  big.Zero(),
	//		expectedPenalty:       expectedPenalty,
	//		skipped:               skipped,
	//	}
	//
	//	actor.submitWindowPoSt(rt, deadline, partitions, infos, cfg)
	//})

	//t.Run("skipping a fault from the wrong deadline is an error", func(t *testing.T) {
	//	rt := builder.Build(t)
	//	deadline, infos, partitions := runTillFirstDeadline(rt)
	//	st := getState(rt)
	//
	//	// look ahead to next deadline to find a sector not in this deadline
	//	deadlines, err := st.LoadDeadlines(rt.AdtStore())
	//	require.NoError(t, err)
	//	nextDeadline := st.DeadlineInfo(deadline.NextOpen())
	//	nextInfos, _ := actor.computePartitions(rt, deadlines, nextDeadline.Index)
	//
	//	pwr := miner.PowerForSectors(actor.sectorSize, nextInfos[:1])
	//
	//	// skip the first sector in the partition
	//	skipped := bitfield.NewFromSet([]uint64{uint64(nextInfos[0].SectorNumber)})
	//	// expected penalty is the fee for an undeclared fault
	//	expectedPenalty := miner.PledgePenaltyForUndeclaredFault(actor.epochReward, actor.networkQAPower, pwr.QA)
	//
	//	cfg := &poStConfig{
	//		expectedRawPowerDelta: big.Zero(),
	//		expectedQAPowerDelta:  big.Zero(),
	//		expectedPenalty:       expectedPenalty,
	//		skipped:               skipped,
	//	}
	//
	//	rt.ExpectAbortConstainsMessage(exitcode.ErrIllegalArgument, "skipped faults contains sectors not due in deadline", func() {
	//		actor.submitWindowPoSt(rt, deadline, partitions, infos, cfg)
	//	})
	//})

	// TODO minerstate
	//t.Run("detects faults from previous missed posts", func(t *testing.T) {
	//	rt := builder.Build(t)
	//
	//	// skip two PoSts
	//	_, infos1, _ := runTillFirstDeadline(rt)
	//	_, infos2, _ := runTillNextDeadline(rt)
	//	deadline, infos3, partitions := runTillNextDeadline(rt)
	//
	//	// assert we have sectors in each deadline
	//	assert.Greater(t, len(infos1), 0)
	//	assert.Greater(t, len(infos2), 0)
	//	assert.Greater(t, len(infos3), 0)
	//
	//	// expect power to be deducted for all sectors in first two deadlines
	//	pwr := miner.PowerForSectors(actor.sectorSize, append(infos1, infos2...))
	//
	//	// expected penalty is the late undeclared fault penalty for all faulted sectors including retracted recoveries..
	//	expectedPenalty := miner.PledgePenaltyForLateUndeclaredFault(actor.epochReward, actor.networkQAPower, pwr.QA)
	//
	//	cfg := &poStConfig{
	//		skipped:               abi.NewBitField(),
	//		expectedRawPowerDelta: pwr.Raw.Neg(),
	//		expectedQAPowerDelta:  pwr.QA.Neg(),
	//		expectedPenalty:       expectedPenalty,
	//	}
	//
	//	actor.submitWindowPoSt(rt, deadline, partitions, infos3, cfg)
	//
	//	// same size and every info is set in bitset implies info1+info2 and st.Faults represent the same sectors
	//	st := getState(rt)
	//	faultCount, err := st.Faults.Count()
	//	require.NoError(t, err)
	//	assert.Equal(t, uint64(len(infos1)+len(infos2)), faultCount)
	//	for _, info := range append(infos1, infos2...) {
	//		set, err := st.Faults.IsSet(uint64(info.SectorNumber))
	//		require.NoError(t, err)
	//		assert.True(t, set)
	//	}
	//})
}

func TestProveCommit(t *testing.T) {
	periodOffset := abi.ChainEpoch(100)
	actor := newHarness(t, periodOffset)
	builder := builderForHarness(actor).
		WithBalance(bigBalance, big.Zero())

	t.Run("prove commit aborts if pledge requirement not met", func(t *testing.T) {
		rt := builder.Build(t)
		actor.constructAndVerify(rt)

		// prove one sector to establish collateral and locked funds
		actor.commitAndProveSectors(rt, 1, 181, nil)

		// preecommit another sector so we may prove it
		expiration := 181*miner.WPoStProvingPeriod + periodOffset - 1
		precommitEpoch := rt.Epoch() + 1
		rt.SetEpoch(precommitEpoch)
		precommit := actor.makePreCommit(actor.nextSectorNo, rt.Epoch()-1, expiration, nil)
		actor.preCommitSector(rt, precommit)

		// alter balance to simulate dipping into it for fees

		st := getState(rt)
		bal := rt.Balance()
		rt.SetBalance(big.Add(st.PreCommitDeposits, st.LockedFunds))
		info := actor.getInfo(rt)

		rt.SetEpoch(precommitEpoch + miner.MaxSealDuration[info.SealProofType] - 1)
		rt.ExpectAbort(exitcode.ErrInsufficientFunds, func() {
			actor.proveCommitSectorAndConfirm(rt, precommit, precommitEpoch, makeProveCommit(actor.nextSectorNo), proveCommitConf{})
		})
		rt.Reset()

		// succeeds when pledge deposits satisfy initial pledge requirement
		rt.SetBalance(bal)
		actor.proveCommitSectorAndConfirm(rt, precommit, precommitEpoch, makeProveCommit(actor.nextSectorNo), proveCommitConf{})
	})

	t.Run("drop invalid prove commit while processing valid one", func(t *testing.T) {
		rt := builder.Build(t)
		actor.constructAndVerify(rt)

		// make two precommits
		expiration := 181*miner.WPoStProvingPeriod + periodOffset - 1
		precommitEpoch := rt.Epoch() + 1
		rt.SetEpoch(precommitEpoch)
		precommitA := actor.makePreCommit(actor.nextSectorNo, rt.Epoch()-1, expiration, nil)
		actor.preCommitSector(rt, precommitA)
		sectorNoA := actor.nextSectorNo
		actor.nextSectorNo++
		precommitB := actor.makePreCommit(actor.nextSectorNo, rt.Epoch()-1, expiration, nil)
		actor.preCommitSector(rt, precommitB)
		sectorNoB := actor.nextSectorNo

		// handle both prove commits in the same epoch
		info := actor.getInfo(rt)
		rt.SetEpoch(precommitEpoch + miner.MaxSealDuration[info.SealProofType] - 1)

		actor.proveCommitSector(rt, precommitA, precommitEpoch, makeProveCommit(sectorNoA))
		actor.proveCommitSector(rt, precommitB, precommitEpoch, makeProveCommit(sectorNoB))

		conf := proveCommitConf{
			verifyDealsExit: map[abi.SectorNumber]exitcode.ExitCode{
				sectorNoA: exitcode.ErrIllegalArgument,
			},
		}
		actor.confirmSectorProofsValid(rt, conf, precommitEpoch, precommitA, precommitB)
	})
}

func TestProvingPeriodCron(t *testing.T) {
	periodOffset := abi.ChainEpoch(100)
	actor := newHarness(t, periodOffset)
	builder := builderForHarness(actor).
		WithBalance(bigBalance, big.Zero())

	t.Run("empty periods", func(t *testing.T) {
		t.Skip("Disabled in miner state refactor #648, restore soon")
		rt := builder.Build(t)
		actor.constructAndVerify(rt)
		st := getState(rt)
		assert.Equal(t, periodOffset, st.ProvingPeriodStart)

		// First cron invocation just before the first proving period starts.
		rt.SetEpoch(periodOffset - 1)
		secondCronEpoch := periodOffset + miner.WPoStProvingPeriod - 1
		actor.onDeadlineCron(rt, &cronConfig{
			expectedEntrollment: secondCronEpoch,
		})
		// The proving period start isn't changed, because the period hadn't started yet.
		st = getState(rt)
		assert.Equal(t, periodOffset, st.ProvingPeriodStart)

		rt.SetEpoch(secondCronEpoch)
		actor.onDeadlineCron(rt, &cronConfig{
			expectedEntrollment: periodOffset + 2*miner.WPoStProvingPeriod - 1,
		})
		// Proving period moves forward
		st = getState(rt)
		assert.Equal(t, periodOffset+miner.WPoStProvingPeriod, st.ProvingPeriodStart)
	})

	t.Run("first period gets randomness from previous epoch", func(t *testing.T) {
		t.Skip("Disabled in miner state refactor #648, restore soon")
		rt := builder.Build(t)
		actor.constructAndVerify(rt)
		//st := getState(rt)

		//sectorInfo := actor.commitAndProveSectors(rt, 1, 100, nil)

		// Flag new sectors to trigger request for randomness
		//rt.Transaction(st, func() interface{} {
		//	st.NewSectors.Set(uint64(sectorInfo[0].SectorNumber))
		//	return nil
		//})

		// First cron invocation just before the first proving period starts
		// requires randomness come from current epoch minus lookback
		rt.SetEpoch(periodOffset - 1)
		secondCronEpoch := periodOffset + miner.WPoStProvingPeriod - 1
		actor.onDeadlineCron(rt, &cronConfig{
			expectedEntrollment: secondCronEpoch,
		})

		// cron invocation after the proving period starts, requires randomness come from end of proving period
		rt.SetEpoch(periodOffset)
		actor.advanceProvingPeriodWithoutFaults(rt)

		// triggers a new request for randomness
		// TODO minerstate
		//rt.Transaction(st, func() interface{} {
		//	st.NewSectors.Set(uint64(sectorInfo[0].SectorNumber))
		//	return nil
		//})

		thirdCronEpoch := secondCronEpoch + miner.WPoStProvingPeriod
		actor.onDeadlineCron(rt, &cronConfig{
			expectedEntrollment: thirdCronEpoch,
		})
	})

	t.Run("detects and penalizes faults", func(t *testing.T) {
		t.Skip("Disabled in miner state refactor #648, restore soon")
		rt := builder.Build(t)
		actor.constructAndVerify(rt)

		allSectors := actor.commitAndProveSectors(rt, 2, 100, nil)

		// advance to end of proving period to add sectors to proving set
		st := getState(rt)
		deadline := st.DeadlineInfo(rt.Epoch())
		nextCron := deadline.NextPeriodStart() + miner.WPoStProvingPeriod - 1
		rt.SetEpoch(deadline.PeriodEnd())
		actor.onDeadlineCron(rt, &cronConfig{
			expectedEntrollment: nextCron,
		})

		// advance to next deadline where we expect the first sectors to appear
		st = getState(rt)
		deadline = st.DeadlineInfo(rt.Epoch() + 1)
		rt.SetEpoch(deadline.NextOpen())
		deadline = st.DeadlineInfo(rt.Epoch())

		// Skip to end of proving period, cron detects all sectors as faulty
		rt.SetEpoch(deadline.PeriodEnd())
		nextCron = deadline.NextPeriodStart() + miner.WPoStProvingPeriod - 1

		// Undetected faults penalized once as a late undetected fault
		rawPower, qaPower := powerForSectors(actor.sectorSize, allSectors)
		undetectedPenalty := miner.PledgePenaltyForUndeclaredFault(actor.epochReward, actor.networkQAPower, qaPower)

		// power for sectors is removed
		powerDeltaClaim := miner.NewPowerPair(rawPower.Neg(), qaPower.Neg())

		// Faults are charged again as ongoing faults
		ongoingPenalty := miner.PledgePenaltyForDeclaredFault(actor.epochReward, actor.networkQAPower, qaPower)

		actor.onDeadlineCron(rt, &cronConfig{
			expectedEntrollment:      nextCron,
			detectedFaultsPenalty:    undetectedPenalty,
			detectedFaultsPowerDelta: &powerDeltaClaim,
			ongoingFaultsPenalty:     ongoingPenalty,
		})

		// expect both faults are added to state
		// TODO minerstate
		//st = getState(rt)
		//set, err := st.Faults.IsSet(uint64(allSectors[0].SectorNumber))
		//require.NoError(t, err)
		//assert.True(t, set)
		//set, err = st.Faults.IsSet(uint64(allSectors[1].SectorNumber))
		//require.NoError(t, err)
		//assert.True(t, set)

		// advance 3 deadlines
		rt.SetEpoch(deadline.NextOpen() + 3*miner.WPoStChallengeWindow)
		deadline = st.DeadlineInfo(rt.Epoch())

		actor.declareRecoveries(rt, 1, sectorInfoAsBitfield(allSectors[1:]))

		// Skip to end of proving period, cron detects all sectors as faulty
		rt.SetEpoch(deadline.PeriodEnd())
		nextCron = deadline.NextPeriodStart() + miner.WPoStProvingPeriod - 1

		// Retracted recovery is penalized as an undetected fault, but power is unchanged
		_, retractedQAPower := powerForSectors(actor.sectorSize, allSectors[1:])
		retractedPenalty := miner.PledgePenaltyForUndeclaredFault(actor.epochReward, actor.networkQAPower, retractedQAPower)

		// Faults are charged again as ongoing faults
		_, faultQAPower := powerForSectors(actor.sectorSize, allSectors)
		ongoingPenalty = miner.PledgePenaltyForDeclaredFault(actor.epochReward, actor.networkQAPower, faultQAPower)

		actor.onDeadlineCron(rt, &cronConfig{
			expectedEntrollment:   nextCron,
			detectedFaultsPenalty: retractedPenalty,
			ongoingFaultsPenalty:  ongoingPenalty,
		})
	})

	// TODO: test cron being called one epoch late because the scheduled epoch had no blocks.
}

func TestDeclareFaults(t *testing.T) {
	t.Skip("Disabled in miner state refactor #648, restore soon")
	periodOffset := abi.ChainEpoch(100)
	actor := newHarness(t, periodOffset)
	builder := builderForHarness(actor).
		WithBalance(bigBalance, big.Zero())

	t.Run("declare fault pays fee", func(t *testing.T) {
		// Get sector into proving state
		rt := builder.Build(t)
		actor.constructAndVerify(rt)
		precommits := actor.commitAndProveSectors(rt, 1, 100, nil)

		// Skip to end of proving period, cron adds sectors to proving set.
		completeProvingPeriod(rt, actor, &cronConfig{})
		info := actor.getSector(rt, precommits[0].SectorNumber)

		// Declare the sector as faulted
		ss, err := info.SealProof.SectorSize()
		require.NoError(t, err)
		sectorQAPower := miner.QAPowerForSector(ss, info)
		totalQAPower := big.NewInt(1 << 52)
		fee := miner.PledgePenaltyForDeclaredFault(actor.epochReward, totalQAPower, sectorQAPower)

		actor.declareFaults(rt, fee, info)
	})
}

func TestExtendSectorExpiration(t *testing.T) {
	//periodOffset := abi.ChainEpoch(100)
	//actor := newHarness(t, periodOffset)
	//precommitEpoch := abi.ChainEpoch(1)
	//builder := builderForHarness(actor).
	//	WithEpoch(precommitEpoch).
	//	WithBalance(bigBalance, big.Zero())
	//
	//commitSector := func(t *testing.T, rt *mock.Runtime) *miner.SectorOnChainInfo {
	//	actor.constructAndVerify(rt)
	//	sectorInfo := actor.commitAndProveSectors(rt, 1, 100, nil)
	//	return sectorInfo[0]
	//}

	// TODO minerstate

	//t.Run("rejects negative extension", func(t *testing.T) {
	//	rt := builder.Build(t)
	//	sector := commitSector(t, rt)
	//	// attempt to shorten epoch
	//	newExpiration := sector.Expiration - abi.ChainEpoch(miner.WPoStProvingPeriod)
	//	params := &miner.ExtendSectorExpirationParams{
	//		SectorNumber:  sector.SectorNumber,
	//		NewExpiration: newExpiration,
	//	}
	//
	//	rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
	//		actor.extendSector(rt, sector, 0, params)
	//	})
	//})
	//
	//t.Run("rejects extension to invalid epoch", func(t *testing.T) {
	//	rt := builder.Build(t)
	//	sector := commitSector(t, rt)
	//
	//	// attempt to extend to an epoch that is not a multiple of the proving period + the commit epoch
	//	extension := 42*miner.WPoStProvingPeriod + 1
	//	newExpiration := sector.Expiration - abi.ChainEpoch(extension)
	//	params := &miner.ExtendSectorExpirationParams{
	//		SectorNumber:  sector.SectorNumber,
	//		NewExpiration: newExpiration,
	//	}
	//
	//	rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
	//		actor.extendSector(rt, sector, extension, params)
	//	})
	//})
	//
	//t.Run("rejects extension too far in future", func(t *testing.T) {
	//	rt := builder.Build(t)
	//	sector := commitSector(t, rt)
	//
	//	// extend by even proving period after max
	//	rt.SetEpoch(sector.Expiration)
	//	extension := miner.WPoStProvingPeriod * (miner.MaxSectorExpirationExtension/miner.WPoStProvingPeriod + 1)
	//	newExpiration := rt.Epoch() + extension
	//	params := &miner.ExtendSectorExpirationParams{
	//		SectorNumber:  sector.SectorNumber,
	//		NewExpiration: newExpiration,
	//	}
	//
	//	rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
	//		actor.extendSector(rt, sector, extension, params)
	//	})
	//})
	//
	//t.Run("rejects extension past max for seal proof", func(t *testing.T) {
	//	rt := builder.Build(t)
	//	sector := commitSector(t, rt)
	//	rt.SetEpoch(sector.Expiration)
	//
	//	maxLifetime := sector.SealProof.SectorMaximumLifetime()
	//
	//	// extend sector until just below threshold
	//	expiration := sector.Activation + sector.SealProof.SectorMaximumLifetime()
	//	extension := expiration - rt.Epoch()
	//	for ; expiration-sector.Activation < maxLifetime; expiration += extension {
	//		params := &miner.ExtendSectorExpirationParams{
	//			SectorNumber:  sector.SectorNumber,
	//			NewExpiration: expiration,
	//		}
	//
	//		actor.extendSector(rt, sector, extension, params)
	//		rt.SetEpoch(expiration)
	//	}
	//
	//	// next extension fails because it extends sector past max lifetime
	//	params := &miner.ExtendSectorExpirationParams{
	//		SectorNumber:  sector.SectorNumber,
	//		NewExpiration: expiration,
	//	}
	//
	//	rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
	//		actor.extendSector(rt, sector, extension, params)
	//	})
	//})
	//
	//t.Run("updates expiration with valid params", func(t *testing.T) {
	//	rt := builder.Build(t)
	//	oldSector := commitSector(t, rt)
	//
	//	extension := 42 * miner.WPoStProvingPeriod
	//	newExpiration := oldSector.Expiration + extension
	//	params := &miner.ExtendSectorExpirationParams{
	//		SectorNumber:  oldSector.SectorNumber,
	//		NewExpiration: newExpiration,
	//	}
	//
	//	actor.extendSector(rt, oldSector, extension, params)
	//
	//	// assert sector expiration is set to the new value
	//	st := getState(rt)
	//	newSector := actor.getSector(rt, oldSector.SectorNumber)
	//	assert.Equal(t, newExpiration, newSector.Expiration)
	//
	//	// assert that an expiration exists at the target epoch
	//	expirations, err := st.GetSectorExpirations(rt.AdtStore(), newExpiration)
	//	require.NoError(t, err)
	//	exists, err := expirations.IsSet(uint64(newSector.SectorNumber))
	//	require.NoError(t, err)
	//	assert.True(t, exists)
	//
	//	// assert that the expiration has been removed from the old epoch
	//	expirations, err = st.GetSectorExpirations(rt.AdtStore(), oldSector.Expiration)
	//	require.NoError(t, err)
	//	exists, err = expirations.IsSet(uint64(newSector.SectorNumber))
	//	require.NoError(t, err)
	//	assert.False(t, exists)
	//})
}

func TestTerminateSectors(t *testing.T) {
	//periodOffset := abi.ChainEpoch(100)
	//actor := newHarness(t, periodOffset)
	//builder := builderForHarness(actor).
	//	WithBalance(bigBalance, big.Zero())
	//
	//commitSector := func(t *testing.T, rt *mock.Runtime) *miner.SectorOnChainInfo {
	//	actor.constructAndVerify(rt)
	//	precommitEpoch := abi.ChainEpoch(1)
	//	rt.SetEpoch(precommitEpoch)
	//	sectorInfo := actor.commitAndProveSectors(rt, 1, 100, nil)
	//	return sectorInfo[0]
	//}

	// TODO minerstate
	//t.Run("removes sector with correct accounting", func(t *testing.T) {
	//	rt := builder.Build(t)
	//	sector := commitSector(t, rt)
	//	var initialLockedFunds abi.TokenAmount
	//
	//	// A miner will pay the minimum of termination fee and locked funds. Add some locked funds to ensure
	//	// correct fee calculation is used.
	//	actor.addLockedFund(rt, big.NewInt(1<<61))
	//
	//	{
	//		// Verify that a sector expiration was registered.
	//		st := getState(rt)
	//		expiration, err := st.GetSectorExpirations(rt.AdtStore(), sector.Expiration)
	//		require.NoError(t, err)
	//		expiringSectorNos, err := expiration.All(1)
	//		require.NoError(t, err)
	//		assert.Len(t, expiringSectorNos, 1)
	//		assert.Equal(t, sector.SectorNumber, abi.SectorNumber(expiringSectorNos[0]))
	//		initialLockedFunds = st.LockedFunds
	//	}
	//
	//	sectorSize, err := sector.SealProof.SectorSize()
	//	require.NoError(t, err)
	//	sectorPower := miner.QAPowerForSector(sectorSize, sector)
	//	sectorAge := rt.Epoch() - sector.Activation
	//	expectedFee := miner.PledgePenaltyForTermination(sector.InitialPledge, sectorAge, actor.epochReward, actor.networkQAPower, sectorPower)
	//
	//	sectors := bitfield.New()
	//	sectors.Set(uint64(sector.SectorNumber))
	//	actor.terminateSectors(rt, &sectors, expectedFee)
	//
	//	{
	//		st := getState(rt)
	//
	//		// expect sector expiration to have been removed
	//		err = st.ForEachSectorExpiration(rt.AdtStore(), func(expiry abi.ChainEpoch, sectors *abi.BitField) error {
	//			assert.Fail(t, "did not expect to find a sector expiration, found expiration at %s", expiry)
	//			return nil
	//		})
	//		assert.NoError(t, err)
	//
	//		// expect sector to have been removed
	//		_, found, err := st.GetSector(rt.AdtStore(), sector.SectorNumber)
	//		require.NoError(t, err)
	//		assert.False(t, found)
	//
	//		// expect fee to have been unlocked and burnt
	//		assert.Equal(t, big.Sub(initialLockedFunds, expectedFee), st.LockedFunds)
	//
	//		// expect pledge requirement to have been decremented
	//		assert.Equal(t, big.Zero(), st.InitialPledgeRequirement)
	//	}
	//})
}

func TestWithdrawBalance(t *testing.T) {
	periodOffset := abi.ChainEpoch(100)
	actor := newHarness(t, periodOffset)
	builder := builderForHarness(actor).
		WithBalance(bigBalance, big.Zero())

	t.Run("happy path withdraws funds", func(t *testing.T) {
		rt := builder.Build(t)
		actor.constructAndVerify(rt)

		// withdraw 1% of balance
		actor.withdrawFunds(rt, big.Mul(big.NewInt(10), big.NewInt(1e18)))
	})

	t.Run("fails if miner is currently undercollateralized", func(t *testing.T) {
		rt := builder.Build(t)
		actor.constructAndVerify(rt)

		// prove one sector to establish collateral and locked funds
		actor.commitAndProveSectors(rt, 1, 181, nil)

		// alter initial pledge requirement to simulate undercollateralization
		st := getState(rt)
		st.InitialPledgeRequirement = big.Mul(big.NewInt(300000), st.InitialPledgeRequirement)
		rt.ReplaceState(st)

		// withdraw 1% of balance
		rt.ExpectAbort(exitcode.ErrInsufficientFunds, func() {
			actor.withdrawFunds(rt, big.Mul(big.NewInt(10), big.NewInt(1e18)))
		})
	})
}

func TestReportConsensusFault(t *testing.T) {
	t.Skip("Disabled in miner state refactor #648, restore soon")
	periodOffset := abi.ChainEpoch(100)
	actor := newHarness(t, periodOffset)
	builder := builderForHarness(actor).
		WithBalance(bigBalance, big.Zero())

	rt := builder.Build(t)
	actor.constructAndVerify(rt)
	precommitEpoch := abi.ChainEpoch(1)
	rt.SetEpoch(precommitEpoch)
	dealIDs := [][]abi.DealID{{1, 2}, {3, 4}}
	sectorInfo := actor.commitAndProveSectors(rt, 2, 10, dealIDs)
	_ = sectorInfo

	params := &miner.ReportConsensusFaultParams{
		BlockHeader1:     nil,
		BlockHeader2:     nil,
		BlockHeaderExtra: nil,
	}

	// miner should send a single call to terminate the deals for all its sectors
	allDeals := []abi.DealID{}
	for _, ids := range dealIDs {
		allDeals = append(allDeals, ids...)
	}
	actor.reportConsensusFault(rt, addr.TestAddress, params, allDeals)
}

func TestAddLockedFund(t *testing.T) {
	periodOffset := abi.ChainEpoch(1808)
	actor := newHarness(t, periodOffset)

	builder := builderForHarness(actor).
		WithBalance(bigBalance, big.Zero())

	t.Run("funds vest", func(t *testing.T) {
		rt := builder.Build(t)
		actor.constructAndVerify(rt)
		st := getState(rt)
		store := rt.AdtStore()

		// Nothing vesting to start
		vestingFunds, err := adt.AsArray(store, st.VestingFunds)
		require.NoError(t, err)
		assert.Equal(t, uint64(0), vestingFunds.Length())
		assert.Equal(t, big.Zero(), st.LockedFunds)

		// Lock some funds with AddLockedFund
		amt := abi.NewTokenAmount(600_000)
		actor.addLockedFund(rt, amt)
		st = getState(rt)
		newVestingFunds, err := adt.AsArray(store, st.VestingFunds)
		require.NoError(t, err)
		require.Equal(t, uint64(180), newVestingFunds.Length())

		// Vested FIL pays out on epochs with expected offset
		lockedEntry := abi.NewTokenAmount(0)
		expectedOffset := periodOffset % miner.PledgeVestingSpec.Quantization
		err = newVestingFunds.ForEach(&lockedEntry, func(k int64) error {
			assert.Equal(t, int64(expectedOffset), k%int64(miner.PledgeVestingSpec.Quantization))
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, amt, st.LockedFunds)

	})

	t.Run("funds vest when under collateralized", func(t *testing.T) {
		rt := builder.Build(t)
		actor.constructAndVerify(rt)
		st := getState(rt)

		assert.Equal(t, big.Zero(), st.LockedFunds)

		balance := rt.Balance()
		st.InitialPledgeRequirement = big.Mul(big.NewInt(2), balance) // ip req twice total balance
		availableBefore := st.GetAvailableBalance(balance)
		assert.True(t, availableBefore.LessThan(big.Zero()))
		rt.ReplaceState(st)

		amt := abi.NewTokenAmount(600_000)
		actor.addLockedFund(rt, amt)
		// manually update actor balance to include the added funds from outside
		newBalance := big.Add(balance, amt)
		rt.SetBalance(newBalance)

		st = getState(rt)
		// no funds used to pay off ip debt
		assert.Equal(t, availableBefore, st.GetAvailableBalance(newBalance))
		assert.False(t, st.MeetsInitialPledgeCondition(newBalance))
		// all funds locked in vesting table
		assert.Equal(t, amt, st.LockedFunds)
	})

	t.Run("unvested funds will recollateralize a miner", func(t *testing.T) {
		rt := builder.Build(t)
		actor.constructAndVerify(rt)
		st := getState(rt)

		balance := rt.Balance()
		st.InitialPledgeRequirement = balance
		underCollateralizedBalance := big.Div(balance, big.NewInt(2)) // ip req twice total balance
		assert.False(t, st.MeetsInitialPledgeCondition(underCollateralizedBalance))

		st.InitialPledgeRequirement = balance
		assert.True(t, st.MeetsInitialPledgeCondition(balance))
	})

}

type actorHarness struct {
	a miner.Actor
	t testing.TB

	receiver addr.Address // The miner actor's own address
	owner    addr.Address
	worker   addr.Address
	key      addr.Address

	sealProofType abi.RegisteredSealProof
	sectorSize    abi.SectorSize
	partitionSize uint64
	periodOffset  abi.ChainEpoch
	nextSectorNo  abi.SectorNumber

	epochReward     abi.TokenAmount
	networkPledge   abi.TokenAmount
	networkRawPower abi.StoragePower
	networkQAPower  abi.StoragePower
	baselinePower   abi.StoragePower
}

func newHarness(t testing.TB, provingPeriodOffset abi.ChainEpoch) *actorHarness {
	sealProofType := abi.RegisteredSealProof_StackedDrg32GiBV1
	sectorSize, err := sealProofType.SectorSize()
	require.NoError(t, err)
	partitionSectors, err := sealProofType.WindowPoStPartitionSectors()
	require.NoError(t, err)
	owner := tutil.NewIDAddr(t, 100)
	worker := tutil.NewIDAddr(t, 101)
	workerKey := tutil.NewBLSAddr(t, 0)
	receiver := tutil.NewIDAddr(t, 1000)
	reward := big.Mul(big.NewIntUnsigned(100), big.NewIntUnsigned(1e18))
	return &actorHarness{
		t:        t,
		receiver: receiver,
		owner:    owner,
		worker:   worker,
		key:      workerKey,

		sealProofType: sealProofType,
		sectorSize:    sectorSize,
		partitionSize: partitionSectors,
		periodOffset:  provingPeriodOffset,
		nextSectorNo:  100,

		epochReward:     reward,
		networkPledge:   big.Mul(reward, big.NewIntUnsigned(1000)),
		networkRawPower: abi.NewStoragePower(1 << 50),
		networkQAPower:  abi.NewStoragePower(1 << 50),
		baselinePower:   abi.NewStoragePower(1 << 50),
	}
}

func (h *actorHarness) setProofType(proof abi.RegisteredSealProof) {
	var err error
	h.sealProofType = proof
	h.sectorSize, err = proof.SectorSize()
	require.NoError(h.t, err)
	h.partitionSize, err = proof.WindowPoStPartitionSectors()
	require.NoError(h.t, err)
}

func (h *actorHarness) constructAndVerify(rt *mock.Runtime) {
	params := miner.ConstructorParams{
		OwnerAddr:     h.owner,
		WorkerAddr:    h.worker,
		SealProofType: h.sealProofType,
		PeerId:        testPid,
	}

	rt.ExpectValidateCallerAddr(builtin.InitActorAddr)
	// Fetch worker pubkey.
	rt.ExpectSend(h.worker, builtin.MethodsAccount.PubkeyAddress, nil, big.Zero(), &h.key, exitcode.Ok)
	// Register proving period cron.
	nextProvingPeriodEnd := h.periodOffset - 1
	for nextProvingPeriodEnd < rt.Epoch() {
		nextProvingPeriodEnd += miner.WPoStProvingPeriod
	}
	rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.EnrollCronEvent,
		makeDeadlineCronEventParams(h.t, nextProvingPeriodEnd), big.Zero(), nil, exitcode.Ok)
	rt.SetCaller(builtin.InitActorAddr, builtin.InitActorCodeID)
	ret := rt.Call(h.a.Constructor, &params)
	assert.Nil(h.t, ret)
	rt.Verify()
}

//
// State access helpers
//

func (h *actorHarness) deadline(rt *mock.Runtime) *miner.DeadlineInfo {
	st := getState(rt)
	return st.DeadlineInfo(rt.Epoch())
}

func (h *actorHarness) getPreCommit(rt *mock.Runtime, sno abi.SectorNumber) *miner.SectorPreCommitOnChainInfo {
	st := getState(rt)
	pc, found, err := st.GetPrecommittedSector(rt.AdtStore(), sno)
	require.NoError(h.t, err)
	require.True(h.t, found)
	return pc
}

func (h *actorHarness) getSector(rt *mock.Runtime, sno abi.SectorNumber) *miner.SectorOnChainInfo {
	st := getState(rt)
	sector, found, err := st.GetSector(rt.AdtStore(), sno)
	require.NoError(h.t, err)
	require.True(h.t, found)
	return sector
}

func (h *actorHarness) getInfo(rt *mock.Runtime) *miner.MinerInfo {
	var st miner.State
	rt.GetState(&st)
	info, err := st.GetInfo(rt.AdtStore())
	require.NoError(h.t, err)
	return info
}

func (h *actorHarness) getDeadlines(rt *mock.Runtime) *miner.Deadlines {
	st := getState(rt)
	deadlines, err := st.LoadDeadlines(rt.AdtStore())
	require.NoError(h.t, err)
	return deadlines
}

func (h *actorHarness) getDeadline(rt *mock.Runtime, idx uint64) *miner.Deadline {
	dls := h.getDeadlines(rt)
	deadline, err := dls.LoadDeadline(rt.AdtStore(), idx)
	require.NoError(h.t, err)
	return deadline
}

func (h *actorHarness) getPartition(rt *mock.Runtime, deadline *miner.Deadline, idx uint64) *miner.Partition {
	partition, err := deadline.LoadPartition(rt.AdtStore(), idx)
	require.NoError(h.t, err)
	return partition
}

func (h *actorHarness) getDeadlineAndPartition(rt *mock.Runtime, dlIdx, pIdx uint64) (*miner.Deadline, *miner.Partition) {
	deadline := h.getDeadline(rt, dlIdx)
	partition := h.getPartition(rt, deadline, pIdx)
	return deadline, partition
}

// Collects all sector infos into a map.
func (h *actorHarness) collectSectors(rt *mock.Runtime) map[abi.SectorNumber]*miner.SectorOnChainInfo {
	sectors := map[abi.SectorNumber]*miner.SectorOnChainInfo{}
	st := getState(rt)
	_ = st.ForEachSector(rt.AdtStore(), func(info *miner.SectorOnChainInfo) {
		sector := *info
		sectors[info.SectorNumber] = &sector
	})
	return sectors
}

func (h *actorHarness) collectDeadlineExpirations(rt *mock.Runtime, deadline *miner.Deadline) map[abi.ChainEpoch][]uint64 {
	st := getState(rt)
	quant := st.QuantEndOfDeadline()
	queue, err := miner.LoadBitfieldQueue(rt.AdtStore(), deadline.ExpirationsEpochs, quant)
	require.NoError(h.t, err)
	expirations := map[abi.ChainEpoch][]uint64{}
	_ = queue.ForEach(func(epoch abi.ChainEpoch, bf *bitfield.BitField) error {
		expanded, err := bf.All(miner.SectorsMax)
		require.NoError(h.t, err)
		expirations[epoch] = expanded
		return nil
	})
	return expirations
}

func (h *actorHarness) collectPartitionExpirations(rt *mock.Runtime, partition *miner.Partition) map[abi.ChainEpoch]*miner.ExpirationSet {
	st := getState(rt)
	quant := st.QuantEndOfDeadline()
	queue, err := miner.LoadExpirationQueue(rt.AdtStore(), partition.ExpirationsEpochs, quant)
	require.NoError(h.t, err)
	expirations := map[abi.ChainEpoch]*miner.ExpirationSet{}
	var es miner.ExpirationSet
	_ = queue.ForEach(&es, func(i int64) error {
		cpy := es
		expirations[abi.ChainEpoch(i)] = &cpy
		return nil
	})
	return expirations
}

//
// Actor method calls
//

func (h *actorHarness) controlAddresses(rt *mock.Runtime) (owner, worker addr.Address) {
	rt.ExpectValidateCallerAny()
	ret := rt.Call(h.a.ControlAddresses, nil).(*miner.GetControlAddressesReturn)
	require.NotNil(h.t, ret)
	rt.Verify()
	return ret.Owner, ret.Worker
}

func (h *actorHarness) preCommitSector(rt *mock.Runtime, params *miner.SectorPreCommitInfo) *miner.SectorPreCommitOnChainInfo {

	rt.SetCaller(h.worker, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerAddr(h.worker)

	{
		expectQueryNetworkInfo(rt, h)
	}
	{
		sectorSize, err := params.SealProof.SectorSize()
		require.NoError(h.t, err)

		vdParams := market.VerifyDealsForActivationParams{
			DealIDs:      params.DealIDs,
			SectorStart:  rt.Epoch(),
			SectorExpiry: params.Expiration,
		}

		vdReturn := market.VerifyDealsForActivationReturn{
			DealWeight:         big.NewInt(int64(sectorSize / 2)),
			VerifiedDealWeight: big.NewInt(int64(sectorSize / 2)),
		}
		rt.ExpectSend(builtin.StorageMarketActorAddr, builtin.MethodsMarket.VerifyDealsForActivation, &vdParams, big.Zero(), &vdReturn, exitcode.Ok)
	}
	{
		eventPayload := miner.CronEventPayload{
			EventType: miner.CronEventPreCommitExpiry,
			Sectors:   bitfield.NewFromSet([]uint64{uint64(params.SectorNumber)}),
		}
		buf := bytes.Buffer{}
		err := eventPayload.MarshalCBOR(&buf)
		require.NoError(h.t, err)
		cronParams := power.EnrollCronEventParams{
			EventEpoch: rt.Epoch() + miner.MaxSealDuration[params.SealProof] + 1,
			Payload:    buf.Bytes(),
		}
		rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.EnrollCronEvent, &cronParams, big.Zero(), nil, exitcode.Ok)
	}

	rt.Call(h.a.PreCommitSector, params)
	rt.Verify()
	return h.getPreCommit(rt, params.SectorNumber)
}

// Options for proveCommitSector behaviour.
// Default zero values should let everything be ok.
type proveCommitConf struct {
	verifyDealsExit map[abi.SectorNumber]exitcode.ExitCode
}

func (h *actorHarness) proveCommitSector(rt *mock.Runtime, precommit *miner.SectorPreCommitInfo, precommitEpoch abi.ChainEpoch,
	params *miner.ProveCommitSectorParams) {
	commd := cbg.CborCid(tutil.MakeCID("commd", &market.PieceCIDPrefix))
	sealRand := abi.SealRandomness([]byte{1, 2, 3, 4})
	sealIntRand := abi.InteractiveSealRandomness([]byte{5, 6, 7, 8})
	interactiveEpoch := precommitEpoch + miner.PreCommitChallengeDelay

	// Prepare for and receive call to ProveCommitSector
	{
		cdcParams := market.ComputeDataCommitmentParams{
			DealIDs:    precommit.DealIDs,
			SectorType: precommit.SealProof,
		}
		rt.ExpectSend(builtin.StorageMarketActorAddr, builtin.MethodsMarket.ComputeDataCommitment, &cdcParams, big.Zero(), &commd, exitcode.Ok)
	}
	{
		var buf bytes.Buffer
		err := rt.Receiver().MarshalCBOR(&buf)
		require.NoError(h.t, err)
		rt.ExpectGetRandomness(crypto.DomainSeparationTag_SealRandomness, precommit.SealRandEpoch, buf.Bytes(), abi.Randomness(sealRand))
		rt.ExpectGetRandomness(crypto.DomainSeparationTag_InteractiveSealChallengeSeed, interactiveEpoch, buf.Bytes(), abi.Randomness(sealIntRand))
	}
	{
		actorId, err := addr.IDFromAddress(h.receiver)
		require.NoError(h.t, err)
		seal := abi.SealVerifyInfo{
			SectorID: abi.SectorID{
				Miner:  abi.ActorID(actorId),
				Number: precommit.SectorNumber,
			},
			SealedCID:             precommit.SealedCID,
			SealProof:             precommit.SealProof,
			Proof:                 params.Proof,
			DealIDs:               precommit.DealIDs,
			Randomness:            sealRand,
			InteractiveRandomness: sealIntRand,
			UnsealedCID:           cid.Cid(commd),
		}
		rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.SubmitPoRepForBulkVerify, &seal, abi.NewTokenAmount(0), nil, exitcode.Ok)
	}
	rt.SetCaller(h.worker, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerAny()
	rt.Call(h.a.ProveCommitSector, params)
	rt.Verify()
}

func (h *actorHarness) confirmSectorProofsValid(rt *mock.Runtime, conf proveCommitConf, precommitEpoch abi.ChainEpoch, precommits ...*miner.SectorPreCommitInfo) {
	// expect calls to get network stats
	expectQueryNetworkInfo(rt, h)

	// Prepare for and receive call to ConfirmSectorProofsValid.
	var validPrecommits []*miner.SectorPreCommitInfo
	var allSectorNumbers []abi.SectorNumber
	for _, precommit := range precommits {
		allSectorNumbers = append(allSectorNumbers, precommit.SectorNumber)

		vdParams := market.ActivateDealsParams{
			DealIDs:      precommit.DealIDs,
			SectorExpiry: precommit.Expiration,
		}
		exit, found := conf.verifyDealsExit[precommit.SectorNumber]
		if !found {
			exit = exitcode.Ok
			validPrecommits = append(validPrecommits, precommit)
		}
		rt.ExpectSend(builtin.StorageMarketActorAddr, builtin.MethodsMarket.ActivateDeals, &vdParams, big.Zero(), nil, exit)
	}

	// expected pledge is the sum of initial pledges
	if len(validPrecommits) > 0 {
		expectPledge := big.Zero()

		expectQAPower := big.Zero()
		expectRawPower := big.Zero()
		for _, precommit := range validPrecommits {
			precommitOnChain := h.getPreCommit(rt, precommit.SectorNumber)

			qaPowerDelta := miner.QAPowerForWeight(h.sectorSize, precommit.Expiration-rt.Epoch(), precommitOnChain.DealWeight, precommitOnChain.VerifiedDealWeight)
			expectQAPower = big.Add(expectQAPower, qaPowerDelta)
			expectRawPower = big.Add(expectRawPower, big.NewIntUnsigned(uint64(h.sectorSize)))
			pledge := miner.InitialPledgeForPower(qaPowerDelta, h.networkQAPower, h.baselinePower,
				h.networkPledge, h.epochReward, rt.TotalFilCircSupply())
			expectPledge = big.Add(expectPledge, pledge)
		}

		pcParams := power.UpdateClaimedPowerParams{
			RawByteDelta:         expectRawPower,
			QualityAdjustedDelta: expectQAPower,
		}
		rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.UpdateClaimedPower, &pcParams, big.Zero(), nil, exitcode.Ok)
		rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.UpdatePledgeTotal, &expectPledge, big.Zero(), nil, exitcode.Ok)
	}

	rt.SetCaller(builtin.StoragePowerActorAddr, builtin.StoragePowerActorCodeID)
	rt.ExpectValidateCallerAddr(builtin.StoragePowerActorAddr)
	rt.Call(h.a.ConfirmSectorProofsValid, &builtin.ConfirmSectorProofsParams{Sectors: allSectorNumbers})
	rt.Verify()
}

func (h *actorHarness) proveCommitSectorAndConfirm(rt *mock.Runtime, precommit *miner.SectorPreCommitInfo, precommitEpoch abi.ChainEpoch,
	params *miner.ProveCommitSectorParams, conf proveCommitConf) *miner.SectorOnChainInfo {
	h.proveCommitSector(rt, precommit, precommitEpoch, params)
	h.confirmSectorProofsValid(rt, conf, precommitEpoch, precommit)

	newSector := h.getSector(rt, params.SectorNumber)
	return newSector
}

// Pre-commits and then proves a number of sectors.
// The sectors will expire at the end of lifetimePeriods proving periods after now.
// The runtime epoch will be moved forward to the epoch of commitment proofs.
func (h *actorHarness) commitAndProveSectors(rt *mock.Runtime, n int, lifetimePeriods uint64, dealIDs [][]abi.DealID) []*miner.SectorOnChainInfo {
	precommitEpoch := rt.Epoch()
	deadline := h.deadline(rt)
	expiration := deadline.PeriodEnd() + abi.ChainEpoch(lifetimePeriods)*miner.WPoStProvingPeriod

	// Precommit
	precommits := make([]*miner.SectorPreCommitInfo, n)
	for i := 0; i < n; i++ {
		sectorNo := h.nextSectorNo
		var sectorDealIDs []abi.DealID
		if dealIDs != nil {
			sectorDealIDs = dealIDs[i]
		}
		precommit := h.makePreCommit(sectorNo, precommitEpoch-1, expiration, sectorDealIDs)
		h.preCommitSector(rt, precommit)
		precommits[i] = precommit
		h.nextSectorNo++
	}

	advanceToEpochWithCron(rt, h, precommitEpoch+miner.PreCommitChallengeDelay+1)

	info := []*miner.SectorOnChainInfo{}
	for _, pc := range precommits {
		sector := h.proveCommitSectorAndConfirm(rt, pc, precommitEpoch, makeProveCommit(pc.SectorNumber), proveCommitConf{})
		info = append(info, sector)
	}
	rt.Reset()
	return info
}

func (h *actorHarness) commitAndProveSector(rt *mock.Runtime, sectorNo abi.SectorNumber, lifetimePeriods uint64, dealIDs []abi.DealID) *miner.SectorOnChainInfo {
	precommitEpoch := rt.Epoch()
	deadline := h.deadline(rt)
	expiration := deadline.PeriodEnd() + abi.ChainEpoch(lifetimePeriods)*miner.WPoStProvingPeriod

	// Precommit
	precommit := h.makePreCommit(sectorNo, precommitEpoch-1, expiration, dealIDs)
	h.preCommitSector(rt, precommit)

	advanceToEpochWithCron(rt, h, precommitEpoch+miner.PreCommitChallengeDelay+1)

	sectorInfo := h.proveCommitSectorAndConfirm(rt, precommit, precommitEpoch, makeProveCommit(precommit.SectorNumber), proveCommitConf{})
	rt.Reset()
	return sectorInfo
}

// Deprecated
func (h *actorHarness) advancePastProvingPeriodWithCron(rt *mock.Runtime) {
	st := getState(rt)
	deadline := st.DeadlineInfo(rt.Epoch())
	rt.SetEpoch(deadline.PeriodEnd())
	nextCron := deadline.NextPeriodStart() + miner.WPoStProvingPeriod - 1
	h.onDeadlineCron(rt, &cronConfig{
		expectedEntrollment: nextCron,
	})
	rt.SetEpoch(deadline.NextPeriodStart())
}

type poStConfig struct {
	expectedRawPowerDelta abi.StoragePower
	expectedQAPowerDelta  abi.StoragePower
	expectedPenalty       abi.TokenAmount
}

func (h *actorHarness) submitWindowPoSt(rt *mock.Runtime, deadline *miner.DeadlineInfo, partitions []miner.PoStPartition, infos []*miner.SectorOnChainInfo, poStCfg *poStConfig) {
	rt.SetCaller(h.worker, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerAddr(h.worker)

	expectQueryNetworkInfo(rt, h)

	var registeredPoStProof, err = h.sealProofType.RegisteredWindowPoStProof()
	require.NoError(h.t, err)

	proofs := make([]abi.PoStProof, 1) // Number of proofs doesn't depend on partition count
	for i := range proofs {
		proofs[i].PoStProof = registeredPoStProof
		proofs[i].ProofBytes = []byte(fmt.Sprintf("proof%d", i))
	}
	challengeRand := abi.SealRandomness([]byte{10, 11, 12, 13})

	allSkipped := map[abi.SectorNumber]struct{}{}
	for _, p := range partitions {
		_ = p.Skipped.ForEach(func(u uint64) error {
			allSkipped[abi.SectorNumber(u)] = struct{}{}
			return nil
		})
	}

	// find the first non-faulty sector in poSt to replace all faulty sectors.
	var goodInfo *miner.SectorOnChainInfo
	for _, ci := range infos {
		if _, contains := allSkipped[ci.SectorNumber]; !contains {
			goodInfo = ci
			break
		}
	}

	// goodInfo == nil indicates all the sectors have been skipped and should PoSt verification should not occur
	if goodInfo != nil {
		var buf bytes.Buffer
		err := rt.Receiver().MarshalCBOR(&buf)
		require.NoError(h.t, err)

		rt.ExpectGetRandomness(crypto.DomainSeparationTag_WindowedPoStChallengeSeed, deadline.Challenge, buf.Bytes(), abi.Randomness(challengeRand))

		actorId, err := addr.IDFromAddress(h.receiver)
		require.NoError(h.t, err)

		// if not all sectors are skipped
		proofInfos := make([]abi.SectorInfo, len(infos))
		for i, ci := range infos {
			si := ci
			_, contains := allSkipped[ci.SectorNumber]
			if contains {
				si = goodInfo
			}
			proofInfos[i] = abi.SectorInfo{
				SealProof:    si.SealProof,
				SectorNumber: si.SectorNumber,
				SealedCID:    si.SealedCID,
			}
		}

		vi := abi.WindowPoStVerifyInfo{
			Randomness:        abi.PoStRandomness(challengeRand),
			Proofs:            proofs,
			ChallengedSectors: proofInfos,
			Prover:            abi.ActorID(actorId),
		}
		rt.ExpectVerifyPoSt(vi, nil)
	}
	if poStCfg != nil {
		// expect power update
		if !poStCfg.expectedRawPowerDelta.IsZero() || !poStCfg.expectedQAPowerDelta.IsZero() {
			claim := &power.UpdateClaimedPowerParams{
				RawByteDelta:         poStCfg.expectedRawPowerDelta,
				QualityAdjustedDelta: poStCfg.expectedQAPowerDelta,
			}
			rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.UpdateClaimedPower, claim, abi.NewTokenAmount(0),
				nil, exitcode.Ok)
		}
		if !poStCfg.expectedPenalty.IsZero() {
			rt.ExpectSend(builtin.BurntFundsActorAddr, builtin.MethodSend, nil, poStCfg.expectedPenalty, nil, exitcode.Ok)
		}
		pledgeDelta := poStCfg.expectedPenalty.Neg()
		if !pledgeDelta.IsZero() {
			rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.UpdatePledgeTotal, &pledgeDelta,
				abi.NewTokenAmount(0), nil, exitcode.Ok)
		}
		//skipped = *poStCfg.skipped
	}

	params := miner.SubmitWindowedPoStParams{
		Deadline:   deadline.Index,
		Partitions: partitions,
		Proofs:     proofs,
	}

	rt.Call(h.a.SubmitWindowedPoSt, &params)
	rt.Verify()
}

func (h *actorHarness) computePartitions(rt *mock.Runtime, deadlines *miner.Deadlines, deadlineIdx uint64) ([]*miner.SectorOnChainInfo, []uint64) {
	panic("todo")
	// TODO minerstate
	//st := getState(rt)
	//firstPartIdx, sectorCount, err := miner.PartitionsForDeadline(deadlines, h.partitionSize, deadlineIdx)
	//require.NoError(h.t, err)
	//if sectorCount == 0 {
	//	return nil, nil
	//}
	//partitionCount, _, err := miner.DeadlineCount(deadlines, h.partitionSize, deadlineIdx)
	//require.NoError(h.t, err)
	//
	//partitions := make([]uint64, partitionCount)
	//for i := uint64(0); i < partitionCount; i++ {
	//	partitions[i] = firstPartIdx + i
	//}
	//
	//partitionsSectors, err := miner.ComputePartitionsSectors(deadlines, h.partitionSize, deadlineIdx, partitions)
	//require.NoError(h.t, err)
	//provenSectors, err := bitfield.MultiMerge(partitionsSectors...)
	//require.NoError(h.t, err)
	//infos, _, err := st.LoadSectorInfosForProof(rt.AdtStore(), provenSectors)
	//require.NoError(h.t, err)
	//
	//return infos, partitions
}

func (h *actorHarness) declareFaults(rt *mock.Runtime, fee abi.TokenAmount, faultSectorInfos ...*miner.SectorOnChainInfo) {
	rt.SetCaller(h.worker, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerAddr(h.worker)

	ss, err := faultSectorInfos[0].SealProof.SectorSize()
	require.NoError(h.t, err)
	expectedRawDelta, expectedQADelta := powerForSectors(ss, faultSectorInfos)
	expectedRawDelta = expectedRawDelta.Neg()
	expectedQADelta = expectedQADelta.Neg()

	expectQueryNetworkInfo(rt, h)

	// expect power update
	claim := &power.UpdateClaimedPowerParams{
		RawByteDelta:         expectedRawDelta,
		QualityAdjustedDelta: expectedQADelta,
	}
	rt.ExpectSend(
		builtin.StoragePowerActorAddr,
		builtin.MethodsPower.UpdateClaimedPower,
		claim,
		abi.NewTokenAmount(0),
		nil,
		exitcode.Ok,
	)

	// expect fee
	rt.ExpectSend(
		builtin.BurntFundsActorAddr,
		builtin.MethodSend,
		nil,
		fee,
		nil,
		exitcode.Ok,
	)

	// expect pledge update
	pledgeDelta := fee.Neg()
	rt.ExpectSend(
		builtin.StoragePowerActorAddr,
		builtin.MethodsPower.UpdatePledgeTotal,
		&pledgeDelta,
		abi.NewTokenAmount(0),
		nil,
		exitcode.Ok,
	)

	// Calculate params from faulted sector infos
	st := getState(rt)
	params := makeFaultParamsFromFaultingSectors(h.t, st, rt.AdtStore(), faultSectorInfos)
	rt.Call(h.a.DeclareFaults, params)
	rt.Verify()
}

func (h *actorHarness) declareRecoveries(rt *mock.Runtime, deadlineIdx uint64, recoverySectors *bitfield.BitField) {
	rt.SetCaller(h.worker, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerAddr(h.worker)

	expectQueryNetworkInfo(rt, h)

	// Calculate params from faulted sector infos
	params := &miner.DeclareFaultsRecoveredParams{Recoveries: []miner.RecoveryDeclaration{{
		Deadline: deadlineIdx,
		Sectors:  recoverySectors,
	}}}

	rt.Call(h.a.DeclareFaultsRecovered, params)
	rt.Verify()
}

func (h *actorHarness) advanceProvingPeriodWithoutFaults(rt *mock.Runtime) {

	// Iterate deadlines in the proving period, setting epoch to the first in each deadline.
	// Submit a window post for all partitions due at each deadline when necessary.
	deadline := h.deadline(rt)
	for !deadline.PeriodElapsed() {
		// TODO minerstate
		//st := getState(rt)
		//store := rt.AdtStore()
		//deadlines, err := st.LoadDeadlines(store)
		//builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "could not load deadlines")

		//firstPartIdx, sectorCount, err := miner.PartitionsForDeadline(deadlines, h.partitionSize, deadline.Index)
		//builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "could not get partitions for deadline")
		//if sectorCount != 0 {
		//	partitionCount, _, err := miner.DeadlineCount(deadlines, h.partitionSize, deadline.Index)
		//	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "could not get partition count")
		//
		//	partitions := make([]uint64, partitionCount)
		//	for i := uint64(0); i < partitionCount; i++ {
		//		partitions[i] = firstPartIdx + i
		//	}
		//
		//	partitionsSectors, err := miner.ComputePartitionsSectors(deadlines, h.partitionSize, deadline.Index, partitions)
		//	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "could not compute partitions")
		//	provenSectors, err := bitfield.MultiMerge(partitionsSectors...)
		//	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "could not get proven sectors")
		//	infos, _, err := st.LoadSectorInfosForProof(store, provenSectors)
		//	builtin.RequireNoErr(rt, err, exitcode.ErrIllegalState, "could not load sector info for proof")
		//
		//	h.submitWindowPoSt(rt, deadline, partitions, infos, nil)
		//}

		rt.SetEpoch(deadline.NextOpen())
		deadline = h.deadline(rt)
	}
	// Rewind one epoch to leave the current epoch as the penultimate one in the proving period,
	// ready for proving-period cron.
	rt.SetEpoch(rt.Epoch() - 1)
}

func (h *actorHarness) extendSector(rt *mock.Runtime, sector *miner.SectorOnChainInfo, extension abi.ChainEpoch, params *miner.ExtendSectorExpirationParams) {
	rt.SetCaller(h.worker, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerAddr(h.worker)

	newSector := *sector
	newSector.Expiration += extension
	qaDelta := big.Sub(miner.QAPowerForSector(h.sectorSize, &newSector), miner.QAPowerForSector(h.sectorSize, sector))

	rt.ExpectSend(builtin.StoragePowerActorAddr,
		builtin.MethodsPower.UpdateClaimedPower,
		&power.UpdateClaimedPowerParams{
			RawByteDelta:         big.Zero(),
			QualityAdjustedDelta: qaDelta,
		},
		abi.NewTokenAmount(0),
		nil,
		exitcode.Ok,
	)
	rt.Call(h.a.ExtendSectorExpiration, params)
	rt.Verify()
}

func (h *actorHarness) terminateSectors(rt *mock.Runtime, sectors *abi.BitField, expectedFee abi.TokenAmount) {
	rt.SetCaller(h.worker, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerAddr(h.worker)

	dealIDs := []abi.DealID{}
	sectorInfos := []*miner.SectorOnChainInfo{}
	err := sectors.ForEach(func(secNum uint64) error {
		sector := h.getSector(rt, abi.SectorNumber(secNum))
		dealIDs = append(dealIDs, sector.DealIDs...)

		sectorInfos = append(sectorInfos, sector)
		return nil
	})
	require.NoError(h.t, err)

	{
		expectQueryNetworkInfo(rt, h)
	}

	{
		// TODO minerstate
		//rawPower, qaPower := miner.PowerForSectors(h.sectorSize, sectorInfos)
		//rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.UpdateClaimedPower, &power.UpdateClaimedPowerParams{
		//	RawByteDelta:         rawPower.Neg(),
		//	QualityAdjustedDelta: qaPower.Neg(),
		//}, abi.NewTokenAmount(0), nil, exitcode.Ok)
	}
	if big.Zero().LessThan(expectedFee) {
		rt.ExpectSend(builtin.BurntFundsActorAddr, builtin.MethodSend, nil, expectedFee, nil, exitcode.Ok)
		pledgeDelta := expectedFee.Neg()
		rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.UpdatePledgeTotal, &pledgeDelta, big.Zero(), nil, exitcode.Ok)
	}

	// TODO minerstate
	//params := &miner.TerminateSectorsParams{Sectors: sectors}
	//rt.Call(h.a.TerminateSectors, params)
	//rt.Verify()
}

func (h *actorHarness) reportConsensusFault(rt *mock.Runtime, from addr.Address, params *miner.ReportConsensusFaultParams, dealIDs []abi.DealID) {
	rt.SetCaller(from, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerType(builtin.CallerTypesSignable...)

	rt.ExpectVerifyConsensusFault(params.BlockHeader1, params.BlockHeader2, params.BlockHeaderExtra, &runtime.ConsensusFault{
		Target: h.receiver,
		Epoch:  rt.Epoch() - 1,
		Type:   runtime.ConsensusFaultDoubleForkMining,
	}, nil)

	// slash reward
	reward := miner.RewardForConsensusSlashReport(1, rt.Balance())
	rt.ExpectSend(from, builtin.MethodSend, nil, reward, nil, exitcode.Ok)

	// power termination
	lockedFunds := getState(rt).LockedFunds
	rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.OnConsensusFault, &lockedFunds, abi.NewTokenAmount(0), nil, exitcode.Ok)

	// expect every deal to be closed out
	rt.ExpectSend(builtin.StorageMarketActorAddr, builtin.MethodsMarket.OnMinerSectorsTerminate, &market.OnMinerSectorsTerminateParams{
		DealIDs: dealIDs,
	}, abi.NewTokenAmount(0), nil, exitcode.Ok)

	// expect actor to be deleted
	rt.ExpectDeleteActor(builtin.BurntFundsActorAddr)

	rt.Call(h.a.ReportConsensusFault, params)
	rt.Verify()
}

func (h *actorHarness) addLockedFund(rt *mock.Runtime, amt abi.TokenAmount) {
	rt.SetCaller(h.worker, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerAddr(h.worker, h.owner, builtin.RewardActorAddr)
	// expect pledge update
	rt.ExpectSend(
		builtin.StoragePowerActorAddr,
		builtin.MethodsPower.UpdatePledgeTotal,
		&amt,
		abi.NewTokenAmount(0),
		nil,
		exitcode.Ok,
	)

	rt.Call(h.a.AddLockedFund, &amt)
	rt.Verify()
}

type cronConfig struct {
	expectedEntrollment       abi.ChainEpoch
	vestingPledgeDelta        abi.TokenAmount // nolint:structcheck,unused
	detectedFaultsPowerDelta  *miner.PowerPair
	detectedFaultsPenalty     abi.TokenAmount
	expiredSectorsPowerDelta  *miner.PowerPair
	expiredSectorsPledgeDelta abi.TokenAmount
	ongoingFaultsPenalty      abi.TokenAmount
}

func (h *actorHarness) onDeadlineCron(rt *mock.Runtime, config *cronConfig) {
	rt.ExpectValidateCallerAddr(builtin.StoragePowerActorAddr)

	// Preamble
	reward := reward.ThisEpochRewardReturn{
		ThisEpochReward:        h.epochReward,
		ThisEpochBaselinePower: h.baselinePower,
	}
	rt.ExpectSend(builtin.RewardActorAddr, builtin.MethodsReward.ThisEpochReward, nil, big.Zero(), &reward, exitcode.Ok)
	networkPower := big.NewIntUnsigned(1 << 50)
	rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.CurrentTotalPower, nil, big.Zero(),
		&power.CurrentTotalPowerReturn{
			RawBytePower:     networkPower,
			QualityAdjPower:  networkPower,
			PledgeCollateral: h.networkPledge,
		},
		exitcode.Ok)

	powerDelta := miner.NewPowerPairZero()
	if config.detectedFaultsPowerDelta != nil {
		powerDelta = powerDelta.Add(*config.detectedFaultsPowerDelta)
	}
	if config.expiredSectorsPowerDelta != nil {
		powerDelta = powerDelta.Add(*config.expiredSectorsPowerDelta)
	}

	if !powerDelta.IsZero() {
		rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.UpdateClaimedPower, &power.UpdateClaimedPowerParams{
			RawByteDelta:         powerDelta.Raw,
			QualityAdjustedDelta: powerDelta.QA,
		},
			abi.NewTokenAmount(0), nil, exitcode.Ok)
	}

	penaltyTotal := big.Zero()
	pledgeDelta := big.Zero()
	if !config.detectedFaultsPenalty.Nil() && !config.detectedFaultsPenalty.IsZero() {
		penaltyTotal = big.Add(penaltyTotal, config.detectedFaultsPenalty)
	}
	if !config.ongoingFaultsPenalty.Nil() && !config.ongoingFaultsPenalty.IsZero() {
		penaltyTotal = big.Add(penaltyTotal, config.ongoingFaultsPenalty)
	}
	if !penaltyTotal.IsZero() {
		rt.ExpectSend(builtin.BurntFundsActorAddr, builtin.MethodSend, nil, penaltyTotal, nil, exitcode.Ok)
		pledgeDelta = big.Sub(pledgeDelta, penaltyTotal)
	}

	if !config.expiredSectorsPledgeDelta.Nil() && !config.expiredSectorsPledgeDelta.IsZero() {
		pledgeDelta = big.Add(pledgeDelta, config.expiredSectorsPledgeDelta)
	}
	if !pledgeDelta.IsZero() {
		rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.UpdatePledgeTotal, &pledgeDelta, big.Zero(), nil, exitcode.Ok)
	}

	// Re-enrollment for next period.
	rt.ExpectSend(builtin.StoragePowerActorAddr, builtin.MethodsPower.EnrollCronEvent,
		makeDeadlineCronEventParams(h.t, config.expectedEntrollment), big.Zero(), nil, exitcode.Ok)

	rt.SetCaller(builtin.StoragePowerActorAddr, builtin.StoragePowerActorCodeID)
	rt.Call(h.a.OnDeferredCronEvent, &miner.CronEventPayload{
		EventType: miner.CronEventProvingDeadline,
	})
	rt.Verify()
}

func (h *actorHarness) withdrawFunds(rt *mock.Runtime, amount abi.TokenAmount) {
	rt.SetCaller(h.owner, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerAddr(h.owner)

	rt.ExpectSend(h.owner, builtin.MethodSend, nil, amount, nil, exitcode.Ok)

	rt.Call(h.a.WithdrawBalance, &miner.WithdrawBalanceParams{
		AmountRequested: amount,
	})
	rt.Verify()
}

func (h *actorHarness) declaredFaultPenalty(sectors []*miner.SectorOnChainInfo) abi.TokenAmount {
	_, qa := powerForSectors(h.sectorSize, sectors)
	return miner.PledgePenaltyForDeclaredFault(h.epochReward, h.networkQAPower, qa)
}

func (h *actorHarness) undeclaredFaultPenalty(sectors []*miner.SectorOnChainInfo) abi.TokenAmount {
	_, qa := powerForSectors(h.sectorSize, sectors)
	return miner.PledgePenaltyForUndeclaredFault(h.epochReward, h.networkQAPower, qa)
}

func (h *actorHarness) powerPairForSectors(sectors []*miner.SectorOnChainInfo) miner.PowerPair {
	rawPower, qaPower := powerForSectors(h.sectorSize, sectors)
	return miner.NewPowerPair(rawPower, qaPower)
}

func (h *actorHarness) makePreCommit(sectorNo abi.SectorNumber, challenge, expiration abi.ChainEpoch, dealIDs []abi.DealID) *miner.SectorPreCommitInfo {
	return &miner.SectorPreCommitInfo{
		SealProof:     h.sealProofType,
		SectorNumber:  sectorNo,
		SealedCID:     tutil.MakeCID("commr", &miner.SealedCIDPrefix),
		SealRandEpoch: challenge,
		DealIDs:       dealIDs,
		Expiration:    expiration,
	}
}

//
// Higher-level orchestration
//

// Completes a proving period by moving the epoch forward to the penultimate one, calling the proving period cron handler,
// and then advancing to the first epoch in the new period.
func completeProvingPeriod(rt *mock.Runtime, h *actorHarness, config *cronConfig) {
	deadline := h.deadline(rt)
	rt.SetEpoch(deadline.PeriodEnd())
	config.expectedEntrollment = deadline.NextPeriodStart() + miner.WPoStProvingPeriod - 1
	h.onDeadlineCron(rt, config)
	rt.SetEpoch(deadline.NextPeriodStart())
}

// Completes a deadline by moving the epoch forward to the penultimate one, calling the deadline cron handler,
// and then advancing to the first epoch in the new deadline.
func advanceDeadline(rt *mock.Runtime, h *actorHarness, config *cronConfig) {
	deadline := h.deadline(rt)
	rt.SetEpoch(deadline.Last())
	config.expectedEntrollment = deadline.Last() + miner.WPoStChallengeWindow
	h.onDeadlineCron(rt, config)
	rt.SetEpoch(deadline.NextOpen())
}

func advanceToEpochWithCron(rt *mock.Runtime, h *actorHarness, e abi.ChainEpoch) {
	deadline := h.deadline(rt)
	for e > deadline.Last() {
		advanceDeadline(rt, h, &cronConfig{})
		deadline = h.deadline(rt)
	}
	rt.SetEpoch(e)
}

//
// Construction helpers, etc
//

func builderForHarness(actor *actorHarness) *mock.RuntimeBuilder {
	return mock.NewBuilder(context.Background(), actor.receiver).
		WithActorType(actor.owner, builtin.AccountActorCodeID).
		WithActorType(actor.worker, builtin.AccountActorCodeID).
		WithHasher(fixedHasher(uint64(actor.periodOffset)))
}

func getState(rt *mock.Runtime) *miner.State {
	var st miner.State
	rt.GetState(&st)
	return &st
}

func makeDeadlineCronEventParams(t testing.TB, epoch abi.ChainEpoch) *power.EnrollCronEventParams {
	eventPayload := miner.CronEventPayload{EventType: miner.CronEventProvingDeadline}
	buf := bytes.Buffer{}
	err := eventPayload.MarshalCBOR(&buf)
	require.NoError(t, err)
	return &power.EnrollCronEventParams{
		EventEpoch: epoch,
		Payload:    buf.Bytes(),
	}
}

func makeProveCommit(sectorNo abi.SectorNumber) *miner.ProveCommitSectorParams {
	return &miner.ProveCommitSectorParams{
		SectorNumber: sectorNo,
		Proof:        []byte("proof"),
	}
}

func makeFaultParamsFromFaultingSectors(t testing.TB, st *miner.State, store adt.Store, faultSectorInfos []*miner.SectorOnChainInfo) *miner.DeclareFaultsParams {
	//deadlines, err := st.LoadDeadlines(store)
	//require.NoError(t, err)
	faultAtDeadline := make(map[uint64][]uint64)
	// TODO minerstate
	// Find the deadline for each faulty sector which must be provided with the fault declaration
	//for _, sectorInfo := range faultSectorInfos {
	//	dl, p, err := miner.FindSector(deadlines, sectorInfo.SectorNumber)
	//	require.NoError(t, err)
	//	faultAtDeadline[dl] = append(faultAtDeadline[dl], uint64(sectorInfo.SectorNumber))
	//}
	params := &miner.DeclareFaultsParams{Faults: []miner.FaultDeclaration{}}
	// Group together faults at the same deadline into a bitfield
	for dl, sectorNumbers := range faultAtDeadline {
		fault := miner.FaultDeclaration{
			Deadline: dl,
			Sectors:  bitfield.NewFromSet(sectorNumbers),
		}
		params.Faults = append(params.Faults, fault)
	}
	return params
}

func sectorInfoAsBitfield(infos []*miner.SectorOnChainInfo) *bitfield.BitField {
	bf := bitfield.New()
	for _, info := range infos {
		bf.Set(uint64(info.SectorNumber))
	}
	return &bf
}

func powerForSectors(sectorSize abi.SectorSize, sectors []*miner.SectorOnChainInfo) (rawBytePower, qaPower big.Int) {
	rawBytePower = big.Mul(big.NewIntUnsigned(uint64(sectorSize)), big.NewIntUnsigned(uint64(len(sectors))))
	qaPower = big.Zero()
	for _, s := range sectors {
		qaPower = big.Add(qaPower, miner.QAPowerForSector(sectorSize, s))
	}
	return rawBytePower, qaPower
}

func assertEmptyBitfield(t *testing.T, b *abi.BitField) {
	empty, err := b.IsEmpty()
	require.NoError(t, err)
	assert.True(t, empty)
}

// Returns a fake hashing function that always arranges the first 8 bytes of the digest to be the binary
// encoding of a target uint64.
func fixedHasher(target uint64) func([]byte) [32]byte {
	return func(_ []byte) [32]byte {
		var buf bytes.Buffer
		err := binary.Write(&buf, binary.BigEndian, target)
		if err != nil {
			panic(err)
		}
		var digest [32]byte
		copy(digest[:], buf.Bytes())
		return digest
	}
}

func expectQueryNetworkInfo(rt *mock.Runtime, h *actorHarness) {
	currentPower := power.CurrentTotalPowerReturn{
		RawBytePower:     h.networkRawPower,
		QualityAdjPower:  h.networkQAPower,
		PledgeCollateral: h.networkPledge,
	}
	currentReward := reward.ThisEpochRewardReturn{
		ThisEpochReward:        h.epochReward,
		ThisEpochBaselinePower: h.baselinePower,
	}

	rt.ExpectSend(
		builtin.RewardActorAddr,
		builtin.MethodsReward.ThisEpochReward,
		nil,
		big.Zero(),
		&currentReward,
		exitcode.Ok,
	)

	rt.ExpectSend(
		builtin.StoragePowerActorAddr,
		builtin.MethodsPower.CurrentTotalPower,
		nil,
		big.Zero(),
		&currentPower,
		exitcode.Ok,
	)
}
