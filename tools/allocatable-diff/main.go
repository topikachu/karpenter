/*
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
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	corecloudprovider "github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/apis/settings"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/cloudprovider"
	awscontext "github.com/aws/karpenter/pkg/context"
)

var clusterName string
var outFile string
var overheadPercent float64

func init() {
	flag.StringVar(&clusterName, "cluster-name", "", "cluster name to use when passing subnets into GetInstanceTypes()")
	flag.StringVar(&outFile, "out-file", "allocatable-diff.csv", "file to output the generated data")
	flag.Float64Var(&overheadPercent, "overhead-percent", 0, "overhead percentage to use for calculations")
	flag.Parse()
}

func main() {
	if clusterName == "" {
		log.Fatalf("cluster name cannot be empty")
	}
	restConfig := config.GetConfigOrDie()
	kubeClient := lo.Must(client.New(restConfig, client.Options{}))
	kubernetesInterface := kubernetes.NewForConfigOrDie(restConfig)
	ctx := context.Background()
	ctx = settings.ToContext(ctx, &settings.Settings{ClusterName: clusterName, IsolatedVPC: true, VMMemoryOverheadPercent: overheadPercent})

	file := lo.Must(os.OpenFile(outFile, os.O_RDWR|os.O_CREATE, 0777))
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	nodeList := &v1.NodeList{}
	lo.Must0(kubeClient.List(ctx, nodeList))

	awsCtx := awscontext.NewOrDie(corecloudprovider.Context{
		Context:             ctx,
		Clock:               clock.RealClock{},
		RESTConfig:          restConfig,
		KubeClient:          kubeClient,
		KubernetesInterface: kubernetesInterface,
	})
	cloudProvider := cloudprovider.New(
		awsCtx.InstanceTypesProvider,
		awsCtx.InstanceProvider,
		awsCtx.KubeClient,
		awsCtx.AMIProvider,
	)
	raw := &runtime.RawExtension{}
	lo.Must0(raw.UnmarshalJSON(lo.Must(json.Marshal(&v1alpha1.AWS{
		SubnetSelector: map[string]string{
			"karpenter.sh/discovery": clusterName,
		},
	}))))
	instanceTypes := lo.Must(cloudProvider.GetInstanceTypes(ctx, &v1alpha5.Provisioner{
		Spec: v1alpha5.ProvisionerSpec{
			Provider: raw,
		},
	}))

	// Write the header information into the CSV
	lo.Must0(w.Write([]string{"Instance Type", "Expected Capacity", "", "", "Expected Allocatable", "", "", "Actual Capacity", "", "", "Actual Allocatable", ""}))
	lo.Must0(w.Write([]string{"", "Memory (Mi)", "CPU (m)", "Storage (Mi)", "Memory (Mi)", "CPU (m)", "Storage (Mi)", "Memory (Mi)", "CPU (m)", "Storage (Mi)", "Memory (Mi)", "CPU (m)", "Storage (Mi)"}))

	nodeList.Items = lo.Filter(nodeList.Items, func(n v1.Node, _ int) bool {
		return n.Labels["karpenter.sh/provisioner-name"] != "" && n.Status.Allocatable.Memory().Value() != 0
	})
	sort.Slice(nodeList.Items, func(i, j int) bool {
		return nodeList.Items[i].Labels[v1.LabelInstanceTypeStable] < nodeList.Items[j].Labels[v1.LabelInstanceTypeStable]
	})
	for _, node := range nodeList.Items {
		instanceType, ok := lo.Find(instanceTypes, func(i *corecloudprovider.InstanceType) bool {
			return i.Name == node.Labels[v1.LabelInstanceTypeStable]
		})
		if !ok {
			log.Fatalf("retrieving instance type for instance %s", node.Labels[v1.LabelInstanceTypeStable])
		}
		allocatable := instanceType.Allocatable()

		// Write the details of the expected instance and the actual instance into a CSV line format
		lo.Must0(w.Write([]string{
			instanceType.Name,
			fmt.Sprintf("%d", instanceType.Capacity.Memory().Value()/1024/1024),
			fmt.Sprintf("%d", instanceType.Capacity.Cpu().MilliValue()),
			fmt.Sprintf("%d", instanceType.Capacity.StorageEphemeral().Value()/1024/1024),
			fmt.Sprintf("%d", allocatable.Memory().Value()/1024/1024),
			fmt.Sprintf("%d", allocatable.Cpu().MilliValue()),
			fmt.Sprintf("%d", allocatable.StorageEphemeral().Value()/1024/1024),
			fmt.Sprintf("%d", node.Status.Capacity.Memory().Value()/1024/1024),
			fmt.Sprintf("%d", node.Status.Capacity.Cpu().MilliValue()),
			fmt.Sprintf("%d", node.Status.Capacity.StorageEphemeral().Value()/1024/1024),
			fmt.Sprintf("%d", node.Status.Allocatable.Memory().Value()/1024/1024),
			fmt.Sprintf("%d", node.Status.Allocatable.Cpu().MilliValue()),
			fmt.Sprintf("%d", node.Status.Allocatable.StorageEphemeral().Value()/1024/1024),
		}))
	}
}
