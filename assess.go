package main

// assess.go — turns two Samples into a verdict.

import "time"

type State int

const (
	StateOK State = iota
	StateStrained
	StatePressured
	StateThrashing
)

func (s State) String() string {
	return [...]string{"OK", "STRAINED", "PRESSURED", "THRASHING"}[s]
}

// Severity names the level without tying it to one resource; used for the
// overall verdict in --check output.
func (s State) Severity() string {
	return [...]string{"OK", "WARNING", "CRITICAL", "CRITICAL"}[s]
}

// Rates are per-second deltas over the sample window, in bytes/s.
type Rates struct {
	Window     float64 `json:"window_seconds"`
	SwapIn     float64 `json:"swap_in_bps"`
	SwapOut    float64 `json:"swap_out_bps"`
	Refault    float64 `json:"refault_bps"`
	DirectScan float64 `json:"direct_scan_bps"`
}

type Report struct {
	At     time.Time
	Source string
	Sample Sample

	State     State
	Estimated bool // no PSI: verdict inferred from paging rates
	Trend     int  // +1 rising, -1 easing, 0 steady

	NaiveUsed   float64 // % — what free/top imply
	RealUsed    float64 // % — excludes droppable cache
	Demand      float64 // % — real used plus swapped-out data; may exceed 100
	HeadroomLow bool

	HasRates bool
	Rates    Rates

	CPU CPUReport
}

// Overall is the worse of the memory and CPU verdicts.
func (r *Report) Overall() State { return max(r.State, r.CPU.State) }

const mib = 1 << 20

func assess(c *config, prev *Sample, cur Sample, source string) *Report {
	r := &Report{At: cur.At, Source: source, Sample: cur}

	if cur.Total > 0 {
		t := float64(cur.Total)
		inUse := sat(cur.Total, cur.Available)
		r.NaiveUsed = 100 * float64(sat(cur.Total, cur.Free)) / t
		r.RealUsed = 100 * float64(inUse) / t
		r.Demand = 100 * float64(inUse+cur.SwapUsed) / t
	}
	r.HeadroomLow = r.RealUsed >= 90

	var prevCPU *CPUSample
	var dt float64
	if prev != nil {
		prevCPU, dt = &prev.CPU, cur.At.Sub(prev.At).Seconds()
	}
	r.CPU = assessCPU(c, prevCPU, cur.CPU, dt)

	if prev != nil {
		if dt > 0.05 {
			ps := float64(pageSize)
			rate := func(a, b uint64) float64 { return float64(sat(b, a)) * ps / dt }
			r.HasRates = true
			r.Rates = Rates{
				Window:     dt,
				SwapIn:     rate(prev.SwapIn, cur.SwapIn),
				SwapOut:    rate(prev.SwapOut, cur.SwapOut),
				Refault:    rate(prev.Refault, cur.Refault),
				DirectScan: rate(prev.DirectScan, cur.DirectScan),
			}
		}
	}

	b := c.memBands
	switch {
	case cur.HasPSI:
		r.State = zoneOf(cur.Some.Avg10, b)
		// "full" means every runnable task was stalled at once — escalate.
		if f := cur.Full.Avg10; f >= 10 {
			r.State = StateThrashing
		} else if f >= 2 && r.State < StatePressured {
			r.State = StatePressured
		}
		r.Trend = trendOf(cur.Some)

	case r.HasRates:
		// Fallback: no PSI, so judge by how much data is being paged back
		// in (swap-in + refaults) and whether allocations reclaim directly.
		r.Estimated = true
		churn := r.Rates.SwapIn + r.Rates.Refault
		switch {
		case churn >= 64*mib:
			r.State = StateThrashing
		case churn >= 16*mib || r.Rates.DirectScan >= 16*mib:
			r.State = StatePressured
		case churn >= 1*mib || r.Rates.DirectScan > 0:
			r.State = StateStrained
		}

	default:
		r.Estimated = true
	}
	return r
}

func zoneOf(stall float64, b [3]float64) State {
	switch {
	case stall >= b[2]:
		return StateThrashing
	case stall >= b[1]:
		return StatePressured
	case stall >= b[0]:
		return StateStrained
	}
	return StateOK
}

func explain(r *Report) string {
	if r.Estimated && !r.HasRates {
		return "Measuring paging activity…"
	}
	switch r.State {
	case StateStrained:
		return "Reclaim is starting to cost a little latency."
	case StatePressured:
		return "Working set is pressing on RAM; tasks are waiting."
	case StateThrashing:
		return "Working set exceeds RAM; time goes to paging."
	}
	if r.HeadroomLow {
		return "Headroom is thin, but nothing waits on memory yet."
	}
	return "Every memory request is met without anyone waiting."
}
