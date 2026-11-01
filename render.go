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

func (f *frame) blank() { f.row(newLn()) }

// ── glyph helpers ─────────────────────────────────────────────────────────

var (
	eighths = []rune(" ▏▎▍▌▋▊▉")
	sparks  = []rune("▁▂▃▄▅▆▇█")
)

// The stall meter uses a square-root scale over 0–50 % so that the bands
// that matter most (1 %, 10 %) get real estate instead of a sliver.
const meterMax = 50.0

func meterPos(v float64) float64 {
	return math.Sqrt(math.Min(math.Max(v, 0), meterMax) / meterMax)
}

func meter(v float64, w int, b [3]float64) *ln {
	l := newLn()
	fill := meterPos(v) * float64(w)
	full := int(fill)
	part := int((fill - float64(full)) * 8)
	for i := 0; i < w; i++ {
		cellV := math.Pow((float64(i)+0.5)/float64(w), 2) * meterMax
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

func meterTicks(w int, b [3]float64) *ln {
	cells := []rune(strings.Repeat(" ", w))
	place := func(v float64, label string) {
		r := []rune(label)
		start := int(math.Round(meterPos(v) * float64(w)))
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
	place(meterMax, fmt.Sprintf("%g%%", meterMax))
	return newLn().put(cFaint, string(cells))
}

func bar(p float64, w int, c rgb) *ln {
	l := newLn()
	fill := math.Min(math.Max(p, 0), 100) / 100 * float64(w)
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

func stallSpark(vals []float64, w int, b [3]float64) *ln {
	vals = tail(vals, w)
	l := newLn().sp(w - len(vals))
	for _, v := range vals {
		if v <= 0 {
			l.put(cFaint, "▁")
			continue
		}
		i := min(int(meterPos(v)*8), 7)
		l.put(stateColor[zoneOf(v, b)], string(sparks[i]))
	}
	return l
}

func rateSpark(vals []float64, w int, hot bool) *ln {
	vals = tail(vals, w)
	peak := float64(mib)
	for _, v := range vals {
		peak = math.Max(peak, v)
	}
	l := newLn().sp(w - len(vals))
	for _, v := range vals {
		if v <= 0 {
			l.put(cFaint, "▁")
			continue
		}
		i := min(int(v/peak*8), 7)
		l.put(rateColor(v, hot), string(sparks[i]))
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

func rateColor(bps float64, hot bool) rgb {
	switch {
	case bps <= 0:
		return cLabel
	case !hot || bps < 1*mib:
		return cText
	case bps < 16*mib:
		return stateColor[StateStrained]
	case bps < 64*mib:
		return stateColor[StatePressured]
	}
	return stateColor[StateThrashing]
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
}

func (h *history) push(r *Report) {
	if !r.HasRates {
		return
	}
	h.at = append(h.at, r.At)
	h.stall = append(h.stall, r.Sample.Some.Avg10)
	h.swapIn = append(h.swapIn, r.Rates.SwapIn)
	h.swapOut = append(h.swapOut, r.Rates.SwapOut)
	h.refault = append(h.refault, r.Rates.Refault)
	h.direct = append(h.direct, r.Rates.DirectScan)
	if n := len(h.at); n > histCap {
		cut := n - histCap
		h.at = h.at[cut:]
		h.stall, h.swapIn, h.swapOut = h.stall[cut:], h.swapIn[cut:], h.swapOut[cut:]
		h.refault, h.direct = h.refault[cut:], h.direct[cut:]
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
	Interval time.Duration
	Hist     *history
	Bands    [3]float64
	Kernel   string
	Host     string
}

func render(r *Report, v view) []string {
	f := newFrame(v.Width)
	iw := f.IW
	s := r.Sample
	sc := stateColor[r.State]

	// ── header
	title := newLn().bold(cAccent, "strain").put(cLabel, " · memory pressure")
	right := newLn().put(cLabel, v.Host+" · "+r.Source+" · "+r.At.Format("15:04:05"))
	if title.w+right.w+8 > f.W {
		right = newLn().put(cLabel, r.At.Format("15:04:05"))
	}
	f.rule("╭", "╮", title, right)
	f.blank()

	// ── verdict
	head := newLn().put(sc, "● ").bold(sc, r.State.String()).to(14)
	if s.HasPSI {
		suffix := " of the last 10 s stalled on memory"
		if iw < 80 {
			suffix = " stalled on memory"
		}
		head.bold(cText, fmt.Sprintf("%.1f%%", s.Some.Avg10)).put(cLabel, suffix)
	} else {
		head.put(cLabel, fit("estimated · "+s.PSIReason, iw-head.w))
	}
	var trend *ln
	switch {
	case !s.HasPSI:
	case r.Trend > 0:
		trend = newLn().put(stateColor[StatePressured], "▲ rising")
	case r.Trend < 0:
		trend = newLn().put(stateColor[StateOK], "▼ easing")
	default:
		trend = newLn().put(cFaint, "━ steady")
	}
	if trend != nil && head.w+trend.w+2 <= iw {
		head.to(iw - trend.w).cat(trend)
	}
	f.row(head)
	f.row(newLn().sp(2).put(cLabel, fit(explain(r), iw-2)))
	f.blank()

	// ── stall meter
	const lw = 9
	mw := iw - lw
	if s.HasPSI {
		f.row(newLn().put(cLabel, "stall").to(lw).cat(meter(s.Some.Avg10, mw, v.Bands)))
		f.row(newLn().to(lw).cat(meterTicks(mw, v.Bands)))
		fig := newLn().to(lw).
			put(cLabel, "some ").bold(cText, fmt.Sprintf("%.2f%%", s.Some.Avg10)).sp(3).
			put(cLabel, "full ").bold(cText, fmt.Sprintf("%.2f%%", s.Full.Avg10))
		avgs := newLn().put(cLabel, "1m ").put(cText, fmt.Sprintf("%.2f%%", s.Some.Avg60)).sp(3).
			put(cLabel, "5m ").put(cText, fmt.Sprintf("%.2f%%", s.Some.Avg300))
		if fig.w+avgs.w+3 <= iw {
			fig.to(iw - avgs.w).cat(avgs)
		}
		f.row(fig)

		if v.Live && v.Hist != nil {
			f.blank()
			f.row(newLn().put(cLabel, "history").to(lw).cat(stallSpark(v.Hist.stall, mw, v.Bands)))
			caption := newLn().to(lw)
			if n := min(len(v.Hist.at), mw); n >= 14 {
				span := v.Hist.at[len(v.Hist.at)-1].Sub(v.Hist.at[len(v.Hist.at)-n])
				caption.to(lw+mw-n).put(cFaint, "◂ "+fmtSpan(span))
			}
			if caption.w <= iw-4 {
				caption.to(iw-3).put(cFaint, "now")
			}
			f.row(caption)
		}
		f.blank()
	}

	// ── capacity
	capR := newLn().put(cLabel, fmtBytes(float64(s.Total))+" RAM")
	if r.Source != "system" {
		if s.Limited {
			capR = newLn().put(cLabel, "limit "+fmtBytes(float64(s.Total)))
		} else {
			capR = newLn().put(cLabel, "no limit · vs host RAM")
		}
	}
	f.rule("├", "┤", newLn().bold(cAccent, "capacity"), capR)
	f.blank()
	const lw2 = 13
	bw := min(max(iw-lw2-9-30, 12), 44)
	noteW := iw - lw2 - bw - 9
	capRow := func(label string, p float64, c rgb, note string, strong bool) {
		l := newLn().put(cLabel, label).to(lw2).cat(bar(p, bw, c)).sp(2)
		pct := rjust(fmt.Sprintf("%.0f%%", p), 5)
		if strong {
			l.bold(c, pct)
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

	// ── paging activity
	f.rule("├", "┤", newLn().bold(cAccent, "paging activity"), newLn().put(cLabel, "per second"))
	f.blank()
	const lw3, vw = 16, 11
	sw := 0
	if v.Live {
		sw = min(max(iw-lw3-vw-4-34, 8), 36)
	}
	noteW3 := iw - lw3 - vw - 2
	if sw > 0 {
		noteW3 -= sw + 2
	}
	var hs struct{ in, out, ref, dir []float64 }
	if v.Hist != nil {
		hs.in, hs.out, hs.ref, hs.dir = v.Hist.swapIn, v.Hist.swapOut, v.Hist.refault, v.Hist.direct
	}
	act := func(label string, bps float64, series []float64, note string, hot, avail bool) {
		l := newLn().put(cLabel, label).to(lw3)
		switch {
		case !avail:
			l.put(cFaint, rjust("n/a", vw))
		case !r.HasRates:
			l.put(cFaint, rjust("measuring", vw))
		case bps <= 0:
			l.put(cFaint, rjust("0", vw))
		default:
			l.bold(rateColor(bps, hot), rjust(fmtBytes(bps), vw))
		}
		l.sp(2)
		if sw > 0 {
			if avail {
				l.cat(rateSpark(series, sw, hot))
			} else {
				l.sp(sw)
			}
			l.sp(2)
		}
		l.put(cFaint, fit(note, noteW3))
		f.row(l)
	}
	act("swap-in", r.Rates.SwapIn, hs.in, "swapped data wanted back", true, s.HasSwapIO)
	act("refaults", r.Rates.Refault, hs.ref, "evicted pages needed again", true, true)
	act("direct reclaim", r.Rates.DirectScan, hs.dir, "stalled allocations", true, true)
	act("swap-out", r.Rates.SwapOut, hs.out, "idle pages, usually benign", false, s.HasSwapIO)
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
