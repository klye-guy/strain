package main

// render.go — draws a Report as a bordered panel.
//
// Palette: warm amber accent on warm greys, borders not shadows. Colours are
// foreground-only so the panel sits on whatever background the terminal has.

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// ── colour ────────────────────────────────────────────────────────────────

type rgb struct{ r, g, b uint8 }

func (c rgb) scale(f float64) rgb {
	m := func(v uint8) uint8 { return uint8(math.Min(255, math.Round(float64(v)*f))) }
	return rgb{m(c.r), m(c.g), m(c.b)}
}

var (
	cAccent = rgb{232, 163, 61} // #e8a33d
	cBorder = rgb{98, 86, 74}
	cLabel  = rgb{158, 145, 130}
	cFaint  = rgb{84, 74, 65}
	cTrack  = rgb{60, 53, 47}
	cText   = rgb{232, 223, 210}

	stateColor = [...]rgb{
		{143, 181, 115}, // OK        sage
		{227, 195, 90},  // STRAINED  straw
		{235, 133, 62},  // PRESSURED ember
		{226, 83, 63},   // THRASHING red
	}
)

const (
	colorNone = iota
	color256
	colorTrue
)

var colorMode = colorNone

func paint(c rgb, bold bool, s string) string {
	if colorMode == colorNone || s == "" {
		return s
	}
	var b strings.Builder
	if bold {
		b.WriteString("\x1b[1m")
	}
	if colorMode == colorTrue {
		fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
	} else {
		fmt.Fprintf(&b, "\x1b[38;5;%dm", to256(c))
	}
	b.WriteString(s)
	b.WriteString("\x1b[0m")
	return b.String()
}

func to256(c rgb) int {
	hi, lo := max(c.r, c.g, c.b), min(c.r, c.g, c.b)
	if int(hi)-int(lo) < 14 { // near-grey: use the grey ramp
		avg := (int(c.r) + int(c.g) + int(c.b)) / 3
		switch {
		case avg < 8:
			return 16
		case avg > 238:
			return 231
		}
		return 232 + (avg-8)/10
	}
	q := func(v uint8) int {
		switch {
		case v < 48:
			return 0
		case v < 115:
			return 1
		}
		return (int(v) - 35) / 40
	}
	return 16 + 36*q(c.r) + 6*q(c.g) + q(c.b)
}

// ── line builder (tracks visible width separately from escape codes) ──────

type ln struct {
	sb strings.Builder
	w  int
}

func newLn() *ln { return &ln{} }

func (l *ln) put(c rgb, s string) *ln {
	l.sb.WriteString(paint(c, false, s))
	l.w += utf8.RuneCountInString(s)
	return l
}

func (l *ln) bold(c rgb, s string) *ln {
	l.sb.WriteString(paint(c, true, s))
	l.w += utf8.RuneCountInString(s)
	return l
}

func (l *ln) sp(n int) *ln {
	if n > 0 {
		l.sb.WriteString(strings.Repeat(" ", n))
		l.w += n
	}
	return l
}

func (l *ln) to(col int) *ln { return l.sp(col - l.w) }

func (l *ln) cat(o *ln) *ln {
	l.sb.WriteString(o.sb.String())
	l.w += o.w
	return l
}

func (l *ln) String() string { return l.sb.String() }

