# virt-disk-operator

A Kubernetes operator for creating virtual disk devices. This is primarily useful for testing distributed storage systems.

## Features

* Based on Linux NBD (Network Block Device).
* Thin provisioning (eg. virtual disks only take up as much space as they need).
* LVM logical volume support.
* Can be run locally (eg. KinD) including on Docker Desktop for Mac (using [containerized udev](tests/config/daemonset-udevd.yaml)).

## Getting Started

### Prerequisites

* [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/)
* [kapp](https://carvel.dev/kapp/)

### Installing

#### Dependencies

```shell
PROMETHEUS_VERSION="v0.68.0"

kapp deploy -y -a prometheus-crds -f "https://github.com/prometheus-operator/prometheus-operator/releases/download/${PROMETHEUS_VERSION}/stripped-down-crds.yaml"
```

#### Operator

```shell
kapp deploy -a virt-disk-operator -f https://github.com/gpu-ninja/virt-disk-operator/releases/latest/download/virt-disk-operator.yaml
```

### Custom Resources

#### Create a Virtual Disk

```shell
kubectl apply -f examples
```