# Platform notes

| Platform | Memory captures | Crash and OOM captures | Core dumps |
|---|---|---|---|
| EKS (AL2023, Bottlerocket) | Yes | Yes | Yes. lastwords replaces any existing core_pattern (such as systemd-coredump) while it runs and restores it on exit |
| GKE Standard | Yes | Yes | Yes. If the helper cannot run from `/var/lib/lastwords` on your node image, set `storage.hostPath` to a path where the kernel can execute files |
| GKE Autopilot | No: Autopilot only allows hostPID and privileged DaemonSets for allowlisted partner workloads | | |
| AKS | Yes | Yes | Yes |
| OpenShift | Yes, with a SecurityContextConstraints that allows hostPID, hostPath and the capabilities listed in [security.md](security.md) | Yes | Yes, with a privileged SCC |
| k3s, kind, minikube | Yes | Yes | Depends on the host kernel (see below) |

## Requirements

- Kubernetes 1.25+
- containerd or CRI-O (the cgroup directory must contain the container ID)
- cgroup v2, or cgroup v1 with the memory controller
- PSI metrics (`memory.pressure`) are used when the kernel has them. Kubernetes 1.36 made them GA.

## Nested and VM-based local clusters

`core_pattern` must point at a path that exists in the root mount namespace of the kernel that runs the containers. With kind, that is the Docker host, not the kind node. The e2e setup in `test/e2e/kind.yaml` shares one directory between both and sets `coreDumps.hostDir`.

Some recent local VM kernels namespace `core_pattern` per container. There, a pattern set by the agent only applies to the agent's own container, so real core dumps from other pods are not captured. Memory, OOM and crash captures are unaffected. The e2e test pushes a synthetic core through the helper to cover the rest of the path.
