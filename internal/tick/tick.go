// Package tick chooses the tick size of a Logb time axis.
//
// A Logb time axis counts integer ticks of 10^axis_exp seconds (§5), so an
// importer holding seconds as a float has to pick an exponent. Both importers
// face the same question and it has the same answer, which is why this is here
// rather than in either of them: a femtosecond tick is right for a 1 ms
// transient and hopeless for a two-week vehicle recording.
package tick

import (
	"fmt"
	"math"
)

// Exponents are the tick sizes an importer will choose between, finest first:
// femtoseconds through milliseconds.
var Exponents = []int8{-15, -12, -9, -6, -3}

// Nanosecond is the finest tick a time.Duration can hold, and so the finest one
// an axis meant to be read as elapsed wall-clock time should use: AxisVal.Duration
// refuses anything below it (ErrTickTooFine).
const Nanosecond int8 = -9

// Exp picks the tick size for a run whose largest absolute time is max seconds:
// the finest exponent, no finer than finest, that still counts it exactly.
//
// The bound is not int64's range but 2^53, because Schema.AxisAt receives the
// explicit axis value as a float64 (Batch.Axis routes it through toFloat) and
// converts back with int64(). Past 2^53 that round trip starts losing whole
// ticks, so an axis that looked exact would quietly stop being so.
//
// finest is the caller's, because the two importers mean different things by
// time. A simulated transient's steps can genuinely be femtoseconds apart and
// its axis is not a clock, so it asks for everything it can get. A measurement
// recording's timestamps come from one, and resolving them past a nanosecond
// describes precision the instrument did not have while costing the axis its
// time.Duration.
func Exp(max float64, finest int8) (int8, error) {
	max = math.Abs(max)
	if math.IsInf(max, 0) || math.IsNaN(max) {
		return 0, fmt.Errorf("tick: axis maximum %g is not a finite number of seconds", max)
	}
	for _, exp := range Exponents {
		if exp < finest {
			continue
		}
		if max/math.Pow10(int(exp)) < 1<<53 {
			return exp, nil
		}
	}
	return 0, fmt.Errorf("tick: a span of %g s is too long for a %s tick", max, Unit(Exponents[len(Exponents)-1]))
}

// Of converts seconds to a count of 10^exp-second ticks.
func Of(sec float64, exp int8) int64 {
	return int64(math.Round(sec / math.Pow10(int(exp))))
}

// Unit names one tick, for a field that stores the count rather than the
// seconds. Without it a reader prints a nanosecond field's raw value with the
// axis's own unit and reports a 1 ms transient as eight million seconds.
func Unit(exp int8) string {
	switch exp {
	case -15:
		return "fs"
	case -12:
		return "ps"
	case -9:
		return "ns"
	case -6:
		return "us"
	case -3:
		return "ms"
	case 0:
		return "s"
	}
	return ""
}
