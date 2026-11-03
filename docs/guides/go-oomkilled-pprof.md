# Find what is leaking in a Go pod before it is OOMKilled

To find the line that is leaking memory in a Go service, take a heap profile from `net/http/pprof` while the process is still alive. `go tool pprof -top` then names the allocation site. The hard part on Kubernetes is timing. The profile only exists in the running process, and a pod that OOM-kills itself every minute spends much of its time in CrashLoopBackOff.

Verified 2026-11-03 on EKS 1.33 (Amazon Linux 2023), Go 1.26, with the [billing-api example](../../examples/leaky-go/main.go): 256 MiB limit, a response cache that is appended to and never trimmed.

## By hand

The program must import `net/http/pprof` and serve it, here on `:6060`. Wait for the container to be running, then:

```sh
kubectl -n storefront port-forward pod/<pod> 16060:6060 &
curl -s -o heap.pb.gz http://localhost:16060/debug/pprof/heap
go tool pprof -top -sample_index=inuse_space heap.pb.gz
```

What it printed for us, about 40 s into a container's life:

```
File: billing-api
Type: inuse_space
Showing nodes accounting for 25394.59kB, 100% of 25394.59kB total
      flat  flat%   sum%        cum   cum%
24882.55kB 97.98% 97.98% 24882.55kB 97.98%  main.renderInvoice
  512.05kB  2.02%   100%   512.05kB  2.02%  runtime.mallocgc
```

`renderInvoice` allocates each invoice, and the cache keeps every one. Our first attempt failed: the pod was in CrashLoopBackOff, so there was nothing to port-forward to. By the time we caught it running, it had restarted 11 times.

## The number that confuses people

lastwords took the same profile at 71% of the limit. Live heap was 54.7 MiB, but the container used 181.7 MiB:

```
- Live Go heap is 54.7 MiB but the container uses 181.7 MiB: the garbage collector lets the heap grow to
  about twice the live data (GOGC=100) before collecting. Set GOMEMLIMIT to about 90% of the container limit
  so the GC works harder before the kernel kills the process. That buys time but does not fix a leak.
- One allocation site holds most of the Go heap: main.renderInvoice (/src/main.go:29) keeps 54.2 MiB in use.
```

So a pprof "in use" figure far below the container's memory does not mean the profile is wrong. With the default `GOGC=100`, the Go runtime lets the heap reach about twice the live data before it collects. Without `GOMEMLIMIT`, it doesn't know about the container limit at all.

## What to change

1. **Fix the leak.** Bound the cache (size or TTL) or stop keeping rendered output after it is sent. A steady climb like this one is a leak, not a limit that's too small.
2. **Set `GOMEMLIMIT`** to about 90% of the container limit, for example `GOMEMLIMIT=230MiB` for 256 MiB. The GC then works harder near the limit instead of letting the kernel kill the process. It does not fix a leak; it only means a short spike no longer kills the pod.
3. **Keep pprof reachable**, but only inside the cluster or on localhost. It exposes memory contents and stack traces.

## Automatically

[lastwords](../../README.md) fetches the heap profile and goroutine dump from port 6060 (or the `lastwords.dev/pprof-port` annotation) when a Go container's memory climbs. It saves the raw `go-heap.pb.gz` next to the summary, so there is nothing to race:

```sh
kubectl lastwords list -n storefront
kubectl lastwords get <capture-id> go-heap.pb.gz
go tool pprof -top go-heap.pb.gz
```

If the program does not serve pprof, lastwords still records the memory curve, the breakdown and the logs of the instance that died. It cannot name the allocation site then.
