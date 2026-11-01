# strain

Is Linux actually short of memory, CPU or I/O?

All the usual numbers mislead. Linux fills spare RAM with page cache, so
`free` and `top` report a high "used" figure on any healthy machine. A CPU at
100 % utilization may be doing ideal work, while one at 60 % can still have
tasks queuing. A disk at "100 % util" may have most of its capacity left.
`strain` ignores those baselines and answers the real question:
**are tasks waiting, on memory, for a core, or on storage?**

Single static binary, Go standard library only, no root required.

```
strain                 # live view, refreshing like top
strain -s              # one snapshot, then exit
strain -j              # snapshot as JSON
strain --check         # one-line result + exit code, for monitoring
strain --cgroup system.slice/postgresql.service
```

Piping the output (`strain | tee …`) automatically prints a snapshot instead
of the live view. In a short terminal the live view sheds its history rows,
then its explanations, then its spacing, so the whole panel still fits.

## Build

```
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o strain .
sudo install -m 0755 strain /usr/local/bin/
```

## How it decides

All three verdicts come from the kernel's **pressure stall information** (PSI,
kernel 4.20+): `/proc/pressure/memory`, `/proc/pressure/cpu` and
`/proc/pressure/io`. Each reports
the share of wall time tasks spent waiting on that resource, averaged over
10 s, 60 s and 300 s. The headline is `some avg10`: the percentage of the last
10 seconds in which at least one task was stalled.

The trend arrow compares the 10 s average against the 60 s average. Stall
meters use a square-root scale, so the low bands that matter most get real
width instead of a sliver at the left edge.

## Memory

### Verdict

| Field | Meaning |
|---|---|
| `some` | % of time at least one task was stalled on memory: reclaim, swap-in, or re-reading evicted pages |
| `full` | % of time *every* runnable task was stalled at once, so no useful work happened |

Bands (`--mem-bands A,B,C`; `--bands` is an alias):

| State | `some avg10` | or `full avg10` |
|---|---|---|
| OK | < 1 % | |
| STRAINED | 1 – 10 % | |
| PRESSURED | 10 – 25 % | ≥ 2 % |
| THRASHING | ≥ 25 % | ≥ 10 % |

### Capacity

| Row | Derivation | Why |
|---|---|---|
| naive used | `(MemTotal − MemFree) / MemTotal` | The misleading figure, shown for contrast |
| real used | `(MemTotal − MemAvailable) / MemTotal` | `MemAvailable` is the kernel's own estimate of what it can hand out without swapping |
| demand | `(MemTotal − MemAvailable + swap used) / MemTotal` | Can exceed 100 %. Swap *occupancy* overstates demand, because Linux parks cold pages there proactively |
| droppable cache | `MemAvailable − MemFree` | What Linux "uses" but will give back on request |

High real usage with 0 % stall is fine. It means RAM is well used, not short.

`MemAvailable` is an estimate. It treats all page cache as droppable, even
cache a workload depends on, and it counts ZFS ARC as used. Trust the stall
figure when the two disagree.

### Paging activity (per-second deltas from `/proc/vmstat`)

| Row | Counter | Meaning |
|---|---|---|
| swap-in | `pswpin` | Swapped data is part of the live working set |
| refaults | `workingset_refault_anon + _file` | Pages evicted, then needed again soon after: the cleanest sign the working set exceeds RAM |
| direct reclaim | `pgscan_direct` | Allocations forced to reclaim memory themselves because kswapd couldn't keep up |
| swap-out | `pswpout` | Idle pages being parked; usually harmless on its own |

**Without PSI**, the memory verdict is estimated from swap-in + refault
throughput (1 / 16 / 64 MiB/s) and direct reclaim, and is labelled
*estimated*.

## CPU

### Verdict

`some` is the % of time at least one runnable task was queued for a core
instead of running. System-wide `full` is always 0, since something is always
running when others wait. It is only meaningful for a cgroup, where it means
the whole group was starved.

Brief queuing is normal scheduling, so the CPU bands sit higher than the
memory ones (`--cpu-bands A,B,C`):

| State | `some avg10` |
|---|---|
| OK | < 5 % |
| BUSY | 5 – 20 % |
| CONTENDED | 20 – 50 % |
| SATURATED | ≥ 50 % |

### CPU activity

| Row | Source | Meaning |
|---|---|---|
| busy | `/proc/stat` jiffies, excluding idle, iowait and steal | Utilization, shown for contrast. 100 % busy with no waiting is healthy |
| run queue | `/proc/schedstat` run_delay, per-second delta | Average number of tasks waiting for a core. Stall % says *how often* tasks waited; this says *how many* |
| steal | `/proc/stat` steal | On a VM: time the hypervisor gave your vCPUs to other guests. PSI can't see it, so high steal with low stall means the *host* is oversubscribed |
| throttled | cgroup `cpu.stat` throttled_usec | cgroup mode only: time held back by the `cpu.max` quota while cores sat free |

