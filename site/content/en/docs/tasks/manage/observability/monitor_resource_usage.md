---
title: "Monitor real resource usage"
date: 2026-02-24
weight: 3
description: >
  Monitor real resource usage with aggregation or filtering by specific queue.
---

This page shows you how to use Kueue labels assigned to pods to monitor resource
usage.

The intended audience for this page are [batch administrators](/docs/tasks#batch-administrator).

## Before you begin

Make sure the following conditions are met:

- A Kubernetes cluster is running.
- The kubectl command-line tool has communication with your cluster.
- [Kueue is installed](/docs/installation).
- The cluster has at least one [local queue and cluster queue](/docs/tasks/manage/administer_cluster_quotas).

{{% alert title="Note" color="primary" %}}
The queries in this page require `AssignQueueLabelsForPods` Feature Gate, which is enabled by default.
If it is not enabled, see [Installation](/docs/installation/#change-the-feature-gates-configuration) for details how to enable it.
{{% /alert %}}

## `kubectl top` for command line resource usage debugging

{{% alert title="Warning" color="warning" %}}
As mentioned on  [Metrics Server repository site](https://github.com/kubernetes-sigs/metrics-server?tab=readme-ov-file#kubernetes-metrics-server),
Metrics Server and provided command line tool - `kubectl top` - is not meant as a monitoring solution. The tool is convenient
for local debugging but it is not a replacement for real monitoring. For the setup more prepared for production see the
[Production resource monitoring](#production-resource-monitoring) section.

{{% /alert %}}

1. To use `kubectl top` you need to install metrics-server for you cluster.
Follow the [Metrics Server Installation](https://github.com/kubernetes-sigs/metrics-server?tab=readme-ov-file#installation)
2. Schedule a couple of jobs that have some real cpu usage to your local queue:
```yaml
apiVersion: batch/v1
kind: Job
metadata:
  generateName: sample-job-
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: "user-queue"
spec:
  parallelism: 1
  completions: 1
  template:
    spec:
      containers:
      - name: dummy-job
        image: registry.k8s.io/e2e-test-images/agnhost:2.53
        command: [ "/bin/sh" ]
        args: [ "-c", "end_time=$(($(date +%s) + 60)); while [ $(date +%s) -lt $end_time ]; do :; done"]
        resources:
          requests:
            cpu: "1"

      restartPolicy: Never
```
3. Monitor the usage of cpu and memory in nearly real time for this local queue with `kubectl top`:
```sh
watch -n 15  'kubectl top pod --sum -l kueue.x-k8s.io/local-queue-name=user-queue'
```
You will see a list of pods currently running on the local queue named "user-queue" with their current cpu and memory measurements and the sum of the usage at the bottom. The list will be automatically refreshed every 15 seconds.

4. Similarly, you can monitor the usage of the resources by jobs admitted to a cluster queue:
```sh
watch -n 15  'kubectl top pod --sum -l kueue.x-k8s.io/cluster-queue-name=cluster-queue'
```

{{% alert title="Note" color="primary" %}}
If you encounter a "error: Metrics API not available" error it may be caused by certificate
verification problems in your cluster. You can skip the verification in metrics-server by
editing the deployment deployment `kubectl edit deployment metrics-server -n kube-system`
and adding `--kubelet-insecure-tls` to the container arguments, however this
is **highly discouraged in production environments**.
{{% /alert %}}


## Production resource monitoring

1. Install prometheus and kube-state-metrics in a way that matches your setup.
Adjust the kube-state-metrics configuration to allowlist Kueue pod labels.

Assuming you are using the kube-prometheus-stack helm chart, add the following fragment to your `values.yaml`:
```yaml
kube-state-metrics:
  metricLabelsAllowlist:
    - pods=[kueue.x-k8s.io/cluster-queue-name, kueue.x-k8s.io/local-queue-name]
```

Deploy Prometheus stack helm chart with values.yaml:
```sh
helm install kube-prometheus-stack oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack -f values.yaml
```
2. If you do not have the Prometheus UI available online - forward port for prometheus service:
```sh
kubectl port-forward svc/kube-prometheus-stack-prometheus 9090:9090
```
3. Deploy some jobs, for example one defined above.

4. Verify that new labels are available in the PromQL queries in [UI](http://localhost:9090/):
```promql
kube_pod_labels{label_kueue_x_k8s_io_local_queue_name!=""}
```

5. Now you can attach queue labels to the existing pod resource usage metrics with `group_left`.

Use them to aggregate the metrics, for example:
```
sum by (label_kueue_x_k8s_io_local_queue_name) (
  sum by (namespace, pod) (
    rate(container_cpu_usage_seconds_total{container!="", pod!=""}[5m])
  )
  * on(namespace, pod) group_left(label_kueue_x_k8s_io_local_queue_name)
  kube_pod_labels{label_kueue_x_k8s_io_local_queue_name!=""}
)
```

Use `label_kueue_x_k8s_io_cluster_queue_name` to filter and aggregate by cluster queue.
