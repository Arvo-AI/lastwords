<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-wordmark-dark.svg">
    <img alt="lastwords" src="assets/logo-wordmark.svg" height="72">
  </picture>
</p>

# lastwords

**Memory snapshots before OOM kills, and logs and core dumps from crashes, for Kubernetes.**

lastwords runs on every node. When a container gets close to its memory limit, it records what is using the memory. When a container dies, it saves the logs of that instance, the pod's events and any core dump. Everything stays on the node (or in S3), and you can read it with `kubectl lastwords` or from an AI agent over MCP.

```sh
helm install lastwords oci://ghcr.io/arvo-ai/charts/lastwords -n lastwords --create-namespace
```

## The problem

When you look at a pod that died hours ago, most of the evidence is gone:

- **OOMKilled leaves no heap dump.** The kernel kills the process with SIGKILL. `-XX:+HeapDumpOnOutOfMemoryError` never fires, and Go, Node and Python get no warning either.
- **`kubectl logs --previous` only keeps one restart.** After a few crash-loop restarts, the logs from the first failure are gone.
- **Events expire after one hour** (`--event-ttl`, default 1h).
- **Core dumps go nowhere.** `core_pattern` is set per node, so crashing containers either dump nothing or dump to a path inside a container that is about to be deleted.

None of it can be recovered later. It has to be collected while the container is alive or right after it dies, which is what lastwords does.

## What it captures

| When | What lastwords saves |
|---|---|
| A container nears its memory limit (90%, again at 97%, or earlier if it is climbing fast) | Memory breakdown (heap vs page cache vs kernel), a memory timeline, processes. **Java:** class histogram, heap info, native memory, thread dump. **Go:** heap profile with the top allocation sites. Optional full heap dump. |
| A container is OOMKilled | The memory curve up to the kill, logs of the instance that died, pod events, and links to the snapshots taken before the kill |
| A process inside a container is OOM-killed but the container survives | What Kubernetes never reports: which process died and the memory state |
| A container crashes (non-zero exit, signal) | Log tail of the dead instance, the crash site parsed from Go/Java/Python stack traces, events, exit code meaning |
| A process dumps core (SIGSEGV, SIGABRT, ...) | The gzipped core, memory maps, binary and image, matched to the pod |
| A pod is evicted | Eviction reason, every container's logs, events |

Every capture comes with a `summary.md` that says what happened in plain words:

```
# checkout-576cc8d588-cw9gs/checkout was OOMKilled at 03:07:12 UTC (limit 384.0 MiB); 2 snapshot(s) taken before the kill

## What lastwords found
- Memory rose from 38.2 MiB to 366.3 MiB over 1m3s (5.2 MiB/s) before the kill.
- The growth looks steady rather than a single spike, which points to a leak or unbounded cache.
- Before the kill (79% of limit): Largest application classes on the heap:
  com.example.checkout.CheckoutService$CartSnapshot: 2860 instances ...
- Before the kill (79% of limit): One class dominates the Java heap: byte[] holds 179.2 MiB (99%).
```

```
# order-consumer-76d5fcb94-8vm4p/consumer crashed at 03:09:30 UTC: Error, exit code 2

## What lastwords found
- Crashed in main.registerDiscount at /src/main.go:19 (assignment to entry in nil map).
```

## Use it

```sh
kubectl lastwords list                      # everything from the last 7 days
kubectl lastwords list -n shop --kind oomkill
kubectl lastwords show 20261102T030712Z-af14b5
kubectl lastwords get 20261102T030712Z-af14b5 jvm-class-histogram.txt
kubectl lastwords top                       # containers closest to their memory limit right now
```

Install the CLI (it is also a kubectl plugin):

```sh
# macOS / Linux
curl -sSL https://github.com/Arvo-AI/lastwords/releases/latest/download/lastwords_$(uname -s)_$(uname -m | sed s/aarch64/arm64/).tar.gz | tar xz
sudo mv lastwords /usr/local/bin/ && sudo ln -sf /usr/local/bin/lastwords /usr/local/bin/kubectl-lastwords
```

The CLI talks to the agents through the Kubernetes API server, so it needs nothing but your kubeconfig.

## Let your AI agent read it

lastwords ships an MCP server. Claude Code, Codex, Cursor and any MCP client can list and read captures:

```sh
claude mcp add lastwords -- kubectl-lastwords mcp
codex mcp add lastwords -- kubectl-lastwords mcp
```

Then ask: *"the checkout pods restarted overnight, why?"* Tools: `lastwords_list_captures`, `lastwords_get_capture`, `lastwords_read_file`, `lastwords_watchlist`. All read-only.

## Get told

```sh
helm upgrade lastwords oci://ghcr.io/arvo-ai/charts/lastwords -n lastwords --reuse-values \
  --set notifications.slackWebhookUrl=https://hooks.slack.com/services/... \
  --set notifications.webhookUrl=https://your-service/lastwords
```

Every capture also becomes a Kubernetes Event on the pod (`LastWordsOOMKill`, `LastWordsMemoryPressure`, `LastWordsCrash`, ...), so it shows up in `kubectl describe pod` and anything that watches events. Prometheus metrics are on `:9095/metrics`.

## Configuration

Common settings (see [values.yaml](deploy/helm/lastwords/values.yaml) for all):

