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
	"bytes"
	"strings"
	"testing"
)

func TestDispatchHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := dispatch([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("dispatch() exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "sealos-access admin approve") {
		t.Fatalf("help output = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestDispatchRejectsUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := dispatch([]string{"unknown"}, &stdout, &stderr); code != 2 {
		t.Fatalf("dispatch() exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "unknown"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDispatchRoutesAdminHelpWithoutLoadingUserConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := dispatch([]string{"admin", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("dispatch() exit code = %d, want 0", code)
	}
}

func TestDispatchRoutesEveryCommandHelp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tests := []struct {
		name string
		args []string
	}{
		{name: "login", args: []string{"login", "--help"}},
		{name: "elevated redeem", args: []string{"elevated", "redeem", "--help"}},
		{name: "admin create", args: []string{"admin", "create", "--help"}},
		{name: "admin approve", args: []string{"admin", "approve", "--help"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := dispatch(test.args, &stdout, &stderr); code != 0 {
				t.Fatalf("dispatch(%v) exit code = %d, want 0; stderr = %q", test.args, code, stderr.String())
			}
		})
	}
}

func TestDispatchRejectsTrailingArguments(t *testing.T) {
	tests := [][]string{
		{"login", "unexpected"},
		{"elevated", "redeem", "unexpected"},
		{"admin", "create", "unexpected"},
		{"admin", "approve", "unexpected"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := dispatch(args, &stdout, &stderr); code != 2 {
			t.Errorf("dispatch(%v) exit code = %d, want 2; stderr = %q", args, code, stderr.String())
		}
	}
}

func TestDispatchRejectsMisplacedAdminFlags(t *testing.T) {
	tests := [][]string{
		{"login", "--kubeconfig", "/secure/admin.kubeconfig"},
		{"elevated", "redeem", "--kubeconfig", "/secure/admin.kubeconfig"},
		{"admin", "create", "--approval-id", "approval-1"},
		{"admin", "approve", "--kubeconfig", "/secure/admin.kubeconfig"},
		{"admin", "--kubeconfig", "/secure/admin.kubeconfig", "create"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := dispatch(args, &stdout, &stderr); code != 2 {
			t.Errorf("dispatch(%v) exit code = %d, want 2; stderr = %q", args, code, stderr.String())
		}
	}
}
