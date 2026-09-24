---
title: Tracing Slow Requests
---

Ceph can send an [OpenTelemetry](https://opentelemetry.io/) trace for every slow request. A trace shows
where the time went: in RGW, on the primary OSD, or waiting for a replica, and in which phase of the op.
Requests faster than the thresholds are not traced, so they cost next to nothing.

The OSDs and RGWs send the spans themselves, over OTLP/HTTP, to any backend that accepts OTLP, such as
[Jaeger](https://www.jaegertracing.io/), [Grafana Tempo](https://grafana.com/oss/tempo/) or an
[OpenTelemetry Collector](https://opentelemetry.io/docs/collector/). Nothing is injected into the pods and
no agent runs next to them.

!!! note
    This requires a Ceph version with slow-request tracing. With an older version, Rook logs a warning
    and leaves the tracing options unset.

## Start a backend

To try it out, start a single Jaeger that keeps the traces in memory:

```console
kubectl create -f deploy/examples/monitoring/jaeger.yaml
```

## Turn on the traces

Point the cluster at the backend's OTLP/HTTP endpoint:

```yaml
spec:
  monitoring:
    tracing:
      enabled: true
      endpoint: http://rook-ceph-jaeger.rook-ceph.svc:4318/v1/traces
      osdSlowOpThreshold: 1s
```

Rook sets the Ceph options in the mon config store, and the daemons pick them up without restarting.
The settings are described in the [CephCluster CRD](../../CRDs/Cluster/ceph-cluster-crd.md#cluster-settings).
Options set in `cephConfig` take precedence over the ones Rook sets for tracing.
RGW requests are traced above the same threshold as OSD ops unless `rgwSlowRequestThreshold` is set.

When the cluster serves only object storage, set `onlyClientRequests: true`. The OSDs then trace only the
ops of requests that RGW traced, and leave out RGW's own background work, such as garbage collection and
the watches on its control pool.

Setting `enabled: false` turns the traces off. Removing the `tracing` section leaves the options as they are.

## Find a slow request

Open the Jaeger UI:

```console
kubectl -n rook-ceph port-forward service/rook-ceph-jaeger 16686
```

In [http://localhost:16686](http://localhost:16686), choose the `rgw` service, an operation such as
`put_obj`, and a minimum duration. Each trace shows the RGW request, the primary OSD's op under it and
the replica ops under that, each split into the phases the OSD recorded, such as queued for the PG or
waiting for the replicas to commit.

An op that is still stuck when the OSD reports it as a slow request is traced then, with the phase it is
still in; `ceph tell osd.1 dump_ops_in_flight` shows its `trace_id`. A traced op also keeps its trace id
in the OSD's op history, so a slow op seen from the toolbox can be looked up in Jaeger with
**Lookup by Trace ID**:

```console
ceph tell osd.1 dump_historic_ops_by_duration
```

The `slow_op_traces` and `slow_op_traces_dropped` counters in `ceph tell osd.1 perf dump trackedop` show
how many traces an OSD exported and how many it left out because of `maxTracesPerSecond`. RGW counts its
own in `slow_request_traces` and `slow_request_traces_dropped`. When there are more slow requests than
that, RGW and the OSDs choose the ones to trace by trace ID, so they keep the same requests and a trace
arrives whole: with the request, and with the ops of it that were slow.
