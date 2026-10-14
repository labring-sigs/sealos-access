package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/labring-sigs/sealos-access/test/mock/oidc-provider"
)

func main() {
	var address, certFile, keyFile string
	flag.StringVar(&address, "listen-address", os.Getenv("SEALOS_TEST_OIDC_ADDR"), "The address the test OIDC provider listens on.")
	flag.StringVar(&certFile, "tls-cert-file", os.Getenv("SEALOS_TEST_OIDC_TLS_CERT_FILE"), "Optional TLS certificate file.")
	flag.StringVar(&keyFile, "tls-key-file", os.Getenv("SEALOS_TEST_OIDC_TLS_KEY_FILE"), "Optional TLS private key file.")
	flag.Parse()
	provider, err := oidc.NewFromJSON([]byte(os.Getenv(oidc.ConfigEnv)))
	if err != nil {
		log.Fatalf("create test OIDC provider: %v", err)
	}
	if address == "" {
		address = ":8080"
	}
	server := &http.Server{Addr: address, Handler: provider}
	fmt.Printf("test OIDC provider listening on %s\n", address)
	var serveErr error
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			log.Fatal("both TLS certificate and key files are required")
		}
		serveErr = server.ListenAndServeTLS(certFile, keyFile)
	} else {
		serveErr = server.ListenAndServe()
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		log.Fatal(serveErr)
	}
}
