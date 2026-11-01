// strain — is Linux actually short of memory, CPU or I/O?
//
// "Used" memory and CPU utilization both mislead: Linux fills spare RAM with
// cache, and a busy CPU is not necessarily an overloaded one. strain reports
// whether tasks are *waiting* — on memory, for a core, or on storage — using
// the kernel's pressure stall information (PSI), backed by MemAvailable,
// paging rates, run-queue depth, steal time, cgroup throttling and per-device
// latency and queue depth.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const version = "0.3.0"

type config struct {
	interval time.Duration
	snapshot bool
	jsonOut  bool
	check    bool
	cgroup   string
	memBands [3]float64
	cpuBands [3]float64
	ioBands  [3]float64
	color    string
}

const usage = `strain — is Linux actually short of memory, CPU or I/O?

Usage:
  strain [flags]

Modes:
  (default)            live view, refreshing like top
  -s, --snapshot       sample once over the interval window, print, exit
  -j, --json           snapshot as JSON
      --check          one-line, monitoring-plugin style result; exit code
                       0 OK · 1 WARNING · 2 CRITICAL · 3 error
                       (worst of memory, CPU and I/O)

Options:
  -i, --interval DUR   refresh interval, and the snapshot sampling window
                       (default 1s; 250ms–60s)
      --cgroup PATH    measure one cgroup v2 group instead of the system,
                       e.g. system.slice/postgresql.service
      --mem-bands A,B,C
                       memory stall %% where STRAINED, PRESSURED, THRASHING
                       begin (default 1,10,25; --bands is an alias)
      --cpu-bands A,B,C
                       CPU stall %% where BUSY, CONTENDED, SATURATED begin
                       (default 5,20,50)
      --io-bands A,B,C
                       I/O stall %% where BUSY, CONGESTED, SATURATED begin
                       (default 5,20,50)
      --color WHEN     auto, always or never (default auto; honours NO_COLOR)
  -v, --version
  -h, --help

Keys (live):  q quit · p pause · + / - slower / faster refresh

Output goes to a pipe?  strain prints a snapshot instead of the live view.
`

func main() { os.Exit(run()) }

func run() int {
	cfg, err := parseFlags(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(strings.ReplaceAll(usage, "%%", "%"))
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "strain: %v\nTry 'strain --help'.\n", err)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "strain: %v\n", err)
		if cfg.check {
			fmt.Printf("UNKNOWN - %v\n", err)
			return 3
		}
		return 1
	}

	src, err := newSource(cfg.cgroup)
	if err != nil {
		return fail(err)
	}
	outTTY := isTerminal(os.Stdout)
	colorMode = pickColor(cfg.color, outTTY)

	if !cfg.snapshot && !cfg.jsonOut && !cfg.check && outTTY {
		if err := runLive(cfg, src); err != nil {
			return fail(err)
		}
		return 0
	}

	rep, err := snapshot(cfg, src)
	if err != nil {
		return fail(err)
	}
	switch {
	case cfg.check:
		fmt.Println(checkLine(rep, cfg))
		switch rep.Overall() {
		case StateOK:
			return 0
		case StateStrained:
			return 1
		}
		return 2
	case cfg.jsonOut:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(toJSON(rep)); err != nil {
			return fail(err)
		}
	default:
		width := 88
		if outTTY {
			if cols, _ := termSize(os.Stdout); cols > 0 {
				width = panelWidth(cols)
			}
		}
		lines := render(rep, view{
			Width: width, MemBands: cfg.memBands, CPUBands: cfg.cpuBands, IOBands: cfg.ioBands,
			Interval: cfg.interval, Host: hostName(), Kernel: kernelRelease(),
		})
		fmt.Println(strings.Join(lines, "\n"))
	}
	return 0
}

