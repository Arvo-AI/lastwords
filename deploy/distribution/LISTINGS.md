# Where lastwords is listed

| Where | How | Status |
|---|---|---|
| krew index | PR adding `plugins/lastwords.yaml` (from `krew-lastwords.yaml`) to kubernetes-sigs/krew-index | draft |
| Artifact Hub | Repository `lastwords` (oci://ghcr.io/arvo-ai/charts/lastwords) | listed 2026-11-03 |
| MCP registry | `mcp-publisher login github` then `mcp-publisher publish` with `server.json` (needs an image built with the `io.modelcontextprotocol.server.name` label, v0.1.3+) | to do |
| awesome-kubernetes | One line under Monitoring / Debugging | draft |
| awesome-mcp-servers | One line under Cloud Platforms / Monitoring | draft |

Line for awesome lists (disclose that we build it in the PR body):

    - [lastwords](https://github.com/Arvo-AI/lastwords) - Captures Java heap histograms, Go heap profiles, crash logs and core dumps from Kubernetes pods before they are OOMKilled or crash; MCP server included.
