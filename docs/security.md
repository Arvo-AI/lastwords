# Security model

lastwords needs node-level visibility to do its job. This page lists exactly what it can access and why.

## Privileges

| Mode | Pod settings |
|---|---|
| Default | `hostPID: true`, runs as root with `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, all capabilities dropped except `SYS_PTRACE`, `DAC_READ_SEARCH`, `DAC_OVERRIDE`, `CHOWN`, `FOWNER`, `KILL` |
| `coreDumps.enabled=true` | `privileged: true`, needed to write `kernel.core_pattern` |

Why each capability is needed:

- `SYS_PTRACE`: read `/proc/<pid>/{smaps_rollup,maps,exe,root}` of other containers' processes
- `DAC_READ_SEARCH`, `DAC_OVERRIDE`: read files and enter directories owned by other users, and create the JVM attach trigger in the target's `/tmp`
- `CHOWN`, `FOWNER`: hand the attach trigger file to the JVM's user, as the JVM requires
- `KILL`: send SIGQUIT to a verified JVM to start its attach listener

## Workloads are untrusted

Every container on the node can try to trick a root agent, so nothing the agent reads from a container is trusted:

- **JVM attach.** The agent opens the target's `/tmp` once through `/proc/<pid>/root` and does everything else relative to that directory without following symlinks: creating the trigger file, checking for the socket, reading a heap dump. It signals the process through a pidfd, so a reused PID never gets the signal, and it checks with `SO_PEERCRED` that the attach socket is served by the JVM itself before sending a command. Replies are capped at 16 MiB, heap dumps at `capture.heapDumpMaxBytes`, and every step stops at the capture timeout.
- **Go pprof.** Profiles are capped at 32 MiB and at 64 MiB once decompressed.
- **Core dumps.** The helper refuses to write a core when 4 are already waiting, when the capture filesystem would have less than 10% or less than twice `coreDumps.maxBytes` free, or when the same pod dumped 3 cores in the last 30 minutes. This also holds if the agent is gone and `kernel.core_pattern` still points at the helper.
- **Crash loops.** For each container, at most 3 captures of each kind are taken in 30 minutes; later ones only increase `lastwords_crashes_suppressed_total`.

Host mounts:

- `/sys/fs/cgroup` (read-only)
- `/var/log/pods` (read-only)
- `storage.hostPath` (read-write, default `/var/lib/lastwords`)

## What ends up in captures

| Evidence | Contains | Default |
|---|---|---|
| Memory accounting, timelines | Numbers | On |
| Process list | Command lines, with values of arguments named like password/secret/token/key redacted | On |
| JVM class histogram, heap info, native memory | Class names and counts | On |
| JVM thread dump | Stack traces, thread names | On (`capture.jvmThreadDump`) |
| Go heap profile, goroutines | Function names and stacks | On when the app serves pprof |
| Container logs (last 500 lines) | Whatever your app logged | On (`capture.logs`) |
| Pod events | Kubernetes event messages | On |
| **Full heap dumps** | **All of the JVM's memory: credentials, tokens, personal data** | **Off.** Per pod with `lastwords.dev/heap-dump: "true"` or cluster-wide with `capture.heapDumps` |
| **Core dumps** | **All of the process's memory** | **Off** (`coreDumps.enabled`) |

Treat the capture directory and any S3 bucket you upload to with the same care as the workloads themselves.

## Who can read captures

The agent API requires the token stored in the `lastwords-api` secret, sent as `X-Lastwords-Token`. An agent started without a token refuses every API request. The CLI and MCP server read the token from the secret and reach the agents through the API server's pod proxy. To read captures, a user therefore needs, in the lastwords namespace:

- `list` on `pods`
- `get` on `pods/proxy`
- `get` on the `lastwords-api` secret

The chart creates the `lastwords-reader` Role with exactly these permissions. With Argo CD or Flux, set `api.createSecret=false` and create the secret yourself: those tools cannot see the existing secret during rendering and would rotate the token on every sync.

Logs, thread dumps and findings come from your workloads. When you hand them to an LLM through the MCP server, treat them as untrusted input, like any log line. Webhook and Slack notifications include the headline and findings, which can quote log lines. `/metrics`, `/healthz` and `/readyz` need no token and expose only counts.

## Reporting vulnerabilities

Report privately through GitHub (Security tab, "Report a vulnerability") rather than opening a public issue.
