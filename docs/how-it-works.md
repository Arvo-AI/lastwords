# How lastwords works

## Memory pressure captures

The agent finds every container cgroup on its node by walking the host cgroup tree (`/sys/fs/cgroup/kubepods*`) and matching the 64-hex container ID in the directory name (`cri-containerd-<id>.scope`, `crio-<id>.scope`, or `<id>` with the cgroupfs driver). It joins that ID to the pod through a pod informer limited to its node.

Every 500 ms it reads:

- `memory.current` and `memory.max` (cgroup v2) or `memory.usage_in_bytes` and `memory.limit_in_bytes` (v1)
- `memory.stat`, to compute the **working set** = usage − `inactive_file`. That is the number the kubelet reports and the one that matters for the OOM killer, since inactive page cache is reclaimed first.
- `memory.events` (`oom_kill` counter) and `memory.pressure` (PSI)

A capture fires once per container instance per tier:

- **threshold**: working set ≥ 90% of the limit, or ≥ 70% and a least-squares slope over the last 10 s says the limit will be hit within 15 s
- **last-gasp**: working set ≥ 97%

JVMs are measured against 75% of the limit instead (`capture.jvmGCHeadroom: 0.25`). A JVM whose `-Xmx` is above its container limit is not killed at 100%: a young GC copies the live objects in eden into the old generation, memory jumps by roughly a quarter of the heap in a fraction of a second, and the kernel kills it. On EKS the demo JVM climbed steadily to 80% and was dead 0.1 s later at 96%. The runtime is detected once per container when it passes 40% of its limit.

Captures run outside the sampling loop, two at a time per node, with a 60 s budget.

### What a pressure capture contains

| File | Source |
|---|---|
| `cgroup-memory.txt` | Raw cgroup memory files |
| `processes.txt` | Every process in the container: RSS, peak RSS, threads, `smaps_rollup`, redacted command line |
| `timeline.csv` | The container's working set for the last 10 minutes |
| `jvm-heap-info.txt` | `jcmd GC.heap_info` |
| `jvm-class-histogram.txt` | `jcmd GC.class_histogram -all` (objects by class, no forced GC) |
| `jvm-native-memory.txt` | `jcmd VM.native_memory summary` (when the JVM runs with `-XX:NativeMemoryTracking=summary`) |
| `jvm-threads.txt` | `jcmd Thread.print` |
| `heap.hprof.gz` | `jcmd GC.heap_dump -gz=1`, only when allowed |
| `go-heap.pb.gz` | `/debug/pprof/heap`, open with `go tool pprof` |
| `go-goroutines.txt` | `/debug/pprof/goroutine?debug=1` |

### JVM attach from outside the container

lastwords implements the HotSpot Dynamic Attach protocol, the same one `jcmd` and `jattach` use:

1. Confirm the process maps `libjvm.so`. This matters because SIGQUIT terminates a non-JVM process.
2. Create `/tmp/.attach_pid<nspid>` in the target's mount namespace (through `/proc/<pid>/root`), owned by the JVM's user.
3. Send SIGQUIT. The JVM starts its attach listener and creates `/tmp/.java_pid<nspid>`.
4. Connect to that UNIX socket and send `1\0jcmd\0<command>\0\0\0`.

Nothing is installed in your image and the JVM needs no flags. `-XX:+DisableAttachMechanism` turns this off, and lastwords records that it could not attach.

`GC.class_histogram -all` walks the heap without forcing a GC. The default form runs a full GC first, and on a JVM close to its container limit that GC needs extra memory and can trigger the very OOM kill being captured. The walk still pauses the JVM briefly (a second or more on a multi-GB heap), once per container instance per tier. Because nothing is collected first, the counts can include garbage; leaked objects are reachable, so they still lead the list.

## Deaths

The pod informer reports each container whose `lastState.terminated` (or `state.terminated`) is new. For anything except a clean exit, lastwords:

- finds the dead instance's log file in `/var/log/pods/<ns>_<pod>_<uid>/<container>/<n>.log`, the newest file written no later than its `finishedAt`, and copies the last 500 lines
- lists the pod's events
- attaches the memory timeline it already had for that container ID
- parses the crash site out of Go panics, Java exceptions and Python tracebacks
- links the pressure captures and core dumps of the same container ID

Crash loops are bounded: the first 3 crashes of a container per 30 minutes are captured. Later ones are counted in `lastwords_crashes_suppressed_total`.

A process OOM-killed inside a container that keeps running (a worker or child process) only shows up as an increase of `oom_kill` in `memory.events`. Kubernetes never reports it. lastwords captures it as `process-oomkill`. On cgroup v2 nodes with Kubernetes 1.28 or later the kubelet sets `memory.oom.group`, so the kernel kills every process in the container together and you get an ordinary `oomkill` instead; `process-oomkill` only appears on cgroup v1 nodes or with the kubelet's `singleProcessOOMKill: true` (1.32+). On EKS 1.33 a child process running out of memory took its whole container down.

## Core dumps

`kernel.core_pattern` is a node-wide kernel setting. With `coreDumps.enabled=true` the agent:

1. copies its own static binary to `<hostPath>/bin/lastwords`
2. writes `|<hostPath>/bin/lastwords core-handler %P %s %t %e` to `/proc/sys/kernel/core_pattern`, after saving the previous value
3. restores the previous value when it stops (`coreDumps.restoreOnExit`)

The kernel runs the helper in the host's root namespaces with the core on stdin. The helper:

- reads `/proc/<pid>/cgroup` to find the container ID, ignoring crashes outside containers
- saves the memory maps and streams the core through gzip into `<hostPath>/cores`, up to `coreDumps.maxBytes`
- writes a metadata file last

The agent picks it up within 2 seconds, matches it to the pod, and turns it into a `coredump` capture.

## Storage, access and notifications

Captures live in `<hostPath>/captures/<id>/`, one directory per capture, with `capture.json` (the manifest) and `summary.md`. A size budget (`storage.maxBytes`) and age budget (`storage.maxAge`) delete the oldest captures first.

The agent serves a small HTTP API on port 9095:

- `/v1/captures`
- `/v1/captures/{id}`
- `/v1/captures/{id}/files/{name}`
- `/v1/watchlist`
- `/v1/node`
- `/metrics`

The CLI and MCP server reach it through the Kubernetes API server's pod proxy (`pods/proxy`), sending the token from the `lastwords-api` secret. No Service, Ingress or port-forward is needed.

Every capture also:

- becomes a Kubernetes Event on the pod
- can be POSTed to a webhook as JSON (`capture` manifest plus `summary`)
- can be posted to Slack
- can be uploaded to S3-compatible storage under `<prefix>/<cluster>/<node>/<id>/`
