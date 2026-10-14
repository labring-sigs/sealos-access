# syntax=docker/dockerfile:1
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -buildvcs=false -ldflags "-s -w" -trimpath -o /out/mock-approval-server ./test/mock/approval-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mock-approval-server /mock-approval-server
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/mock-approval-server"]
