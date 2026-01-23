//go:build linux

/*
Copyright 2021 The Kubernetes Authors.

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

package cm

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	libcontainercgroups "github.com/opencontainers/cgroups"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	kubefeatures "k8s.io/kubernetes/pkg/features"
)

func activeTestPods() []*v1.Pod {
	return []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "12345678",
				Name:      "guaranteed-pod",
				Namespace: "test",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  "foo",
						Image: "busybox",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceMemory: resource.MustParse("128Mi"),
								v1.ResourceCPU:    resource.MustParse("1"),
							},
							Limits: v1.ResourceList{
								v1.ResourceMemory: resource.MustParse("128Mi"),
								v1.ResourceCPU:    resource.MustParse("1"),
							},
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "87654321",
				Name:      "burstable-pod-1",
				Namespace: "test",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  "foo",
						Image: "busybox",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceMemory: resource.MustParse("128Mi"),
								v1.ResourceCPU:    resource.MustParse("1"),
							},
							Limits: v1.ResourceList{
								v1.ResourceMemory: resource.MustParse("256Mi"),
								v1.ResourceCPU:    resource.MustParse("2"),
							},
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				UID:       "01234567",
				Name:      "burstable-pod-2",
				Namespace: "test",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  "foo",
						Image: "busybox",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceMemory: resource.MustParse("256Mi"),
								v1.ResourceCPU:    resource.MustParse("2"),
							},
						},
					},
				},
			},
		},
	}
}

func createTestQOSContainerManager(logger klog.Logger) (*qosContainerManagerImpl, error) {
	subsystems, err := GetCgroupSubsystems()
	if err != nil {
		return nil, fmt.Errorf("failed to get mounted cgroup subsystems: %v", err)
	}

	cgroupRoot := ParseCgroupfsToCgroupName("/")
	cgroupRoot = NewCgroupName(cgroupRoot, defaultNodeAllocatableCgroupName)

	qosContainerManager := &qosContainerManagerImpl{
		subsystems:    subsystems,
		cgroupManager: NewCgroupManager(logger, subsystems, "cgroupfs"),
		cgroupRoot:    cgroupRoot,
		qosReserved:   nil,
	}

	qosContainerManager.activePods = activeTestPods

	return qosContainerManager, nil
}

func TestQoSContainerCgroup(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	m, err := createTestQOSContainerManager(logger)
	assert.NoError(t, err)

	qosConfigs := map[v1.PodQOSClass]*CgroupConfig{
		v1.PodQOSGuaranteed: {
			Name:               m.qosContainersInfo.Guaranteed,
			ResourceParameters: &ResourceConfig{},
		},
		v1.PodQOSBurstable: {
			Name:               m.qosContainersInfo.Burstable,
			ResourceParameters: &ResourceConfig{},
		},
		v1.PodQOSBestEffort: {
			Name:               m.qosContainersInfo.BestEffort,
			ResourceParameters: &ResourceConfig{},
		},
	}

	m.setMemoryQoS(logger, qosConfigs)

	burstableMin := resource.MustParse("384Mi")
	guaranteedMin := resource.MustParse("128Mi")
	assert.Equal(t, qosConfigs[v1.PodQOSGuaranteed].ResourceParameters.Unified["memory.min"], strconv.FormatInt(burstableMin.Value()+guaranteedMin.Value(), 10))
	assert.Equal(t, qosConfigs[v1.PodQOSBurstable].ResourceParameters.Unified["memory.min"], strconv.FormatInt(burstableMin.Value(), 10))
}

func TestQoSContainerCPUIdle(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	m, err := createTestQOSContainerManager(logger)
	assert.NoError(t, err)

	qosConfigs := map[v1.PodQOSClass]*CgroupConfig{
		v1.PodQOSGuaranteed: {
			Name:               m.qosContainersInfo.Guaranteed,
			ResourceParameters: &ResourceConfig{},
		},
		v1.PodQOSBurstable: {
			Name:               m.qosContainersInfo.Burstable,
			ResourceParameters: &ResourceConfig{},
		},
		v1.PodQOSBestEffort: {
			Name:               m.qosContainersInfo.BestEffort,
			ResourceParameters: &ResourceConfig{},
		},
	}

	// Test with CPUIdleForBestEffortQoS feature gate enabled
	// Note: This test will only set cpu.idle if running in cgroup v2 mode
	err = m.setCPUCgroupConfig(qosConfigs)
	assert.NoError(t, err)

	// Verify that best-effort cgroup has either cpu.idle or cpu.shares set
	// depending on whether CPUIdleForBestEffortQoS feature gate is enabled and cgroup v2 mode
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CPUIdleForBestEffortQoS) &&
		libcontainercgroups.IsCgroup2UnifiedMode() {
		// CPUIdleForBestEffortQoS feature gate is enabled and running in cgroup v2 mode
		assert.NotNil(t, qosConfigs[v1.PodQOSBestEffort].ResourceParameters.Unified)
		cpuIdle, exists := qosConfigs[v1.PodQOSBestEffort].ResourceParameters.Unified["cpu.idle"]
		assert.True(t, exists)
		assert.Equal(t, "1", cpuIdle)
		// cpu.shares should not be set when cpu.idle is set
		assert.Nil(t, qosConfigs[v1.PodQOSBestEffort].ResourceParameters.CPUShares)
	} else if libcontainercgroups.IsCgroup2UnifiedMode() {
		// CPUIdleForBestEffortQoS feature gate is disabled and running in cgroup v2 mode
		// cpu.idle should be "0" and cpu.shares should be set
		assert.NotNil(t, qosConfigs[v1.PodQOSBestEffort].ResourceParameters.Unified)
		cpuIdle, exists := qosConfigs[v1.PodQOSBestEffort].ResourceParameters.Unified["cpu.idle"]
		assert.True(t, exists)
		assert.Equal(t, "0", cpuIdle)
		assert.NotNil(t, qosConfigs[v1.PodQOSBestEffort].ResourceParameters.CPUShares)
		assert.Equal(t, uint64(MinShares), *qosConfigs[v1.PodQOSBestEffort].ResourceParameters.CPUShares)
	} else {
		// Running in cgroup v1 mode
		// cpu.shares should be set, cpu.idle should not be set
		assert.Equal(t, uint64(MinShares), *qosConfigs[v1.PodQOSBestEffort].ResourceParameters.CPUShares)
		if qosConfigs[v1.PodQOSBestEffort].ResourceParameters.Unified != nil {
			_, exists := qosConfigs[v1.PodQOSBestEffort].ResourceParameters.Unified["cpu.idle"]
			assert.False(t, exists)
		}
	}

	// Verify that burstable cgroup has cpu.shares set (not affected by CPUIdleForBestEffortQoS)
	assert.NotNil(t, qosConfigs[v1.PodQOSBurstable].ResourceParameters.CPUShares)
	assert.Greater(t, *qosConfigs[v1.PodQOSBurstable].ResourceParameters.CPUShares, uint64(0))
}

func TestQoSContainerCPUIdleFeatureGateToggle(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	m, err := createTestQOSContainerManager(logger)
	assert.NoError(t, err)

	// Test scenario: Feature gate was previously enabled, now we need to disable it
	// This simulates the case where cpu.idle was set to 1, and now we need to clear it
	// and set cpu.shares instead
	qosConfigs := map[v1.PodQOSClass]*CgroupConfig{
		v1.PodQOSGuaranteed: {
			Name:               m.qosContainersInfo.Guaranteed,
			ResourceParameters: &ResourceConfig{},
		},
		v1.PodQOSBurstable: {
			Name:               m.qosContainersInfo.Burstable,
			ResourceParameters: &ResourceConfig{},
		},
		v1.PodQOSBestEffort: {
			Name:               m.qosContainersInfo.BestEffort,
			ResourceParameters: &ResourceConfig{
				// Simulate previous state with cpu.idle set
				Unified: map[string]string{
					Cgroup2CPUIdle: "1",
				},
			},
		},
	}

	// Call setCPUCgroupConfig which should handle feature gate being disabled
	// (assuming it's disabled for this test, or in cgroup v1 mode)
	err = m.setCPUCgroupConfig(qosConfigs)
	assert.NoError(t, err)

	// Verify the behavior based on feature gate state and cgroup mode
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CPUIdleForBestEffortQoS) &&
		libcontainercgroups.IsCgroup2UnifiedMode() {
		// Feature gate enabled and cgroup v2: cpu.idle should remain "1"
		assert.Equal(t, "1", qosConfigs[v1.PodQOSBestEffort].ResourceParameters.Unified[Cgroup2CPUIdle])
		assert.Nil(t, qosConfigs[v1.PodQOSBestEffort].ResourceParameters.CPUShares)
	} else if libcontainercgroups.IsCgroup2UnifiedMode() {
		// Feature gate disabled and cgroup v2: cpu.idle should be "0" and cpu.shares set
		assert.Equal(t, "0", qosConfigs[v1.PodQOSBestEffort].ResourceParameters.Unified[Cgroup2CPUIdle])
		assert.NotNil(t, qosConfigs[v1.PodQOSBestEffort].ResourceParameters.CPUShares)
		assert.Equal(t, uint64(MinShares), *qosConfigs[v1.PodQOSBestEffort].ResourceParameters.CPUShares)
	} else {
		// Running in cgroup v1 mode: cpu.shares should be set, cpu.idle should not be set
		if qosConfigs[v1.PodQOSBestEffort].ResourceParameters.Unified != nil {
			_, exists := qosConfigs[v1.PodQOSBestEffort].ResourceParameters.Unified[Cgroup2CPUIdle]
			assert.False(t, exists, "cpu.idle should not be set in cgroup v1 mode")
		}
		assert.NotNil(t, qosConfigs[v1.PodQOSBestEffort].ResourceParameters.CPUShares)
		assert.Equal(t, uint64(MinShares), *qosConfigs[v1.PodQOSBestEffort].ResourceParameters.CPUShares)
	}

	// Verify that burstable cgroup is unaffected
	assert.NotNil(t, qosConfigs[v1.PodQOSBurstable].ResourceParameters.CPUShares)
	assert.Greater(t, *qosConfigs[v1.PodQOSBurstable].ResourceParameters.CPUShares, uint64(0))
}
