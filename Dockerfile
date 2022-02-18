# syntax=docker/dockerfile:experimental

FROM golang:1.17.6-alpine3.15 as dev
RUN apk add --no-cache git ca-certificates make
RUN adduser -D appuser
COPY . /src/
WORKDIR /src

ENV GO111MODULE=on
RUN --mount=type=cache,sharing=locked,id=gomod,target=/go/pkg/mod/cache \
    --mount=type=cache,sharing=locked,id=goroot,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux make build

FROM scratch
# Add Certificates into the image, for anything that does API calls
COPY --from=dev /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# Add kube-vip binary
COPY --from=dev /src/kube-vip /
ENTRYPOINT ["/kube-vip"]
