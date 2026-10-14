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

package broker

import (
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
	api "k8s.io/client-go/tools/clientcmd/api"
)

func buildKubeconfig(config TargetSettings, username, token string) ([]byte, error) {
	if username == "" || token == "" {
		return nil, fmt.Errorf("kubeconfig requires a username and token")
	}
	clusterName := config.ClusterName
	cluster := api.NewCluster()
	cluster.Server = config.ClusterServer
	cluster.CertificateAuthorityData = append([]byte(nil), config.ClusterCAData...)
	result := &api.Config{
		Clusters: map[string]*api.Cluster{clusterName: cluster},
		AuthInfos: map[string]*api.AuthInfo{
			username: {Token: token},
		},
		Contexts: map[string]*api.Context{
			username: {Cluster: clusterName, AuthInfo: username},
		},
		CurrentContext: username,
	}
	return clientcmd.Write(*result)
}
