/*
 * Copyright 2026 The HAMi Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pkgflags "github.com/Project-HAMi/k8s-dra-driver/pkg/flags"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

func TestDeviceMemoryQuantity(t *testing.T) {
	for _, tt := range []struct {
		raw     string
		want    uint64
		invalid bool
	}{
		{raw: "0"}, {raw: "64Gi", want: 68719476736}, {raw: "65536Mi", want: 68719476736},
		{raw: "1.5Gi", want: 1610612736}, {raw: "1.1Mi", want: 2097152},
		{raw: "64G", want: 64000884736}, {raw: "400m", want: 1048576},
		{raw: "65536", want: 1048576}, {raw: "1n", want: 1048576},
		{raw: "1048576", want: 1048576}, {raw: "1048576.001", want: 2097152},
		{raw: "1E+6", want: 1 << 20}, {raw: "1E", want: 1000000000000786432}, {raw: "1Ei", want: 1 << 60},
		{raw: "9223372036853727232", want: 9223372036853727232},
		{raw: "9223372036853727233", invalid: true}, {raw: "9Ei", invalid: true},
		{raw: "1e4294967296", invalid: true}, {raw: "1e-4294967296", invalid: true},
		{raw: "-1Mi", invalid: true}, {raw: "64GB", invalid: true}, {raw: "", invalid: true},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			budget, err := normalizeDeviceMemory(tt.raw, "default")
			if tt.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, budget.bytes)
		})
	}
}

func TestLoadDeviceMemoryBudget(t *testing.T) {
	for _, tt := range []struct {
		name          string
		enabled       bool
		annotation    *string
		defaultBudget string
		want          uint64
		wantError     string
	}{
		{name: "disabled ignores config", annotation: ptr.To("bad"), defaultBudget: "bad"},
		{name: "no budget", enabled: true, defaultBudget: "0"},
		{name: "global default", enabled: true, defaultBudget: "1.1Mi", want: 2 << 20},
		{name: "annotation overrides default", enabled: true, annotation: ptr.To("64Gi"), defaultBudget: "2Gi", want: 64 << 30},
		{name: "annotation bypasses invalid default", enabled: true, annotation: ptr.To("1.1Mi"), defaultBudget: "bad", want: 2 << 20},
		{name: "zero annotation disables fallback", enabled: true, annotation: ptr.To("0"), defaultBudget: "2Gi"},
		{name: "invalid annotation fails", enabled: true, annotation: ptr.To("bad"), defaultBudget: "2Gi", wantError: deviceMemoryAnnotation},
		{name: "empty annotation fails", enabled: true, annotation: ptr.To(""), defaultBudget: "2Gi", wantError: deviceMemoryAnnotation},
		{name: "invalid default fails without annotation", enabled: true, defaultBudget: "bad", wantError: `device memory "default"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
			if tt.annotation != nil {
				node.Annotations = map[string]string{deviceMemoryAnnotation: *tt.annotation}
			}
			client := fake.NewClientset(node)
			c := &Config{
				flags:      &Flags{nodeName: "node", preConfiguredDeviceMemory: tt.defaultBudget},
				clientsets: pkgflags.ClientSets{Core: client},
			}
			err := c.loadDeviceMemoryBudget(context.Background(), tt.enabled)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, c.deviceMemoryBytes)
			}

			if tt.enabled {
				require.Len(t, client.Actions(), 1)
				action := client.Actions()[0]
				require.Equal(t, "get", action.GetVerb())
				require.Equal(t, "nodes", action.GetResource().Resource)
				require.Equal(t, "node", action.(clienttesting.GetAction).GetName())
			} else {
				require.Empty(t, client.Actions())
			}
		})
	}
}

func TestLoadDeviceMemoryNodeTimeout(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	c := &Config{
		flags:      &Flags{nodeName: "node", preConfiguredDeviceMemory: "2Gi"},
		clientsets: pkgflags.ClientSets{Core: client},
	}
	require.NoError(t, c.loadDeviceMemoryBudget(context.Background(), true))
	require.Len(t, client.Actions(), 1)
	require.Equal(t, uint64(2<<30), c.deviceMemoryBytes)
}

func TestDeviceMemoryStartupLogs(t *testing.T) {
	state := klog.CaptureState()
	defer state.Restore()
	klog.LogToStderr(false)
	var logs bytes.Buffer
	klog.SetOutput(&logs)
	client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: map[string]string{deviceMemoryAnnotation: "1.1Mi"}}})
	c := &Config{flags: &Flags{nodeName: "node"}, clientsets: pkgflags.ClientSets{Core: client}}
	require.NoError(t, c.loadDeviceMemoryBudget(context.Background(), true))
	klog.Flush()
	for _, field := range []string{"Device memory rounded up", deviceMemoryAnnotation, "requested=1.1Mi", "effective=2Mi"} {
		require.Contains(t, logs.String(), field)
	}
	logs.Reset()
	_, err := client.CoreV1().Nodes().Update(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}, metav1.UpdateOptions{})
	require.NoError(t, err)
	c.flags.preConfiguredDeviceMemory = "64Gi"
	require.NoError(t, c.loadDeviceMemoryBudget(context.Background(), true))
	klog.Flush()
	require.Contains(t, logs.String(), "Device memory budget selected")
	require.NotContains(t, logs.String(), "rounded up")
}

func TestDeviceMemoryNodeRequestCancellation(t *testing.T) {
	started := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.URL.Path
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	c := &Config{flags: &Flags{nodeName: "node"}, clientsets: pkgflags.ClientSets{Core: client}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.loadDeviceMemoryBudget(ctx, true) }()
	select {
	case path := <-started:
		require.Equal(t, "/api/v1/nodes/node", path)
	case <-time.After(5 * time.Second):
		t.Fatal("Node request did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Node request ignored cancellation")
	}
}
