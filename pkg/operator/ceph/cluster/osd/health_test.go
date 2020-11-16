/*
Copyright 2017 The Rook Authors. All rights reserved.

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
package osd

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/client/clientset/versioned/scheme"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/operator/k8sutil"
	testexec "github.com/rook/rook/pkg/operator/test"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestOSDHealthCheck(t *testing.T) {
	ctx := context.TODO()
	clientset := testexec.New(t, 2)
	clusterInfo := client.AdminClusterInfo("fake")

	var execCount = 0
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutputFile: func(command string, outFileArg string, args ...string) (string, error) {
			return "{\"key\":\"mysecurekey\", \"osdid\":3.0}", nil
		},
	}
	executor.MockExecuteCommandWithOutputFile = func(command string, outFileArg string, args ...string) (string, error) {
		logger.Infof("ExecuteCommandWithOutputFile: %s %v", command, args)
		execCount++
		if args[1] == "dump" {
			// Mock executor for OSD Dump command, returning an osd in Down state
			return `{"OSDs": [{"OSD": 0, "Up": 0, "In": 0}]}`, nil
		} else if args[1] == "safe-to-destroy" {
			// Mock executor for OSD Dump command, returning an osd in Down state
			return `{"safe_to_destroy":[0],"active":[],"missing_stats":[],"stored_pgs":[]}`, nil
		}
		return "", nil
	}

	executor.MockExecuteCommandWithOutput = func(command string, args ...string) (string, error) {
		logger.Infof("ExecuteCommandWithOutput: %s %v", command, args)
		return "", nil
	}

	// Setting up objects needed to create OSD
	context := &clusterd.Context{
		Executor:  executor,
		Clientset: clientset,
	}

	labels := map[string]string{
		k8sutil.AppAttr:     AppName,
		k8sutil.ClusterAttr: clusterInfo.Namespace,
		OsdIdLabelKey:       "0",
	}

	deployment := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "osd0",
			Namespace: clusterInfo.Namespace,
			Labels:    labels,
		},
	}
	if _, err := context.Clientset.AppsV1().Deployments(clusterInfo.Namespace).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		logger.Errorf("Error creating fake deployment: %v", err)
	}

	// Check if the osd deployment is created
	dp, _ := context.Clientset.AppsV1().Deployments(clusterInfo.Namespace).List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("%v=%d", OsdIdLabelKey, 0)})
	assert.Equal(t, 1, len(dp.Items))

	// Initializing an OSD monitoring
	osdMon := NewOSDHealthMonitor(context, clusterInfo, true, cephv1.CephClusterHealthCheckSpec{})

	// Run OSD monitoring routine
	err := osdMon.checkOSDDump()
	assert.Nil(t, err)
	// After creating an OSD, the dump has 1 mocked cmd and safe to destroy has 1 mocked cmd
	assert.Equal(t, 2, execCount)

	// Check if the osd deployment was deleted
	dp, _ = context.Clientset.AppsV1().Deployments(clusterInfo.Namespace).List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("%v=%d", OsdIdLabelKey, 0)})
	assert.Equal(t, 0, len(dp.Items))
}

func TestMonitorStart(t *testing.T) {
	stopCh := make(chan struct{})
	osdMon := NewOSDHealthMonitor(&clusterd.Context{}, client.AdminClusterInfo("ns"), true, cephv1.CephClusterHealthCheckSpec{})
	logger.Infof("starting osd monitor")
	go osdMon.Start(stopCh)
	close(stopCh)
}

func TestOSDRestartIfStuck(t *testing.T) {
	ctx := context.TODO()
	clientset := testexec.New(t, 1)
	clusterInfo := client.AdminClusterInfo("test")
	// Setting up objects needed to create OSD
	context := &clusterd.Context{
		Clientset: clientset,
	}

	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "osd0",
			Namespace: clusterInfo.Namespace,
			Labels: map[string]string{
				"ceph-osd-id": "23",
				"portable":    "true",
			},
		},
	}
	pod.Spec.NodeName = "node0"
	_, err := context.Clientset.CoreV1().Pods(clusterInfo.Namespace).Create(ctx, &pod, metav1.CreateOptions{})
	assert.NoError(t, err)

	m := NewOSDHealthMonitor(context, clusterInfo, false, cephv1.CephClusterHealthCheckSpec{})

	assert.NoError(t, k8sutil.ForceDeletePodIfStuck(m.context, pod))

	// The pod should still exist since it wasn't in a deleted state
	p, err := context.Clientset.CoreV1().Pods(clusterInfo.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotNil(t, p)

	// Add a deletion timestamp to the pod
	pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	_, err = context.Clientset.CoreV1().Pods(clusterInfo.Namespace).Update(ctx, &pod, metav1.UpdateOptions{})
	assert.NoError(t, err)

	assert.NoError(t, k8sutil.ForceDeletePodIfStuck(m.context, pod))

	// The pod should still exist since the node is ready
	p, err = context.Clientset.CoreV1().Pods(clusterInfo.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotNil(t, p)

	// Set the node to a not ready state
	nodes, err := context.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	assert.NoError(t, err)
	for _, node := range nodes.Items {
		node.Status.Conditions[0].Status = v1.ConditionFalse
		localnode := node
		_, err := context.Clientset.CoreV1().Nodes().Update(ctx, &localnode, metav1.UpdateOptions{})
		assert.NoError(t, err)
	}

	assert.NoError(t, k8sutil.ForceDeletePodIfStuck(m.context, pod))

	// The pod should be deleted since the pod is marked as deleted and the node is not ready
	_, err = context.Clientset.CoreV1().Pods(clusterInfo.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	assert.Error(t, err)
	assert.True(t, kerrors.IsNotFound(err))
}

func TestNewOSDHealthMonitor(t *testing.T) {
	clusterInfo := client.AdminClusterInfo("test")
	c := &clusterd.Context{}
	time10s, _ := time.ParseDuration("10s")
	type args struct {
		context                        *clusterd.Context
		removeOSDsIfOUTAndSafeToRemove bool
		healthCheck                    cephv1.CephClusterHealthCheckSpec
	}
	tests := []struct {
		name string
		args args
		want *OSDHealthMonitor
	}{
		{"default-interval", args{c, false, cephv1.CephClusterHealthCheckSpec{}}, &OSDHealthMonitor{c, clusterInfo, false, defaultHealthCheckInterval}},
		{"10s-interval", args{c, false, cephv1.CephClusterHealthCheckSpec{DaemonHealth: cephv1.DaemonHealthSpec{ObjectStorageDaemon: cephv1.HealthCheckSpec{Interval: "10s"}}}}, &OSDHealthMonitor{c, clusterInfo, false, time10s}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewOSDHealthMonitor(tt.args.context, clusterInfo, tt.args.removeOSDsIfOUTAndSafeToRemove, tt.args.healthCheck); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewOSDHealthMonitor() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeviceClasses(t *testing.T) {
	clusterInfo := client.AdminClusterInfo("fake")
	clusterInfo.SetName("rook-ceph")

	var execCount = 0
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutputFile: func(command string, outFileArg string, args ...string) (string, error) {
			return "{\"key\":\"mysecurekey\", \"osdid\":3.0}", nil
		},
	}
	executor.MockExecuteCommandWithOutputFile = func(command string, outFileArg string, args ...string) (string, error) {
		logger.Infof("ExecuteCommandWithOutputFile: %s %v", command, args)
		execCount++
		if args[1] == "crush" && args[2] == "class" && args[3] == "ls" {
			// Mock executor for OSD crush class list command, returning ssd as available device class
			return `["ssd"]`, nil
		}
		return "", nil
	}

	cephCluster := &cephv1.CephCluster{}
	// Objects to track in the fake client.
	object := []runtime.Object{
		cephCluster,
	}
	s := scheme.Scheme
	// Create a fake client to mock API calls.
	cl := fake.NewFakeClientWithScheme(s, object...)

	context := &clusterd.Context{
		Executor: executor,
		Client:   cl,
	}

	// Initializing an OSD monitoring
	osdMon := NewOSDHealthMonitor(context, clusterInfo, true, cephv1.CephClusterHealthCheckSpec{})

	// Run OSD monitoring routine
	err := osdMon.checkDeviceClasses()
	assert.Nil(t, err)
	// checkDeviceClasses has 1 mocked cmd for fetching the device classes
	assert.Equal(t, 1, execCount)
}
