# Scale benchmark

Measures the cycle of the daemon at 100 to 2000 published routes, to check the
limits the design states. `make scale` runs it and prints the tables; it needs
the `scale` build tag, so `make test` does not run it. It takes about four
minutes and has a limit of ten.

`SCALE_DIR` puts the store on another disk, such as a RAM disk or `/dev/shm`,
which takes the disk out of the numbers.

## What runs

The real engine, store, planner, reconcilers, resolver and Cloudflare client.
The Cloudflare client talks HTTP to `cffake` behind `httptest`, with the real
limiter. What is faked:

- the inventory: N guests with a NIC each, M of them tagged with one route in
  their notes, the rest plain guests;
- the host prober, which answers every ARP, forwarding-table and dial question
  at once, so the resolver, which is the real one, runs all its checks;
- the watch of the network, which the engine is told runs, as it does on a
  node: a proof it vouches for stands until its address comes due, once a
  minute at a time of its own;
- the connectors and the egress filter;
- Proxmox: its API time is not in the numbers.

One tunnel, one zone, one credential. Cycles run on a clock the benchmark
moves, so a grace period passes without waiting for it.

The cycles of a size: the first enforcing cycle; the second, which proves
every address again, as the watch had no pin of it when the first proved it;
an idle one, which proves the addresses that came due in it, a sixth of them
in a cycle of 10 seconds; one where 10 % of the routes are renamed (new records, and the
old ones removed after the grace), and one where 1 % of the guests vanish
(taken from the guests that hold a route). Wall time, calls to Cloudflare by
kind, allocations and the peak of the heap come with each, and the number of
files the store wrote, counted by their inodes. The peak heap is above the heap
before the cycle and also holds the garbage of the fake Cloudflare, which runs
in the same process.

## Results

Apple M5 Pro, 18 cores, 48 GB, macOS 26.5.1, Go 1.27.1, the store on the
default disk of this machine, where writing a file as the store does (write,
fsync, rename) takes 4 to 7 ms, as macOS has the disk flush on fsync.

| guests/routes | cycle | wall | CF calls | dns r/w | tunnel r/w | least at 1000/5m | alloc MB | allocs k | peak heap MB | files written |
|---|---|---|---|---|---|---|---|---|---|---|
| 200/100 | first enforcing cycle | 820ms | 111 | 1/100 | 6/2 | 0s | 7 | 59 | 2 | 203 |
| 200/100 | second cycle | 10ms | 3 | 1/0 | 2/0 | 0s | 4 | 37 | 1 | 0 |
| 200/100 | idle | 10ms | 3 | 1/0 | 2/0 | 0s | 4 | 37 | 1 | 0 |
| 200/100 | 10% of routes renamed | 140ms | 15 | 1/10 | 3/1 | 0s | 6 | 46 | 1 | 30 |
| 200/100 | ... old names removed (2 cycles later) | 70ms | 27 | 11/10 | 4/0 | 0s | 5 | 49 | 1 | 11 |
| 200/100 | 1% of guests vanish (2) | 20ms | 5 | 1/0 | 3/1 | 0s | 5 | 41 | 1 | 3 |
| 200/100 | ... their records removed (2 cycles later) | 60ms | 8 | 3/2 | 3/0 | 0s | 4 | 44 | 1 | 11 |
| 1000/500 | first enforcing cycle | 6s | 511 | 1/500 | 6/2 | 0s | 31 | 275 | 7 | 1003 |
| 1000/500 | second cycle | 70ms | 3 | 1/0 | 2/0 | 0s | 21 | 182 | 6 | 0 |
| 1000/500 | idle | 50ms | 3 | 1/0 | 2/0 | 0s | 21 | 180 | 6 | 0 |
| 1000/500 | 10% of routes renamed | 950ms | 55 | 1/50 | 3/1 | 0s | 26 | 224 | 7 | 150 |
| 1000/500 | ... old names removed (2 cycles later) | 280ms | 107 | 51/50 | 4/0 | 0s | 25 | 242 | 8 | 51 |
| 1000/500 | 1% of guests vanish (10) | 100ms | 5 | 1/0 | 3/1 | 0s | 24 | 202 | 8 | 11 |
| 1000/500 | ... their records removed (2 cycles later) | 430ms | 24 | 11/10 | 3/0 | 0s | 23 | 221 | 8 | 51 |
| 2000/1000 | first enforcing cycle | 11.75s | 1011 | 1/1000 | 6/2 | 5m0s | 62 | 549 | 16 | 2003 |
| 2000/1000 | second cycle | 130ms | 3 | 1/0 | 2/0 | 0s | 43 | 364 | 18 | 0 |
| 2000/1000 | idle | 120ms | 3 | 1/0 | 2/0 | 0s | 43 | 361 | 17 | 0 |
| 2000/1000 | 10% of routes renamed | 1.36s | 105 | 1/100 | 3/1 | 0s | 52 | 447 | 17 | 300 |
| 2000/1000 | ... old names removed (2 cycles later) | 550ms | 207 | 101/100 | 4/0 | 0s | 50 | 486 | 19 | 101 |
| 2000/1000 | 1% of guests vanish (20) | 180ms | 5 | 1/0 | 3/1 | 0s | 48 | 403 | 18 | 21 |
| 2000/1000 | ... their records removed (2 cycles later) | 520ms | 44 | 21/20 | 3/0 | 0s | 46 | 443 | 17 | 101 |
| 5000/2000 | first enforcing cycle | 16.31s | 2011 | 1/2000 | 6/2 | 10m0s | 125 | 1106 | 29 | 4003 |
| 5000/2000 | second cycle | 460ms | 3 | 1/0 | 2/0 | 0s | 88 | 739 | 37 | 0 |
| 5000/2000 | idle | 200ms | 3 | 1/0 | 2/0 | 0s | 87 | 732 | 38 | 0 |
| 5000/2000 | 10% of routes renamed | 2.62s | 206 | 1/200 | 4/1 | 0s | 107 | 906 | 35 | 600 |
| 5000/2000 | ... old names removed (2 cycles later) | 1.11s | 404 | 201/200 | 3/0 | 0s | 102 | 985 | 39 | 201 |
| 5000/2000 | 1% of guests vanish (50) | 420ms | 5 | 1/0 | 3/1 | 0s | 97 | 816 | 40 | 51 |
| 5000/2000 | ... their records removed (2 cycles later) | 1.04s | 104 | 51/50 | 3/0 | 0s | 94 | 900 | 41 | 201 |

