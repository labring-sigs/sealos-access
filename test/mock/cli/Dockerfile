# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY user /src/user
COPY license /src/license
COPY pkg /src/pkg

WORKDIR /src/user
RUN go mod edit \
      -require=github.com/labring/sealos/controllers/license@v0.0.0 \
      -require=github.com/labring/sealos/controllers/pkg@v0.0.0 \
      -replace=github.com/labring/sealos/controllers/license=../license \
      -replace=github.com/labring/sealos/controllers/pkg=../pkg \
      -replace=k8s.io/api=k8s.io/api@v0.32.3 \
      -replace=k8s.io/apiextensions-apiserver=k8s.io/apiextensions-apiserver@v0.32.1 \
      -replace=k8s.io/apimachinery=k8s.io/apimachinery@v0.32.1 \
      -replace=k8s.io/client-go=k8s.io/client-go@v0.32.1 \
      -replace=k8s.io/component-base=k8s.io/component-base@v0.32.1 \
      -replace=sigs.k8s.io/controller-runtime=sigs.k8s.io/controller-runtime@v0.20.4
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
      go build -mod=mod -buildvcs=false -ldflags "-s -w" -trimpath \
      -o /out/sealos-access ./cmd/sealos-access

FROM alpine:3.21
RUN apk add --no-cache curl && \
    adduser -D -u 65532 testuser
COPY --from=build /out/sealos-access /usr/local/bin/sealos-access
RUN mkdir -p /home/testuser/.config/sealos/access && \
    chown -R testuser:testuser /home/testuser
USER testuser
WORKDIR /home/testuser
ENTRYPOINT ["/bin/sh", "-c", "echo 'CLI tools ready. Run: sealos-access --help'; tail -f /dev/null"]
