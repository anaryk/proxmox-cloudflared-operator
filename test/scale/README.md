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
  node: a proof it vouches for stands until it is a minute old;
- the connectors and the egress filter;
- Proxmox: its API time is not in the numbers.

One tunnel, one zone, one credential. Cycles run on a clock the benchmark
moves, so a grace period passes without waiting for it.

The cycles of a size: the first enforcing cycle; the second, which proves
every address again, as the watch had no pin of it when the first proved it;
an idle one; one where 10 % of the routes are renamed (new records, and the
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
| 200/100 | first enforcing cycle | 1.34s | 111 | 1/100 | 6/2 | 0s | 6 | 57 | 1 | 203 |
| 200/100 | second cycle | 10ms | 3 | 1/0 | 2/0 | 0s | 4 | 37 | 1 | 0 |
| 200/100 | idle | 10ms | 3 | 1/0 | 2/0 | 0s | 4 | 36 | 1 | 0 |
| 200/100 | 10% of routes renamed | 220ms | 16 | 1/10 | 4/1 | 0s | 5 | 45 | 1 | 30 |
| 200/100 | ... old names removed (2 cycles later) | 100ms | 26 | 11/10 | 3/0 | 0s | 5 | 49 | 1 | 11 |
| 200/100 | 1% of guests vanish (2) | 40ms | 5 | 1/0 | 3/1 | 0s | 5 | 40 | 1 | 3 |
| 200/100 | ... their records removed (2 cycles later) | 100ms | 8 | 3/2 | 3/0 | 0s | 4 | 44 | 1 | 11 |
| 1000/500 | first enforcing cycle | 7.16s | 511 | 1/500 | 6/2 | 0s | 30 | 273 | 7 | 1003 |
| 1000/500 | second cycle | 110ms | 3 | 1/0 | 2/0 | 0s | 21 | 181 | 7 | 0 |
| 1000/500 | idle | 40ms | 3 | 1/0 | 2/0 | 0s | 21 | 177 | 6 | 0 |
| 1000/500 | 10% of routes renamed | 1.16s | 56 | 1/50 | 4/1 | 0s | 26 | 221 | 7 | 150 |
| 1000/500 | ... old names removed (2 cycles later) | 470ms | 106 | 51/50 | 3/0 | 0s | 24 | 241 | 8 | 51 |
| 1000/500 | 1% of guests vanish (10) | 150ms | 5 | 1/0 | 3/1 | 0s | 23 | 198 | 7 | 11 |
| 1000/500 | ... their records removed (2 cycles later) | 450ms | 24 | 11/10 | 3/0 | 0s | 22 | 220 | 8 | 51 |
| 2000/1000 | first enforcing cycle | 10.81s | 1011 | 1/1000 | 6/2 | 5m0s | 61 | 545 | 14 | 2003 |
| 2000/1000 | second cycle | 180ms | 3 | 1/0 | 2/0 | 0s | 42 | 363 | 18 | 0 |
| 2000/1000 | idle | 130ms | 3 | 1/0 | 2/0 | 0s | 42 | 354 | 17 | 0 |
| 2000/1000 | 10% of routes renamed | 1.85s | 106 | 1/100 | 4/1 | 0s | 52 | 441 | 18 | 300 |
| 2000/1000 | ... old names removed (2 cycles later) | 890ms | 206 | 101/100 | 3/0 | 0s | 49 | 483 | 19 | 101 |
| 2000/1000 | 1% of guests vanish (20) | 290ms | 5 | 1/0 | 3/1 | 0s | 47 | 396 | 18 | 21 |
| 2000/1000 | ... their records removed (2 cycles later) | 850ms | 44 | 21/20 | 3/0 | 0s | 46 | 441 | 20 | 101 |
| 5000/2000 | first enforcing cycle | 27.67s | 2011 | 1/2000 | 6/2 | 10m0s | 123 | 1100 | 31 | 4003 |
| 5000/2000 | second cycle | 510ms | 3 | 1/0 | 2/0 | 0s | 87 | 737 | 37 | 0 |
| 5000/2000 | idle | 230ms | 3 | 1/0 | 2/0 | 0s | 86 | 719 | 38 | 0 |
| 5000/2000 | 10% of routes renamed | 4.41s | 206 | 1/200 | 4/1 | 0s | 105 | 893 | 39 | 600 |
| 5000/2000 | ... old names removed (2 cycles later) | 1.87s | 404 | 201/200 | 3/0 | 0s | 100 | 980 | 39 | 201 |
| 5000/2000 | 1% of guests vanish (50) | 630ms | 5 | 1/0 | 3/1 | 0s | 96 | 802 | 41 | 51 |
| 5000/2000 | ... their records removed (2 cycles later) | 2.21s | 104 | 51/50 | 3/0 | 0s | 93 | 896 | 38 | 201 |

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

- idle: the watch vouches for every proof, which stands;
- re-check: a minute after the last cycle, every proof is a minute old and is
  made again; the time of every proof moved on by more than 75 seconds since
  its file was written, so every binding is written as well;
- no watch: the watch is not running, so the cycle proves every address.

The model is the idle cycle and routes/32 times the latency.

| guests/routes | cycle | wall | model | files written |
|---|---|---|---|---|
| 200/100 | idle, ARP 20ms | 10ms | | 0 |
| 200/100 | re-check, ARP 20ms | 810ms | 90ms | 100 |
| 200/100 | no watch, ARP 20ms | 100ms | 90ms | 0 |
| 200/100 | idle, ARP 600ms | 10ms | | 0 |
| 200/100 | re-check, ARP 600ms | 3.13s | 2.41s | 100 |
| 200/100 | no watch, ARP 600ms | 2.42s | 2.41s | 0 |
| 1000/500 | idle, ARP 20ms | 50ms | | 0 |
| 1000/500 | re-check, ARP 20ms | 3.9s | 360ms | 500 |
| 1000/500 | no watch, ARP 20ms | 390ms | 360ms | 0 |
| 1000/500 | idle, ARP 600ms | 50ms | | 0 |
| 1000/500 | re-check, ARP 600ms | 13.2s | 9.64s | 500 |
| 1000/500 | no watch, ARP 600ms | 9.67s | 9.64s | 0 |
| 2000/1000 | idle, ARP 20ms | 110ms | | 0 |
| 2000/1000 | re-check, ARP 20ms | 8.06s | 770ms | 1000 |
| 2000/1000 | no watch, ARP 20ms | 820ms | 770ms | 0 |
| 2000/1000 | idle, ARP 600ms | 110ms | | 0 |
| 2000/1000 | re-check, ARP 600ms | 25.6s | 19.33s | 1000 |
| 2000/1000 | no watch, ARP 600ms | 19.54s | 19.33s | 0 |

What a re-check takes beyond the model is the files it writes. With the
default `reverifyInterval` of a minute every second re-check writes them, the
other one does not.

### The default budget

`TestDefaultLimiter` starts 1000 routes on the default budget of a credential,
with a fake Cloudflare that counts 1200 requests in 5 minutes and says in every
answer what is left, as Cloudflare does. The limiter waits on the clock of the
benchmark, and the time the cycles worked is added to it.

| routes | cycles until every record exists | CF calls | the first cycle says | took on the default budget | least |
|---|---|---|---|---|---|
| 1000 | 29 | 1015 | 11 changes wait for Cloudflare's rate limit | 5m29s | 5m0s |

The first cycle creates 989 records and stops when the next request would wait
for longer than 20 seconds. The cycles that follow are refused at their first
request, and do nothing, until Cloudflare starts its count again; the one after
that creates the rest.

## Against the limits of the design

At 2000 guests and 1000 routes, on this disk:

| | cb54b24 | now |
|---|---|---|
| idle cycle | 4.39s, 12 calls, 1000 files, 427 MB in 24.3 million allocations | 130ms, 3 calls, no file, 42 MB in 354 thousand |
| idle cycle with the ARP window of a node | 75s, modelled | 110ms; 19.5s without the watch; 25.6s when every proof is a minute old |
| first start | 2011 calls, one cycle of 33 minutes holding the cycle lock | 1015 calls, 29 cycles of a few seconds each over 5m29s |
| rename of 10 % | 214 calls | 106 calls |

What is within the design: every cycle at 1000 routes takes less than 30 s
on this disk, and the cycle lock is never held for longer than one cycle works.

What is close to it: the cycle that makes every proof again, once a minute,
proves 1000 addresses, 19 s at 600 ms each, and every second time also writes
1000 files. On a disk that takes 15 ms for a file that one is over 30 s. A
cycle that proves only a share of the addresses each time would spread both.

At 5000 guests and 2000 routes the idle cycle takes 230 ms; the cycle that
makes every proof again would take 38 s and more for the ARP window alone.

## Not measured

Memory per connector at 1, 5 and 25 tunnels (the connectors are fakes), the
detection of DNS zone quotas, the refresh of the Proxmox inventory, more than
one tunnel, zone or credential, and the watch of the network itself.
