package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var ansi = func(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

type cpuSpec struct{ stall, busy, steal, waiting float64 }

var testCfg = &config{memBands: [3]float64{1, 10, 25}, cpuBands: [3]float64{5, 20, 50}, ioBands: [3]float64{5, 20, 50}}

func synth(stall, full float64, realUsed, swapGiB float64, swapInMiB, refaultMiB float64, cs cpuSpec, source string) (*Report, *history) {
	const gib = 1 << 30
	gibf := float64(gib)
	total := uint64(31.3 * gibf)
	avail := uint64(float64(total) * (1 - realUsed/100))
	t0 := time.Date(2026, 10, 31, 23, 10, 0, 0, time.Local)
	ps := float64(pageSize)

	const cores, jiffies = 8, 800.0 // one second of 8 CPUs at 100 Hz
	prevCPU := CPUSample{HasTicks: true, HasRunDelay: true, Cores: cores, Ticks: cpuTicks{total: 1e6}, HasUsage: source != "system", HasThrottle: source != "system"}
	curCPU := prevCPU
	curCPU.HasPSI = true
	curCPU.Some = PSILine{cs.stall, cs.stall * 0.6, cs.stall * 0.25}
	curCPU.Ticks = cpuTicks{
		total: 1e6 + jiffies,
		idle:  uint64(jiffies * (1 - cs.busy/100 - cs.steal/100)),
		steal: uint64(jiffies * cs.steal / 100),
	}
	curCPU.RunDelay = uint64(cs.waiting * 1e9)
	curCPU.Usage = uint64(cs.busy / 100 * cores * 1e6)
	curCPU.Throttled = uint64(cs.stall * 8000)

	// I/O scales with the memory stall so each preview shows a matching state.
	disk := func(reads, writes, ms, weighted uint64) diskStat {
		return diskStat{reads: reads, writes: writes, rsect: reads * 256, wsect: writes * 64,
			rms: ms / 2, wms: ms / 2, ioms: min(ms, 1000), weightedMs: weighted}
	}
	ioLoad := uint64(stall*40) + 1
	prevIO := IOSample{HasDisks: true, Disks: map[string]diskStat{
		"nvme0n1": {}, "sda": {}, "pve-root": {}, "pve-data": {}, "sdb": {}, "sdc": {},
	}}
	curIO := IOSample{HasDisks: true, HasPSI: true,
		Some: PSILine{stall * 1.4, stall, stall * 0.4}, Full: PSILine{stall * 0.6, stall * 0.4, stall * 0.1},
		Disks: map[string]diskStat{
			"nvme0n1":  disk(3200*ioLoad/40, 900, 400+ioLoad*20, 1300+ioLoad*80),
			"sda":      disk(40, 12*ioLoad, 120*ioLoad, 300*ioLoad),
			"pve-root": disk(2, 30, 6, 9),
			"pve-data": disk(0, 0, 0, 0), "sdb": disk(0, 0, 0, 0), "sdc": disk(0, 0, 0, 0),
		}}

	prev := Sample{At: t0, CPU: prevCPU, IO: prevIO}
	cur := Sample{
		At: t0.Add(time.Second), Total: total, Available: avail, Free: total / 25,
		SwapTotal: 8 * gib, SwapUsed: uint64(swapGiB * gib), HasPSI: true, HasSwapIO: true,
		Some:   PSILine{stall, stall * 0.7, stall * 0.3},
		Full:   PSILine{full, full * 0.6, full * 0.2},
		SwapIn: uint64(swapInMiB * mib / ps), Refault: uint64(refaultMiB * mib / ps),
		DirectScan: uint64(refaultMiB * 0.3 * mib / ps), SwapOut: uint64(swapInMiB * 0.5 * mib / ps),
		CPU: curCPU, IO: curIO, Limited: source != "system",
	}
	cur.Droppable = sat(cur.Available, cur.Free)
	r := assess(testCfg, &prev, cur, source)
	h := &history{}
	for i := 0; i < 150; i++ {
		f := 0.5 + 0.5*math.Sin(float64(i)/9)
		rr := *r
		rr.At = t0.Add(time.Duration(i-150) * time.Second)
		rr.Sample.Some.Avg10 = stall * f * float64(i) / 150
		rr.Sample.CPU.Some.Avg10 = cs.stall * (1 - f*0.6)
		rr.Rates.SwapIn, rr.Rates.Refault = r.Rates.SwapIn*f, r.Rates.Refault*f
		rr.Rates.DirectScan, rr.Rates.SwapOut = r.Rates.DirectScan*f, r.Rates.SwapOut*f
		rr.CPU.Waiting, rr.CPU.Steal, rr.CPU.Throttled = r.CPU.Waiting*f, r.CPU.Steal*f, r.CPU.Throttled*f
		rr.Sample.IO.Some.Avg10 = stall * 1.4 * f
		h.push(&rr)
	}
	return r, h
}

func TestPreview(t *testing.T) {
	if os.Getenv("STRAIN_COLOR") != "" {
		colorMode = colorTrue
	}
	sys := "system"
	cases := []struct {
		name string
		r    func() (*Report, *history)
	}{
		{"ok", func() (*Report, *history) { return synth(0, 0, 41, 0, 0, 0, cpuSpec{0.4, 12, 0, 0.02}, sys) }},
		{"ok-thin", func() (*Report, *history) { return synth(0.2, 0, 93, 0.3, 0, 0.1, cpuSpec{2, 97, 0, 0.1}, sys) }},
		{"strained", func() (*Report, *history) { return synth(4.2, 0.3, 88, 0.8, 0.6, 3, cpuSpec{9, 70, 2.1, 0.6}, sys) }},
		{"pressured", func() (*Report, *history) { return synth(14.2, 1.8, 96, 1.3, 2.4, 24, cpuSpec{31.4, 74, 0, 2.3}, sys) }},
		{"thrashing", func() (*Report, *history) { return synth(38, 14, 99, 5.1, 80, 140, cpuSpec{72, 100, 18, 11}, sys) }},
		{"cgroup", func() (*Report, *history) {
			return synth(2, 0, 70, 0, 0, 1, cpuSpec{24, 95, 0, 0}, "/system.slice/postgresql.service")
		}},
	}
	for _, w := range []int{100, 72} {
		for _, c := range cases {
			r, h := c.r()
			for _, live := range []bool{true, false} {
				for compact := 0; compact <= 3; compact++ {
					if !live && compact > 0 {
						continue
					}
					lines := render(r, view{Width: w, Live: live, Compact: compact, Interval: time.Second, Hist: h,
						MemBands: testCfg.memBands, CPUBands: testCfg.cpuBands, IOBands: testCfg.ioBands, Host: "hearth", Kernel: "6.18.44"})
					for i, l := range lines {
						if got := utf8.RuneCountInString(ansi(l)); got != w {
							t.Errorf("%s w=%d live=%v compact=%d line %d width %d != %d: %q", c.name, w, live, compact, i, got, w, ansi(l))
						}
					}
					if testing.Verbose() && compact == 0 && (live || c.name == "pressured") {
						fmt.Printf("\n── %s  width=%d live=%v lines=%d\n%s\n", c.name, w, live, len(lines), strings.Join(lines, "\n"))
					}
					if testing.Verbose() && compact > 0 && w == 100 && c.name == "ok" {
						fmt.Printf("\n── %s compact=%d lines=%d\n", c.name, compact, len(lines))
					}
				}
			}
		}
	}
}

func TestStepInterval(t *testing.T) {
	if got := stepInterval(time.Second, 1); got != 2*time.Second {
		t.Fatal(got)
	}
	if got := stepInterval(time.Second, -1); got != 500*time.Millisecond {
		t.Fatal(got)
	}
	if got := stepInterval(1500*time.Millisecond, -1); got != time.Second {
		t.Fatal(got)
	}
	if got := stepInterval(10*time.Second, 1); got != 10*time.Second {
		t.Fatal(got)
	}
}

func TestAssessBands(t *testing.T) {
	cfg := testCfg
	for _, tc := range []struct {
		some, full float64
		want       State
	}{{0, 0, StateOK}, {1.5, 0, StateStrained}, {12, 0, StatePressured}, {3, 2.5, StatePressured}, {30, 0, StateThrashing}, {5, 11, StateThrashing}} {
		s := Sample{HasPSI: true, Total: 1, Some: PSILine{Avg10: tc.some}, Full: PSILine{Avg10: tc.full}}
		if got := assess(cfg, nil, s, "t").State; got != tc.want {
			t.Errorf("some=%v full=%v: got %v want %v", tc.some, tc.full, got, tc.want)
		}
	}
}

func TestAssessCPU(t *testing.T) {
	for _, tc := range []struct {
		some float64
		want State
	}{{0, StateOK}, {6, StateStrained}, {25, StatePressured}, {60, StateThrashing}} {
		c := CPUSample{HasPSI: true, Cores: 4, Some: PSILine{Avg10: tc.some}}
		if got := assessCPU(testCfg, nil, c, 0).State; got != tc.want {
			t.Errorf("cpu some=%v: got %v want %v", tc.some, got, tc.want)
		}
	}
	// No PSI: estimate from queue depth relative to cores.
	prev := CPUSample{Cores: 4, HasRunDelay: true}
	cur := CPUSample{Cores: 4, HasRunDelay: true, RunDelay: 2e9} // 2 tasks waiting on 4 cores
	if r := assessCPU(testCfg, &prev, cur, 1); r.State != StatePressured || !r.Estimated || r.Waiting != 2 {
		t.Errorf("estimated cpu: %+v", r)
	}
	// Busy and steal from jiffies.
	prev = CPUSample{HasTicks: true, Ticks: cpuTicks{total: 1000}}
	cur = CPUSample{HasTicks: true, Ticks: cpuTicks{total: 1400, idle: 100, iowait: 0, steal: 40}}
	if r := assessCPU(testCfg, &prev, cur, 1); math.Abs(r.Busy-65) > 0.01 || math.Abs(r.Steal-10) > 0.01 {
		t.Errorf("busy/steal: %+v", r)
	}
}

func TestAssessIO(t *testing.T) {
	for _, tc := range []struct {
		some, full float64
		want       State
	}{{0, 0, StateOK}, {6, 0, StateStrained}, {25, 0, StatePressured}, {3, 6, StatePressured}, {60, 0, StateThrashing}, {10, 25, StateThrashing}} {
		s := IOSample{HasPSI: true, Some: PSILine{Avg10: tc.some}, Full: PSILine{Avg10: tc.full}}
		if got := assessIO(testCfg, nil, s, 0).State; got != tc.want {
			t.Errorf("io some=%v full=%v: got %v want %v", tc.some, tc.full, got, tc.want)
		}
	}
	// Device maths over a 2 s window: 400 ops, 2000 ms of op time,
	// 1000 ms busy, 3000 ms·requests in flight.
	prev := IOSample{HasDisks: true, Disks: map[string]diskStat{"sda": {}, "sdb": {}}}
	cur := IOSample{HasDisks: true, Disks: map[string]diskStat{
		"sda": {reads: 300, writes: 100, rsect: 2048, wsect: 1024, rms: 1500, wms: 500, ioms: 1000, weightedMs: 3000},
		"sdb": {},
	}}
	r := assessIO(testCfg, &prev, cur, 2)
	if len(r.Disks) != 1 || r.Idle != 1 {
		t.Fatalf("disks=%+v idle=%d", r.Disks, r.Idle)
	}
	d := r.Disks[0]
	if d.IOPS != 200 || d.AwaitMs != 5 || d.Queue != 1.5 || d.Util != 50 || d.ReadBps != 524288 || d.WriteBps != 262144 {
		t.Errorf("sda: %+v", d)
	}
	if !r.Estimated || r.State != StateOK {
		t.Errorf("no-PSI estimate: %+v", r)
	}
}
