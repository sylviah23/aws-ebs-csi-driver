// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the 'License');
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an 'AS IS' BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metadata

import (
	"context"
	json "encoding/json"
	"strconv"
	"strings"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/kubernetes-sigs/aws-ebs-csi-driver/pkg/cloud"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type enisVolumes struct {
	ENIs    int
	Volumes int
}

func MetadataInformer(clientset kubernetes.Interface, cloud cloud.Cloud, region string) informers.SharedInformerFactory {
	factory := informers.NewSharedInformerFactory(clientset, 0)
	nodesInformer := factory.Core().V1().Nodes().Informer()
	var handler cache.ResourceEventHandlerFuncs
	handler.AddFunc = func(obj interface{}) {
		if nodeObj, ok := obj.(*v1.Node); ok {
			node := &v1.NodeList{
				Items: []v1.Node{*nodeObj},
			}
			err := UpdateMetadataEC2(clientset, cloud, region, node)
			if err != nil {
				klog.ErrorS(err, "unable to update ENI/Volume count on node labels", "node", node.Items[0].Name)
			}
		}
	}
	_, err := nodesInformer.AddEventHandler(handler)
	if err != nil {
		klog.ErrorS(err, "unable to add event handler for node informer")
	}

	return factory
}

func GetNodes(kubeclient kubernetes.Interface) *v1.NodeList {
	nodes, _ := kubeclient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	return nodes
}

func UpdateMetadataEC2(kubeclient kubernetes.Interface, c cloud.Cloud, region string, nodes *v1.NodeList) error {
	ENIsVolumeMap, err := GetMetadata(c, region, nodes)
	if err != nil {
		klog.ErrorS(err, "unable to get ENI/Volume count")
		return err
	}

	err = PatchNodes(nodes, ENIsVolumeMap, kubeclient)
	if err != nil {
		return err
	}
	return nil
}

func parseNode(providerID string) string {
	if providerID != "" {
		parts := strings.Split(providerID, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	return ""
}

func GetMetadata(client cloud.Cloud, region string, nodes *v1.NodeList) (map[string]enisVolumes, error) {
	nodeIds := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		nodeIds = append(nodeIds, parseNode(node.Spec.ProviderID))
	}

	var resp *ec2types.Instance
	var err error
	var respList []*ec2types.Instance

	if len(nodeIds) > 1 {
		respList, err = client.GetInstances(context.TODO(), nodeIds)
	} else {
		resp, err = client.GetInstance(context.TODO(), nodeIds[0])
		respList = []*ec2types.Instance{resp}
	}

	if err != nil {
		klog.ErrorS(err, "failed to describe instances")
		return nil, err
	}

	ENIsVolumesMap := make(map[string]enisVolumes)
	for _, instance := range respList {
		numAttachedENIs := 1
		if instance.NetworkInterfaces != nil {
			numAttachedENIs = len(instance.NetworkInterfaces)
		}
		numBlockDeviceMappings := 0
		if instance.BlockDeviceMappings != nil {
			numBlockDeviceMappings = len(instance.BlockDeviceMappings)
		}
		instanceID := *instance.InstanceId
		ENIsVolumesMap[instanceID] = enisVolumes{ENIs: numAttachedENIs, Volumes: numBlockDeviceMappings}
	}

	return ENIsVolumesMap, nil
}

func PatchNodes(nodes *v1.NodeList, enisVolumeMap map[string]enisVolumes, clientset kubernetes.Interface) error {
	for _, node := range nodes.Items {
		newNode := node.DeepCopy()
		numAttachedENIs := enisVolumeMap[parseNode(node.Spec.ProviderID)].ENIs
		numBlockDeviceMappings := enisVolumeMap[parseNode(node.Spec.ProviderID)].Volumes
		newNode.Labels["num-volumes"] = strconv.Itoa(numBlockDeviceMappings)
		newNode.Labels["num-ENIs"] = strconv.Itoa(numAttachedENIs)

		oldData, err := json.Marshal(node)
		if err != nil {
			klog.ErrorS(err, "failed to marshal the existing node", "node", node.Name)
			return err
		}
		newData, err := json.Marshal(newNode)
		if err != nil {
			klog.ErrorS(err, "failed to marshal the new node", "node", newNode.Name)
			return err
		}
		patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, &v1.Node{})
		if err != nil {
			klog.ErrorS(err, "failed to create two way merge", "node", node.Name)
			return err
		}
		if _, err := clientset.CoreV1().Nodes().Patch(context.TODO(), node.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
			klog.ErrorS(err, "Failed to patch node", "node", node.Name)
			return err
		}
	}
	return nil
}