func parseFlags(args []string) (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("strain", flag.ContinueOnError)
	fs.SetOutput(new(strings.Builder)) // we print our own messages
	var memBands, cpuBands, ioBands string
	var showVersion bool
	fs.BoolVar(&c.snapshot, "s", false, "")
	fs.BoolVar(&c.snapshot, "snapshot", false, "")
	fs.BoolVar(&c.jsonOut, "j", false, "")
	fs.BoolVar(&c.jsonOut, "json", false, "")
	fs.BoolVar(&c.check, "check", false, "")
	fs.DurationVar(&c.interval, "i", time.Second, "")
	fs.DurationVar(&c.interval, "interval", time.Second, "")
	fs.StringVar(&c.cgroup, "cgroup", "", "")
	fs.StringVar(&memBands, "bands", "1,10,25", "")
	fs.StringVar(&memBands, "mem-bands", "1,10,25", "")
	fs.StringVar(&cpuBands, "cpu-bands", "5,20,50", "")
	fs.StringVar(&ioBands, "io-bands", "5,20,50", "")
	fs.StringVar(&c.color, "color", "auto", "")
	fs.BoolVar(&showVersion, "v", false, "")
	fs.BoolVar(&showVersion, "version", false, "")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if showVersion {
		fmt.Println("strain", version)
		os.Exit(0)
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if c.interval < 250*time.Millisecond || c.interval > time.Minute {
		return c, fmt.Errorf("--interval must be between 250ms and 60s")
	}
	var err error
	if c.memBands, err = parseBands("--mem-bands", memBands); err != nil {
		return c, err
	}
	if c.cpuBands, err = parseBands("--cpu-bands", cpuBands); err != nil {
		return c, err
	}
	if c.ioBands, err = parseBands("--io-bands", ioBands); err != nil {
		return c, err
	}
	switch c.color {
	case "auto", "always", "never":
	default:
		return c, fmt.Errorf("--color must be auto, always or never")
	}
	return c, nil
}

func parseBands(flagName, spec string) (b [3]float64, err error) {
	parts := strings.Split(spec, ",")
	if len(parts) != 3 {
		return b, fmt.Errorf("%s needs three comma-separated numbers", flagName)
	}
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || v <= 0 || v > 100 || (i > 0 && v <= b[i-1]) {
			return b, fmt.Errorf("%s must be three increasing percentages, got %q", flagName, spec)
		}
		b[i] = v
	}
	return b, nil
}

func pickColor(when string, tty bool) int {
	if when == "never" || (when == "auto" && (!tty || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb")) {
		return colorNone
	}
	switch strings.ToLower(os.Getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return colorTrue
	}
	return color256
}

func panelWidth(cols int) int {
	switch {
	case cols >= 100:
		return 100
	case cols < 60:
		return max(cols, 40)
	}
	return cols
}

func snapshot(cfg *config, src Source) (*Report, error) {
	a, err := src.Probe()
	if err != nil {
		return nil, err
	}
	time.Sleep(cfg.interval)
	b, err := src.Probe()
	if err != nil {
		return nil, err
	}
	return assess(cfg, &a, b, src.Name()), nil
}

// ── live mode ─────────────────────────────────────────────────────────────

var intervalSteps = []time.Duration{
	250 * time.Millisecond, 500 * time.Millisecond, time.Second,
	2 * time.Second, 3 * time.Second, 5 * time.Second, 10 * time.Second,
}

func stepInterval(cur time.Duration, dir int) time.Duration {
	if dir > 0 {
		for _, s := range intervalSteps {
			if s > cur {
				return s
			}
		}
		return cur
	}
	for i := len(intervalSteps) - 1; i >= 0; i-- {
		if intervalSteps[i] < cur {
			return intervalSteps[i]
		}
	}
	return cur
}

func runLive(cfg *config, src Source) error {
	restore, rawErr := rawInput(os.Stdin)
	defer restore()

	out := bufio.NewWriterSize(os.Stdout, 64<<10)
	out.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J")
	out.Flush()
	defer func() {
		out.WriteString("\x1b[?25h\x1b[?1049l")
		out.Flush()
	}()

	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	defer signal.Stop(sig)

	var keys chan byte
	if rawErr == nil {
		keys = make(chan byte, 16)
		go func() {
			buf := make([]byte, 32)
			for {
				n, err := os.Stdin.Read(buf)
				if err != nil {
					close(keys)
					return
				}
				for _, b := range buf[:n] {
					keys <- b
				}
			}
		}()
	}

	host, kernel := hostName(), kernelRelease()
	hist := &history{}
	prev, err := src.Probe()
	if err != nil {
		return err
	}
	rep := assess(cfg, nil, prev, src.Name()) // PSI is meaningful immediately
	interval, paused := cfg.interval, false

	redraw := func() {
		cols, rows := termSize(os.Stdout)
		if cols == 0 {
			cols, rows = 88, 0
		}
		w := panelWidth(cols)
		v := view{
			Width: w, Live: true, Paused: paused, Interval: interval, Hist: hist,
			MemBands: cfg.memBands, CPUBands: cfg.cpuBands, IOBands: cfg.ioBands,
			Host: host, Kernel: kernel,
		}
		// Short terminal: shed history rows, then explanations, then spacer
		// rows, before letting the bottom of the panel fall off-screen.
		lines := render(rep, v)
		for rows > 0 && len(lines) > rows && v.Compact < 3 {
			v.Compact++
			lines = render(rep, v)
		}
		drawFrame(out, lines, cols, rows, w)
	}
	redraw()

	tick := time.NewTimer(interval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if !paused {
				cur, err := src.Probe()
				if err != nil {
					return err
				}
				rep = assess(cfg, &prev, cur, src.Name())
				prev = cur
				hist.push(rep)
			}
			redraw()
			tick.Reset(interval)

		case k, ok := <-keys:
			if !ok {
				keys = nil
				continue
			}
			switch k {
			case 'q', 'Q', 3:
				return nil
			case 'p', 'P', ' ':
				paused = !paused
			case '+', '=':
				interval = stepInterval(interval, +1)
			case '-', '_':
				interval = stepInterval(interval, -1)
			case 'r', 12: // r or Ctrl-L: full repaint
				out.WriteString("\x1b[2J")
			}
			redraw()

		case s := <-sig:
			if s == syscall.SIGWINCH {
				out.WriteString("\x1b[2J")
				redraw()
				continue
			}
			return nil
		}
	}
}

