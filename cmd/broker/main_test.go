package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/labring-sigs/sealos-access/pkg/broker"
	"k8s.io/client-go/transport"
)

func TestTargetRESTConfigUsesPerTargetCredentialFile(t *testing.T) {
	config := targetRESTConfig(broker.TargetFileConfig{
		APIServer:      "https://api.cluster-a.example",
		CredentialFile: "/var/run/secrets/cluster-a/token",
	}, []byte("cluster-a-ca"))
	if config.Host != "https://api.cluster-a.example" {
		t.Fatalf("Host = %q", config.Host)
	}
	if config.BearerToken != "" || config.BearerTokenFile != "/var/run/secrets/cluster-a/token" {
		t.Fatalf("credential config = bearer token %q, file %q", config.BearerToken, config.BearerTokenFile)
	}
	if string(config.CAData) != "cluster-a-ca" {
		t.Fatalf("CAData = %q", config.CAData)
	}
}

func TestOIDCHTTPClientLoadsPrivateCAAndServerName(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	certificate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "issuer.internal.example"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		DNSNames:              []string{"issuer.internal.example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile := t.TempDir() + "/ca.crt"
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	httpClient, err := oidcHTTPClient(caFile, "issuer.internal.example")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		t.Fatalf("OIDC HTTP client transport = %#v", httpClient.Transport)
	}
	if transport.TLSClientConfig.ServerName != "issuer.internal.example" || transport.TLSClientConfig.RootCAs == nil {
		t.Fatalf("OIDC TLS config = %#v", transport.TLSClientConfig)
	}
}

func TestTargetCredentialFileCanBeReloadedWithoutRebuildingConfig(t *testing.T) {
	tokenFile := t.TempDir() + "/target.token"
	if err := os.WriteFile(tokenFile, []byte("old-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := targetRESTConfig(broker.TargetFileConfig{CredentialFile: tokenFile}, nil)
	source := transport.NewCachedFileTokenSource(config.BearerTokenFile)
	first, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if first.AccessToken != "old-token" {
		t.Fatalf("initial token = %q", first.AccessToken)
	}
	if err := os.WriteFile(tokenFile, []byte("new-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source.ResetTokenOlderThan(time.Now().Add(time.Second))
	rotated, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if rotated.AccessToken != "new-token" {
		t.Fatalf("rotated token = %q", rotated.AccessToken)
	}
}
