package consensus

import (
	"math"

	"github.com/dfinity/go-dfinity-crypto/bls"
	log "github.com/helinwang/log15"
)

// validator validates the data received from peers.
type validator struct {
	chain *Chain
}

func newValidator(chain *Chain) *validator {
	return &validator{chain: chain}
}

func rankToWeight(rank int) float64 {
	if rank < 0 {
		panic(rank)
	}
	return math.Pow(0.5, float64(rank))
}

func (v *validator) ValidateBlock(b *Block) (float64, bool) {
	// TODO: validate txns
	if depth := v.chain.RandomBeacon.Depth(); b.Round > depth {
		// TODO: sync with the sender
		log.Warn("received block of too high round, can't validate", "round", b.Round, "depth", depth)
		return 0, false
	}

	var sign bls.Sign
	err := sign.Deserialize(b.NotarizationSig)
	if err != nil {
		log.Warn("valid block sig error", "err", err)
		return 0, false
	}

	msg := string(b.Encode(false))
	_, _, nt := v.chain.RandomBeacon.Committees(b.Round)
	success := sign.Verify(&v.chain.RandomBeacon.groups[nt].PK, msg)
	if !success {
		log.Warn("validate block group sig failed", "group", nt, "block", b.Hash())
		return 0, false
	}

	rank, err := v.chain.RandomBeacon.Rank(b.Owner, b.Round)
	if err != nil {
		log.Error("error get rank, but group sig is valid", "err", err)
		return 0, false
	}

	return rankToWeight(rank), true
}

func (v *validator) ValidateBlockProposal(bp *BlockProposal) (float64, bool) {
	// TODO: validate sig, validate txns, validate owner, validate
	// round is correct
	round := v.chain.Round()
	if bp.Round != round {
		if bp.Round > round {
			log.Warn("received block proposal of higher round", "round", bp.Round, "my round", round)
		} else {
			log.Debug("received block proposal of lower round", "round", bp.Round, "my round", round)
		}

		return 0, false
	}
	return 0, true
}

func (v *validator) ValidateNtShare(n *NtShare) (int, bool) {
	round := v.chain.Round()
	if n.Round != round {
		if n.Round > round {
			log.Warn("received nt share of higher round", "round", n.Round, "my round", round)
		} else {
			log.Debug("received nt share of lower round", "round", n.Round, "my round", round)
		}
		return 0, false
	}

	_, _, nt := v.chain.RandomBeacon.Committees(round)
	// TODO: validate sig, validate owner, validate round is
	// correct, validate share is signed correctly.
	return nt, true
}

func (v *validator) ValidateRandBeaconSig(r *RandBeaconSig) bool {
	// TODO: validate sig, owner, round, share
	targetDepth := v.chain.RandomBeacon.Depth()
	if r.Round != targetDepth {
		if r.Round > targetDepth {
			log.Warn("received RandBeaconSig of higher round", "round", r.Round, "target depth", targetDepth)
		} else {
			log.Debug("received RandBeaconSig of lower round", "round", r.Round, "target depth", targetDepth)
		}
		return false
	}

	return true
}

func (v *validator) ValidateRandBeaconSigShare(r *RandBeaconSigShare) (int, bool) {
	targetDepth := v.chain.RandomBeacon.Depth()
	if r.Round != targetDepth {
		if r.Round > targetDepth {
			log.Warn("received RandBeaconSigShare of higher round", "round", r.Round, "target depth", targetDepth)
		} else {
			log.Debug("received RandBeaconSigShare of lower round", "round", r.Round, "target depth", targetDepth)
		}
		return 0, false
	}

	rb, _, _ := v.chain.RandomBeacon.Committees(targetDepth)
	// TODO: validate sig, owner, round
	return rb, true
}