// ── machine-readable output ───────────────────────────────────────────────

type jsonStall struct {
	Some PSILine `json:"some"`
	Full PSILine `json:"full"`
}

type jsonMemory struct {
	State     string     `json:"state"`
	Estimated bool       `json:"estimated"`
	Trend     string     `json:"trend,omitempty"`
	Stall     *jsonStall `json:"stall"`
	StallNote string     `json:"stall_note,omitempty"`

	Total       uint64  `json:"total_bytes"`
	Available   uint64  `json:"available_bytes"`
	Droppable   uint64  `json:"droppable_cache_bytes"`
	SwapUsed    uint64  `json:"swap_used_bytes"`
	SwapTotal   uint64  `json:"swap_total_bytes"`
	NaiveUsed   float64 `json:"naive_used_pct"`
	RealUsed    float64 `json:"real_used_pct"`
	Demand      float64 `json:"demand_pct"`
	CgroupLimit bool    `json:"cgroup_limited,omitempty"`

	Rates *Rates `json:"rates"`
}

type jsonCPU struct {
	State     string     `json:"state"`
	Estimated bool       `json:"estimated"`
	Trend     string     `json:"trend,omitempty"`
	Stall     *jsonStall `json:"stall"`
	StallNote string     `json:"stall_note,omitempty"`

	Cores        float64  `json:"cores"`
	Quota        bool     `json:"cgroup_quota,omitempty"`
	BusyPct      *float64 `json:"busy_pct"`
	StealPct     *float64 `json:"steal_pct"`
	Waiting      *float64 `json:"run_queue_tasks"`
	WaitingNow   bool     `json:"run_queue_instantaneous,omitempty"`
	ThrottledMsS *float64 `json:"throttled_ms_per_s,omitempty"`
}

type jsonIO struct {
	State     string       `json:"state"`
	Estimated bool         `json:"estimated"`
	Trend     string       `json:"trend,omitempty"`
	Stall     *jsonStall   `json:"stall"`
	StallNote string       `json:"stall_note,omitempty"`
	Devices   []DiskReport `json:"devices"`
	Idle      int          `json:"idle_devices"`
}

type jsonReport struct {
	Time   time.Time  `json:"time"`
	Host   string     `json:"host"`
	Kernel string     `json:"kernel"`
	Source string     `json:"source"`
	Level  string     `json:"level"` // ok · warning · critical (worst of the three)
	CPU    jsonCPU    `json:"cpu"`
	Memory jsonMemory `json:"memory"`
	IO     jsonIO     `json:"io"`
}

var trendNames = [...]string{"easing", "steady", "rising"}

