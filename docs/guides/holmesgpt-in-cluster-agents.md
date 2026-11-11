# Give HolmesGPT and other in-cluster agents lastwords

An agent that runs inside the cluster can't start `kubectl lastwords mcp` as a
local process. Turn on the chart's MCP server instead. It serves the same four
read-only tools over streamable HTTP:

```sh
helm upgrade lastwords oci://ghcr.io/arvo-ai/charts/lastwords -n lastwords --reuse-values \
  --set mcp.enabled=true
```

The endpoint is `http://lastwords-mcp.lastwords.svc:8090/mcp`. Every request needs
`Authorization: Bearer <token>`. The token is the one in the `lastwords-api`
Secret:

```sh
kubectl -n lastwords get secret lastwords-api -o jsonpath='{.data.token}' | base64 -d
```

The MCP pod runs as non-root with no capabilities. It can only list the agent
pods, use their pod proxy and read that one Secret.

Verified 2026-11-11 on kind (Kubernetes 1.37) with HolmesGPT 0.43.0 and Claude
Sonnet 5.5 on Bedrock, using the [demo workloads](../../examples/demo.yaml).

## HolmesGPT

Add lastwords as an MCP server in the toolsets file (`-t` on the CLI, or
`mcp_servers` in the Helm values when Holmes runs in the cluster):

```yaml
mcp_servers:
  lastwords:
    description: "lastwords: memory snapshots taken before OOM kills, logs and core dumps from crashed Kubernetes containers"
    config:
      url: "http://lastwords-mcp.lastwords.svc:8090/mcp"
      mode: streamable-http
      headers:
        Authorization: "Bearer {{ env.LASTWORDS_TOKEN }}"
    llm_instructions: "Use lastwords whenever a pod was OOMKilled, crashed or restarted. It keeps evidence that kubectl no longer has: a Java class histogram or Go heap profile taken before the kill, and the logs of the instance that died. Start with lastwords_list_captures for the namespace or pod, then lastwords_get_capture."
```

Put the token in `LASTWORDS_TOKEN`. For the Holmes Helm chart, load it from a
Secret through `additionalEnvVars`.

What we asked:

```sh
holmes ask "The checkout pod in the shop namespace keeps getting OOMKilled. What is using the memory, and which code is responsible?" -t lastwords.yaml
```

Holmes made 12 lastwords calls (2 list, 4 get, 6 read_file) and answered:

```
• Likely culprit: com.example.checkout.CheckoutService. Each checkout adds a
  CartSnapshot to the one SessionCache (probably a ConcurrentHashMap, since
  ConcurrentHashMap$Node entries are present). Nothing removes them.
...
• The container has JAVA_TOOL_OPTIONS=-Xmx1g, but the memory limit is 384 MiB.
  The JVM allows a heap about 2.7 times the limit, so the kernel kills the pod
  before the JVM would throw its own OutOfMemoryError.
```

Both are the bugs planted in the demo. That pod had already restarted several
times, so `kubectl logs --previous` only held the last instance's output, with
no allocation data.

## Any other MCP client

Anything that speaks streamable HTTP with a custom header works the same way,
for example Claude Code against a port-forward:

```sh
kubectl -n lastwords port-forward svc/lastwords-mcp 8090:8090 &
claude mcp add --transport http lastwords http://localhost:8090/mcp \
  --header "Authorization: Bearer $(kubectl -n lastwords get secret lastwords-api -o jsonpath='{.data.token}' | base64 -d)"
```
