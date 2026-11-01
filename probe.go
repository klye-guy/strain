package main

// probe.go — reads raw memory state from procfs / cgroupfs.
//
// Everything here is a plain read of kernel-exported text files; no root is
// needed for the system view, and cgroup files are world-readable by default.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PSILine is one line of a PSI file: the share of wall time (percent) that
// tasks spent stalled on memory, averaged over 10 s, 60 s and 300 s.
type PSILine struct {
	Avg10  float64 `json:"avg10"`
	Avg60  float64 `json:"avg60"`
	Avg300 float64 `json:"avg300"`
}

// Sample is one point-in-time read. Byte fields are bytes; counter fields are
// monotonically increasing page counts that only mean something as deltas.
type Sample struct {
	At time.Time

	Total     uint64 // RAM (or cgroup limit)
	Available uint64 // what could be handed out without swapping
	Free      uint64 // truly unused — the misleading number
	Droppable uint64 // cache the kernel can reclaim on demand
	SwapTotal uint64
	SwapUsed  uint64
	Limited   bool // cgroup with an explicit memory.max

	HasPSI    bool
	PSIReason string
	Some      PSILine
	Full      PSILine

	SwapIn, SwapOut, Refault, DirectScan uint64 // pages
	HasSwapIO                            bool

	CPU CPUSample
	IO  IOSample
}

// Source produces Samples.
type Source interface {
	Name() string
	Probe() (Sample, error)
}

var pageSize = uint64(os.Getpagesize())

func sat(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return 0
}

// readKV parses "key value [kB]" and "key: value kB" files
// (meminfo, vmstat, memory.stat). kB values are converted to bytes.
func readKV(path string) (map[string]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := make(map[string]uint64, 128)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fs[1], 10, 64)
		if err != nil {
			continue
		}
		if len(fs) > 2 && fs[2] == "kB" {
			v *= 1024
		}
		m[strings.TrimSuffix(fs[0], ":")] = v
	}
	return m, sc.Err()
}

func readPSI(path string) (some, full PSILine, reason string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			reason = "kernel has no PSI support"
		case errors.Is(err, syscall.EOPNOTSUPP):
			reason = "PSI disabled (boot with psi=1)"
		default:
			reason = "PSI unreadable"
		}
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		fs := strings.Fields(line)
		if len(fs) < 4 {
			continue
		}
		var p PSILine
		for _, kv := range fs[1:] {
			k, v, found := strings.Cut(kv, "=")
			if !found {
				continue
			}
			x, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			switch k {
			case "avg10":
				p.Avg10 = x
			case "avg60":
				p.Avg60 = x
			case "avg300":
				p.Avg300 = x
			}
		}
		switch fs[0] {
		case "some":
			some, ok = p, true
		case "full":
			full = p
		}
	}
	if !ok {
		reason = "PSI file empty"
	}
	return
}

// refaults: pages evicted and then faulted back in shortly after.
// Split into anon/file since 5.9; a single counter before that.
func refaults(m map[string]uint64) uint64 {
	a, okA := m["workingset_refault_anon"]
	f, okF := m["workingset_refault_file"]
	if okA || okF {
		return a + f
	}
	return m["workingset_refault"]
}

// directScan: pages scanned by tasks forced to reclaim inside their own
// allocation (as opposed to the background kswapd thread).
func directScan(m map[string]uint64) uint64 {
	if v, ok := m["pgscan_direct"]; ok {
		return v
	}
	var sum uint64 // older kernels split per zone
	for k, v := range m {
		if strings.HasPrefix(k, "pgscan_direct_") && !strings.HasSuffix(k, "_throttle") {
			sum += v
		}
	}
	return sum
}

// ── whole system ──────────────────────────────────────────────────────────

type systemSource struct{}

func (systemSource) Name() string { return "system" }

