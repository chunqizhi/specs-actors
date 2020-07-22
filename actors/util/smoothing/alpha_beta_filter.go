package smoothing

import (
	gbig "math/big"

	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/abi/big"
	"github.com/filecoin-project/specs-actors/actors/util/math"
)

var (
	// Coefficents in Q.128 format
	lnNumCoef   []*gbig.Int
	lnDenomCoef []*gbig.Int
	ln2         big.Int

	initialPosition big.Int
	initialVelocity big.Int

	DefaultAlpha                   big.Int // Q.128 value of 0.000164
	DefaultBeta                    big.Int //  Q.128 value of 0.000115
	ExtrapolatedCumSumRatioEpsilon big.Int // Q.128 value of 2^-50
)

func init() {
	initialPosition = big.Zero()
	initialVelocity = big.Zero()

	// ln approximation coefficients
	// parameters are in integer format,
	// coefficients are *2^-128 of that
	// so we can just load them if we treat them as Q.128
	num := []string{
		"261417938209272870992496419296200268025",
		"7266615505142943436908456158054846846897",
		"32458783941900493142649393804518050491988",
		"17078670566130897220338060387082146864806",
		"-35150353308172866634071793531642638290419",
		"-20351202052858059355702509232125230498980",
		"-1563932590352680681114104005183375350999",
	}
	lnNumCoef = math.Parse(num)

	denom := []string{
		"49928077726659937662124949977867279384",
		"2508163877009111928787629628566491583994",
		"21757751789594546643737445330202599887121",
		"53400635271583923415775576342898617051826",
		"41248834748603606604000911015235164348839",
		"9015227820322455780436733526367238305537",
		"340282366920938463463374607431768211456",
	}
	lnDenomCoef = math.Parse(denom)

	// Alpha Beta Filter constants
	constStrs := []string{
		"55806300000000000000000000000000000",     // DefaultAlpha
		"39132500000000000000000000000000000",     // DefaultBeta
		"302231454903657293676544",                // Epsilon
		"235865763225513294137944142764154484399", // ln(2)
	}
	constBigs := math.Parse(constStrs)
	_ = math.Parse(constStrs)
	DefaultAlpha = big.Int{Int: constBigs[0]}
	DefaultBeta = big.Int{Int: constBigs[1]}
	ExtrapolatedCumSumRatioEpsilon = big.Int{Int: constBigs[2]}
	ln2 = big.Int{Int: constBigs[3]}
}

// Alpha Beta Filter "position" (value) and "velocity" (rate of change of value) estimates
// Estimates are in Q.128 format
type FilterEstimate struct {
	PositionEstimate big.Int // Q.128
	VelocityEstimate big.Int // Q.128
}

// Returns the Q.0 position estimate of the filter
func (fe *FilterEstimate) Estimate() big.Int {
	return big.Rsh(fe.PositionEstimate, math.Precision) // Q.128 => Q.0
}

func InitialEstimate() *FilterEstimate {
	return &FilterEstimate{
		PositionEstimate: initialPosition,
		VelocityEstimate: initialVelocity,
	}
}

type AlphaBetaFilter struct {
	prevEstimate *FilterEstimate
	alpha        big.Int // Q.128
	beta         big.Int // Q.128
}

func LoadFilter(prevEstimate *FilterEstimate, alpha, beta big.Int) *AlphaBetaFilter {
	return &AlphaBetaFilter{
		prevEstimate: prevEstimate,
		alpha:        alpha,
		beta:         beta,
	}
}

func (f *AlphaBetaFilter) NextEstimate(observation big.Int, epochDelta abi.ChainEpoch) *FilterEstimate {
	deltaT := big.Lsh(big.NewInt(int64(epochDelta)), math.Precision) // Q.0 => Q.128
	deltaX := big.Mul(deltaT, f.prevEstimate.VelocityEstimate)       // Q.128 * Q.128 => Q.256
	deltaX = big.Rsh(deltaX, math.Precision)                         // Q.256 => Q.128
	position := big.Sum(f.prevEstimate.PositionEstimate, deltaX)

	observation = big.Lsh(observation, math.Precision) // Q.0 => Q.128
	residual := big.Sub(observation, position)
	revisionX := big.Mul(f.alpha, residual)        // Q.128 * Q.128 => Q.256
	revisionX = big.Rsh(revisionX, math.Precision) // Q.256 => Q.128
	position = big.Sum(position, revisionX)

	revisionV := big.Mul(f.beta, residual) // Q.128 * Q.128 => Q.256
	revisionV = big.Div(revisionV, deltaT) // Q.256 / Q.128 => Q.128
	velocity := big.Sum(f.prevEstimate.VelocityEstimate, revisionV)

	return &FilterEstimate{
		PositionEstimate: position,
		VelocityEstimate: velocity,
	}
}

