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
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/rbacadmission"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func main() {
	var listenAddress, certFile, keyFile string
	flag.StringVar(&listenAddress, "listen-address", ":8443", "The HTTPS address the admission server binds to.")
	flag.StringVar(&certFile, "tls-cert-file", "/tls/tls.crt", "The TLS serving certificate file.")
	flag.StringVar(&keyFile, "tls-key-file", "/tls/tls.key", "The TLS serving key file.")
	flag.Parse()

	runtimeScheme := runtime.NewScheme()
	if err := scheme.AddToScheme(runtimeScheme); err != nil {
		log.Fatalf("add Kubernetes scheme: %v", err)
	}
	if err := rbacv1.AddToScheme(runtimeScheme); err != nil {
		log.Fatalf("add RBAC scheme: %v", err)
	}
	if err := userv1.AddToScheme(runtimeScheme); err != nil {
		log.Fatalf("add internal-user scheme: %v", err)
	}

	cfg := ctrl.GetConfigOrDie()
	cfg.QPS = 20
	cfg.Burst = 40
	reader, err := client.New(cfg, client.Options{Scheme: runtimeScheme})
	if err != nil {
		log.Fatalf("create Kubernetes client: %v", err)
	}
	validator := rbacadmission.NewValidator(reader, runtimeScheme)

	mux := http.NewServeMux()
	mux.Handle("/validate", &admission.Webhook{Handler: validator})
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/readyz", healthHandler)
	server := &http.Server{
		Addr:              listenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	log.Printf("starting internal-user admission webhook on %s", listenAddress)
	if err := server.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve admission webhook: %v", err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
}
