# virt-disk-operator

A Kubernetes operator for creating virtual disk devices. This is primarily useful for testing distributed storage systems.

## Getting Started

### Prerequisites

* [kapp](https://carvel.dev/kapp/)

### Installing

```shell
kapp deploy -a virt-disk-operator -f https://github.com/gpu-ninja/virt-disk-operator/releases/latest/download/virt-disk-operator.yaml
```

### Creating a Virtual Disk

```shell
kubectl apply -f examples
```

## Credits

* [go-nbd](https://github.com/pojntfx/go-nbd)