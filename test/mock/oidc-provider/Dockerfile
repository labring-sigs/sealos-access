FROM golang:1.25-alpine AS build

WORKDIR /src
RUN printf 'module github.com/labring/sealos/controllers/user\n\ngo 1.25\n' > go.mod
COPY test/mock/oidc-provider/server.go ./test/mock/oidc-provider/server.go
COPY test/mock/oidc-provider/cmd/test-oidc/main.go ./test/mock/oidc-provider/cmd/test-oidc/main.go
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/test-oidc ./test/mock/oidc-provider/cmd/test-oidc

FROM scratch
COPY --from=build /out/test-oidc /test-oidc
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /etc/passwd /etc/passwd
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/test-oidc"]