If `/proc/schedstat` is missing, the run queue falls back to the
instantaneous `procs_running` count minus online cores, labelled *now*
instead of *avg*. Without CPU PSI, the CPU verdict is estimated from run-queue
depth per core (0.05 / 0.25 / 1).

## I/O

### Verdict

`some` is the % of time at least one task was stalled waiting on storage.
Unlike CPU, `full` is meaningful system-wide: the % of time *every* non-idle
task was stalled on I/O at once.

Bands (`--io-bands A,B,C`):

| State | `some avg10` | or `full avg10` |
|---|---|---|
| OK | < 5 % | |
| BUSY | 5 – 20 % | |
| CONGESTED | 20 – 50 % | ≥ 5 % |
| SATURATED | ≥ 50 % | ≥ 20 % |

### Disk activity (per-second deltas from `/proc/diskstats`)

Four fixed rows, so the table keeps the same height whether disks are busy or
idle. Rows go to the devices with the most traffic since boot, an order that
barely changes while strain runs. Idle devices show dim dots instead of
disappearing. If a device outside the four becomes active, it takes over an
idle row so it is never hidden, and a "+N more devices" line notes the rest.
Partitions, loop, ram, zram, floppy and optical devices are skipped.
Device-mapper volumes show their dm name (`pve-root` rather than `dm-0`).

| Column | Derivation | Meaning |
|---|---|---|
| iops | completed reads + writes | Requests per second |
| read, write | sectors × 512 | Throughput |
| await | (read ms + write ms) / requests | Average latency per request, queueing included. The number users feel |
| queue | weighted ms / elapsed ms | Average requests in flight (iostat's `aqu-sz`) |
| util | ms with any I/O in flight / elapsed ms | Shown dimmed, for contrast. It was meaningful for a single spinning disk; NVMe, RAID and virtual disks serve many requests in parallel, so 100 % says nothing about remaining capacity |

Await turns amber at 10 ms, orange at 50 ms and red at 200 ms. Those are loose
on purpose, to flag real trouble on anything from NVMe to spinning disks.

**Without PSI**, the I/O verdict is estimated from the worst device await
(20 / 100 / 500 ms).

## cgroup mode (`--cgroup PATH`, v2 only)

PATH is absolute or relative to `/sys/fs/cgroup`.

- **Memory** comes from `memory.pressure`, `memory.current`, `memory.max`,
  `memory.stat` and `memory.swap.*`. The working set is
  `memory.current − inactive_file`, the same convention the kubelet uses. A
  group with no limit is sized against host RAM.
- **CPU** comes from `cpu.pressure` and `cpu.stat`. Busy % is measured against
  the `cpu.max` quota when one is set (shown as e.g. "quota 1.5 cores"),
  otherwise against all host cores. Run-queue depth has no per-cgroup source,
  so it shows n/a; steal is the host's.
- **I/O** verdict comes from `io.pressure`. The disk table stays host-wide
  (labelled "host devices"), since per-device latency has no cgroup source.

## Live keys

`q` quit · `p` / space pause · `+` / `-` slower / faster refresh · `r` repaint

## Machine-readable output

`-j` prints one JSON object with `cpu`, `memory` and `io` sections, each
carrying its own `state`, `stall` figures and supporting metrics (`io` lists
every device that was active during the window, plus an `idle_devices` count). The top-level `level` is `ok`, `warning` or `critical`:
the worst of the three.

`--check` prints one line with perfdata after the `|`:

```
CRITICAL - cpu BUSY (stall 6.7%, busy 28%), memory OK (stall 0.0%, real used 6%), io SATURATED (stall 49.7%) | cpu_stall=6.70%;5;20 …
```

## Exit codes

| Mode | Code |
|---|---|
| `--check` | 0 OK · 1 WARNING (STRAINED / BUSY) · 2 CRITICAL (anything worse) · 3 error, judged on the worst of the three |
| Everything else | 0 success · 1 runtime error · 2 bad flags |

## Trying it under real pressure

```
# memory: a 256 MiB box for a hungry process
systemd-run --user --scope -p MemoryMax=256M -p MemorySwapMax=1G \
    stress-ng --vm 1 --vm-bytes 600M --vm-keep

# cpu: twice as many busy workers as cores
stress-ng --cpu $(( $(nproc) * 2 )) --timeout 60s

# io: parallel synchronous direct writes
stress-ng --hdd 4 --hdd-opts direct,dsync --timeout 60s

# then, in another terminal
strain
```