| Value | Default | |
|---|---|---|
| `capture.threshold` | `0.90` | Capture at this share of the memory limit (working set, like the kubelet) |
| `capture.lastGasp` | `0.97` | Second capture right before the kill |
| `capture.predictive` | `true` | Capture early if the limit will be hit within `predictSeconds` |
| `capture.jvmGCHeadroom` | `0.25` | JVMs are usually killed by a GC at ~75-80% of the limit, so their tiers aim below it |
| `capture.heapDumps` | `false` | Full Java heap dumps for every JVM. Pods can opt in with the `lastwords.dev/heap-dump: "true"` annotation |
| `capture.goPprofPort` | `6060` | Where Go programs serve `net/http/pprof` |
| `coreDumps.enabled` | `false` | Capture core dumps (sets the node's `kernel.core_pattern`, needs a privileged agent) |
| `storage.maxBytes` | 5 GiB | Disk budget per node |
| `s3.bucket` | | Upload captures to S3, GCS (interoperability) or MinIO |
| `ignoreNamespaces` | `[kube-system]` | Never watch these |
| `api.createSecret` | `true` | Create the API token secret. Set `false` with Argo CD or Flux and create `lastwords-api` yourself ([why](docs/security.md#who-can-read-captures)) |

Pod annotations:

| Annotation | |
|---|---|
| `lastwords.dev/ignore: "true"` | Don't watch this pod |
| `lastwords.dev/threshold: "0.8"` | Per-pod capture threshold |
| `lastwords.dev/heap-dump: "true"` | Allow a full heap dump for this pod |
| `lastwords.dev/pprof-port: "8080"` | Go pprof port |

## How it works

A DaemonSet runs one small agent per node (a static Go binary; requests 20m CPU and 48 MiB). Measured on EKS (t3.large, Amazon Linux 2023): 8m CPU and 12 MiB on a quiet node, 20m CPU and 15 MiB with 35 containers, about 0.5m CPU per container watched.

1. It reads every container's memory accounting from the node's cgroup tree twice a second (`memory.current`, `memory.stat`, `memory.events`, PSI), computes the working set the way the kubelet does, and keeps a 10-minute timeline.
2. When a container crosses the threshold, or is climbing fast enough to hit its limit within 15 seconds, it collects evidence while the process is still alive. For the JVM it uses the HotSpot attach protocol (what `jcmd` uses) from outside the container; nothing is installed in your images. For Go it pulls `/debug/pprof/heap`.
3. It watches pods on its node. When a container terminates with an error, it copies the dead instance's log file before the next restart rotates it, and saves the pod's events.
4. With core dumps on, it points `kernel.core_pattern` at a small helper that writes the gzipped core next to the captures, and the agent matches it to the pod.
5. Captures stay on the node's disk under a size and age budget, optionally upload to S3, and are served to the CLI and MCP server through the API server's pod proxy.

More in [docs/how-it-works.md](docs/how-it-works.md).

## Security

lastwords reads other processes' memory accounting, so it needs node-level access. Read [docs/security.md](docs/security.md) before installing it in production. In short:

- Without core dumps the agent is **not privileged**. It runs with `hostPID` and the capabilities `SYS_PTRACE`, `DAC_READ_SEARCH`, `DAC_OVERRIDE`, `CHOWN`, `FOWNER`, `KILL`, plus a read-only root filesystem.
- Core dumps need `privileged` to set `kernel.core_pattern`. They are off by default.
- **Full heap dumps are off by default** because they contain whatever was in memory. The default class histogram contains class names and counts, not data.
- Command lines are redacted for arguments that look like secrets.
- Reading captures needs RBAC on `pods/proxy` in the lastwords namespace plus the API token secret. The chart creates a `lastwords-reader` Role.

## FAQ

**My pod was OOMKilled. How do I find out what used the memory?**
If lastwords was running, `kubectl lastwords list --kind oomkill` shows the kill, and the linked `pressure` capture shows what was using memory before it. If it wasn't running, nothing can recover that information. Install it so the next kill is captured.

**Why didn't `HeapDumpOnOutOfMemoryError` produce a heap dump?**
That flag fires when the JVM itself runs out of heap. A container OOM kill is the kernel sending SIGKILL when the whole process (heap plus metaspace, threads, direct buffers, native memory) hits the cgroup limit. The JVM never gets a chance to run. lastwords takes the histogram before that point.

**How do I get core dumps from Kubernetes pods?**
`helm upgrade lastwords ... --set coreDumps.enabled=true`. Cores are stored per capture as `core.gz`. Debug them with `gdb` and the same binary from the image.

**`kubectl logs --previous` shows nothing useful. Where are the logs of the first crash?**
The kubelet only keeps the last terminated instance. lastwords copies each dead instance's log at the moment it dies (the first 3 crashes of a crash loop per 30 minutes, to bound disk use).

**Does it replace Prometheus, Datadog or continuous profiling?**
No. Continuous profilers show where memory is allocated over time. lastwords takes a snapshot right before the kill without changes to your app, and keeps the logs and events that would otherwise expire.

**Does it work on EKS, GKE, AKS, OpenShift?**
It works on any Kubernetes 1.25+ node with cgroup v2 (cgroup v1 is supported for memory too) and containerd or CRI-O. Core dumps need a node path the kernel can execute the helper from; see [docs/platforms.md](docs/platforms.md) for per-platform notes.

## Aurora

lastwords is built by the team behind [Aurora](https://www.aurorasre.ai), an open-source AI SRE that investigates incidents. Point `notifications.webhookUrl` at Aurora, or give Aurora the MCP server, and every OOM kill and crash arrives with its evidence attached.

## License

Apache 2.0
