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

func synth(stall, full float64, realUsed, swapGiB float64, swapInMiB, refaultMiB float64) (*Report, *history) {
	cfg := &config{bands: [3]float64{1, 10, 25}}
	const gib = 1 << 30
	gibf := float64(gib)
	total := uint64(31.3 * gibf)
	avail := uint64(float64(total) * (1 - realUsed/100))
	t0 := time.Date(2026, 10, 31, 23, 10, 0, 0, time.Local)
	ps := float64(pageSize)
	prev := Sample{At: t0}
	cur := Sample{
		At: t0.Add(time.Second), Total: total, Available: avail, Free: total / 25,
		SwapTotal: 8 * gib, SwapUsed: uint64(swapGiB * gib), HasPSI: true, HasSwapIO: true,
		Some:   PSILine{stall, stall * 0.7, stall * 0.3},
		Full:   PSILine{full, full * 0.6, full * 0.2},
		SwapIn: uint64(swapInMiB * mib / ps), Refault: uint64(refaultMiB * mib / ps),
		DirectScan: uint64(refaultMiB * 0.3 * mib / ps), SwapOut: uint64(swapInMiB * 0.5 * mib / ps),
	}
	cur.Droppable = sat(cur.Available, cur.Free)
	r := assess(cfg, &prev, cur, "system")
	h := &history{}
	for i := 0; i < 150; i++ {
		f := 0.5 + 0.5*math.Sin(float64(i)/9)
		rr := *r
		rr.At = t0.Add(time.Duration(i-150) * time.Second)
		rr.Sample.Some.Avg10 = stall * f * float64(i) / 150
		rr.Rates.SwapIn, rr.Rates.Refault = r.Rates.SwapIn*f, r.Rates.Refault*f
		rr.Rates.DirectScan, rr.Rates.SwapOut = r.Rates.DirectScan*f, r.Rates.SwapOut*f
		h.push(&rr)
	}
	return r, h
}

func TestPreview(t *testing.T) {
	if os.Getenv("STRAIN_COLOR") != "" {
		colorMode = colorTrue
	}
	cases := []struct {
		name string
		r    func() (*Report, *history)
	}{
		{"ok", func() (*Report, *history) { return synth(0, 0, 41, 0, 0, 0) }},
		{"ok-thin", func() (*Report, *history) { return synth(0.2, 0, 93, 0.3, 0, 0.1) }},
		{"strained", func() (*Report, *history) { return synth(4.2, 0.3, 88, 0.8, 0.6, 3) }},
		{"pressured", func() (*Report, *history) { return synth(14.2, 1.8, 96, 1.3, 2.4, 24) }},
		{"thrashing", func() (*Report, *history) { return synth(38, 14, 99, 5.1, 80, 140) }},
	}
	for _, w := range []int{100, 72} {
		for _, c := range cases {
			r, h := c.r()
			for _, live := range []bool{true, false} {
				lines := render(r, view{Width: w, Live: live, Interval: time.Second, Hist: h,
					Bands: [3]float64{1, 10, 25}, Host: "hearth", Kernel: "6.18.44"})
				for i, l := range lines {
					if got := utf8.RuneCountInString(ansi(l)); got != w {
						t.Errorf("%s w=%d live=%v line %d width %d != %d: %q", c.name, w, live, i, got, w, ansi(l))
					}
				}
				if testing.Verbose() && (live || c.name == "pressured") {
					fmt.Printf("\n── %s  width=%d live=%v\n%s\n", c.name, w, live, strings.Join(lines, "\n"))
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
	cfg := &config{bands: [3]float64{1, 10, 25}}
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
