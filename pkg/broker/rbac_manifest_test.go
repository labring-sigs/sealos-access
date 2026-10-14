package broker

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

func TestBrokerRBACKeepsIdentityCreationOutsideIssuanceBoundary(t *testing.T) {
	clusterRole := readRBACManifest[rbacv1.ClusterRole](t, filepath.Join("..", "..", "config", "broker", "clusterrole.yaml"))
	for _, rule := range clusterRole.Rules {
		for _, resource := range rule.Resources {
			if resource != "internalusers" {
				continue
			}
			for _, verb := range rule.Verbs {
				switch verb {
				case "create", "update", "patch", "delete", "deletecollection":
					t.Fatalf("Broker ClusterRole grants %q on InternalUser", verb)
				}
			}
		}
	}
}

func TestBrokerRBACScopesLeaseAndTokenRequestWrites(t *testing.T) {
	leaseRole := readRBACManifest[rbacv1.Role](t, filepath.Join("..", "..", "config", "broker", "broker-role.yaml"))
	if leaseRole.Namespace != "access-broker-system" {
		t.Fatalf("lease Role namespace = %q", leaseRole.Namespace)
	}
	for _, rule := range leaseRole.Rules {
		for _, resource := range rule.Resources {
			if resource == "credentialleases" || resource == "credentialleases/status" {
				continue
			}
			t.Fatalf("lease Role grants unexpected resource %q", resource)
		}
	}

	tokenRole := readRBACManifest[rbacv1.Role](t, filepath.Join("..", "..", "config", "broker", "user-system-role.yaml"))
	if tokenRole.Namespace != "access-system" {
		t.Fatalf("TokenRequest Role namespace = %q", tokenRole.Namespace)
	}
	for _, rule := range tokenRole.Rules {
		if len(rule.Resources) != 1 || rule.Resources[0] != "serviceaccounts/token" || len(rule.Verbs) != 1 || rule.Verbs[0] != "create" {
			t.Fatalf("TokenRequest Role rule = %#v", rule)
		}
	}
}

func TestTargetRBACTemplateHasAllNarrowPermissionBoundaries(t *testing.T) {
	path := filepath.Join("..", "..", "config", "broker", "target-rbac.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	seen := map[string]bool{}
	for {
		var object struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if object.Kind == "" {
			continue
		}
		seen[object.Kind+"/"+object.Metadata.Namespace] = true
		if object.Kind != "ClusterRole" {
			continue
		}
		for _, rule := range object.Rules {
			for _, resource := range rule.Resources {
				if resource != "internalusers" {
					continue
				}
				for _, verb := range rule.Verbs {
					if verb != "get" {
						t.Fatalf("target ClusterRole grants %q on InternalUser", verb)
					}
				}
			}
		}
	}
	for _, required := range []string{"ServiceAccount/access-broker-system", "ClusterRole/", "ClusterRoleBinding/", "Role/access-broker-system", "Role/access-system", "RoleBinding/access-broker-system", "RoleBinding/access-system"} {
		if !seen[required] {
			t.Fatalf("target RBAC template is missing %s; seen %#v", required, seen)
		}
	}
}

func readRBACManifest[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := json.Unmarshal(jsonData, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
