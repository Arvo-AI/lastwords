# Java pod OOMKilled at 80% of its memory limit

Your dashboards show the pod at about 80% of its memory limit, and then it is `OOMKilled`. The JVM was not at 80% when it died. A garbage collection pushed it from 80% to over 100% faster than any metrics scrape can see. We measured this with SerialGC, with the heap limit (`-Xmx`) above the container limit: the collector copies live objects into the old generation, and that touches a quarter or more of the heap at once. The 80% figure belongs to this setup; see [how far it generalizes](#how-far-the-80-goes).

Verified 2026-11-03 on EKS 1.33 (Amazon Linux 2023, kernel 6.12), t3.large (2 vCPU), Eclipse Temurin 21.0.13.

## The measurement

The JVM ran with `-Xmx1g` in a 384 MiB container and a cache that never evicts. Nothing else was attached; even lastwords was removed from the node. `memory.current` was read every 0.1 s from inside the container:

```
16:47:21.556 79%
16:47:21.670 79%
16:47:21.784 80%
16:47:21.904 96%
command terminated with exit code 137
```

Memory climbed in a straight line at about 5.5 MiB/s, then jumped 16 points between two samples and the kernel killed the process. The cycles we watched died the same way, at 79–80%.

## Why

With 2 CPUs and a 384 MiB limit, the JVM chooses SerialGC on its own (`UseSerialGC = true {ergonomic}` in `java -XX:+PrintFlagsFinal`). The young generation is about a third of the heap. In a leak, almost everything in eden is still live at the next young collection, so it all gets copied into the old generation. The eden pages stay committed, and the old generation grows by the same amount. For a moment the process holds both copies.

Because `-Xmx1g` is far above 384 MiB, the JVM thinks it has plenty of room and keeps growing the heap. It never throws `OutOfMemoryError`. The kernel kills it first, and `HeapDumpOnOutOfMemoryError` never runs ([why](heapdumponoutofmemoryerror-kubernetes.md)).

## How far the 80% goes

It is one setup, not a rule for JVMs. In a later sweep on a local Docker host (not EKS), three runs per collector, the same kind of leak behaved differently:

| Collector | Last memory sample before the kill | Largest GC jump |
|---|---|---|
| SerialGC | 92%, 100%, 88% | 15, 16, 15 points |
| G1 | 100%, 100%, 100% | 8, 5, 6 points |
| ParallelGC | 100%, 100%, 100% | 2, 3, 2 points |

The jump is real for SerialGC and much smaller for the others. Small containers are the ones that matter here: HotSpot only picks G1 with at least 2 CPUs and about 1.75 GiB, so a small pod gets SerialGC by default.

## What it breaks

- **A 90% alert never fires before the kill.** Neither does a "predicted to hit the limit in N seconds" alert, which extrapolates the straight line and misses the jump.
- **A diagnostic capture taken late is too late.** Ours first triggered at 79%, one sample before the kill, and the JVM died before it answered. We moved the JVM trigger to 75% of the limit. After that, the same leak was captured at 67–73%, on every cycle within the capture budget (3 snapshots per container in 30 minutes).
- **The capture command can be the kill.** The default `jcmd GC.class_histogram` forces a full GC. From 55% it took the container to 75–76% peak (4 of 4 runs); `-all` peaked at 61–62%.

## What to do

1. **Fix the heap size first.** Drop `-Xmx` above the limit and use `-XX:MaxRAMPercentage=50`–`70`. The JVM then throws `OutOfMemoryError` inside its own heap, and you get a stack trace and, with the flag, a heap dump.
2. **Alert earlier for JVMs.** Watch `container_memory_working_set_bytes / limit` at around 70%, not 90%, for containers running a JVM with a large `-Xmx`.
3. **Capture at 60–70% with `-all`.** `kubectl exec <pod> -- jcmd 1 GC.class_histogram -all`, if the image has `jcmd` (JRE images don't).

[lastwords](../../README.md) applies this automatically. It detects the JVM, measures its capture thresholds against 75% of the limit (`capture.jvmGCHeadroom`), and uses `-all`. For the leak above, it named `CheckoutService$CartSnapshot` at 67% of the limit, 18 seconds before the kill.
