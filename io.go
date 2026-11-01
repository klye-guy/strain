package main

// io.go — I/O pressure: are tasks waiting on storage?
//
// Headline: /proc/pressure/io "some" — % of wall time at least one task was
// stalled on I/O. Unlike CPU, "full" is meaningful system-wide: every
// non-idle task stalled on I/O at once. Supporting per-device detail comes
// from /proc/diskstats: IOPS, throughput, average latency (await), average
// queue depth and %util — the last shown only for contrast, because on any
// device that serves requests in parallel (NVMe, RAID, SAN) "100 % util"
// says nothing about remaining capacity.

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type diskStat struct {
	reads, writes    uint64 // completed requests
	rsect, wsect     uint64 // 512-byte sectors
	rms, wms         uint64 // ms spent on reads / writes
	ioms, weightedMs uint64 // ms with I/O in flight; ms × requests in flight
}

type IOSample struct {
	HasPSI    bool
	PSIReason string
	Some      PSILine
	Full      PSILine

	Disks    map[string]diskStat
	HasDisks bool
}

type DiskReport struct {
	Name     string  `json:"device"`
	IOPS     float64 `json:"iops"`
	ReadBps  float64 `json:"read_bps"`
	WriteBps float64 `json:"write_bps"`
	AwaitMs  float64 `json:"await_ms"`
	Queue    float64 `json:"queue_depth"`
	Util     float64 `json:"util_pct"`

	Active   bool   `json:"-"` // any I/O in this window
	Lifetime uint64 `json:"-"` // sectors moved since boot: a stable sort key
}

type IOReport struct {
	State     State
	Estimated bool
	Trend     int

	HasRates bool
	Disks    []DiskReport // every device, busiest-since-boot first (stable)
	Idle     int          // devices with no I/O in the window
}

var ioStateNames = [...]string{"OK", "BUSY", "CONGESTED", "SATURATED"}

// blockDevices maps kernel device names to display names for whole devices
// worth showing: no partitions (absent from /sys/block), loop, ram, zram,
// floppy or optical. Device-mapper volumes take their dm name (pve-root).
func blockDevices() map[string]string {
	ents, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(ents))
	for _, e := range ents {
		n := e.Name()
		skip := false
		for _, p := range []string{"loop", "ram", "zram", "fd", "sr"} {
			if strings.HasPrefix(n, p) {
				skip = true
			}
		}
		if skip {
			continue
		}
		label := n
		if strings.HasPrefix(n, "dm-") {
			if b, err := os.ReadFile(filepath.Join("/sys/block", n, "dm", "name")); err == nil {
				if dm := strings.TrimSpace(string(b)); dm != "" {
					label = dm
				}
			}
		}
		out[n] = label
	}
	return out
}

// devices is resolved once: the set of block devices rarely changes while
// strain runs, and re-reading sysfs every tick is wasted work.
var devices = blockDevices()

func readDiskstats() (map[string]diskStat, bool) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil, false
	}
	defer f.Close()
	out := make(map[string]diskStat)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 14 {
			continue
		}
		label, ok := devices[fs[2]]
		if !ok {
			continue
		}
		u := func(i int) uint64 { v, _ := strconv.ParseUint(fs[i], 10, 64); return v }
		out[label] = diskStat{
			reads: u(3), rsect: u(5), rms: u(6),
			writes: u(7), wsect: u(9), wms: u(10),
			ioms: u(12), weightedMs: u(13),
		}
	}
	return out, sc.Err() == nil
}

func probeIO(psiPath string) IOSample {
	var s IOSample
	s.Some, s.Full, s.PSIReason, s.HasPSI = readPSI(psiPath)
	s.Disks, s.HasDisks = readDiskstats()
	return s
}

func assessIO(c *config, prev *IOSample, cur IOSample, dt float64) IOReport {
	var r IOReport

	// Every device gets a row, idle or not, ordered by lifetime traffic.
	// That order barely changes while strain runs, so the table neither
	// grows and shrinks nor reshuffles as disks go busy and quiet.
	r.HasRates = prev != nil && dt > 0.05 && prev.HasDisks && cur.HasDisks
	ms := dt * 1000
	for name, b := range cur.Disks {
		d := DiskReport{Name: name, Lifetime: b.rsect + b.wsect}
		if a, ok := prev.diskOK(name); ok && r.HasRates {
			ops := float64(sat(b.reads, a.reads) + sat(b.writes, a.writes))
			busy := float64(sat(b.ioms, a.ioms))
			d.Active = ops > 0 || busy > 0
			d.IOPS = ops / dt
			d.ReadBps = float64(sat(b.rsect, a.rsect)) * 512 / dt
			d.WriteBps = float64(sat(b.wsect, a.wsect)) * 512 / dt
			d.Queue = float64(sat(b.weightedMs, a.weightedMs)) / ms
			d.Util = clamp(100*busy/ms, 0, 100)
			if ops > 0 {
				d.AwaitMs = float64(sat(b.rms, a.rms)+sat(b.wms, a.wms)) / ops
			}
		}
		if !d.Active && r.HasRates {
			r.Idle++
		}
		r.Disks = append(r.Disks, d)
	}
	sort.Slice(r.Disks, func(i, j int) bool {
		if r.Disks[i].Lifetime != r.Disks[j].Lifetime {
			return r.Disks[i].Lifetime > r.Disks[j].Lifetime
		}
		return r.Disks[i].Name < r.Disks[j].Name
	})

	switch {
	case cur.HasPSI:
		r.State = zoneOf(cur.Some.Avg10, c.ioBands)
		// "full": every non-idle task was stalled on I/O at the same time.
		if f := cur.Full.Avg10; f >= 20 {
			r.State = StateThrashing
		} else if f >= 5 && r.State < StatePressured {
			r.State = StatePressured
		}
		r.Trend = trendOf(cur.Some)
	case r.HasRates:
		// No PSI: judge by the worst device latency. Loose thresholds that
		// flag real trouble on any media, from NVMe to spinning disks.
		r.Estimated = true
		worst := 0.0
		for _, d := range r.Disks {
			worst = max(worst, d.AwaitMs)
		}
		switch {
		case worst >= 500:
			r.State = StateThrashing
		case worst >= 100:
			r.State = StatePressured
		case worst >= 20:
			r.State = StateStrained
		}
	default:
		r.Estimated = true
	}
	return r
}

func ioExplain(r *IOReport, hasPSI bool) string {
	if !hasPSI && !r.HasRates {
		return "Measuring disk activity…"
	}
	return [...]string{
		"Tasks rarely wait on storage.",
		"Some waiting on storage; latency starting to slip.",
		"Tasks regularly stall waiting on storage.",
		"Storage can't keep up; work is waiting on I/O.",
	}[r.State]
}

func (s *IOSample) diskOK(name string) (diskStat, bool) {
	if s == nil {
		return diskStat{}, false
	}
	d, ok := s.Disks[name]
	return d, ok
}
