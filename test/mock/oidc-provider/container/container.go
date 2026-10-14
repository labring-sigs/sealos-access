// Package oidccontainer starts the test OIDC provider in Testcontainers.
package oidccontainer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime"

	"github.com/labring-sigs/sealos-access/test/mock/oidc-provider"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const containerPort = "8080/tcp"

// Container is a test OIDC provider running in Docker.
type Container struct {
	testcontainers.Container
	issuer string
}

// Start builds and starts the test provider image. The issuer is derived from
// the mapped port, so Config.Issuer must be empty.
func Start(ctx context.Context, config oidc.Config) (*Container, error) {
	if config.Issuer != "" {
		return nil, fmt.Errorf("container OIDC config must leave issuer empty")
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode container OIDC config: %w", err)
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("locate OIDC container build context")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", ".."))
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:        moduleRoot,
				Dockerfile:     "test/mock/oidc-provider/Dockerfile",
				Repo:           "sealos-test-oidc",
				Tag:            "latest",
				KeepImage:      true,
				BuildLogWriter: io.Discard,
			},
			Env:          map[string]string{oidc.ConfigEnv: string(configJSON)},
			ExposedPorts: []string{containerPort},
			WaitingFor: wait.ForHTTP("/healthz").
				WithPort(containerPort),
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("start OIDC test container: %w", err)
	}
	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("get OIDC container host: %w", err)
	}
	port, err := container.MappedPort(ctx, containerPort)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("get OIDC container port: %w", err)
	}
	return &Container{
		Container: container,
		issuer:    "http://" + net.JoinHostPort(host, port.Port()),
	}, nil
}

// Issuer returns the externally reachable issuer URL.
func (c *Container) Issuer() string {
	return c.issuer
}
