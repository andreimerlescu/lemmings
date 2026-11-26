package swarm

import "time"

// RampDelays computes the n-1 inter-terrain launch delays for n terrains over
// total duration using shape (RampLinear, RampFibonacci, or RampStep).
func RampDelays(n int, total time.Duration, shape string) []time.Duration {
	if n <= 1 {
		return nil
	}
	gaps := n - 1
	delays := make([]time.Duration, gaps)
	if total <= 0 {
		return delays
	}

	switch shape {
	case RampFibonacci:
		// Longer warm-up gaps early, accelerating as later terrains surge online.
		weights := make([]float64, gaps)
		a, b := 1.0, 1.0
		var sum float64
		for i := gaps - 1; i >= 0; i-- {
			weights[i] = a
			sum += a
			a, b = b, a+b
			// Cap float growth for very large terrain counts.
			if b > 1e12 {
				b = 1e12
			}
		}
		for i := range delays {
			delays[i] = time.Duration(float64(total) * (weights[i] / sum))
		}
		return delays

	case RampStep:
		// Bring terrains online in 4 discrete plateaus to expose capacity cliffs.
		steps := min(4, n)
		stepSize := (n + steps - 1) / steps
		pauseCount := 0
		for i := range gaps {
			if (i+1)%stepSize == 0 {
				pauseCount++
			}
		}
		if pauseCount == 0 {
			return delays
		}
		pause := total / time.Duration(pauseCount)
		for i := range gaps {
			if (i+1)%stepSize == 0 {
				delays[i] = pause
			}
		}
		return delays

	default: // RampLinear
		interval := total / time.Duration(gaps)
		for i := range delays {
			delays[i] = interval
		}
		return delays
	}
}
