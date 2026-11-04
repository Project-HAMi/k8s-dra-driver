# E2E tests

Black-box [BATS](https://github.com/bats-core/bats-core) tests of the driver,
laid out like the upstream
[NVIDIA DRA driver](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/main/tests/bats)
suite. They run against the cluster of the current kubectl context and install
the driver from the local chart. Checks that compare against the node's GPUs run
Mokka's `nvidia-smi` in the kind node, so the suite expects the kind cluster
that the script below creates.

**Warning:** the suite changes that cluster: it installs or upgrades the
driver, and creates and deletes workloads in the `default` namespace.

## GPU-less run with Mokka

[hack/ci/mock-nvml/e2e-test.sh](../../hack/ci/mock-nvml/e2e-test.sh) runs the
suite without a GPU, which is also what CI runs. It builds the driver image
from this tree, creates the kind cluster `hami-e2e`, installs
[Mokka](https://github.com/NVIDIA/k8s-test-infra) to mock NVML, the driver root
and the device nodes, runs every `tests/bats/*.bats` file, and deletes the
cluster. It stops if that cluster already exists, unless
`RECREATE_CLUSTER=true`. It needs docker, git, kind, kubectl, helm and jq,
plus make and go to build the image, and fetches pinned versions of bats
itself. The header of the script lists its settings.

```bash
hack/ci/mock-nvml/e2e-test.sh
```

When it fails, the cluster state and kind logs are written to
`/tmp/hami-e2e-artifacts`, and CI uploads them as the `mock-nvml-e2e-logs`
artifact.

To work on a test, keep the cluster and rerun just that test:

```bash
SKIP_CLEANUP=true hack/ci/mock-nvml/e2e-test.sh

export PATH=/tmp/hami-e2e-bats-v1.14.0/core/bin:$PATH \
    BATS_LIB_PATH=/tmp/hami-e2e-bats-v1.14.0/lib \
    KIND_CLUSTER_NAME=hami-e2e \
    TEST_IMAGE=projecthami/k8s-dra-driver:e2e \
    TEST_NVIDIA_DRIVER_ROOT=/var/lib/nvml-mock/driver
bats tests/bats/test_hami_basic.bats --filter "1 pod"
```

## Writing tests

- `setup_file` installs the driver with
  `iupgrade_wait CHART VERSION [HELM_ARGS...]`. Pass `"${TEST_CHART_REPO}"
  "${TEST_CHART_VERSION}"` for the chart under test; only that chart gets
  `TEST_IMAGE`, so a published release keeps its own image.
- Workloads live in `specs/` and carry the label `env: batssuite`; `teardown`
  deletes them, also after a failure, so every test starts clean.
- `bats::on_failure` prints the workloads and the kubelet plugin logs.
- Tag tests with `# bats test_tags=...` so a run can select them with
  `--filter-tags`.

| Test | Checks |
|---|---|
| one device per GPU | Each mock GPU is published once as `hami-gpu-<minor>`, with its UUID, PCI bus ID, 100 cores and its full memory |
| 1 pod, 1 GPU | A pod claiming 30 cores and 4Gi gets its GPU and the limit env vars, and `nvidia-smi` in it shows only that GPU with 4096 MiB |

## Not covered by Mokka

Mokka does not execute CUDA, so these stay in the real-GPU test plan:

- CUDA-level enforcement of the SM and memory limits
- Node reboot
- MIG, MPS, time-slicing and passthrough, which are disabled in HAMi mode
