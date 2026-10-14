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
	"fmt"
	"io"
	"os"

	"github.com/labring-sigs/sealos-access/internal/access/admincmd"
	"github.com/labring-sigs/sealos-access/internal/access/usercmd"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}

	switch args[0] {
	case "--help", "-h":
		printUsage(stdout)
		return 0
	case "login":
		return usercmd.Main(args[1:], stdout, stderr)
	case "elevated":
		return usercmd.Main(args, stdout, stderr)
	case "admin":
		return admincmd.Main(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, `Usage:
  sealos-access login [flags]
  sealos-access elevated redeem [flags]
  sealos-access admin create [flags]
  sealos-access admin approve [flags]

Commands:
  login              Authenticate with OIDC and issue a base credential.
  elevated redeem    Redeem one approved elevated credential reference.
  admin create       Create or reconcile an InternalUser through Kubernetes.
  admin approve      Submit an elevated approval through the Broker.

The admin namespace uses separate administrator configuration and
authorization. Command-line routing does not grant administrator access.`)
}
