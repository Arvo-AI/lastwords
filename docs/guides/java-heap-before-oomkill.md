# Get a Java heap histogram before Kubernetes OOMKills the pod

If your JVM's heap limit (`-Xmx`, or the default from `MaxRAMPercentage`) is above the container's memory limit, the kernel kills the JVM before Java ever throws `OutOfMemoryError`. So `-XX:+HeapDumpOnOutOfMemoryError` never writes a file. You have to capture the heap while the JVM is still alive. Use `jcmd <pid> GC.class_histogram -all` before memory passes about 70% of the limit, not the default form, and not at 90%. Or let [lastwords](../../README.md) do it automatically.

Verified 2026-11-03 on EKS 1.33 (Amazon Linux 2023, kernel 6.12, containerd 2.3), t3.large nodes, Eclipse Temurin 21.0.13.

## What we measured

We ran a JVM with `-Xmx1g -XX:+HeapDumpOnOutOfMemoryError -XX:HeapDumpPath=/dumps` in a container limited to 384 MiB, with a map that never evicts ([lab manifest](lab/java-xmx-above-limit.yaml)). On a 2-CPU node with that limit, the JVM picks SerialGC.

**1. Nothing is written.** After the first OOM kill, the dump directory (an `emptyDir`, so it survives the container restart) was empty:

```
restarts=1 lastReason=OOMKilled exit=137
--- ls -la /dumps
total 0
drwxrwxrwx+ 2 root root  6 Nov  3 18:09 .
drwxr-xr-x+ 1 root root 41 Nov  3 18:10 ..
```

The last log line of the dead container was the JVM printing its flags at startup. There was no `OutOfMemoryError` and no "Dumping heap".

**2. The JVM died at 80% of the limit, not 100%.** We polled `memory.current` every 0.1 s from inside the container with nothing else attached:

```
16:47:21.670 79%
16:47:21.784 80%
16:47:21.904 96%
command terminated with exit code 137
```

A young collection copied the live objects into the old generation, and memory jumped 16 points in a tenth of a second. Details: [Java pod OOMKilled at 80% of its memory limit](jvm-oomkilled-below-limit.md).

**3. The usual capture command can cause the kill.** `jcmd GC.class_histogram` forces a full GC first, and that GC needs memory. We ran each form on a fresh pod at 55% of the limit, 4 times each:

| Command | Memory before | Memory after | Peak during | Time |
|---|---|---|---|---|
| `jcmd 1 GC.class_histogram` | 55% | 69–70% | 75–76% | 404–469 ms |
| `jcmd 1 GC.class_histogram -all` | 55% | 55% | 61–62% | 328–356 ms |

The default form added about 20 points of the limit, which is enough to push a JVM at 60% or more into the kill zone. `-all` walks the heap without collecting, so it can also count garbage. Leaked objects are reachable, so they still lead the list.

## Capture it by hand

You need `jcmd` in the container. **The `eclipse-temurin:21-jre` image does not ship it** (its `bin/` has `java jfr jrunscript jwebserver keytool rmiregistry`). JDK images do.

```sh
POD=$(kubectl -n <ns> get pod -l app=<app> -o name | head -1)
kubectl -n <ns> exec $POD -- sh -c 'cat /sys/fs/cgroup/memory.current /sys/fs/cgroup/memory.max'
kubectl -n <ns> exec $POD -- jcmd 1 GC.class_histogram -all > histogram.txt
head -15 histogram.txt
```

Do it while memory is still climbing, well before the 80% point. If the pod is in CrashLoopBackOff, `exec` fails until the next start, so you are racing the backoff timer.

A shallow histogram puts the bytes under `byte[]` or `long[]`, not under the class that owns them. Look for an application class whose instance count matches the big array's count. In our G1 test, `long[]` held 860 MiB in 57,532 instances and `Indexer$Posting` had 55,001 instances: each Posting held one array.

## Capture it automatically

lastwords watches every container's working set and runs `GC.class_histogram -all`, `GC.heap_info`, `VM.native_memory` and `Thread.print` through the JVM attach API. For JVMs it fires below the GC kill point, at 75% of the limit (`capture.jvmGCHeadroom: 0.25`). It works on JRE images because it does not need `jcmd` in the container. The capture from the same leak, taken at 67% of the limit:

```
- Largest application classes on the heap: com.example.checkout.CheckoutService$CartSnapshot:
  2900 instances, 91 KiB in total; com.example.checkout.CheckoutService$SessionCache: 1 instance, 16 B in total.
- One class dominates the Java heap: byte[] holds 181.7 MiB (99%) in 13643 instances.
```

```sh
helm install lastwords oci://ghcr.io/arvo-ai/charts/lastwords -n lastwords --create-namespace
kubectl lastwords list -n <ns>
kubectl lastwords show <capture-id>
```

## Or make the JVM throw instead

If the heap limit is below the container limit, the JVM fails with `OutOfMemoryError` and `HeapDumpOnOutOfMemoryError` works. With `-XX:MaxRAMPercentage=50` in the same 384 MiB container ([lab manifest](lab/java-heap-below-limit.yaml)):

```
java.lang.OutOfMemoryError: Java heap space
Dumping heap to /dumps/java_pid1.hprof ...
Heap dump file created [199585126 bytes in 0.786 secs]
```

The container exits with code 1 instead of 137. Put `HeapDumpPath` on a volume that outlives the pod if you need the dump after a reschedule (an `emptyDir` is lost when the pod is deleted). A full `.hprof` contains everything in memory, including tokens and personal data, so treat it as a secret.

## Mistakes we see

- **Raising the memory limit.** It moves the kill point; a leak still gets there.
- **Alerting at 90% for JVMs.** A JVM like this one is already dead by then.
- **Running `GC.class_histogram` (without `-all`) on a JVM near its limit.** It can trigger the kill you are trying to diagnose.
- **Setting `-Xmx` to the container limit.** Metaspace, thread stacks, code cache and direct buffers live outside the heap. Leave 25% or more of the limit for them.