Wall times on this disk move by up to 40 % between runs; the calls, files and
allocations do not. A cycle writes the file of a binding when the binding
changes, and for the time of its proof alone only once that moved on by more
than 75 seconds, a quarter of the age a proof may reach.

"Least at 1000/5m" is what the calls alone take on the default budget of a
credential, 1000 requests in 5 minutes, all at once if need be: the requests
over the budget wait for the next 5 minutes. It is a floor, as a cycle that
starts after others spent part of the budget waits longer.

### Checks on the wire

The prober of a node waits out an ARP window of 600 ms for every address it
checks, 32 addresses at a time. Up to 1000 routes these cycles are run with that
latency, and with 20 ms, on every ARP request:

- busiest of a minute: the slowest of the six cycles of a minute while the
  watch runs, each of which proves the addresses that came due in it;
- no watch: the watch is not running, so the cycle proves every address.

The model is the addresses proven, 32 at a time, times the latency.

| guests/routes | cycle | wall | addresses proven | model | files written |
|---|---|---|---|---|---|
| 200/100 | busiest of a minute, ARP 20ms | 90ms | 14 | 20ms | 14 |
| 200/100 | no watch, ARP 20ms | 440ms | 100 | 80ms | 86 |
| 200/100 | busiest of a minute, ARP 600ms | 620ms | 14 | 600ms | 0 |
| 200/100 | no watch, ARP 600ms | 2.82s | 100 | 2.4s | 100 |
| 1000/500 | busiest of a minute, ARP 20ms | 670ms | 81 | 60ms | 81 |
| 1000/500 | no watch, ARP 20ms | 3.2s | 500 | 320ms | 419 |
| 1000/500 | busiest of a minute, ARP 600ms | 1.87s | 84 | 1.8s | 0 |
| 1000/500 | no watch, ARP 600ms | 13.14s | 500 | 9.6s | 500 |
| 2000/1000 | busiest of a minute, ARP 20ms | 1.28s | 166 | 120ms | 166 |
| 2000/1000 | no watch, ARP 20ms | 6.12s | 1000 | 640ms | 834 |
| 2000/1000 | busiest of a minute, ARP 600ms | 3.73s | 165 | 3.6s | 0 |
| 2000/1000 | no watch, ARP 600ms | 23.52s | 1000 | 19.2s | 1000 |

What a cycle takes beyond the model is the files it writes: a binding whose
proof moved on by more than 75 seconds since its file was written. Before the
proofs came due at times of their own, every proof of a size came due in the
same cycle once a minute: at 1000 routes that cycle took 24.8 s with the ARP
window of a node, now the busiest cycle of a minute takes 3.7 s.

### The default budget

`TestDefaultLimiter` starts 1000 routes on the default budget of a credential,
with a fake Cloudflare that counts 1200 requests in 5 minutes and says in every
answer what is left, as Cloudflare does. The limiter waits on the clock of the
benchmark, and the time the cycles worked is added to it.

| routes | cycles until every record exists | CF calls | the first cycle says | took on the default budget | least |
|---|---|---|---|---|---|
| 1000 | 29 | 1014 | 10 changes wait for Cloudflare's rate limit | 5m19s | 5m0s |

The first cycle creates 990 records and stops when the next request would wait
for longer than 20 seconds. The cycles that follow are refused at their first
request, and do nothing, until Cloudflare starts its count again; their state
says so in one line, `the tunnel of account acc1 and the listing of zone
example.com wait for Cloudflare's rate limit`. The one after that creates the
rest.

## Against the limits of the design

At 2000 guests and 1000 routes, on this disk:

| | cb54b24 | now |
|---|---|---|
| idle cycle | 4.39s, 12 calls, 1000 files, 427 MB in 24.3 million allocations | 120ms, 3 calls, no file, 43 MB in 361 thousand |
| idle cycle with the ARP window of a node | 75s, modelled | 3.7s at most, a sixth of the addresses; 23.5s without the watch |
| first start | 2011 calls, one cycle of 33 minutes holding the cycle lock | 1014 calls, 29 cycles of a few seconds each over 5m19s |
| rename of 10 % | 214 calls | 105 calls |

What is within the design: every cycle at 1000 routes takes less than 30 s
on this disk, and the cycle lock is never held for longer than one cycle works.

What is close to it: without the watch, as on a node where it cannot run,
every cycle proves every address, 19 s for the ARP window alone at 1000 routes,
and writes the files of the bindings that moved on by more than 75 seconds.

At 5000 guests and 2000 routes an idle cycle takes 200 ms without the ARP
window; with it, for a sixth of the addresses, about 7 s, modelled.

## Not measured

Memory per connector at 1, 5 and 25 tunnels (the connectors are fakes), the
detection of DNS zone quotas, the refresh of the Proxmox inventory, more than
one tunnel, zone or credential, and the watch of the network itself.
