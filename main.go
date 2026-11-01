// strain — how hard is Linux actually working to find memory?
//
// Linux fills spare RAM with cache, so "used" memory looks alarming on any
// healthy box. strain instead reports whether tasks are *waiting* on memory
// (PSI), how much memory is genuinely spoken for (MemAvailable), and whether
// evicted data is being pulled back in (swap-in, refaults, direct reclaim).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

type config struct {
	interval time.Duration
	snapshot bool
	jsonOut  bool
	check    bool
	cgroup   string
	bands    [3]float64
	color    string
}

const usage = `strain — how hard is Linux actually working to find memory?

Usage:
  strain [flags]

Modes:
  (default)            live view, refreshing like top
  -s, --snapshot       sample once over the interval window, print, exit
  -j, --json           snapshot as JSON
      --check          one-line, monitoring-plugin style result; exit code
                       0 OK · 1 STRAINED · 2 PRESSURED/THRASHING · 3 error

Options:
  -i, --interval DUR   refresh interval, and the snapshot sampling window
                       (default 1s; 250ms–60s)
      --cgroup PATH    measure one cgroup v2 group instead of the system,
                       e.g. system.slice/postgresql.service
      --bands A,B,C    stall %% where STRAINED, PRESSURED, THRASHING begin
                       (default 1,10,25)
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
		switch rep.State {
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
			Width: width, Bands: cfg.bands, Interval: cfg.interval,
			Host: hostName(), Kernel: kernelRelease(),
		})
		fmt.Println(strings.Join(lines, "\n"))
	}
	return 0
}

func parseFlags(args []string) (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("strain", flag.ContinueOnError)
	fs.SetOutput(new(strings.Builder)) // we print our own messages
	var bands string
	var showVersion bool
	fs.BoolVar(&c.snapshot, "s", false, "")
	fs.BoolVar(&c.snapshot, "snapshot", false, "")
	fs.BoolVar(&c.jsonOut, "j", false, "")
	fs.BoolVar(&c.jsonOut, "json", false, "")
	fs.BoolVar(&c.check, "check", false, "")
	fs.DurationVar(&c.interval, "i", time.Second, "")
	fs.DurationVar(&c.interval, "interval", time.Second, "")
	fs.StringVar(&c.cgroup, "cgroup", "", "")
	fs.StringVar(&bands, "bands", "1,10,25", "")
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
	parts := strings.Split(bands, ",")
	if len(parts) != 3 {
		return c, fmt.Errorf("--bands needs three comma-separated numbers")
	}
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || v <= 0 || v > 100 || (i > 0 && v <= c.bands[i-1]) {
			return c, fmt.Errorf("--bands must be three increasing percentages, got %q", bands)
		}
		c.bands[i] = v
	}
	switch c.color {
	case "auto", "always", "never":
	default:
		return c, fmt.Errorf("--color must be auto, always or never")
	}
	return c, nil
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
		lines := render(rep, view{
			Width: w, Live: true, Paused: paused, Interval: interval,
			Hist: hist, Bands: cfg.bands, Host: host, Kernel: kernel,
		})
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

type jsonReport struct {
	Time      time.Time `json:"time"`
	Host      string    `json:"host"`
	Kernel    string    `json:"kernel"`
	Source    string    `json:"source"`
	State     string    `json:"state"`
	Estimated bool      `json:"estimated"`
	Trend     string    `json:"trend,omitempty"`
	Stall     *struct {
		Some PSILine `json:"some"`
		Full PSILine `json:"full"`
	} `json:"stall"`
	StallNote string `json:"stall_note,omitempty"`
	Memory    struct {
		Total       uint64  `json:"total_bytes"`
		Available   uint64  `json:"available_bytes"`
		Droppable   uint64  `json:"droppable_cache_bytes"`
		SwapUsed    uint64  `json:"swap_used_bytes"`
		SwapTotal   uint64  `json:"swap_total_bytes"`
		NaiveUsed   float64 `json:"naive_used_pct"`
		RealUsed    float64 `json:"real_used_pct"`
		Demand      float64 `json:"demand_pct"`
		CgroupLimit bool    `json:"cgroup_limited,omitempty"`
	} `json:"memory"`
	Rates *Rates `json:"rates"`
}

func toJSON(r *Report) jsonReport {
	s := r.Sample
	j := jsonReport{
		Time: r.At, Host: hostName(), Kernel: kernelRelease(), Source: r.Source,
		State: strings.ToLower(r.State.String()), Estimated: r.Estimated,
	}
	if s.HasPSI {
		j.Stall = &struct {
			Some PSILine `json:"some"`
			Full PSILine `json:"full"`
		}{s.Some, s.Full}
		j.Trend = [...]string{"easing", "steady", "rising"}[r.Trend+1]
	} else {
		j.StallNote = s.PSIReason
	}
	m := &j.Memory
	m.Total, m.Available, m.Droppable = s.Total, s.Available, s.Droppable
	m.SwapUsed, m.SwapTotal = s.SwapUsed, s.SwapTotal
	m.NaiveUsed, m.RealUsed, m.Demand = round1(r.NaiveUsed), round1(r.RealUsed), round1(r.Demand)
	m.CgroupLimit = s.Limited
	if r.HasRates {
		rt := r.Rates
		j.Rates = &rt
	}
	return j
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

func checkLine(r *Report, c *config) string {
	s := r.Sample
	var b strings.Builder
	fmt.Fprintf(&b, "%s - ", r.State)
	if s.HasPSI {
		fmt.Fprintf(&b, "memory stall %.1f%% (full %.1f%%)", s.Some.Avg10, s.Full.Avg10)
	} else {
		fmt.Fprintf(&b, "estimated, %s", s.PSIReason)
	}
	fmt.Fprintf(&b, ", real used %.0f%%, demand %.0f%%", r.RealUsed, r.Demand)
	fmt.Fprintf(&b, " | stall_some=%.2f%%;%g;%g stall_full=%.2f%% real_used=%.1f%% demand=%.1f%%",
		s.Some.Avg10, c.bands[0], c.bands[1], s.Full.Avg10, r.RealUsed, r.Demand)
	fmt.Fprintf(&b, " swap_in=%.0fB refault=%.0fB direct_scan=%.0fB",
		r.Rates.SwapIn, r.Rates.Refault, r.Rates.DirectScan)
	return b.String()
}
