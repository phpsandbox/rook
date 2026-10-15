# HTTP relay load testing

`cmd/relay-load` runs a compiled Rook process against a local mock control plane
and HTTP origin. It sends real MessagePack frames over Rook's data WebSocket,
including streamed POST bodies, and validates response status, request ID,
frame ordering, and every response byte before counting a success.

## HTTP admission and cancellation

The implemented proxy owns a shared HTTP client and transport. It keeps up to
64 idle connections per local app and 256 across apps, with a global limit of
64 active upstream HTTP responses. A slot stays occupied until its response
body closes; completed responses release their slots before sending the
terminal relay frame.

Bodyless requests can wait in a bounded queue of 1024. Requests with bodies
must acquire an active slot immediately. Otherwise an upload waiting for a
connection would block the shared relay receive loop before active uploads
could finish. Saturated uploads and bodyless requests beyond the queue limit
receive `stream.reset` with `HTTP relay at capacity` before contacting the
application. Callers can retry this overload response later.

All HTTP streams are tracked, including GETs and queued requests. A stream
reset cancels its request; a data-plane disconnect cancels the previous
connection's HTTP streams. Stream cleanup only removes its own entry, and
completed HTTP entries retire before the terminal frame so an ID can be reused
for a WebSocket upgrade immediately.

