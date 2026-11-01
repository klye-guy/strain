# strain

How hard is Linux actually working to find memory?

Linux fills spare RAM with page cache, so `free` and `top` report a high
"used" figure on any healthy machine. `strain` ignores that baseline and answers
the real question: **are tasks waiting on memory, and is the working set bigger
than RAM?**

Single static binary, Go standard library only, no root required.

```
strain                 # live view, refreshing like top
strain -s              # one snapshot, then exit
strain -j              # snapshot as JSON
strain --check         # one-line result + exit code, for monitoring
strain --cgroup system.slice/postgresql.service
```

Piping the output (`strain | tee …`) automatically prints a snapshot instead
of the live view.

## Build

```
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o strain .
sudo install -m 0755 strain /usr/local/bin/
```

## What it shows

### Verdict: memory stall % (the headline)

Read from `/proc/pressure/memory` (PSI, kernel 4.20+).

| Field | Meaning |
|---|---|
| `some` | % of wall time at least one task was stalled waiting on memory: reclaim, swap-in, or re-reading evicted pages |
| `full` | % of time *every* runnable task was stalled at once, so no useful work happened |

The headline is `some avg10`. The verdict uses these bands (`--bands A,B,C`
changes them):

| State | `some avg10` | or `full avg10` |
|---|---|---|
| OK | < 1 % | |
| STRAINED | 1 – 10 % | |
| PRESSURED | 10 – 25 % | ≥ 2 % |
| THRASHING | ≥ 25 % | ≥ 10 % |

The trend arrow compares the 10 s average against the 60 s average.

The stall meter uses a square-root scale over 0–50 %, so the 1 % and 10 %
bands get real width instead of a sliver at the left edge.

### Capacity

| Row | Derivation | Why |
|---|---|---|
| naive used | `(MemTotal − MemFree) / MemTotal` | The misleading figure, shown for contrast |
| real used | `(MemTotal − MemAvailable) / MemTotal` | `MemAvailable` is the kernel's own estimate of what it can hand out without swapping |
| demand | `(MemTotal − MemAvailable + swap used) / MemTotal` | Can exceed 100 %. Swap *occupancy* overstates demand, because Linux parks cold pages there proactively |
| droppable cache | `MemAvailable − MemFree` | What Linux "uses" but will give back on request |

High real usage with 0 % stall is fine. It means RAM is well used, not short.

### Paging activity (per-second deltas from `/proc/vmstat`)

| Row | Counter | Meaning |
|---|---|---|
| swap-in | `pswpin` | Swapped data is part of the live working set |
| refaults | `workingset_refault_anon + _file` | Pages evicted, then needed again soon after: the cleanest sign the working set exceeds RAM |
| direct reclaim | `pgscan_direct` | Allocations forced to reclaim memory themselves because kswapd couldn't keep up |
| swap-out | `pswpout` | Idle pages being parked; usually harmless on its own |

These explain *why* the stall figure is elevated.

**Without PSI** (kernel built without it, or RHEL-family kernels booted
without `psi=1`), the verdict is estimated from swap-in + refault throughput
(1 / 16 / 64 MiB/s) and direct reclaim, and is labelled *estimated*.

### cgroup mode (`--cgroup PATH`, v2 only)

PATH is absolute or relative to `/sys/fs/cgroup`. The figures come from
`memory.pressure`, `memory.current`, `memory.max`, `memory.stat` and
`memory.swap.*`. The working set is `memory.current − inactive_file`, the same
convention the kubelet uses. A group with no limit is sized against host RAM.

## Live keys

`q` quit · `p` / space pause · `+` / `-` slower / faster refresh · `r` repaint

## Exit codes

| Mode | Code |
|---|---|
| `--check` | 0 OK · 1 STRAINED · 2 PRESSURED or THRASHING · 3 error |
| `--check` output | Includes monitoring-plugin perfdata after the `\|` |
| Everything else | 0 success · 1 runtime error · 2 bad flags |

## Trying it under real pressure

```
# a 256 MiB box for a hungry process, watched from another terminal
systemd-run --user --scope -p MemoryMax=256M -p MemorySwapMax=1G \
    stress-ng --vm 1 --vm-bytes 600M --vm-keep
strain
```
