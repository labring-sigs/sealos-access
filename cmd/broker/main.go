/*
Copyright 2026 labring.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/broker"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	var address, adminGroup string
	var configFile, tlsCertFile, tlsKeyFile string
	var requestRateLimit float64
	var requestRateBurst int
	flag.StringVar(&address, "listen-address", ":8443", "The address the Broker listens on.")
	flag.StringVar(&configFile, "config", os.Getenv("BROKER_CONFIG_FILE"), "Required versioned multi-cluster Broker YAML configuration.")
	flag.StringVar(&adminGroup, "admin-group", envOr("BROKER_ADMIN_GROUP", "sealos.internal.admin"), "The OIDC group allowed to use the admin endpoint.")
	flag.StringVar(&tlsCertFile, "tls-cert-file", os.Getenv("BROKER_TLS_CERT_FILE"), "TLS certificate file.")
	flag.StringVar(&tlsKeyFile, "tls-key-file", os.Getenv("BROKER_TLS_KEY_FILE"), "TLS private key file.")
	flag.Float64Var(&requestRateLimit, "request-rate-limit", envFloatOr("BROKER_REQUEST_RATE_LIMIT", 10), "Maximum Broker requests per second.")
	flag.IntVar(&requestRateBurst, "request-rate-burst", envIntOr("BROKER_REQUEST_RATE_BURST", 20), "Maximum burst of Broker requests.")
	flag.Parse()
	if configFile == "" {
		fatal("multi-cluster Broker config is required")
	}
	if tlsCertFile == "" || tlsKeyFile == "" {
		fatal("TLS certificate/key are required")
	}

	fileConfig, err := broker.LoadMultiClusterConfig(configFile)
	if err != nil {
		fatal(err.Error())
	}
	trustedProxyCIDRs, err := broker.ParseCIDRs(splitEnv("BROKER_TRUSTED_PROXY_CIDRS"))
	if err != nil {
		fatal(err.Error())
	}
	allowedClientCIDRs, err := broker.ParseCIDRs(splitEnv("BROKER_ALLOWED_CLIENT_CIDRS"))
	if err != nil {
		fatal(err.Error())
	}
	oidcClient, err := oidcHTTPClient(fileConfig.OIDC.CAFile, fileConfig.OIDC.ServerName)
	if err != nil {
		fatal(err.Error())
	}
	verifier, err := broker.NewOIDCVerifier(broker.OIDCConfig{
		Issuer:     fileConfig.OIDC.Issuer,
		Audience:   fileConfig.OIDC.Audience,
		JWKSURL:    fileConfig.OIDC.JWKSURL,
		HTTPClient: oidcClient,
	})
	if err != nil {
		fatal(err.Error())
	}
	targets, err := buildTargetBackends(fileConfig)
	if err != nil {
		fatal(err.Error())
	}
	server, err := broker.NewMultiTargetServer(targets, verifier, broker.Config{
		AllowedIssuers: map[string]struct{}{fileConfig.OIDC.Issuer: {}},
		AdminGroup:     adminGroup,
		SourceIP: broker.SourceIPPolicy{
			TrustedProxyCIDRs:     trustedProxyCIDRs,
			AllowedClientCIDRs:    allowedClientCIDRs,
			TrustedClientIPHeader: envOr("BROKER_TRUSTED_CLIENT_IP_HEADER", "X-Trusted-Client-IP"),
		},
		RequestRateLimit: rate.Limit(requestRateLimit),
		RequestRateBurst: requestRateBurst,
	})
	if err != nil {
		fatal(err.Error())
	}
	server.Audit = &broker.LogAuditSink{Writer: os.Stderr}
	httpServer := &http.Server{Addr: address, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	go func() {
		if err := httpServer.ListenAndServeTLS(tlsCertFile, tlsKeyFile); err != nil && err != http.ErrServerClosed {
			fatal(err.Error())
		}
	}()
	signalContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-signalContext.Done()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownContext)
}

func buildTargetBackends(config broker.MultiClusterFileConfig) (map[string]broker.TargetBackend, error) {
	targets := make(map[string]broker.TargetBackend)
	for _, target := range config.Targets {
		if !target.Enabled {
			continue
		}
		caData, err := os.ReadFile(target.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read target %q CA: %w", target.ID, err)
		}
		credentialData, err := os.ReadFile(target.CredentialFile)
		if err != nil {
			return nil, fmt.Errorf("read target %q credential: %w", target.ID, err)
		}
		credential := strings.TrimSpace(string(credentialData))
		if credential == "" {
			return nil, fmt.Errorf("target %q credential file is empty", target.ID)
		}
		restConfig := targetRESTConfig(target, caData)
		clientset, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return nil, fmt.Errorf("create target %q Kubernetes client: %w", target.ID, err)
		}
		scheme := runtime.NewScheme()
		if err := v1.AddToScheme(scheme); err != nil {
			return nil, fmt.Errorf("create target %q scheme: %w", target.ID, err)
		}
		leaseClient, err := client.New(restConfig, client.Options{Scheme: scheme})
		if err != nil {
			return nil, fmt.Errorf("create target %q Lease client: %w", target.ID, err)
		}
		backend, err := broker.NewStaticTargetBackend(target.Settings(caData), leaseClient, &broker.KubernetesTokenIssuer{Client: clientset})
		if err != nil {
			return nil, fmt.Errorf("configure target %q backend: %w", target.ID, err)
		}
		targets[target.ID] = backend
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no enabled target backend is available")
	}
	return targets, nil
}

func targetRESTConfig(target broker.TargetFileConfig, caData []byte) *rest.Config {
	// client-go periodically rereads BearerTokenFile, so replacing one mounted
	// Secret key rotates only this target's API credential.
	return &rest.Config{
		Host:            target.APIServer,
		BearerTokenFile: target.CredentialFile,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: append([]byte(nil), caData...),
		},
	}
}

func splitEnv(name string) []string {
	value := os.Getenv(name)
	if value == "" {
		return nil
	}
	values := strings.Split(value, ",")
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envFloatOr(name string, fallback float64) float64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		fatal(fmt.Sprintf("invalid %s: %v", name, err))
	}
	return parsed
}

func envIntOr(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		fatal(fmt.Sprintf("invalid %s: %v", name, err))
	}
	return parsed
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}

func oidcHTTPClient(caFile, serverName string) (*http.Client, error) {
	if caFile == "" && serverName == "" {
		return nil, nil
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}
	transport = transport.Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read OIDC CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("OIDC CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}, nil
}