func toJSON(r *Report) jsonReport {
	s, c := r.Sample, r.CPU
	j := jsonReport{
		Time: r.At, Host: hostName(), Kernel: kernelRelease(), Source: r.Source,
		Level: strings.ToLower(r.Overall().Severity()),
	}

	m := &j.Memory
	m.State, m.Estimated = strings.ToLower(r.State.String()), r.Estimated
	if s.HasPSI {
		m.Stall, m.Trend = &jsonStall{s.Some, s.Full}, trendNames[r.Trend+1]
	} else {
		m.StallNote = s.PSIReason
	}
	m.Total, m.Available, m.Droppable = s.Total, s.Available, s.Droppable
	m.SwapUsed, m.SwapTotal = s.SwapUsed, s.SwapTotal
	m.NaiveUsed, m.RealUsed, m.Demand = round1(r.NaiveUsed), round1(r.RealUsed), round1(r.Demand)
	m.CgroupLimit = s.Limited
	if r.HasRates {
		rt := r.Rates
		m.Rates = &rt
	}

	p := &j.CPU
	p.State, p.Estimated = strings.ToLower(cpuStateNames[c.State]), c.Estimated
	if s.CPU.HasPSI {
		p.Stall, p.Trend = &jsonStall{s.CPU.Some, s.CPU.Full}, trendNames[c.Trend+1]
	} else {
		p.StallNote = s.CPU.PSIReason
	}
	p.Cores, p.Quota = round1(c.Cores), s.CPU.Quota
	ptr := func(v float64) *float64 { v = round1(v); return &v }
	if c.HasRates {
		p.BusyPct = ptr(c.Busy)
		if c.HasSteal {
			p.StealPct = ptr(c.Steal)
		}
		if c.HasThrottle {
			p.ThrottledMsS = ptr(c.Throttled)
		}
	}
	if c.HasWaiting {
		p.Waiting, p.WaitingNow = ptr(c.Waiting), c.WaitApprox
	}

	io, q := r.IO, &j.IO
	q.State, q.Estimated = strings.ToLower(ioStateNames[io.State]), io.Estimated
	if s.IO.HasPSI {
		q.Stall, q.Trend = &jsonStall{s.IO.Some, s.IO.Full}, trendNames[io.Trend+1]
	} else {
		q.StallNote = s.IO.PSIReason
	}
	q.Devices, q.Idle = []DiskReport{}, io.Idle
	for _, d := range io.Disks {
		if !d.Active {
			continue
		}
		d.IOPS, d.AwaitMs, d.Queue, d.Util = round1(d.IOPS), round1(d.AwaitMs), round1(d.Queue), round1(d.Util)
		d.ReadBps, d.WriteBps = math.Round(d.ReadBps), math.Round(d.WriteBps)
		q.Devices = append(q.Devices, d)
	}
	return j
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func checkLine(r *Report, c *config) string {
	s, cp := r.Sample, r.CPU
	var b strings.Builder
	fmt.Fprintf(&b, "%s - cpu %s", r.Overall().Severity(), cpuStateNames[cp.State])
	if s.CPU.HasPSI {
		fmt.Fprintf(&b, " (stall %.1f%%, busy %.0f%%)", s.CPU.Some.Avg10, cp.Busy)
	} else {
		fmt.Fprintf(&b, " (estimated, busy %.0f%%)", cp.Busy)
	}
	fmt.Fprintf(&b, ", memory %s", r.State)
	if s.HasPSI {
		fmt.Fprintf(&b, " (stall %.1f%%, real used %.0f%%)", s.Some.Avg10, r.RealUsed)
	} else {
		fmt.Fprintf(&b, " (estimated, real used %.0f%%)", r.RealUsed)
	}
	fmt.Fprintf(&b, ", io %s", ioStateNames[r.IO.State])
	if s.IO.HasPSI {
		fmt.Fprintf(&b, " (stall %.1f%%)", s.IO.Some.Avg10)
	} else {
		b.WriteString(" (estimated)")
	}
	fmt.Fprintf(&b, " | cpu_stall=%.2f%%;%g;%g cpu_busy=%.1f%% cpu_steal=%.1f%% run_queue=%.2f",
		s.CPU.Some.Avg10, c.cpuBands[0], c.cpuBands[1], cp.Busy, cp.Steal, cp.Waiting)
	fmt.Fprintf(&b, " mem_stall=%.2f%%;%g;%g mem_stall_full=%.2f%% real_used=%.1f%% demand=%.1f%%",
		s.Some.Avg10, c.memBands[0], c.memBands[1], s.Full.Avg10, r.RealUsed, r.Demand)
	fmt.Fprintf(&b, " swap_in=%.0fB refault=%.0fB direct_scan=%.0fB",
		r.Rates.SwapIn, r.Rates.Refault, r.Rates.DirectScan)
	worstAwait := 0.0
	for _, d := range r.IO.Disks {
		worstAwait = max(worstAwait, d.AwaitMs)
	}
	fmt.Fprintf(&b, " io_stall=%.2f%%;%g;%g io_stall_full=%.2f%% io_await_max=%.1fms",
		s.IO.Some.Avg10, c.ioBands[0], c.ioBands[1], s.IO.Full.Avg10, worstAwait)
	return b.String()
}