Relay writes have a cancelable producer queue. An individual stream's
cancellation skips its queued frames without closing the shared data socket.
A frame already being written completes under a separate five-second socket
write timeout. The HTTP admission limits do not apply to WebSocket upgrades.
See the [fixed-build checks](#fixed-build-checks) for implementation results.

## Run

```sh
go build -o bin/rook ./cmd/rook
go build -o bin/relay-load ./cmd/relay-load
./bin/relay-load -duration 10s -concurrency 1,2,4,16,64,256 \
  -output /tmp/rook-ramp.jsonl
```

The default profiles are:

| Profile | HTTP request | HTTP response |
| --- | --- | --- |
| `get-1k` | GET, no body | 1 KiB |
| `get-64k` | GET, no body | 64 KiB |
| `post-64k` | POST, 64 KiB in two data frames | 64 KiB |

An additional `get-3m` profile returns exactly 3 MiB (3,145,728 bytes):

```sh
./bin/relay-load -profiles get-3m -duration 10s \
  -concurrency 1,2,4,16,64,256,1024 -output /tmp/rook-3m-ramp.jsonl
```

Run a longer check or simulate application processing time:

```sh
./bin/relay-load -duration 60s -concurrency 2 -output /tmp/rook-soak.jsonl
./bin/relay-load -duration 30s -profiles get-1k -concurrency 16,64,256 \
  -origin-delay 10ms -output /tmp/rook-delayed-origin.jsonl
```

Each phase warms up for one second by default. A worker starts its next request
only after its previous request completes or fails. Concurrency is the maximum
number of outstanding requests, not a fixed arrival rate. The measurement
stops starting requests after `-duration` and includes the final drain in
elapsed time and throughput. `-timeout` defaults to ten seconds per request.
Failure-free warmups are omitted; failing warmups are emitted with a `-warmup`
profile suffix. Any warmup or measurement failure makes the command exit
nonzero; the remaining measurements continue unless the WebSocket disconnects.

JSONL includes successful requests/second, response MiB/second, failure counts
and reasons, successful-request p50/p95/p99/max latency, and new origin TCP
connections. POST also transfers an equal amount of request payload, in
addition to the reported response throughput. Latencies include WebSocket
write queueing and full response validation. They exclude failed requests;
always read latency alongside the failure rate.

The tool creates temporary state, uses loopback listeners, and terminates its
child Rook process when finished. It does not contact PHPSandbox or an existing
deployment. It does not include public HTTP ingress, TLS, WAN latency, or
application business logic. The generator and origin share the same machine
as Rook, so measurements describe this setup rather than a portable maximum.

## Baseline and pool-only experiments (2026-10-15)

The baseline build sustained about **13,800 1 KiB GETs/s, 5,500 64 KiB
GETs/s, and 3,000 64 KiB POSTs/s**, each for 60 seconds with two workers and
zero failures. Increasing concurrency caused connection churn and upstream
connection failures. An isolated larger-pool build sustained about **20,800
1 KiB GETs/s at 256 workers** for 60 seconds with zero failures.

Production source at commit `e0736ed6ba7cb2566c6b10adacc8d4fa5b93a2a4`.
Host: Apple M4 Pro, 14 CPU cores, 48 GiB RAM, macOS 26.7.1, Go 1.26.5 arm64.
All measurements use one data WebSocket, an immediate HTTP origin, one
deployment, and sequential phases; phases share the process and transport.
Higher-concurrency rows therefore include the effects of earlier connection
churn, as continuous production load would. Docker was unavailable, so these
measurements were not repeated on Rook's supported Linux target.

The baseline transport retains only two idle connections per destination
(`http.DefaultTransport`, `DefaultMaxIdleConnsPerHost = 2`). Overlapping
requests create many new TCP connections and close excess idle connections.
The Mac's ephemeral port range is 49152–65535, and its configured TCP MSL is
15 seconds. Under sustained churn, Rook returned stream resets with:

```text
proxy request failed: ... dial tcp 127.0.0.1:<port>: connect: can't assign requested address
```

The larger-pool comparison uses a separate binary built with a Go source
overlay. The [experimental patch](load-results/pool-experiment.patch) creates
a shared cloned HTTP transport and sets both `MaxIdleConns` and
`MaxIdleConnsPerHost` to 4096. It is a diagnostic experiment, not a production
pool sizing recommendation. The baseline source was unchanged during these
experiments; the implemented policy above adds admission limits.

To reproduce that comparison, use a disposable checkout of baseline commit
`e0736ed`, apply the saved zero-context patch with
`git apply --unidiff-zero <patch-file>`, build Rook there, and pass its binary
to `-rook`. Run the workloads serially to avoid
competing load generators.

### Ten-second ramps

**Baseline production implementation at e0736ed** — [raw results](load-results/current-ramp.jsonl).

| Profile | Concurrent workers | Successful requests/s | Failures | Successful p99 (ms) | New origin connections |
| --- | ---: | ---: | ---: | ---: | ---: |
| get-1k | 1 | 9,879 | 0.00% | 0.23 | 0 |
| get-1k | 2 | 13,374 | 0.00% | 0.36 | 0 |
| get-1k | 4 | 7,324 | 0.84% | 2.98 | 12,753 |
| get-1k | 16 | 276 | 81.57% | 26.59 | 2 |
| get-1k | 64 | 1,252 | 48.29% | 95.64 | 5,847 |
| get-1k | 256 | 1,598 | 39.95% | 370.95 | 7,628 |
| get-64k | 1 | 3,980 | 0.00% | 0.53 | 0 |
| get-64k | 2 | 4,793 | 0.00% | 0.72 | 0 |
| get-64k | 4 | 5,407 | 0.00% | 1.14 | 12,551 |
| get-64k | 16 | 425 | 69.13% | 32.71 | 579 |
| get-64k | 64 | 667 | 62.17% | 103.16 | 1,799 |
| get-64k | 256 | 2,521 | 17.49% | 454.50 | 12,203 |
| post-64k | 1 | 2,302 | 0.00% | 0.75 | 0 |
| post-64k | 2 | 2,732 | 0.00% | 1.04 | 0 |
| post-64k | 4 | 2,993 | 0.00% | 2.04 | 3,541 |
| post-64k | 16 | 2,871 | 0.00% | 15.57 | 3,825 |
| post-64k | 64 | 2,029 | 17.42% | 60.27 | 6,816 |
| post-64k | 256 | 887 | 49.25% | 378.85 | 3,629 |

727,702 measured requests; 74,155 failures.

**Separate larger-pool experiment** — [raw results](load-results/pool-ramp.jsonl).

| Profile | Concurrent workers | Successful requests/s | Failures | Successful p99 (ms) | New origin connections |
| --- | ---: | ---: | ---: | ---: | ---: |
| get-1k | 1 | 9,735 | 0.00% | 0.23 | 0 |
| get-1k | 16 | 17,061 | 0.00% | 1.42 | 0 |
| get-1k | 64 | 17,870 | 0.00% | 6.21 | 10 |
| get-1k | 256 | 21,130 | 0.00% | 13.13 | 0 |
| get-1k | 1024 | 21,579 | 0.00% | 55.51 | 0 |
| get-64k | 1 | 4,666 | 0.00% | 0.57 | 0 |
| get-64k | 16 | 7,685 | 0.00% | 3.10 | 0 |
| get-64k | 64 | 7,395 | 0.00% | 14.69 | 0 |
| get-64k | 256 | 7,666 | 0.00% | 36.54 | 0 |
| get-64k | 1024 | 7,476 | 0.00% | 197.33 | 0 |
| post-64k | 1 | 3,073 | 0.00% | 1.03 | 0 |
| post-64k | 16 | 4,824 | 0.00% | 4.65 | 0 |
| post-64k | 64 | 4,811 | 0.00% | 15.84 | 0 |
| post-64k | 256 | 4,883 | 0.00% | 81.84 | 0 |
| post-64k | 1024 | 5,090 | 0.00% | 267.36 | 0 |

1,451,190 measured requests; 0 failures.

All baseline-build failures in the earlier small-payload ramps were upstream
connection resets with the error above; the test did not observe successful-status responses with corrupted
bodies. Current-ramp warmup connection counts were not sampled; only measured
rows have meaningful origin connection counts in that file.

### Sixty-second checks

The baseline build passed all three profiles at two concurrent workers.
Every measured phase reused the warmed-up upstream connections without opening
new ones. [Raw results](load-results/current-soak.jsonl).

| Profile | Requests | Successful requests/s | Failures | p99 (ms) | Response MiB/s |
| --- | ---: | ---: | ---: | ---: | ---: |
| get-1k | 828,241 | 13,804 | 0 | 0.35 | 13.5 |
| get-64k | 332,564 | 5,543 | 0 | 0.67 | 346.4 |
| post-64k | 182,467 | 3,041 | 0 | 0.98 | 190.1 |

These are observed failure-free rates, not universal ceilings or a long-term
reliability guarantee. Higher concurrency failed in the baseline-build ramp.
The larger-pool experiment isolates a transport bottleneck on this machine;
production sizing still needs testing on Linux with representative request
sizes, application processing time, deployment count, and network latency.

The separate larger-pool build also passed a 60-second `get-1k` check at 256
workers: **20,763 requests/second**, 1,245,930 requests, zero failures,
and p99 15.10 ms. It opened 24 new upstream connections during the
measured minute. [Raw results](load-results/pool-soak.jsonl).

### Validation

`go test ./...`, `go test -race ./...`, and `go vet ./...` passed.
The final runner and Rook binaries also passed a race-instrumented load check
across all three profiles at four workers, with a 10 ms simulated application
delay and zero failures or race reports. `git diff --check` passed.

## Additional 3 MiB GET measurements

`get-3m` returns exactly 3,145,728 bytes. The runner checks every byte of
the streamed response. The ten-second ramps used the same local topology and
ten-second request deadline as the earlier workloads.

| Build | Concurrent workers | Successful requests/s | Response MiB/s | Successful p99 (ms) | Failures |
| --- | ---: | ---: | ---: | ---: | ---: |
| Baseline | 1 | 263.4 | 790.3 | 4.6 | 0 |
| Baseline | 2 | 247.1 | 741.3 | 10.1 | 0 |
| Baseline | 4 | 232.5 | 697.5 | 24.5 | 0 |
| Baseline | 16 | 226.0 | 677.9 | 82.7 | 0 |
| Baseline | 64 | 242.0 | 726.0 | 285.1 | 0 |
| Larger pool | 1 | 259.6 | 778.9 | 4.7 | 0 |
| Larger pool | 2 | 250.9 | 752.7 | 8.9 | 0 |
| Larger pool | 4 | 243.4 | 730.3 | 17.8 | 0 |
| Larger pool | 16 | 221.1 | 663.3 | 124.9 | 0 |
| Larger pool | 64 | 242.0 | 725.9 | 304.5 | 0 |
| Larger pool | 256 | 240.6 | 721.8 | 1434.5 | 0 |

Raw ramps: [current](load-results/current-get3m-ramp.jsonl),
[larger pool](load-results/pool-get3m-ramp.jsonl). The baseline ramp aborted
during the 256-worker warmup; the larger-pool ramp aborted during the
1024-worker warmup. The zero-duration measurement rows following those failed
warmups are aborted phases, not valid throughput measurements. The runner now
stops immediately on a warmup connection failure.

A fresh [256-worker baseline-build repeat](load-results/current-get3m-256-repeat.jsonl)
with temporary write-error logging passed: 2,560 requests, zero failures,
242 requests/s, p99 1,261 ms over roughly ten seconds. The earlier disconnect
at that concurrency was intermittent.

A fresh [1024-worker larger-pool repeat](load-results/pool-get3m-1024-repeat.jsonl)
with the same diagnostic logging disconnected before any requests completed.
The [first write error](load-results/get3m-disconnect-diagnostic.txt) was
`write: no buffer space available` on Rook's data WebSocket. Subsequent writes
failed on the closed connection, and Rook reconnected the data plane.
The logging-only [diagnostic patch](load-results/get3m-diagnostic.patch) was
applied using a source overlay to the baseline source.

The Mac was configured with `kern.ipc.maxsockbuf = 8388608` and
`kern.ipc.nmbclusters = 262144`. A subsequent `netstat -m` snapshot reported
577,806 cumulative requests for network memory denied; that cumulative count
includes activity outside these runs. The socket write error directly confirms
a buffer-allocation failure in the diagnostic run. No kernel settings were
changed. A larger HTTP connection pool does not eliminate this failure.

The **60-second baseline-build check at 64 workers passed**: 14,947
requests, zero failures, **248.6 requests/s** (745.9 MiB/s),
p99 340.1 ms. It opened 2,329 new upstream connections during the
measurement. [Raw results](load-results/current-get3m-soak.jsonl).

The **60-second larger-pool check at 256 workers passed**: 14,336
requests, zero failures, **235.5 requests/s** (706.4 MiB/s),
p99 1394.0 ms, and no new upstream connections after warmup.
[Raw results](load-results/pool-get3m-soak.jsonl).

For this workload, increasing concurrency did not increase throughput; it
increased the time to complete each download. The earlier connection-pool
change alone cannot be considered a solution to the high-concurrency 3 MiB
failures. These loopback results include the local origin and load generator
and should be repeated on the intended Linux host with representative network
conditions before setting a production capacity limit.

The final 3 MiB runner and Rook also passed a race-instrumented four-worker
check with zero failures or race reports. `go vet ./...` and
`git diff --check` passed after the profile and warmup-abort changes.

## Fixed-build checks

The implemented pool, global admission limits, stream cleanup, and isolated
relay writer were tested with the same compiled-process loopback harness.
These figures describe this Mac and include the final request drain.

| Workload | Concurrent callers | Requests | Successful requests/s | Failures | p99 (ms) |
| --- | ---: | ---: | ---: | ---: | ---: |
| get-1k | 1024 | 1,141,481 | 19,010 | 0 | 73.8 |
| get-3m | 1024 | 12,610 | 195 | 0 | 5703.6 |
| post-64k | 1 | 27,538 | 2,754 | 0 | 0.6 |
| post-64k | 16 | 35,702 | 3,569 | 0 | 9.7 |
| post-64k | 64 | 45,953 | 4,593 | 0 | 16.9 |

GET measurements ran for 60 seconds each; POST measurements ran for ten
seconds each. All measured phases opened zero new upstream connections after
warmup. [GET raw results](load-results/fixed-get-soak.jsonl),
[POST raw results](load-results/fixed-post-ramp.jsonl).

The fixed 1024-caller 3 MiB test completed 12,610 verified responses with
zero failures. The corresponding pool-only experiment had disconnected with
`no buffer space available`. Admission limits kept the active upstream count
at 64 while the remaining callers waited.

Regression tests cover connection reuse across concurrent waves, global
admission across two deployments, response-close/error capacity release,
bounded-queue overload and cancellation, upload overload without blocking the
receive loop, HTTP ID reuse for WebSocket upgrades, reconnect cleanup, stale
relay sends, and cancellation during a backpressured socket write.
`go test ./...`, `go test -race ./...`, `go vet ./...`, formatting checks,
installer shell syntax, and `git diff --check` passed.

At 1024 concurrent POST callers, overload was handled explicitly: 6,208
verified responses and 93,120 `HTTP relay at capacity` resets over ten
seconds. There were no transport disconnects, timeouts, or body corruption.
The runner deliberately exits nonzero for these expected rejections.
[Overload raw results](load-results/fixed-post-overload.jsonl).
