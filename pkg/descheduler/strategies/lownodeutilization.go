/*
Copyright 2017 The Kubernetes Authors.

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

package strategies

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"sigs.k8s.io/descheduler/pkg/api"
	"sigs.k8s.io/descheduler/pkg/descheduler/evictions"
	nodeutil "sigs.k8s.io/descheduler/pkg/descheduler/node"
	podutil "sigs.k8s.io/descheduler/pkg/descheduler/pod"
	"sigs.k8s.io/descheduler/pkg/utils"
)

// LowNodeUtilization evicts pods from overutilized nodes to underutilized nodes. Note that CPU/Memory requests are used
// to calculate nodes' utilization and not the actual resource usage.
func LowNodeUtilization(ctx context.Context, client clientset.Interface, strategy api.DeschedulerStrategy, nodes []*v1.Node, podEvictor *evictions.PodEvictor) {
	// todo: move to config validation?
	// TODO: May be create a struct for the strategy as well, so that we don't have to pass along the all the params?
	if strategy.Params == nil || strategy.Params.NodeResourceUtilizationThresholds == nil {
		klog.V(1).Infof("NodeResourceUtilizationThresholds not set")
		return
	}

	thresholds := strategy.Params.NodeResourceUtilizationThresholds.Thresholds
	targetThresholds := strategy.Params.NodeResourceUtilizationThresholds.TargetThresholds
	if err := validateLowNodeUtilizationStrategyConfig(thresholds, targetThresholds); err != nil {
		klog.Errorf("LowNodeUtilization config is not valid: %v", err)
		return
	}
	
	setMaxValuesForMissingThresholds(thresholds, targetThresholds)

	npm := createNodePodsMap(ctx, client, nodes)
	desiredNodes, targetNodes := classifyNodesForLowUtilization(npm, thresholds, targetThresholds)

	klog.V(1).Infof("Criteria for a node under utilization: CPU: %v, Mem: %v, Pods: %v",
		thresholds[v1.ResourceCPU], thresholds[v1.ResourceMemory], thresholds[v1.ResourcePods])

	if len(desiredNodes) == 0 {
		klog.V(1).Infof("No node is underutilized, nothing to do here, you might tune your thresholds further")
		return
	}
	klog.V(1).Infof("Total number of underutilized nodes: %v", len(desiredNodes))

	if len(desiredNodes) < strategy.Params.NodeResourceUtilizationThresholds.NumberOfNodes {
		klog.V(1).Infof("number of nodes underutilized (%v) is less than NumberOfNodes (%v), nothing to do here", len(desiredNodes), strategy.Params.NodeResourceUtilizationThresholds.NumberOfNodes)
		return
	}

	if len(desiredNodes) == len(nodes) {
		klog.V(1).Infof("all nodes are underutilized, nothing to do here")
		return
	}

	if len(targetNodes) == 0 {
		klog.V(1).Infof("all nodes are under target utilization, nothing to do here")
		return
	}

	klog.V(1).Infof("Criteria for a node above target utilization: CPU: %v, Mem: %v, Pods: %v",
		targetThresholds[v1.ResourceCPU], targetThresholds[v1.ResourceMemory], targetThresholds[v1.ResourcePods])
	klog.V(1).Infof("Total number of nodes above target utilization: %v", len(targetNodes))

	evictPodsFromHighUsageTargetNodes(
		ctx,
		targetNodes,
		desiredNodes,
		targetThresholds,
		podEvictor)

	klog.V(1).Infof("Total number of pods evicted: %v", podEvictor.TotalEvicted())
}

// classifyNodes classifies the nodes into low-utilization or high-utilization nodes. If a node lies between
// low and high thresholds, it is simply ignored.
func classifyNodesForLowUtilization(npm NodePodsMap, thresholds api.ResourceThresholds, targetThresholds api.ResourceThresholds) ([]NodeUsageMap, []NodeUsageMap) {
	desiredNodes, targetNodes := []NodeUsageMap{}, []NodeUsageMap{}
	for node, pods := range npm {
		usage := nodeUtilization(node, pods)
		nuMap := NodeUsageMap{
			node:    node,
			usage:   usage,
			allPods: pods,
		}
		// Check if node is underutilized and if we can schedule pods on it.
		if !nodeutil.IsNodeUnschedulable(node) && isNodeBelowThresholdUtilization(usage, thresholds) {
			klog.V(2).Infof("Node %#v is under utilized with usage: %#v", node.Name, usage)
			desiredNodes = append(desiredNodes, nuMap)
		} else if isNodeAboveThresholdUtilization(usage, targetThresholds) {
			klog.V(2).Infof("Node %#v is over utilized with usage: %#v", node.Name, usage)
			targetNodes = append(targetNodes, nuMap)
		} else {
			klog.V(2).Infof("Node %#v is appropriately utilized with usage: %#v", node.Name, usage)
		}
	}
	return desiredNodes, targetNodes
}

// evictPodsFromTargetNodes evicts pods based on priority, if all the pods on the node have priority, if not
// evicts them based on QoS as fallback option.
func evictPodsFromHighUsageTargetNodes(
	ctx context.Context,
	targetNodes, desiredNodes []NodeUsageMap,
	targetThresholds api.ResourceThresholds,
	podEvictor *evictions.PodEvictor,
) {

	sortNodesByUsage(targetNodes, true)

	// upper bound on total number of pods/cpu/memory to be moved
	totalPods, totalCPU, totalMem, taintsOfDesiredNodes := computeDesiredNodeResourcesAndTaints(desiredNodes, targetThresholds)
	klog.V(1).Infof("Total capacity to be moved: CPU:%v, Mem:%v, Pods:%v", totalCPU, totalMem, totalPods)
	klog.V(1).Infof("********Number of pods evicted from each node:***********")

	for _, node := range targetNodes {
		nodeCapacity := node.node.Status.Capacity
		if len(node.node.Status.Allocatable) > 0 {
			nodeCapacity = node.node.Status.Allocatable
		}
		klog.V(3).Infof("evicting pods from node %#v with usage: %#v", node.node.Name, node.usage)

		nonRemovablePods, removablePods := classifyPods(node.allPods, podEvictor)
		klog.V(2).Infof("allPods:%v, nonRemovablePods:%v, removablePods:%v", len(node.allPods), len(nonRemovablePods), len(removablePods))

		if len(removablePods) == 0 {
			klog.V(1).Infof("no removable pods on node %#v, try next node", node.node.Name)
			continue
		}

		klog.V(1).Infof("evicting pods based on priority, if they have same priority, they'll be evicted based on QoS tiers")
		// sort the evictable Pods based on priority. This also sorts them based on QoS. If there are multiple pods with same priority, they are sorted based on QoS tiers.
		podutil.SortPodsBasedOnPriorityLowToHigh(removablePods)
		evictPodsFromHighUsageNode(ctx, removablePods, targetThresholds, nodeCapacity, node.usage, &totalPods, &totalCPU, &totalMem, taintsOfDesiredNodes, podEvictor, node.node)

		klog.V(1).Infof("%v pods evicted from node %#v with usage %v", podEvictor.NodeEvicted(node.node), node.node.Name, node.usage)
	}
}

func evictPodsFromHighUsageNode(
	ctx context.Context,
	inputPods []*v1.Pod,
	targetThresholds api.ResourceThresholds,
	nodeCapacity v1.ResourceList,
	nodeUsage api.ResourceThresholds,
	totalPods *float64,
	totalCPU *float64,
	totalMem *float64,
	taintsOfDesiredNodes map[string][]v1.Taint,
	podEvictor *evictions.PodEvictor,
	node *v1.Node) {
	// stop if node utilization drops below target threshold or any of required capacity (cpu, memory, pods) is moved
	if isNodeAboveThresholdUtilization(nodeUsage, targetThresholds) && *totalPods > 0 && *totalCPU > 0 && *totalMem > 0 {
		onePodPercentage := api.Percentage((float64(1) * 100) / float64(nodeCapacity.Pods().Value()))
		for _, pod := range inputPods {
			if !utils.PodToleratesTaints(pod, taintsOfDesiredNodes) {
				klog.V(3).Infof("Skipping eviction for Pod: %#v, doesn't tolerate node taint", pod.Name)
				continue
			}

			cUsage := utils.GetResourceRequest(pod, v1.ResourceCPU)
			mUsage := utils.GetResourceRequest(pod, v1.ResourceMemory)

			success, err := podEvictor.EvictPod(ctx, pod, node, "LowNodeUtilization")
			if err != nil {
				klog.Errorf("Error evicting pod: (%#v)", err)
				break
			}

			if success {
				klog.V(3).Infof("Evicted pod: %#v", pod.Name)
				// update remaining pods
				nodeUsage[v1.ResourcePods] -= onePodPercentage
				*totalPods--

				// update remaining cpu
				*totalCPU -= float64(cUsage)
				nodeUsage[v1.ResourceCPU] -= api.Percentage((float64(cUsage) * 100) / float64(nodeCapacity.Cpu().MilliValue()))

				// update remaining memory
				*totalMem -= float64(mUsage)
				nodeUsage[v1.ResourceMemory] -= api.Percentage(float64(mUsage) / float64(nodeCapacity.Memory().Value()) * 100)

				klog.V(3).Infof("updated node usage: %#v", nodeUsage)
				// check if node utilization drops below target threshold or any required capacity (cpu, memory, pods) is moved
				if !isNodeAboveThresholdUtilization(nodeUsage, targetThresholds) || *totalPods <= 0 || *totalCPU <= 0 || *totalMem <= 0 {
					break
				}
			}
		}
	}
}

func validateLowNodeUtilizationStrategyConfig(thresholds, targetThresholds api.ResourceThresholds) error{
	if err := validateStrategyConfig(thresholds, targetThresholds); err != nil {
		return err
	}

	for resourceName, value := range thresholds {
		if targetValue, ok := targetThresholds[resourceName]; !ok {
			return fmt.Errorf("thresholds and targetThresholds configured different resources")
		} else if value > targetValue {
			return fmt.Errorf("thresholds' %v percentage is greater than targetThresholds'", resourceName)
		}
	}
	return nil
}