func ExtrapolatedCumSumOfRatio(delta abi.ChainEpoch, relativeStart abi.ChainEpoch, estimateNum, estimateDenom *FilterEstimate) big.Int {
	deltaT := big.Lsh(big.NewInt(int64(delta)), math.Precision)     // Q.0 => Q.128
	t0 := big.Lsh(big.NewInt(int64(relativeStart)), math.Precision) // Q.0 => Q.128
	// Renaming for ease of following spec and clarity
	position1 := estimateNum.PositionEstimate
	position2 := estimateDenom.PositionEstimate
	velocity1 := estimateNum.VelocityEstimate
	velocity2 := estimateDenom.VelocityEstimate

	squaredVelocity2 := big.Mul(velocity2, velocity2)            // Q.128 * Q.128 => Q.256
	squaredVelocity2 = big.Rsh(squaredVelocity2, math.Precision) // Q.256 => Q.128

	if squaredVelocity2.GreaterThan(ExtrapolatedCumSumRatioEpsilon) {
		x2a := big.Mul(t0, velocity2)      // Q.128 * Q.128 => Q.256
		x2a = big.Rsh(x2a, math.Precision) // Q.256 => Q.128
		x2a = big.Sum(position2, x2a)

		x2b := big.Mul(deltaT, velocity2)  // Q.128 * Q.128 => Q.256
		x2b = big.Rsh(x2b, math.Precision) // Q.256 => Q.128
		x2b = big.Sum(x2a, x2b)

		x2a = ln(x2a) // Q.128
		x2b = ln(x2b) // Q.128

		m1 := big.Sub(x2b, x2a)
		m1 = big.Mul(velocity2, big.Mul(position1, m1)) // Q.128 * Q.128 * Q.128 => Q.384
		m1 = big.Rsh(m1, math.Precision*2)              // Q.384 => Q.128

		m2L := big.Sub(x2a, x2b)
		m2L = big.Mul(position2, m2L)     // Q.128 * Q.128 => Q.256
		m2R := big.Mul(velocity2, deltaT) // Q.128 * Q.128 => Q.256
		m2 := big.Sum(m2L, m2R)
		m2 = big.Mul(velocity1, m2)        // Q.256 => Q.384
		m2 = big.Rsh(m2, math.Precision*2) // Q.384 => Q.128

		return big.Div(big.Sum(m1, m2), squaredVelocity2) // Q.128 / Q.128 => Q.0

	}

	halfDeltaT := big.Div(deltaT, big.NewInt(2))                          // Q.128 / Q.0 => Q.128
	x1m := big.Mul(estimateNum.VelocityEstimate, big.Sum(t0, halfDeltaT)) // Q.128 * Q.128 => Q.256
	x1m = big.Rsh(x1m, math.Precision)                                    // Q.256 => Q.128
	x1m = big.Add(estimateNum.PositionEstimate, x1m)

	cumsumRatio := big.Mul(x1m, deltaT)                                // Q.128 * Q.128 => Q.256
	cumsumRatio = big.Div(cumsumRatio, estimateDenom.PositionEstimate) // Q.256 / Q.128 => Q.128
	return big.Rsh(cumsumRatio, math.Precision)                        // Q.128 => Q.0
}

// The natural log of Q.128 x.
func ln(z big.Int) big.Int {
	// ln(z) = ln(x * 2^k) = ln(x) + k * ln2
	intK := uint(z.BitLen() - 1 - math.Precision)
	k := big.NewInt(int64(intK))   // Q.0
	k = big.Lsh(k, math.Precision) // Q.0 => Q.128

	x := big.Zero()
	if k.GreaterThan(big.Zero()) {
		x = big.Rsh(z, intK) // Q.128
	} else {
		x = big.Lsh(z, intK) // Q.128
	}
	lnz := big.Mul(k, ln2)                     // Q.128 * Q.128 => Q.256
	lnz = big.Rsh(lnz, math.Precision)         // Q.256 => Q.128
	return big.Sum(lnz, lnBetweenOneAndTwo(x)) // Q.128
}

// The natural log of x, specified in Q.128 format
// Should only use with 1 <= x <= 2
// Output is in Q.128 format.
func lnBetweenOneAndTwo(x big.Int) big.Int {
	// ln is approximated by rational function
	// polynomials of the rational function are evaluated using Horner's method
	num := math.Polyval(lnNumCoef, x.Int)     // Q.128
	denom := math.Polyval(lnDenomCoef, x.Int) // Q.128

	num = num.Lsh(num, math.Precision)       // Q.128 => Q.256
	return big.Int{Int: num.Div(num, denom)} // Q.256 / Q.128 => Q.128
}
