# syntax=docker/dockerfile:experimental

FROM golang:1.19-alpine as dev
RUN apk add --no-cache git ca-certificates
RUN adduser -D appuser
COPY main.go /src/
WORKDIR /src

ENV GO111MODULE=on
RUN --mount=type=cache,sharing=locked,id=gomod,target=/go/pkg/mod/cache \
    --mount=type=cache,sharing=locked,id=goroot,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags '-s -w -extldflags -static' -o demo /src/main.go

FROM scratch
COPY --from=dev /src/demo /
CMD ["/demo"]
