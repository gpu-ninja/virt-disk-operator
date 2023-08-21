FROM golang:1.20 as builder
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN make

FROM debian:bookworm-slim
WORKDIR /

RUN apt update \
  && apt install -y lvm2 kmod

COPY --from=builder /workspace/bin/manager .

ENTRYPOINT ["/manager"]