func (systemSource) Probe() (Sample, error) {
	s := Sample{At: time.Now()}
	mi, err := readKV("/proc/meminfo")
	if err != nil {
		return s, fmt.Errorf("reading /proc/meminfo: %w", err)
	}
	s.Total, s.Free = mi["MemTotal"], mi["MemFree"]
	if v, ok := mi["MemAvailable"]; ok {
		s.Available = v
	} else { // pre-3.14 kernels: rough equivalent
		s.Available = s.Free + mi["Buffers"] + mi["Cached"]
	}
	s.Available = min(s.Available, s.Total)
	s.Droppable = sat(s.Available, s.Free)
	s.SwapTotal = mi["SwapTotal"]
	s.SwapUsed = sat(mi["SwapTotal"], mi["SwapFree"])

	vm, err := readKV("/proc/vmstat")
	if err != nil {
		return s, fmt.Errorf("reading /proc/vmstat: %w", err)
	}
	s.SwapIn, s.SwapOut, s.HasSwapIO = vm["pswpin"], vm["pswpout"], true
	s.Refault = refaults(vm)
	s.DirectScan = directScan(vm)

	s.Some, s.Full, s.PSIReason, s.HasPSI = readPSI("/proc/pressure/memory")
	s.CPU = probeCPUSystem()
	s.IO = probeIO("/proc/pressure/io")
	return s, nil
}

// ── one cgroup v2 group ──────────────────────────────────────────────────

type cgroupSource struct{ dir, label string }

func newSource(arg string) (Source, error) {
	if arg == "" {
		return systemSource{}, nil
	}
	dir := arg
	if !filepath.IsAbs(dir) {
		dir = filepath.Join("/sys/fs/cgroup", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory.current")); err != nil {
		return nil, fmt.Errorf("%s is not a cgroup v2 group with the memory controller enabled", dir)
	}
	rel := strings.TrimPrefix(dir, "/sys/fs/cgroup")
	if rel == "" {
		rel = "/"
	}
	return cgroupSource{dir: dir, label: rel}, nil
}

func (c cgroupSource) Name() string { return c.label }

func readCgroupNum(path string) (v uint64, unlimited bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false, err
	}
	t := strings.TrimSpace(string(b))
	if t == "max" {
		return 0, true, nil
	}
	v, err = strconv.ParseUint(t, 10, 64)
	return v, false, err
}

func (c cgroupSource) Probe() (Sample, error) {
	s := Sample{At: time.Now()}
	at := func(name string) string { return filepath.Join(c.dir, name) }

	host, err := readKV("/proc/meminfo")
	if err != nil {
		return s, fmt.Errorf("reading /proc/meminfo: %w", err)
	}
	cur, _, err := readCgroupNum(at("memory.current"))
	if err != nil {
		return s, fmt.Errorf("reading %s: %w", at("memory.current"), err)
	}
	limit, unlimited, err := readCgroupNum(at("memory.max"))
	if err != nil || unlimited || limit >= host["MemTotal"] {
		limit = host["MemTotal"]
	} else {
		s.Limited = true
	}
	st, err := readKV(at("memory.stat"))
	if err != nil {
		return s, fmt.Errorf("reading %s: %w", at("memory.stat"), err)
	}

	// Working set = usage minus inactive page cache (same convention as
	// the kubelet). Inactive file pages are the first thing reclaim drops.
	inactive := min(st["inactive_file"], cur)
	s.Total = limit
	s.Free = sat(limit, cur)
	s.Available = sat(limit, cur-inactive)
	s.Droppable = inactive

	s.SwapTotal = host["SwapTotal"]
	if v, unl, err := readCgroupNum(at("memory.swap.max")); err == nil && !unl {
		s.SwapTotal = min(v, s.SwapTotal)
	}
	if v, _, err := readCgroupNum(at("memory.swap.current")); err == nil {
		s.SwapUsed = v
	}

	s.Refault = refaults(st)
	s.DirectScan = directScan(st)
	if v, ok := st["pswpin"]; ok {
		s.SwapIn, s.SwapOut, s.HasSwapIO = v, st["pswpout"], true
	}

	s.Some, s.Full, s.PSIReason, s.HasPSI = readPSI(at("memory.pressure"))
	s.CPU = probeCPUCgroup(c.dir)
	s.IO = probeIO(at("io.pressure")) // device rows stay host-wide
	return s, nil
}

// ── host facts (read once) ────────────────────────────────────────────────

func hostName() string {
	h, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}
