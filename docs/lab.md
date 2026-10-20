# Lab

Local, reproducible environment used to inject faults and measure
KubePilot. Status: increment 1 of P1 (cluster + observability stack). The
model server, GenAI app and fault injection come in later increments.

## Usage

```sh
make lab-up      # create cluster + install Tempo, Prometheus, OTel Collector
make lab-check   # end-to-end check, non-zero exit on any failure
make lab-stats   # memory/CPU of the kind node containers
make lab-down    # delete the cluster
make lint        # shellcheck the lab scripts
```

The lab uses its own kubeconfig (`.tools/kubeconfig`) and its own pinned
`kind`/`kubectl` (`.tools/bin`); it does not modify `~/.kube/config`.

```sh
export KUBECONFIG=$PWD/.tools/kubeconfig
```

## What runs where

| Component | Namespace | Role |
|---|---|---|
| kind (1 control-plane, 2 workers) | | Kubernetes 1.37 |
| OpenTelemetry Collector | `observability` | OTLP entry point; adds `k8s.*` attributes (`k8sattributes`); traces → Tempo, metrics → Prometheus OTLP receiver |
| Tempo (single binary) | `observability` | Trace storage and search (24 h retention) |
| Prometheus + kube-state-metrics | `observability` | cAdvisor (CPU throttling, memory), object state (phase, readiness, restarts, termination reasons), OTLP metrics |

All versions and digests are pinned in [`lab/versions.env`](../lab/versions.env)
and checked by `lab/scripts/verify-pins.sh` before anything is created.

## What `make lab-check` proves, and what it does not

- The trace path is proven end to end: a synthetic trace sent to the collector
  is found in Tempo with `k8s.pod.name` equal to the sending pod.
- The OTLP metric path is proven: a synthetic metric reaches Prometheus with
  a `k8s_pod_name` label.
- **CPU throttling check: limited.** It only proves that cAdvisor exports
  `container_cpu_cfs_throttled_periods_total` (the series exists because the
  collector has a CPU limit). It does **not** prove that throttling happens,
  nor that the series belongs to a running container: during the negative
  test the series of the previous collector pod was still returned. Actual
  throttling is verified by the `cpu-throttle` fault in a later increment.

### Negative test

To check that lab-check can fail, scale the collector to zero and run it:

```sh
export KUBECONFIG=$PWD/.tools/kubeconfig
.tools/bin/kubectl -n observability scale deploy/otel-collector --replicas=0
make lab-check   # must fail on the trace and OTLP metric checks
.tools/bin/kubectl -n observability scale deploy/otel-collector --replicas=1
make lab-check   # must pass again
```

**Do not run `make lab-up` during a negative test.** `lab-up` runs
`helm upgrade`, which resets the collector to its declared replica count (1)
and silently invalidates the test. Check `replicas=0` before trusting a
failing run.

## Kubernetes event retention

Kubernetes Events are kept by the API server for `--event-ttl`, **1 hour by
default upstream**. The lab keeps that default so it behaves like a real
cluster. It is configurable at cluster creation:

```sh
make lab-down && make lab-up EVENT_TTL=24h
```

(see [`lab/kind/cluster.yaml`](../lab/kind/cluster.yaml)).

Consequence for the benchmark: an evaluation run cannot rely on events still
being in the API server when it is scored later. **The benchmark will export
the Kubernetes Events of the relevant namespaces at the end of each run** and
store them with the run's ground truth in `eval/`, so every run can be
re-scored offline. This also reflects a real limitation KubePilot has on live
clusters: evidence older than the event TTL is gone unless an exporter kept
it.
