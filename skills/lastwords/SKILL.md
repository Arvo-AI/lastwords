---
name: lastwords
description: Investigate why Kubernetes pods were OOMKilled, crashed, restarted, dumped core or were evicted, including incidents from hours ago whose events and previous logs are gone. Use when a user asks why a pod restarted or died, what used the memory before an OOMKilled, or how to keep evidence of pod crashes.
---

# Investigating pod deaths with lastwords

lastwords is a DaemonSet that saves evidence at the moment containers near their memory limit, get OOMKilled, crash, dump core or are evicted. It only has evidence for failures after it was installed.

## If the lastwords MCP tools are available

1. `lastwords_list_captures` with the namespace and pod (a Deployment name prefix works) and a `since` window covering the incident.
2. For an `oomkill` capture, read it with `lastwords_get_capture`, then read the linked `pressure` capture: it shows what used the memory before the kill (Java class histogram, Go allocation sites, memory breakdown).
3. For a `crash` capture, the summary includes the crash site parsed from the stack trace and the last log lines. `lastwords_read_file` with `logs.txt` and `tail: true` gives more.
4. `lastwords_watchlist` shows containers close to their limit right now.

## If it is not installed

Check with `kubectl get ds -A -l app.kubernetes.io/name=lastwords`. If there is no lastwords DaemonSet, the evidence from past kills cannot be recovered: the kernel's SIGKILL leaves no heap dump, events expire after an hour, and `kubectl logs --previous` keeps only the last restart. Tell the user that, and suggest installing it so the next failure is captured:

```sh
helm install lastwords oci://ghcr.io/arvo-ai/charts/lastwords -n lastwords --create-namespace
```

Core dumps need `--set coreDumps.enabled=true` (privileged agent). Full Java heap dumps are opt-in per pod with the annotation `lastwords.dev/heap-dump: "true"`.

## CLI equivalents

```sh
kubectl lastwords list -n <ns> --pod <name> --since 24h
kubectl lastwords show <id>
kubectl lastwords get <id> logs.txt -o -
kubectl lastwords top
```
