# Scale benchmark

Measures the cycle of the daemon at 100 to 2000 published routes, to check the
limits the design states. `make scale` runs it and prints the tables; it needs
the `scale` build tag, so `make test` does not run it. It takes about five
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
- the connectors and the egress filter;
- Proxmox: its API time is not in the numbers.

One tunnel, one zone, one credential. Cycles run on a clock the benchmark
moves, so a grace period passes without waiting for it.

The cycles of a size: the first enforcing cycle, an idle one, one where 10 % of
the routes are renamed (new records, and the old ones removed after the grace),
and one where 1 % of the guests vanish (taken from the guests that hold a
route). Wall time, calls to Cloudflare by kind, allocations and the peak of the
heap come with each, and the number of files the store wrote, counted by their
inodes. The peak heap is above the heap before the cycle and also holds the
garbage of the fake Cloudflare, which runs in the same process.

## Results

Apple M5 Pro, 18 cores, 48 GB, macOS 26.5.1, Go 1.27.1. The SSD column is the
default disk of this machine; writing a file as the store does (write, fsync,
rename) takes 6.9 ms on it, as macOS has the disk flush on fsync. The RAM disk
column keeps the store on a RAM disk, which is the cost of the engine. A Linux
host will lie in between, by what its disk takes for an fsync.

| guests/routes | cycle | wall, SSD | wall, RAM disk | CF calls | dns r/w | tunnel r/w | least at 300/5m | alloc MB | allocs M | peak heap MB | files written |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 200/100 | first enforcing cycle | 810ms | 80ms | 211 | 101/100 | 6/2 | 3m11s | 11 | 0.3 | 2 | 203 |
| 200/100 | idle | 410ms | 30ms | 3 | 1/0 | 2/0 | 0s | 8 | 0.3 | 3 | 100 |
| 200/100 | idle, ARP 20ms (model 670ms) | 680ms | 290ms | 3 | 1/0 | 2/0 | 0s | 8 | 0.3 | 2 | 100 |
| 200/100 | idle, ARP 600ms (model 8.21s) | 8.23s | 7.83s | 4 | 1/0 | 3/0 | 0s | 8 | 0.3 | 2 | 100 |
| 200/100 | 10% of routes renamed | 510ms | 40ms | 25 | 11/10 | 3/1 | 5s | 9 | 0.3 | 2 | 120 |
| 200/100 | ... old names removed (2 cycles later) | 430ms | 30ms | 25 | 12/10 | 3/0 | 5s | 9 | 0.3 | 2 | 101 |
| 200/100 | 1% of guests vanish (2) | 420ms | 30ms | 5 | 1/0 | 3/1 | 0s | 8 | 0.3 | 3 | 101 |
| 200/100 | ... their records removed (2 cycles later) | 420ms | 30ms | 8 | 3/2 | 3/0 | 0s | 8 | 0.3 | 4 | 99 |
| 1000/500 | first enforcing cycle | 4.11s | 270ms | 1011 | 501/500 | 6/2 | 16m31s | 132 | 6.3 | 8 | 1003 |
| 1000/500 | idle | 2.11s | 160ms | 7 | 5/0 | 2/0 | 0s | 116 | 6.2 | 13 | 500 |
| 1000/500 | idle, ARP 20ms (model 3.37s) | 3.37s | 1.42s | 7 | 5/0 | 2/0 | 0s | 116 | 6.2 | 9 | 500 |
| 1000/500 | idle, ARP 600ms (model 39.91s) | 40.04s | 37.96s | 8 | 5/0 | 3/0 | 0s | 116 | 6.2 | 9 | 500 |
| 1000/500 | 10% of routes renamed | 2.55s | 160ms | 109 | 55/50 | 3/1 | 1m29s | 122 | 6.2 | 13 | 600 |
| 1000/500 | ... old names removed (2 cycles later) | 2.13s | 140ms | 109 | 56/50 | 3/0 | 1m29s | 120 | 6.2 | 14 | 501 |
| 1000/500 | 1% of guests vanish (10) | 2.16s | 130ms | 9 | 5/0 | 3/1 | 0s | 116 | 6.0 | 16 | 501 |
| 1000/500 | ... their records removed (2 cycles later) | 2.09s | 140ms | 28 | 15/10 | 3/0 | 8s | 115 | 6.0 | 14 | 491 |
| 2000/1000 | first enforcing cycle | 8.55s | 540ms | 2011 | 1001/1000 | 6/2 | 33m11s | 455 | 24.7 | 20 | 2003 |
| 2000/1000 | idle | 4.33s | 320ms | 12 | 10/0 | 2/0 | 0s | 427 | 24.3 | 24 | 1000 |
| 2000/1000 | 10% of routes renamed | 6.01s | 370ms | 214 | 110/100 | 3/1 | 3m14s | 438 | 24.4 | 36 | 1200 |
| 2000/1000 | ... old names removed (2 cycles later) | 4.35s | 360ms | 214 | 111/100 | 3/0 | 3m14s | 434 | 24.4 | 24 | 1001 |
| 2000/1000 | 1% of guests vanish (20) | 4.21s | 330ms | 14 | 10/0 | 3/1 | 0s | 421 | 23.7 | 24 | 1001 |
| 2000/1000 | ... their records removed (2 cycles later) | 4.19s | 330ms | 53 | 30/20 | 3/0 | 33s | 419 | 23.7 | 24 | 981 |
| 5000/2000 | first enforcing cycle | 17.62s | 1.36s | 4011 | 2001/2000 | 6/2 | 1h6m31s | 2063 | 121.3 | 44 | 4003 |
| 5000/2000 | idle | 15.11s | 910ms | 22 | 20/0 | 2/0 | 2s | 2019 | 120.7 | 47 | 2000 |
| 5000/2000 | 10% of routes renamed | 23.33s | 970ms | 424 | 220/200 | 3/1 | 6m44s | 2041 | 120.9 | 46 | 2400 |
| 5000/2000 | ... old names removed (2 cycles later) | 12.38s | 950ms | 425 | 222/200 | 3/0 | 6m45s | 2034 | 120.9 | 45 | 2001 |
| 5000/2000 | 1% of guests vanish (50) | 8.92s | 880ms | 24 | 20/0 | 3/1 | 4s | 1964 | 116.6 | 50 | 2001 |
| 5000/2000 | ... their records removed (2 cycles later) | 14.18s | 1.01s | 123 | 70/50 | 3/0 | 1m43s | 1958 | 116.6 | 50 | 1951 |

