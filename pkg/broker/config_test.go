package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMultiClusterConfigValidatesStrictTargetConfiguration(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "target.ca")
	credentialPath := filepath.Join(dir, "target.token")
	if err := os.WriteFile(caPath, []byte("ca"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialPath, []byte("token"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "broker.yaml")
	config := "version: 1\n" +
		"oidc:\n" +
		"  issuer: https://issuer.internal.example\n" +
		"  audience: broker\n" +
		"targets:\n" +
		"- id: cluster-a\n" +
		"  apiServer: https://api.cluster-a.example\n" +
		"  clusterServer: https://api.cluster-a.example\n" +
		"  caFile: " + caPath + "\n" +
		"  credentialFile: " + credentialPath + "\n" +
		"  kubernetesAudience: https://kubernetes.cluster-a.example\n" +
		"  userNamespace: access-system\n" +
		"  brokerNamespace: access-broker-system\n" +
		"  clusterName: cluster-a\n" +
		"  enabled: true\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMultiClusterConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OIDC.Issuer != "https://issuer.internal.example" || len(loaded.Targets) != 1 || loaded.Targets[0].ID != "cluster-a" {
		t.Fatalf("loaded config = %#v", loaded)
	}
}

func TestLoadMultiClusterConfigRejectsUnknownFieldsAndDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "target.ca")
	credentialPath := filepath.Join(dir, "target.token")
	if err := os.WriteFile(caPath, []byte("ca"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialPath, []byte("token"), 0600); err != nil {
		t.Fatal(err)
	}
	template := func(extra string) string {
		return "version: 1\n" +
			"oidc:\n  issuer: https://issuer.internal.example\n  audience: broker\n" +
			"targets:\n" +
			"- id: cluster-a\n  apiServer: https://api.cluster-a.example\n  clusterServer: https://api.cluster-a.example\n" +
			"  caFile: " + caPath + "\n  credentialFile: " + credentialPath + "\n" +
			"  kubernetesAudience: audience-a\n  userNamespace: users\n  brokerNamespace: broker\n  clusterName: cluster-a\n  enabled: true\n" + extra
	}
	unknownPath := filepath.Join(dir, "unknown.yaml")
	if err := os.WriteFile(unknownPath, []byte(template("unexpected: true\n")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMultiClusterConfig(unknownPath); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown field error = %v", err)
	}
	duplicatePath := filepath.Join(dir, "duplicate.yaml")
	duplicate := template("- id: cluster-a\n  apiServer: https://api.cluster-a.example\n  clusterServer: https://api.cluster-a.example\n  caFile: " + caPath + "\n  credentialFile: " + credentialPath + "\n  kubernetesAudience: audience-a\n  userNamespace: users\n  brokerNamespace: broker\n  clusterName: cluster-a\n  enabled: true\n")
	if err := os.WriteFile(duplicatePath, []byte(duplicate), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMultiClusterConfig(duplicatePath); err == nil || !strings.Contains(err.Error(), "duplicate target ID") {
		t.Fatalf("duplicate ID error = %v", err)
	}
}
