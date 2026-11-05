# Agent eval, 2026-11-03

Do coding agents find the root cause of a crash-looping service faster with lastwords? This folder holds the setup, grader, final answers and transcripts of the run. The write-up is [AI Agents Debugging OOMKilled Pods: 4/12 vs 12/12](https://www.aurorasre.ai/blog/ai-agents-oomkilled-root-cause-eval).

- **Setup:** EKS 1.33 in us-east-2, 2 t3.large nodes, lastwords v0.1.1. The namespace `storefront` runs two leaking services, Java `checkout` and Go `billing-api`, plus healthy `web` and `cart-redis` ([storefront.yaml](storefront.yaml), [workloads/](workloads)).
- **Agents:** Claude Code headless, Opus and Sonnet, Bash only, a read-only `oncall` kubeconfig with exec ([rbac.yaml](rbac.yaml)). The prompts are in [prompts.json](prompts.json).
- **Arms:** A is kubectl only. B adds the plugin on PATH and one casual line: "(Someone on the team installed lastwords on the cluster a while back, `kubectl lastwords --help`, not sure if it's useful here.)"
- **Runs:** 2 agents × 2 services × 2 arms × 3 repetitions = 24. Every run is here, none were dropped.

| | kubectl only | + lastwords |
|---|---|---|
| Opus, checkout | 3/3 | 3/3 |
| Opus, billing | 1/3 | 3/3 |
| Sonnet, checkout | 0/3 | 3/3 |
| Sonnet, billing | 0/3 | 3/3 |
| **Total** | **4/12** | **12/12** |

`python3 grade.py` regrades `runs/` and rewrites `grades.json`.

## Read this before quoting it

- We built the tool and wrote both failures, and n=3 per cell.
- Arm B got a hint, so the result covers "told it exists", not "found it unaided".
- The grader is narrow. A pass means the answer names the code-level cause and says it is unbounded, and the "unbounded" check matched all 24 answers. In practice a pass is naming a symbol that lastwords prints. Billing passes depend on the fixture serving pprof.
- The checkout image in arm A contained `.class` files that an agent could copy out and disassemble, which is how Opus got 3/3.
- The billing image was copied from a demo image by digest, so its `imageID` shows the original name `lastwords-demo/leaky-go`. 10 of the 12 billing answers mention it.
- `used_lw` in arm A means the agent tried lastwords after seeing its pod Events. 10 of 12 did. All 10 failed because the plugin was not on PATH; three Opus runs then ran the binary in the agent pod and could not read the API token.
- Codex was not run.

The cluster no longer exists. The account ID, local paths and node names are replaced with placeholders; lines other than messages and results are removed. To rerun, build `workloads/`, push them, set the images in `storefront.yaml`, and install lastwords.
