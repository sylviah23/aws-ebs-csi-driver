/*
Copyright 2023 The Kubernetes Authors.

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

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/kubernetes-sigs/aws-ebs-csi-driver/pkg/cloud"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

type metadata struct {
	ENIs             int
	Volumes          int
	InstanceType     string
	AllocatableCount int32
	NodeID           string
	AvailabilityZone string
}

const (
	// VolumesLabel is the label name for the number of volumes on a node
	VolumesLabel = "ebs.csi.aws.com/non-csi-ebs-volumes-count"

	// ENIsLabel is the label name for the number of ENIs on a node
	ENIsLabel = "ebs.csi.aws.com/enis-count"
)

var _ = Describe("EBS CSI Driver Node Labeling", func() {
	f := framework.NewDefaultFramework("ebs")
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged

	var (
		ec2Client        *ec2.Client
		cs               clientset.Interface
		expectedMetadata map[string]*metadata
		labeledMetadata  map[string]*metadata
		changedInstance  string
		createdVolumeID  string
		attachedVolume   bool
	)

	BeforeEach(func() {
		cfg, err := config.LoadDefaultConfig(context.TODO())
		Expect(err).NotTo(HaveOccurred(), "Failed to load AWS SDK config")
		ec2Client = ec2.NewFromConfig(cfg)

		cs = f.ClientSet

		labeledMetadata = make(map[string]*metadata)
		createdVolumeID = ""
		attachedVolume = false
	})

	AfterEach(func() { //TODO: make sure no volume leaks
		if createdVolumeID != "" && attachedVolume {
			By("Detaching the volume")
			instanceID := changedInstance

			detachInput := &ec2.DetachVolumeInput{
				VolumeId:   aws.String(createdVolumeID),
				InstanceId: aws.String(instanceID),
			}

			_, err := ec2Client.DetachVolume(context.TODO(), detachInput)
			Expect(err).NotTo(HaveOccurred(), "Failed to detach volume")

			By("Waiting for volume to be detached")
			Eventually(func() bool {
				describeInput := &ec2.DescribeVolumesInput{
					VolumeIds: []string{createdVolumeID},
				}

				result, err := ec2Client.DescribeVolumes(context.TODO(), describeInput)
				if err != nil {
					return false
				}

				if len(result.Volumes) == 0 {
					return false
				}

				return result.Volumes[0].State == types.VolumeStateAvailable
			}, "2m", "5s").Should(BeTrue(), "Volume did not detach within expected time")

			By("Deleting the volume")
			deleteInput := &ec2.DeleteVolumeInput{
				VolumeId: aws.String(createdVolumeID),
			}

			_, err = ec2Client.DeleteVolume(context.TODO(), deleteInput)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete volume")

			expectedMetadata[instanceID].Volumes -= 1
			createdVolumeID = ""
		} else if createdVolumeID != "" {
			instanceID := changedInstance
			By("Deleting the volume")
			deleteInput := &ec2.DeleteVolumeInput{
				VolumeId: aws.String(createdVolumeID),
			}

			_, err := ec2Client.DeleteVolume(context.TODO(), deleteInput)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete volume")

			expectedMetadata[instanceID].Volumes -= 1
			createdVolumeID = ""
		}
	})

	Describe("Node labeling volumes and ENIs", func() {
		It("should correctly label nodes with volume and ENI counts and have correct csinode allocatable counts", func() {
			By("Getting EC2 instance information")
			resp, err := ec2Client.DescribeInstances(context.TODO(), &ec2.DescribeInstancesInput{})
			Expect(err).NotTo(HaveOccurred(), "Failed to describe EC2 instances")
			expectedMetadata = getVolENIs(resp)

			By("Checking initial node labels")
			nodes, err := cs.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to list nodes")
			checkVolENI(expectedMetadata, labeledMetadata, nodes)

			By("Checking CSI node allocatable counts")
			csiNodes, err := cs.StorageV1().CSINodes().List(context.TODO(), metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to list CSI nodes")
			checkAllocatable(expectedMetadata, labeledMetadata, csiNodes)

			By("Creating and attaching a new volume")
			changedInstance, createdVolumeID = createVolume(ec2Client, expectedMetadata)
			attachedVolume = attachVolume(ec2Client, createdVolumeID, changedInstance, expectedMetadata)
			By("Deleting the EBS CSI node pod to trigger label update")
			deletePod(labeledMetadata[changedInstance].NodeID, cs)

			By("Waiting for labels to update")
			Eventually(func() bool {
				updatedNodes, err := cs.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
				if err != nil {
					return false
				}

				for _, node := range updatedNodes.Items {
					id := parseNode(node.Spec.ProviderID)
					if id == changedInstance {
						volStr, ok := node.Labels[VolumesLabel]
						if !ok {
							return false
						}
						vol, _ := strconv.Atoi(volStr)
						return vol == expectedMetadata[id].Volumes
					}
				}
				return false
			}, "2m", "5s").Should(BeTrue(), "Node labels were not updated with correct volume count")

			By("Verifying updated node labels")
			updatedNodes, err := cs.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to list updated nodes")
			checkVolENI(expectedMetadata, labeledMetadata, updatedNodes)

			By("Verifying updated CSI node allocatable counts")
			updatedCsiNodes, err := cs.StorageV1().CSINodes().List(context.TODO(), metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to list updated CSI nodes")
			checkAllocatable(expectedMetadata, labeledMetadata, updatedCsiNodes)
		})
	})
})

// getAllocatableCount returns the limit of volumes that the node supports.
func getAllocatableCount(instanceType string, volumes, enis int) int32 {
	isNitro := cloud.IsNitroInstanceType(instanceType)
	availableAttachments := cloud.GetMaxAttachments(isNitro)
	reservedVolumeAttachments := volumes + 1 // +1 for root device
	dedicatedLimit := cloud.GetDedicatedLimitForInstanceType(instanceType)
	maxEBSAttachments, hasMaxVolumeLimit := cloud.GetEBSLimitForInstanceType(instanceType)

	if hasMaxVolumeLimit {
		availableAttachments = min(maxEBSAttachments, availableAttachments)
	}
	if dedicatedLimit != 0 {
		availableAttachments = dedicatedLimit
	} else if isNitro {
		reservedSlots := cloud.GetReservedSlotsForInstanceType(instanceType)
		if hasMaxVolumeLimit {
			availableAttachments = availableAttachments - (enis - 1) - reservedSlots
		} else {
			availableAttachments = availableAttachments - enis - reservedSlots
		}
	}
	availableAttachments -= reservedVolumeAttachments
	if availableAttachments <= 0 {
		availableAttachments = 1
	}
	return int32(availableAttachments)
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

func getVolENIs(resp *ec2.DescribeInstancesOutput) map[string]*metadata {
	expectedMetadata := map[string]*metadata{}
	for _, reservation := range resp.Reservations {
		for _, instance := range reservation.Instances {
			instanceID := *instance.InstanceId

			numAttachedENIs := 0
			if instance.NetworkInterfaces != nil {
				numAttachedENIs = len(instance.NetworkInterfaces)
			}

			numBlockDeviceMappings := 0
			if instance.BlockDeviceMappings != nil {
				numBlockDeviceMappings = len(instance.BlockDeviceMappings)
			}
			expectedMetadata[instanceID] = &metadata{
				ENIs:             numAttachedENIs,
				Volumes:          numBlockDeviceMappings,
				InstanceType:     string(instance.InstanceType),
				AvailabilityZone: *instance.Placement.AvailabilityZone,
			}
		}
	}
	return expectedMetadata
}

func createVolume(ec2svc *ec2.Client, metadata map[string]*metadata) (string, string) {
	var instanceID string
	for k := range metadata {
		instanceID = k
		break
	}

	createInput := &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(metadata[instanceID].AvailabilityZone),
		Size:             aws.Int32(1),
		VolumeType:       types.VolumeTypeGp3,
	}

	volumeResult, err := ec2svc.CreateVolume(context.TODO(), createInput)
	Expect(err).NotTo(HaveOccurred(), "Failed to create volume")

	By("Waiting for volume to become available")
	Eventually(func() bool {
		describeInput := &ec2.DescribeVolumesInput{
			VolumeIds: []string{*volumeResult.VolumeId},
		}

		result, err := ec2svc.DescribeVolumes(context.TODO(), describeInput)
		if err != nil {
			return false
		}

		if len(result.Volumes) == 0 {
			return false
		}

		return result.Volumes[0].State == types.VolumeStateAvailable
	}, "2m", "5s").Should(BeTrue(), "Volume did not become available within expected time")

	volumeID := *volumeResult.VolumeId
	return instanceID, volumeID
}

func attachVolume(ec2svc *ec2.Client, volumeID string, instanceID string, metadata map[string]*metadata) bool {
	device := "/dev/sdp"

	attachInput := &ec2.AttachVolumeInput{
		Device:     aws.String(device),
		InstanceId: aws.String(instanceID),
		VolumeId:   aws.String(volumeID),
	}

	_, err := ec2svc.AttachVolume(context.TODO(), attachInput)
	Expect(err).NotTo(HaveOccurred(), "Failed to attach volume")

	metadata[instanceID].Volumes += 1
	return true
}

func deletePod(nodeID string, cs clientset.Interface) {
	pods, err := cs.CoreV1().Pods("kube-system").List(context.TODO(), metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeID,
	})
	Expect(err).NotTo(HaveOccurred(), "Failed to list pods")

	var targetPod string
	for _, pod := range pods.Items {
		if strings.HasPrefix(pod.Name, "ebs-csi-node-") {
			targetPod = pod.Name
			break
		}
	}

	Expect(targetPod).NotTo(BeEmpty(), "Could not find ebs-csi-node pod on node "+nodeID)

	deletePolicy := metav1.DeletePropagationForeground
	deleteOptions := metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}

	err = cs.CoreV1().Pods("kube-system").Delete(context.TODO(), targetPod, deleteOptions)
	Expect(err).NotTo(HaveOccurred(), "Failed to delete pod "+targetPod)
}

func checkVolENI(expectedMetadata, labeledMetadata map[string]*metadata, nodes *corev1.NodeList) {
	for _, node := range nodes.Items {
		vol, _ := strconv.Atoi(node.GetLabels()[VolumesLabel])
		enis, _ := strconv.Atoi(node.GetLabels()[ENIsLabel])
		id := parseNode(node.Spec.ProviderID)
		labeledMetadata[id] = &metadata{}
		labeledMetadata[id].ENIs = enis
		labeledMetadata[id].Volumes = vol
		labeledMetadata[id].NodeID = node.Name

		if expectedMetadata[id] != nil {
			if labeledMetadata[id].Volumes != expectedMetadata[id].Volumes {
				Fail(fmt.Sprintf("Volume count mismatch for node %s: expected %d, got %d\n",
					node.Name, expectedMetadata[id].Volumes, labeledMetadata[id].Volumes))
			}
			if labeledMetadata[id].ENIs != expectedMetadata[id].ENIs {
				Fail(fmt.Sprintf("ENI count mismatch for node %s: expected %d, got %d\n",
					node.Name, expectedMetadata[id].ENIs, labeledMetadata[id].ENIs))
			}
		}
	}
}

func checkAllocatable(expectedMetadata, labeledMetadata map[string]*metadata, csiNodes *storagev1.CSINodeList) {
	for _, csiNode := range csiNodes.Items {
		nodeID := csiNode.Name
		for _, driver := range csiNode.Spec.Drivers {
			labeledMetadata[nodeID].AllocatableCount = *driver.Allocatable.Count

			expectedMetadata[nodeID].AllocatableCount = getAllocatableCount(
				expectedMetadata[nodeID].InstanceType,
				expectedMetadata[nodeID].Volumes,
				expectedMetadata[nodeID].ENIs)

			if labeledMetadata[nodeID].AllocatableCount != expectedMetadata[nodeID].AllocatableCount {
				Fail(fmt.Sprintf("Allocatable count mismatch for csi node %s, expected %d, got %d",
					nodeID, expectedMetadata[nodeID].AllocatableCount, labeledMetadata[nodeID].AllocatableCount))
			}

		}
	}
}
