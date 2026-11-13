# Onehouse-spark-operator

Fork of kubeflow/spark-operator, forked on Aug-4-2021.

# Changes
- The Dockerfile extends a Spark/Hadoop base image (`onehouse/spark-3.5.3-base`) instead of the plain Spark image.

# Publishing the image

The data plane runs `docker.io/onehouse/onehouse-spark-operator:<tag>`. Publish it with the
**Publish Onehouse operator image** workflow (`.github/workflows/publish-onehouse-image.yaml`):

```
gh workflow run publish-onehouse-image.yaml -R onehouseinc/spark-on-k8s-operator \
  -f tag=v2.1.0.<n>-dev -f ref=master
```

- It builds `linux/amd64` and `linux/arm64` and records the commit in the image labels and the run summary.
- It refuses to push a tag that already exists. The data plane pulls with `pullPolicy: Always`, so a
  re-pushed tag would silently change every org's operator. Always use the next `v2.1.0.<n>-dev`.
- `release.yaml` publishes the upstream `kubeflow/spark-operator` image and is not used for this.

Then bump `image.tag` in infra's `terraform/data-plane/spark-resources/spark-operator-values.yaml`.
The tag reaches an org with the infra module release that org is pinned to.
