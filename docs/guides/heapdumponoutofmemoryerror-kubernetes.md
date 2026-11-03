# Why -XX:+HeapDumpOnOutOfMemoryError never writes a file in Kubernetes

There are two memory limits, and only one of them produces a heap dump.

- **The Java heap limit** (`-Xmx`, or `-XX:MaxRAMPercentage` of the container limit). When the heap is full, the JVM throws `java.lang.OutOfMemoryError`. `HeapDumpOnOutOfMemoryError` runs at that point.
- **The container limit** (`resources.limits.memory`, enforced by the kernel's cgroup). When the whole process (heap, metaspace, threads, code cache, direct buffers, malloc) reaches it, the kernel sends SIGKILL. Kubernetes reports `OOMKilled`, exit code 137. The JVM gets no chance to run anything.

If the heap limit is above the container limit, the second one always wins, and no file is written. That is the setup in most "the heap dump never appears" reports.

Verified 2026-11-03 on EKS 1.33, Amazon Linux 2023, Eclipse Temurin 21.0.13.

## Both cases, same code, same 384 MiB container

The same leaking program ran twice, with only `JAVA_TOOL_OPTIONS` changed. Both runs used `-XX:+HeapDumpOnOutOfMemoryError -XX:HeapDumpPath=/dumps`.

| Heap setting | What happened | Exit | Dump written |
|---|---|---|---|
| `-Xmx1g` ([manifest](lab/java-xmx-above-limit.yaml)) | Kernel OOM kill at the container limit | 137, `OOMKilled` | No: `/dumps` empty |
| `-XX:MaxRAMPercentage=50` ([manifest](lab/java-heap-below-limit.yaml)) | `java.lang.OutOfMemoryError: Java heap space` | 1, `Error` | Yes: 190 MB in 0.79 s |

The second run's log:

```
java.lang.OutOfMemoryError: Java heap space
Dumping heap to /dumps/java_pid1.hprof ...
Heap dump file created [199585126 bytes in 0.786 secs]
```

## What to change

1. **Size the heap from the container limit**, not as a fixed `-Xmx`. Use `-XX:MaxRAMPercentage=50` to `70`, and drop any hard-coded `-Xmx`. The rest of the limit is for non-heap memory. If you set it near 100%, you are back to the kernel kill.
2. **Write the dump where it survives.** `HeapDumpPath` on an `emptyDir` survives a container restart but not a pod deletion. Use a PVC or copy the file out on start. A 190 MB dump also needs that much free space on the volume.
3. **Decide whether you want the JVM to stay up.** A JVM that throws `OutOfMemoryError` can keep running half-broken. `-XX:+ExitOnOutOfMemoryError` makes it exit (after writing the dump), so Kubernetes restarts it cleanly.

## When you cannot change the flags

Sometimes the image is not yours, or the leak only shows in production. Then the only evidence is what you capture while the JVM is still alive. We measured how much room you have: a JVM like this one dies at about 80% of the limit, not 100%, and the default `jcmd GC.class_histogram` adds about 20 points of memory. See [Get a Java heap histogram before Kubernetes OOMKills the pod](java-heap-before-oomkill.md).

[lastwords](../../README.md) does this without flags or `jcmd` in the image. It takes a class histogram, heap info, native memory and threads through the attach API when a JVM's memory climbs. It also explains both failure modes in its summary. For the second case above, it reported:

```
- The JVM ran out of Java heap (its -Xmx or MaxRAMPercentage), not container memory. Raise the heap only if
  the load is real; to see what filled it, add -XX:+HeapDumpOnOutOfMemoryError -XX:HeapDumpPath= pointing at a
  volume that survives the restart.
- Crashed in PriceCache.main(PriceCache.java:7) (Exception in thread "main" java.lang.OutOfMemoryError: Java heap space).
```
