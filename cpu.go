package main

// cpu.go — CPU pressure: are runnable tasks waiting for a core?
//
// Headline: /proc/pressure/cpu "some" — % of wall time at least one runnable
// task was queued instead of running. Supporting signals: run-queue depth
// (/proc/schedstat run_delay, or procs_running as a fallback), steal time
// and utilization (/proc/stat), and quota throttling for cgroups (cpu.stat).

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type cpuTicks struct{ total, idle, iowait, steal uint64 }

type CPUSample struct {
	HasPSI    bool
	PSIReason string
	Some      PSILine
	Full      PSILine

	Ticks    cpuTicks // host-wide jiffies
	HasTicks bool

	RunDelay    uint64 // ns, cumulative across CPUs (schedstat)
	HasRunDelay bool
	Running     int // procs_running right now (fallback queue gauge)

	Cores float64 // online CPUs, or cgroup quota in cores
	Quota bool

	Usage       uint64 // cgroup usage_usec
	HasUsage    bool
	Throttled   uint64 // cgroup throttled_usec
	HasThrottle bool
}

type CPUReport struct {
	State     State
	Estimated bool
	Trend     int
	Cores     float64

	HasRates    bool
	Busy, Steal float64 // %
	HasSteal    bool
	Waiting     float64 // tasks waiting for a core, on average
	HasWaiting  bool
	WaitApprox  bool    // instantaneous procs_running, not an average
	Throttled   float64 // ms of throttling per second
	HasThrottle bool
}

// readProcStat returns the aggregate cpu line, online CPU count and
// procs_running.
func readProcStat() (t cpuTicks, cores, running int, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "cpu "):
			fs := strings.Fields(line)
			// user nice system idle iowait irq softirq steal [guest guest_nice]
			// guest time is already included in user/nice, so stop at steal.
			for i := 1; i < len(fs) && i <= 8; i++ {
				v, _ := strconv.ParseUint(fs[i], 10, 64)
				t.total += v
				switch i {
				case 4:
					t.idle = v
				case 5:
					t.iowait = v
				case 8:
					t.steal = v
				}
			}
		case strings.HasPrefix(line, "cpu"):
			cores++
		case strings.HasPrefix(line, "procs_running "):
			running, _ = strconv.Atoi(strings.TrimSpace(line[len("procs_running "):]))
		}
	}
	if cores == 0 {
		cores = runtime.NumCPU()
	}
	return t, cores, running, sc.Err()
}

// readRunDelay sums field 8 (time spent waiting on a run queue, ns) of every
// cpu line in /proc/schedstat. Requires schedstat version 15 or later.
func readRunDelay() (uint64, bool) {
	f, err := os.Open("/proc/schedstat")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var sum uint64
	ok := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) == 0 {
			continue
		}
		if fs[0] == "version" {
			if v, _ := strconv.Atoi(fs[len(fs)-1]); v < 15 {
				return 0, false
			}
			continue
		}
		if strings.HasPrefix(fs[0], "cpu") && len(fs) >= 9 {
			v, err := strconv.ParseUint(fs[8], 10, 64)
			if err == nil {
				sum += v
				ok = true
			}
		}
	}
	return sum, ok
}

func probeCPUSystem() CPUSample {
	var c CPUSample
	t, cores, running, err := readProcStat()
	if err == nil {
		c.Ticks, c.HasTicks = t, true
	}
	c.Cores, c.Running = float64(cores), running
	c.RunDelay, c.HasRunDelay = readRunDelay()
	c.Some, c.Full, c.PSIReason, c.HasPSI = readPSI("/proc/pressure/cpu")
	return c
}

