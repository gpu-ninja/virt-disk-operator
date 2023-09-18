VERSION 0.7
FROM golang:1.21-bookworm
WORKDIR /app

docker-all:
  BUILD --platform=linux/amd64 --platform=linux/arm64 +docker

docker:
  ARG TARGETARCH
  FROM debian:bookworm-slim
  RUN apt update \
    && apt install -y udev lvm2 kmod
  COPY LICENSE /usr/local/share/virt-disk-operator/
  COPY (+virt-disk-operator/virt-disk-operator --GOARCH=${TARGETARCH}) /manager
  ENTRYPOINT ["/manager"]
  ARG VERSION
  SAVE IMAGE --push ghcr.io/gpu-ninja/virt-disk-operator:${VERSION}
  SAVE IMAGE --push ghcr.io/gpu-ninja/virt-disk-operator:latest

bundle:
  FROM +tools
  COPY config ./config
  COPY hack ./hack
  ARG VERSION
  RUN ytt --data-value version=${VERSION} -f config -f hack/set-version.yaml | kbld -f - > virt-disk-operator.yaml
  SAVE ARTIFACT ./virt-disk-operator.yaml AS LOCAL dist/virt-disk-operator.yaml

virt-disk-operator:
  ARG GOOS=linux
  ARG GOARCH=amd64
  COPY go.mod go.sum ./
  RUN go mod download
  COPY . .
  RUN go build -ldflags '-s' -o virt-disk-operator cmd/virt-disk-operator/main.go
  SAVE ARTIFACT ./virt-disk-operator AS LOCAL dist/virt-disk-operator-${GOOS}-${GOARCH}

generate:
  FROM +tools
  COPY . .
  RUN controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."
  RUN controller-gen crd:generateEmbeddedObjectMeta=true rbac:roleName=virt-disk-manager-role webhook paths="./..." output:crd:artifacts:config=config/crd/bases
  SAVE ARTIFACT ./api/v1alpha1/zz_generated.deepcopy.go AS LOCAL api/v1alpha1/zz_generated.deepcopy.go
  SAVE ARTIFACT ./config/crd/bases AS LOCAL config/crd/bases
  SAVE ARTIFACT ./config/rbac/role.yaml AS LOCAL config/rbac/role.yaml

tidy:
  LOCALLY
  RUN go mod tidy
  RUN go fmt ./...

lint:
  FROM golangci/golangci-lint:v1.54.2
  WORKDIR /app
  COPY . ./
  RUN golangci-lint run --timeout 5m ./...

test:
  COPY go.mod go.sum ./
  RUN go mod download
  COPY . .
  RUN go test -coverprofile=coverage.out -v ./...
  SAVE ARTIFACT ./coverage.out AS LOCAL coverage.out

integration-test:
  FROM +tools
  COPY . .
  WITH DOCKER --allow-privileged --load ghcr.io/gpu-ninja/virt-disk-operator:latest-dev=(+docker --VERSION=latest-dev)
    RUN SKIP_BUILD=1 ./tests/integration.sh
  END

tools:
  ARG TARGETARCH
  RUN apt update && apt install -y git ca-certificates curl libdigest-sha-perl rhash jq
  RUN curl -fsSL https://get.docker.com | bash
  RUN curl -fsSL https://carvel.dev/install.sh | bash
  ARG K3D_VERSION=v5.6.0
  RUN curl -fsSL -o /usr/local/bin/k3d "https://github.com/k3d-io/k3d/releases/download/${K3D_VERSION}/k3d-linux-${TARGETARCH}" \
    && chmod +x /usr/local/bin/k3d
  ARG KUBECTL_VERSION=v1.28.2
  RUN curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
    && chmod +x /usr/local/bin/kubectl
  ARG CONTROLLER_TOOLS_VERSION=v0.12.0
  RUN go install sigs.k8s.io/controller-tools/cmd/controller-gen@${CONTROLLER_TOOLS_VERSION}