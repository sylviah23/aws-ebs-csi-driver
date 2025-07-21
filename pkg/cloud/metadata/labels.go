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
	"time"

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

const (
	// VolumesLabel is the label name for the number of volumes on a node
	VolumesLabel = "ebs.csi.aws.com/non-csi-ebs-volumes-count"

	// ENIsLabel is the label name for the number of ENIs on a node
	ENIsLabel = "ebs.csi.aws.com/enis-count"
)

type enisVolumes struct {
	ENIs    int
	Volumes int
}

// ContinuousUpdateLabels is a go routine that updates the metadata labels of each node once every
// `updateTime` minutes and uses an informer to update the labels of new nodes that join the cluster.
func ContinuousUpdateLabels(k8sClient kubernetes.Interface, cloud cloud.Cloud, updateTime int) {
	ticker := time.NewTicker(time.Duration(updateTime) * time.Minute)

	go func() {
		defer ticker.Stop()
		updateLabels(k8sClient, cloud)
		for range ticker.C {
			updateLabels(k8sClient, cloud)
		}
	}()

	informer := metadataInformer(k8sClient, cloud)
	stopCh := make(chan struct{})
	informer.Start(stopCh)
	informer.WaitForCacheSync(stopCh)
}

// metadataInformer returns an informer factory that patches metadata labels for new nodes that join the cluster.
func metadataInformer(clientset kubernetes.Interface, cloud cloud.Cloud) informers.SharedInformerFactory {
	factory := informers.NewSharedInformerFactory(clientset, 0)
	nodesInformer := factory.Core().V1().Nodes().Informer()
	var handler cache.ResourceEventHandlerFuncs
	handler.AddFunc = func(obj interface{}) {
		if nodeObj, ok := obj.(*v1.Node); ok {
			node := &v1.NodeList{
				Items: []v1.Node{*nodeObj},
			}
			err := updateMetadataEC2(clientset, cloud, node)
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

func updateLabels(k8sClient kubernetes.Interface, cloud cloud.Cloud) {
	nodes, err := getNodes(k8sClient)
	if err != nil {
		klog.ErrorS(err, "could not get nodes")
		return
	}
	err = updateMetadataEC2(k8sClient, cloud, nodes)
	if err != nil {
		klog.ErrorS(err, "unable to update ENI/Volume count on node labels")
	}
}

func getNodes(kubeclient kubernetes.Interface) (*v1.NodeList, error) {
	nodes, err := kubeclient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		klog.ErrorS(err, "could not get nodes")
		return nil, err
	}
	return nodes, nil
}

func updateMetadataEC2(kubeclient kubernetes.Interface, c cloud.Cloud, nodes *v1.NodeList) error {
	ENIsVolumeMap, err := getMetadata(c, nodes)
	if err != nil {
		klog.ErrorS(err, "unable to get ENI/Volume count")
		return err
	}

	err = patchNodes(nodes, ENIsVolumeMap, kubeclient)
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

func getMetadata(client cloud.Cloud, nodes *v1.NodeList) (map[string]enisVolumes, error) {
	nodeIds := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		nodeIds = append(nodeIds, parseNode(node.Spec.ProviderID))
	}

	var resp *ec2types.Instance
	var err error
	var respList []*ec2types.Instance

	if len(nodeIds) > 1 {
		respList, err = client.GetInstances(context.TODO(), nodeIds)
	} else if len(nodeIds) == 1 {
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

func patchNodes(nodes *v1.NodeList, enisVolumeMap map[string]enisVolumes, clientset kubernetes.Interface) error {
	for _, node := range nodes.Items {
		newNode := node.DeepCopy()
		numAttachedENIs := enisVolumeMap[parseNode(node.Spec.ProviderID)].ENIs
		numBlockDeviceMappings := enisVolumeMap[parseNode(node.Spec.ProviderID)].Volumes
		newNode.Labels[VolumesLabel] = strconv.Itoa(numBlockDeviceMappings)
		newNode.Labels[ENIsLabel] = strconv.Itoa(numAttachedENIs)

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