func fit(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

func rjust(s string, n int) string {
	if w := utf8.RuneCountInString(s); w < n {
		return strings.Repeat(" ", n-w) + s
	}
	return s
}

// ── frame ─────────────────────────────────────────────────────────────────

const padX = 2

type frame struct {
	W, IW int
	tight bool // drop spacer rows (very short terminals)
	out   []string
}

func newFrame(w int) *frame { return &frame{W: w, IW: w - 2 - 2*padX} }

func (f *frame) rule(lc, rc string, left, right *ln) {
	l := newLn().put(cBorder, lc+"─")
	if left != nil && left.w > 0 {
		l.sp(1).cat(left).sp(1)
	}
	rw := 0
	if right != nil && right.w > 0 {
		rw = right.w + 2
	}
	fill := f.W - l.w - rw - 2
	if fill < 1 {
		fill = 1
	}
	l.put(cBorder, strings.Repeat("─", fill))
	if rw > 0 {
		l.sp(1).cat(right).sp(1)
	}
	l.put(cBorder, "─"+rc)
	f.out = append(f.out, l.String())
}

func (f *frame) row(c *ln) {
	l := newLn().put(cBorder, "│").sp(padX).cat(c)
	l.to(f.W-1).put(cBorder, "│")
	f.out = append(f.out, l.String())
}

func (f *frame) blank() {
	if !f.tight {
		f.row(newLn())
	}
}

// ── glyph helpers ─────────────────────────────────────────────────────────

var (
	eighths = []rune(" ▏▎▍▌▋▊▉")
	sparks  = []rune("▁▂▃▄▅▆▇█")
)

// Stall meters use a square-root scale so the bands that matter most get
// real width instead of a sliver: memory spans 0–50 %, CPU 0–100 %.
const (
	memMeterMax = 50.0
	cpuMeterMax = 100.0
)

func meterPos(v, top float64) float64 {
	return math.Sqrt(clamp(v, 0, top) / top)
}

func meter(v float64, w int, b [3]float64, top float64) *ln {
	l := newLn()
	fill := meterPos(v, top) * float64(w)
	full := int(fill)
	part := int((fill - float64(full)) * 8)
	for i := 0; i < w; i++ {
		cellV := math.Pow((float64(i)+0.5)/float64(w), 2) * top
		zc := stateColor[zoneOf(cellV, b)]
		switch {
		case i < full:
			l.put(zc, "█")
		case i == full && part > 0:
			l.put(zc, string(eighths[part]))
		default:
			l.put(zc.scale(0.32), "░")
		}
	}
	return l
}

func meterTicks(w int, b [3]float64, top float64) *ln {
	cells := []rune(strings.Repeat(" ", w))
	place := func(v float64, label string) {
		r := []rune(label)
		start := int(math.Round(meterPos(v, top) * float64(w)))
		if start+len(r) > w {
			start = w - len(r)
		}
		for i, ch := range r {
			if start+i >= 0 && start+i < w {
				cells[start+i] = ch
			}
		}
	}
	place(0, "0")
	for _, v := range b {
		place(v, fmt.Sprintf("%g", v))
	}
	place(top, fmt.Sprintf("%g%%", top))
	return newLn().put(cFaint, string(cells))
}

func bar(p float64, w int, c rgb) *ln {
	l := newLn()
	fill := clamp(p, 0, 100) / 100 * float64(w)
	full := int(fill)
	part := int((fill - float64(full)) * 8)
	for i := 0; i < w; i++ {
		switch {
		case i < full:
			l.put(c, "█")
		case i == full && part > 0:
			l.put(c, string(eighths[part]))
		default:
			l.put(cTrack, "░")
		}
	}
	return l
}

func stallSpark(vals []float64, w int, b [3]float64, top float64) *ln {
	vals = tail(vals, w)
	l := newLn().sp(w - len(vals))
	for _, v := range vals {
		if v <= 0 {
			l.put(cFaint, "▁")
			continue
		}
		i := min(int(meterPos(v, top)*8), 7)
		l.put(stateColor[zoneOf(v, b)], string(sparks[i]))
	}
	return l
}

// spark draws a series scaled to its own peak (never below floor), so a
// quiet system shows a flat baseline rather than amplified noise.
func spark(vals []float64, w int, floor float64, color func(float64) rgb) *ln {
	if w <= 0 {
		return nil
	}
	vals = tail(vals, w)
	peak := floor
	for _, v := range vals {
		peak = math.Max(peak, v)
	}
	l := newLn().sp(w - len(vals))
	for _, v := range vals {
		if v <= 0 {
			l.put(cFaint, "▁")
			continue
		}
		l.put(color(v), string(sparks[min(int(v/peak*8), 7)]))
	}
	return l
}

func usageColor(p float64) rgb {
	switch {
	case p >= 100:
		return stateColor[StateThrashing]
	case p >= 90:
		return stateColor[StatePressured]
	case p >= 80:
		return stateColor[StateStrained]
	}
	return stateColor[StateOK]
}

// level picks a colour from three ascending thresholds.
func level(v, a, b, c float64) rgb {
	switch {
	case v <= 0:
		return cLabel
	case v < a:
		return cText
	case v < b:
		return stateColor[StateStrained]
	case v < c:
		return stateColor[StatePressured]
	}
	return stateColor[StateThrashing]
}

func rateColor(bps float64, hot bool) rgb {
	if !hot && bps > 0 {
		return cText
	}
	return level(bps, 1*mib, 16*mib, 64*mib)
}

func fmtBytes(b float64) string {
	const k = 1024.0
	switch {
	case b >= k*k*k*k:
		return fmt.Sprintf("%.1f TiB", b/(k*k*k*k))
	case b >= k*k*k:
		return fmt.Sprintf("%.1f GiB", b/(k*k*k))
	case b >= 10*k*k:
		return fmt.Sprintf("%.0f MiB", b/(k*k))
	case b >= k*k:
		return fmt.Sprintf("%.1f MiB", b/(k*k))
	case b >= k:
		return fmt.Sprintf("%.0f KiB", b/k)
	}
	return fmt.Sprintf("%.0f B", b)
}

func fmtCores(c float64) string {
	if c == math.Trunc(c) {
		if c == 1 {
			return "1 core"
		}
		return fmt.Sprintf("%.0f cores", c)
	}
	return fmt.Sprintf("%.2g cores", c)
}

func fmtSpan(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// ── history ───────────────────────────────────────────────────────────────

const histCap = 400

type history struct {
	at                                      []time.Time
	stall, swapIn, swapOut, refault, direct []float64
	cpuStall, queue, steal, throttle        []float64
	ioStall                                 []float64
}

func (h *history) push(r *Report) {
	if !r.HasRates {
		return
	}
	c := r.CPU
	h.at = append(h.at, r.At)
	h.stall = append(h.stall, r.Sample.Some.Avg10)
	h.swapIn = append(h.swapIn, r.Rates.SwapIn)
	h.swapOut = append(h.swapOut, r.Rates.SwapOut)
	h.refault = append(h.refault, r.Rates.Refault)
	h.direct = append(h.direct, r.Rates.DirectScan)
	h.cpuStall = append(h.cpuStall, r.Sample.CPU.Some.Avg10)
	h.queue = append(h.queue, c.Waiting)
	h.steal = append(h.steal, c.Steal)
	h.throttle = append(h.throttle, c.Throttled)
	h.ioStall = append(h.ioStall, r.Sample.IO.Some.Avg10)
	if len(h.at) > histCap {
		cut := len(h.at) - histCap
		h.at = h.at[cut:]
		for _, s := range []*[]float64{&h.stall, &h.swapIn, &h.swapOut, &h.refault,
			&h.direct, &h.cpuStall, &h.queue, &h.steal, &h.throttle, &h.ioStall} {
			*s = (*s)[cut:]
		}
	}
}

func tail[T any](v []T, n int) []T {
	if n < 0 {
		n = 0
	}
	if len(v) > n {
		return v[len(v)-n:]
	}
	return v
}

// ── the panel ─────────────────────────────────────────────────────────────

type view struct {
	Width    int
	Live     bool
	Paused   bool
	Compact  int // 0 full · 1 drop history · 2 drop explanations · 3 drop spacers
	Interval time.Duration
	Hist     *history
	MemBands [3]float64
	CPUBands [3]float64
	IOBands  [3]float64
	Kernel   string
	Host     string
}

// pressureBlock is one resource's verdict, stall meter and history.
type pressureBlock struct {
	label    string
	names    [4]string
	state    State
	unknown  bool // no verdict possible yet
	hasPSI   bool
	reason   string
	some     PSILine
	trend    int
	explain  string
	bands    [3]float64
	top      float64
	hist     []float64
	figExtra *ln
}

const labelW = 9

func (f *frame) pressure(b pressureBlock, v view) {
	iw := f.IW
	sc := stateColor[b.state]

	head := newLn().bold(cAccent, b.label).to(labelW)
	if b.unknown {
		head.put(cFaint, "○ ").put(cLabel, fit(b.reason, iw-head.w-2))
	} else {
		head.put(sc, "● ").bold(sc, b.names[b.state]).to(labelW + 13)
		if b.hasPSI {
			suffix := " of the last 10 s stalled"
			if iw < 80 {
				suffix = " stalled"
			}
			head.bold(cText, fmt.Sprintf("%.1f%%", b.some.Avg10)).put(cLabel, suffix)
		} else {
			head.put(cLabel, fit("estimated · "+b.reason, iw-head.w))
		}
	}
	var trend *ln
	switch {
	case !b.hasPSI:
	case b.trend > 0:
		trend = newLn().put(stateColor[StatePressured], "▲ rising")
	case b.trend < 0:
		trend = newLn().put(stateColor[StateOK], "▼ easing")
	default:
		trend = newLn().put(cFaint, "━ steady")
	}
	if trend != nil && head.w+trend.w+2 <= iw {
		head.to(iw - trend.w).cat(trend)
	}
	f.row(head)
	if v.Compact < 2 {
		f.row(newLn().to(labelW).put(cLabel, fit(b.explain, iw-labelW)))
	}
	if !b.hasPSI {
		return
	}
	if v.Compact < 2 {
		f.blank()
	}
	mw := iw - labelW
	f.row(newLn().put(cLabel, "stall").to(labelW).cat(meter(b.some.Avg10, mw, b.bands, b.top)))
	f.row(newLn().to(labelW).cat(meterTicks(mw, b.bands, b.top)))

	fig := newLn().to(labelW).put(cLabel, "some ").bold(cText, fmt.Sprintf("%.2f%%", b.some.Avg10))
	if b.figExtra != nil {
		fig.sp(3).cat(b.figExtra)
	}
	avgs := newLn().put(cLabel, "1m ").put(cText, fmt.Sprintf("%.2f%%", b.some.Avg60)).sp(3).
		put(cLabel, "5m ").put(cText, fmt.Sprintf("%.2f%%", b.some.Avg300))
	if fig.w+avgs.w+3 <= iw {
		fig.to(iw - avgs.w).cat(avgs)
	}
	f.row(fig)

	if v.Live && v.Hist != nil && v.Compact < 1 {
		f.row(newLn().put(cLabel, "history").to(labelW).cat(stallSpark(b.hist, mw, b.bands, b.top)))
		caption := newLn().to(labelW)
		if n := min(len(v.Hist.at), mw); n >= 14 {
			span := v.Hist.at[len(v.Hist.at)-1].Sub(v.Hist.at[len(v.Hist.at)-n])
			caption.to(labelW+mw-n).put(cFaint, "◂ "+fmtSpan(span))
		}
		if caption.w <= iw-4 {
			caption.to(iw-3).put(cFaint, "now")
		}
		f.row(caption)
	}
}

func render(r *Report, v view) []string {
	f := newFrame(v.Width)
	f.tight = v.Compact >= 3
	iw := f.IW
	s, c := r.Sample, r.CPU
	h := v.Hist
	if h == nil {
		h = &history{}
	}

	// ── header
	title := newLn().bold(cAccent, "strain").put(cLabel, " · system pressure")
	right := newLn().put(cLabel, v.Host+" · "+r.Source+" · "+r.At.Format("15:04:05"))
	if title.w+right.w+8 > f.W {
		right = newLn().put(cLabel, r.At.Format("15:04:05"))
	}
	f.rule("╭", "╮", title, right)
	f.blank()

	// ── verdicts
	f.pressure(pressureBlock{
		label: "memory", names: [4]string{"OK", "STRAINED", "PRESSURED", "THRASHING"},
		state: r.State, unknown: r.Estimated && !r.HasRates && !s.HasPSI,
		hasPSI: s.HasPSI, reason: s.PSIReason, some: s.Some, trend: r.Trend,
		explain: explain(r), bands: v.MemBands, top: memMeterMax, hist: h.stall,
		figExtra: newLn().put(cLabel, "full ").bold(cText, fmt.Sprintf("%.2f%%", s.Full.Avg10)),
	}, v)
	f.blank()

	var waitFig *ln
	if c.HasWaiting {
		unit := "avg"
		if c.WaitApprox {
			unit = "now"
		}
		waitFig = newLn().put(cLabel, "waiting ").bold(cText, fmt.Sprintf("%.1f", c.Waiting)).put(cLabel, " "+unit)
	}
	cpuUnknown := !s.CPU.HasPSI && !c.HasWaiting
	cpuReason := s.CPU.PSIReason
	if cpuUnknown && !c.HasRates {
		cpuReason = "measuring…"
	}
	f.pressure(pressureBlock{
		label: "cpu", names: cpuStateNames, state: c.State, unknown: cpuUnknown,
		hasPSI: s.CPU.HasPSI, reason: cpuReason, some: s.CPU.Some, trend: c.Trend,
		explain: cpuExplain(&c, s.CPU.HasPSI), bands: v.CPUBands, top: cpuMeterMax,
		hist: h.cpuStall, figExtra: waitFig,
	}, v)
	f.blank()

	io := r.IO
	ioReason := s.IO.PSIReason
	ioUnknown := !s.IO.HasPSI && !io.HasRates
	if ioUnknown {
		ioReason = "measuring…"
	}
	f.pressure(pressureBlock{
		label: "io", names: ioStateNames, state: io.State, unknown: ioUnknown,
		hasPSI: s.IO.HasPSI, reason: ioReason, some: s.IO.Some, trend: io.Trend,
		explain: ioExplain(&io, s.IO.HasPSI), bands: v.IOBands, top: cpuMeterMax,
		hist:     h.ioStall,
		figExtra: newLn().put(cLabel, "full ").bold(cText, fmt.Sprintf("%.2f%%", s.IO.Full.Avg10)),
	}, v)
	f.blank()

	// ── memory capacity
	capR := newLn().put(cLabel, fmtBytes(float64(s.Total))+" RAM")
	if r.Source != "system" {
		if s.Limited {
			capR = newLn().put(cLabel, "limit "+fmtBytes(float64(s.Total)))
		} else {
			capR = newLn().put(cLabel, "no limit · vs host RAM")
		}
	}
	f.rule("├", "┤", newLn().bold(cAccent, "memory capacity"), capR)
	const lw2 = 13
	bw := min(max(iw-lw2-9-30, 12), 44)
	noteW := iw - lw2 - bw - 9
	capRow := func(label string, p float64, col rgb, note string, strong bool) {
		l := newLn().put(cLabel, label).to(lw2).cat(bar(p, bw, col)).sp(2)
		pct := rjust(fmt.Sprintf("%.0f%%", p), 5)
		if strong {
			l.bold(col, pct)
		} else {
			l.put(cLabel, pct)
		}
		l.sp(2).put(cLabel, fit(note, noteW))
		f.row(l)
	}
	capRow("naive used", r.NaiveUsed, cLabel.scale(0.7), "what free and top call used", false)
	capRow("real used", r.RealUsed, usageColor(r.RealUsed),
		fmtBytes(float64(s.Available))+" of real headroom", true)
	demandNote := "nothing parked in swap"
	if s.SwapUsed > 0 {
		demandNote = "incl. " + fmtBytes(float64(s.SwapUsed)) + " parked in swap"
	}
	capRow("demand", r.Demand, usageColor(r.Demand), demandNote, true)

	det := newLn().to(lw2).put(cLabel, "droppable cache ").put(cText, fmtBytes(float64(s.Droppable))).
		put(cFaint, "   ·   ").put(cLabel, "swap ")
	if s.SwapTotal == 0 && s.SwapUsed == 0 {
		det.put(cText, "none configured")
	} else {
		det.put(cText, fmtBytes(float64(s.SwapUsed))).put(cLabel, " of ").put(cText, fmtBytes(float64(s.SwapTotal)))
	}
	if det.w <= iw {
		f.row(det)
	}
	f.blank()

	// ── activity rows (shared layout for paging and cpu)
	const lw3, vw = 16, 11
	sw := 0
	if v.Live {
		sw = min(max(iw-lw3-vw-4-34, 8), 36)
	}
	noteW3 := iw - lw3 - vw - 2
	if sw > 0 {
		noteW3 -= sw + 2
	}
	actRow := func(label, val string, vc rgb, strong bool, sp *ln, note string) {
		l := newLn().put(cLabel, label).to(lw3)
		if strong {
			l.bold(vc, rjust(val, vw))
		} else {
			l.put(vc, rjust(val, vw))
		}
		l.sp(2)
		if sw > 0 {
			if sp != nil {
				l.cat(sp)
			} else {
				l.sp(sw)
			}
			l.sp(2)
		}
		l.put(cFaint, fit(note, noteW3))
		f.row(l)
	}
	// metric renders one activity row, handling the n/a and warm-up cases.
	metric := func(label string, avail bool, v float64, text string, col func(float64) rgb,
		series []float64, floor float64, note string) {
		switch {
		case !avail:
			actRow(label, "n/a", cFaint, false, nil, note)
		case !r.HasRates:
			actRow(label, "measuring", cFaint, false, nil, note)
		case v <= 0:
			actRow(label, "0", cFaint, false, spark(series, sw, floor, col), note)
		default:
			actRow(label, text, col(v), true, spark(series, sw, floor, col), note)
		}
	}

	f.rule("├", "┤", newLn().bold(cAccent, "paging activity"), newLn().put(cLabel, "per second"))
	hot := func(v float64) rgb { return rateColor(v, true) }
	cold := func(v float64) rgb { return rateColor(v, false) }
	metric("swap-in", s.HasSwapIO, r.Rates.SwapIn, fmtBytes(r.Rates.SwapIn), hot, h.swapIn, mib, "swapped data wanted back")
	metric("refaults", true, r.Rates.Refault, fmtBytes(r.Rates.Refault), hot, h.refault, mib, "evicted pages needed again")
	metric("direct reclaim", true, r.Rates.DirectScan, fmtBytes(r.Rates.DirectScan), hot, h.direct, mib, "stalled allocations")
	metric("swap-out", s.HasSwapIO, r.Rates.SwapOut, fmtBytes(r.Rates.SwapOut), cold, h.swapOut, mib, "idle pages, usually benign")
	f.blank()

	// ── cpu activity
	coreLabel := fmtCores(c.Cores)
	if s.CPU.Quota {
		coreLabel = "quota " + coreLabel
	}
	f.rule("├", "┤", newLn().bold(cAccent, "cpu activity"), newLn().put(cLabel, coreLabel))
	busyNote := "what top calls CPU usage"
	if !c.HasRates {
		busyNote = "measuring…"
	}
	capRow("busy", c.Busy, cLabel.scale(0.7), busyNote, false)
	cores := max(c.Cores, 1)
	qNote := "waiting for a core (avg)"
	if c.WaitApprox {
		qNote = "waiting right now"
	}
	queueCol := func(v float64) rgb { return level(v/cores, 0.05, 0.25, 1) }
	if c.WaitApprox { // instantaneous: don't wait for a delta
		if c.Waiting <= 0 {
			actRow("run queue", "0", cFaint, false, spark(h.queue, sw, 1, queueCol), qNote)
		} else {
			actRow("run queue", fmt.Sprintf("%.0f tasks", c.Waiting), queueCol(c.Waiting), true,
				spark(h.queue, sw, 1, queueCol), qNote)
		}
	} else {
		metric("run queue", c.HasWaiting || r.Source == "system", c.Waiting,
			fmt.Sprintf("%.1f tasks", c.Waiting), queueCol, h.queue, 1, qNote)
	}
	metric("steal", c.HasSteal || !r.HasRates, c.Steal, fmt.Sprintf("%.1f%%", c.Steal),
		func(v float64) rgb { return level(v, 1, 5, 15) }, h.steal, 5, "taken by the hypervisor")
	if r.Source != "system" {
		metric("throttled", s.CPU.HasThrottle, c.Throttled, fmt.Sprintf("%.0f ms", c.Throttled),
			func(v float64) rgb { return level(v, 50, 200, 500) }, h.throttle, 100, "held back by cpu.max quota")
	}
	f.blank()

	// ── disk activity
	diskR := "per second"
	if r.Source != "system" {
		diskR = "host devices"
	}
	f.rule("├", "┤", newLn().bold(cAccent, "disk activity"), newLn().put(cLabel, diskR))
	f.diskTable(&io)
	f.blank()

	// ── footer
	if v.Live {
		keys := newLn().put(cAccent, "q").put(cLabel, " quit  ").
			put(cAccent, "p").put(cLabel, " pause  ").
			put(cAccent, "+/-").put(cLabel, " interval")
		st := newLn().put(cLabel, fmt.Sprintf("every %.2gs", v.Interval.Seconds()))
		if v.Paused {
			st = newLn().bold(stateColor[StateStrained], "PAUSED")
		}
		f.rule("╰", "╯", keys, st)
	} else {
		win := "snapshot"
		if r.HasRates {
			win = fmt.Sprintf("snapshot · %.1fs window", r.Rates.Window)
		}
		f.rule("╰", "╯", newLn().put(cLabel, win), newLn().put(cLabel, "kernel "+v.Kernel))
	}
	return f.out
}

// ── disk table ────────────────────────────────────────────────────────────

const maxDisks = 4

type diskCol struct {
	title string
	w     int
}

var baseDiskCols = [...]diskCol{{"device", 12}, {"iops", 7}, {"read", 10}, {"write", 10}, {"await", 9}, {"queue", 7}, {"util", 6}}

// diskColumns widens the numeric columns evenly when the panel has room.
func diskColumns(iw int) [7]diskCol {
	cols := baseDiskCols
	used := 0
	for _, c := range cols {
		used += c.w
	}
	pad := min(max((iw-used)/6, 0), 5)
	for i := 1; i < len(cols); i++ {
		cols[i].w += pad
	}
	return cols
}

func fmtIOPS(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1fk", v/1000)
	}
	return fmt.Sprintf("%.0f", v)
}

func fmtMs(v float64) string {
	if v < 10 {
		return fmt.Sprintf("%.1f ms", v)
	}
	return fmt.Sprintf("%.0f ms", v)
}

func (f *frame) diskTable(io *IOReport) {
	if !io.HasRates {
		f.row(newLn().put(cFaint, "measuring…"))
		return
	}
	if len(io.Disks) == 0 {
		msg := "no block devices found"
		if io.Idle > 0 {
			msg = fmt.Sprintf("all %d devices idle", io.Idle)
		}
		f.row(newLn().put(cFaint, msg))
		return
	}

	diskCols := diskColumns(f.IW)
	head := newLn()
	for i, c := range diskCols {
		if i == 0 {
			head.put(cFaint, c.title).to(c.w)
		} else {
			head.put(cFaint, rjust(c.title, c.w))
		}
	}
	f.row(head)

	for _, d := range io.Disks[:min(len(io.Disks), maxDisks)] {
		l := newLn().put(cText, fit(d.Name, diskCols[0].w-1)).to(diskCols[0].w)
		cell := func(i int, text string, c rgb, bold bool) {
			if bold {
				l.bold(c, rjust(text, diskCols[i].w))
			} else {
				l.put(c, rjust(text, diskCols[i].w))
			}
		}
		bytesCell := func(i int, b float64) {
			if b <= 0 {
				cell(i, "0", cFaint, false)
			} else {
				cell(i, fmtBytes(b), cText, false)
			}
		}
		cell(1, fmtIOPS(d.IOPS), cText, false)
		bytesCell(2, d.ReadBps)
		bytesCell(3, d.WriteBps)
		cell(4, fmtMs(d.AwaitMs), level(d.AwaitMs, 10, 50, 200), d.AwaitMs >= 10)
		cell(5, fmt.Sprintf("%.1f", d.Queue), cText, false)
		cell(6, fmt.Sprintf("%.0f%%", d.Util), cLabel.scale(0.7), false)
		f.row(l)
	}

	var more []string
	if n := len(io.Disks) - maxDisks; n > 0 {
		more = append(more, fmt.Sprintf("+%d more active", n))
	}
	if io.Idle > 0 {
		more = append(more, fmt.Sprintf("%d idle", io.Idle))
	}
	if len(more) > 0 {
		f.row(newLn().put(cFaint, strings.Join(more, " · ")))
	}
}