func probeCPUCgroup(dir string) CPUSample {
	c := probeCPUSystem() // host steal, core count
	c.HasRunDelay, c.Running = false, 0
	c.Some, c.Full, c.PSIReason, c.HasPSI = readPSI(filepath.Join(dir, "cpu.pressure"))

	if b, err := os.ReadFile(filepath.Join(dir, "cpu.max")); err == nil {
		fs := strings.Fields(string(b))
		if len(fs) == 2 && fs[0] != "max" {
			q, e1 := strconv.ParseFloat(fs[0], 64)
			p, e2 := strconv.ParseFloat(fs[1], 64)
			if e1 == nil && e2 == nil && p > 0 && q/p < c.Cores {
				c.Cores, c.Quota = q/p, true
			}
		}
	}
	if st, err := readKV(filepath.Join(dir, "cpu.stat")); err == nil {
		if v, ok := st["usage_usec"]; ok {
			c.Usage, c.HasUsage = v, true
		}
		if v, ok := st["throttled_usec"]; ok {
			c.Throttled, c.HasThrottle = v, true
		}
	}
	return c
}

func assessCPU(c *config, prev *CPUSample, cur CPUSample, dt float64) CPUReport {
	r := CPUReport{Cores: cur.Cores}

	if prev != nil && dt > 0.05 {
		r.HasRates = true
		if prev.HasTicks && cur.HasTicks {
			dT := float64(sat(cur.Ticks.total, prev.Ticks.total))
			if dT > 0 {
				idle := float64(sat(cur.Ticks.idle, prev.Ticks.idle) + sat(cur.Ticks.iowait, prev.Ticks.iowait))
				steal := float64(sat(cur.Ticks.steal, prev.Ticks.steal))
				r.Busy = 100 * (dT - idle - steal) / dT
				r.Steal, r.HasSteal = 100*steal/dT, true
			}
		}
		if cur.HasUsage && prev.HasUsage && cur.Cores > 0 {
			r.Busy = 100 * float64(sat(cur.Usage, prev.Usage)) / (dt * 1e6 * cur.Cores)
		}
		r.Busy = clamp(r.Busy, 0, 100)
		if cur.HasRunDelay && prev.HasRunDelay {
			r.Waiting, r.HasWaiting = float64(sat(cur.RunDelay, prev.RunDelay))/(dt*1e9), true
		}
		if cur.HasThrottle && prev.HasThrottle {
			r.Throttled, r.HasThrottle = float64(sat(cur.Throttled, prev.Throttled))/1000/dt, true
		}
	}
	// No schedstat: use the instantaneous run-queue length. procs_running
	// includes strain itself, which is on a CPU while it reads the file.
	if !r.HasWaiting && cur.Running > 0 && !cur.HasUsage {
		r.Waiting = max(0, float64(cur.Running-1)-cur.Cores)
		r.HasWaiting, r.WaitApprox = true, true
	}

	b := c.cpuBands
	switch {
	case cur.HasPSI:
		r.State = zoneOf(cur.Some.Avg10, b)
		// "full" is always 0 system-wide; in a cgroup it means the whole
		// group was starved at once.
		if cur.Full.Avg10 >= 10 && r.State < StatePressured {
			r.State = StatePressured
		}
		r.Trend = trendOf(cur.Some)
	case r.HasWaiting && cur.Cores > 0:
		r.Estimated = true
		switch q := r.Waiting / cur.Cores; {
		case q >= 1:
			r.State = StateThrashing
		case q >= 0.25:
			r.State = StatePressured
		case q >= 0.05:
			r.State = StateStrained
		}
	default:
		r.Estimated = true
	}
	return r
}

func trendOf(p PSILine) int {
	switch now, base := p.Avg10, p.Avg60; {
	case now > base*1.25+0.25:
		return 1
	case now < base*0.75-0.25:
		return -1
	}
	return 0
}

func clamp(v, lo, hi float64) float64 { return min(max(v, lo), hi) }

var cpuStateNames = [...]string{"OK", "BUSY", "CONTENDED", "SATURATED"}

func cpuExplain(r *CPUReport, hasPSI bool) string {
	if !hasPSI && !r.HasWaiting {
		if !r.HasRates {
			return "Measuring run-queue activity…"
		}
		return "No CPU pressure data on this kernel."
	}
	return [...]string{
		"Runnable tasks get a core almost immediately.",
		"Some queuing for cores; latency starting to slip.",
		"Tasks regularly wait for a free core.",
		"Demand exceeds the cores available.",
	}[r.State]
}
