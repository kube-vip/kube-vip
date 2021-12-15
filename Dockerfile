# syntax=docker/dockerfile:experimental

FROM golang:1.16.12-alpine3.15 as dev
RUN apk add --no-cache git ca-certificates make
RUN adduser -D appuser
COPY . /src/
WORKDIR /src

ENV GO111MODULE=on
RUN --mount=type=cache,sharing=locked,id=gomod,target=/go/pkg/mod/cache \
    --mount=type=cache,sharing=locked,id=goroot,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux make build

FROM alpine:3.15
RUN apk add --no-cache iproute2 net-tools ca-certificates iptables && update-ca-certificates
COPY dist/iptables-wrapper-installer.sh /
RUN /iptables-wrapper-installer.sh

# Add kube-vip binary
COPY --from=dev /src/kube-vip /
ENTRYPOINT ["/kube-vip"]
