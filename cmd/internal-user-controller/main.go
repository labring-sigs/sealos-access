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
	"flag"
	"os"
	"strings"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/controllers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

var schemeBuilder = runtime.NewSchemeBuilder(scheme.AddToScheme, corev1.AddToScheme, userv1.AddToScheme)

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool
	var webhookPort int
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "The port the admission webhook server binds to.")
	options := zap.Options{Development: false}
	options.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&options)))

	runtimeScheme := runtime.NewScheme()
	if err := schemeBuilder.AddToScheme(runtimeScheme); err != nil {
		panic(err)
	}
	cfg := ctrl.GetConfigOrDie()
	cfg.QPS = 50
	cfg.Burst = 100
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 runtimeScheme,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "internal-user-controller.user.sealos.io",
		Cache: cache.Options{
			// Resync frequently enough to recover terminal lease cleanup after a
			// missed watch event or a status/finalizer update race.
			SyncPeriod: func() *time.Duration {
				period := 30 * time.Second
				return &period
			}(),
			// ServiceAccounts and Secrets are only managed in the protected
			// access-system namespace. Keep their informers namespace
			// scoped so the controller does not require cluster-wide Secret
			// permissions.
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ServiceAccount{}: {
					Namespaces: map[string]cache.Config{
						userv1.InternalUserSystemNamespace: {},
					},
				},
				&corev1.Secret{}: {
					Namespaces: map[string]cache.Config{
						userv1.InternalUserSystemNamespace: {},
					},
				},
			},
		},
		WebhookServer: webhook.NewServer(webhook.Options{Port: webhookPort}),
	})
	if err != nil {
		os.Exit(1)
	}

	if err := (&controllers.InternalUserReconciler{}).SetupWithManager(mgr); err != nil {
		os.Exit(1)
	}
	if err := (&controllers.CredentialLeaseReconciler{}).SetupWithManager(mgr); err != nil {
		os.Exit(1)
	}
	if os.Getenv("DISABLE_WEBHOOKS") != "true" {
		if err := (&userv1.InternalUserWebhook{AllowedIssuers: configuredIssuers(os.Getenv("INTERNAL_USER_OIDC_ISSUERS"))}).SetupWebhookWithManager(mgr); err != nil {
			os.Exit(1)
		}
		if err := (&userv1.CredentialLeaseWebhook{}).SetupWebhookWithManager(mgr); err != nil {
			os.Exit(1)
		}
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		os.Exit(1)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		os.Exit(1)
	}
}

func configuredIssuers(value string) map[string]struct{} {
	issuers := make(map[string]struct{})
	for _, issuer := range strings.Split(value, ",") {
		if issuer = strings.TrimSpace(issuer); issuer != "" {
			issuers[issuer] = struct{}{}
		}
	}
	return issuers
}