Wall times on the SSD move by up to 40 % between runs; the calls, files and
allocations do not.
The same cycles on two CPUs (`GOMAXPROCS=2`, RAM disk) take 0.54 s idle at
2000/1000 and 2.0 s at 5000/2000.

"Least at 300/5m" is what the calls alone take on the default budget of a
credential, 300 requests per 5 minutes with room for 20 at once: one second for
each call after the first 20. It is a floor, as a cycle that starts with fewer
than 20 tokens waits longer.

The ARP rows add the latency of an ARP exchange to every resolve. The real
prober waits out an ARP window of 600 ms for every route, eight routes at a
time, so a cycle takes routes/8 times 600 ms more than the idle row. The model
column is that sum; it is within 2 % of every measured point, so the larger
sizes are not run with the real window: at 1000 routes it is 75 s more, at 2000
150 s more.

The default budget is also run for real. `TestDefaultLimiter` starts 1000 routes
on the real limiter at 300 requests per 5 minutes with the time of its waits
shortened sixty times (it sleeps in real time), and counts the time of the
waits as they would be:

| routes | cycles until every record exists | CF calls | took on the default budget | least |
|---|---|---|---|---|
| 1000 | 1 | 2011 | 33m39s | 33m11s |

## Against the limits of the design

Cycle time, 2000 guests and 1000 routes, against fakes: within 30 s, at 0.3 to
0.5 s on a RAM disk and 4 to 9 s on this SSD. At 5000 guests and 2000 routes
the idle cycle takes 15 s and the busiest 23 s on the SSD. What the cycle waits
for is the disk: it writes a file for every route in every cycle, as the
verification time of each binding moves. With 2000 routes, a disk that takes
15 ms for a file write makes an idle cycle 30 s.

What is not within it:

- The resolver on a real host. The ARP window makes an idle cycle at least
  routes/8 times 600 ms: 38 s at 500 routes, 75 s at 1000, 150 s at 2000. A
  cycle over 30 s is reached at about 400 routes.
- The default budget of Cloudflare. A record costs two requests, the lookup of
  its name and the create, and a zone is listed 100 records to a request. The
  first cycle at 1000 routes is 2011 requests, 33 minutes in one cycle, which
  holds the cycle lock: nothing else is reconciled and removals wait. At 2000
  routes it is 66 minutes. A rename of 10 % of 1000 routes takes at least 3
minutes. An idle
  cycle is 12 requests at 1000 routes where the budget refills 10 in a poll
  interval, and 22 at 2000 routes.
- Allocations grow with guests times routes, about 12 for each pair: 427 MB and
  24 million allocations per idle cycle at 2000/1000, 2 GB and 121 million at
  5000/2000. Of them 96 % are in `resolve.sharedWith`, which compares the MAC of a
  route's guest with the NICs of every guest and parses both MACs for each
  comparison.

The peak heap stays under 70 MB above the baseline in every cycle.

## Not measured

Memory per connector at 1, 5 and 25 tunnels (the connectors are fakes), the
detection of DNS zone quotas, the refresh of the Proxmox inventory, and more
than one tunnel, zone or credential.
