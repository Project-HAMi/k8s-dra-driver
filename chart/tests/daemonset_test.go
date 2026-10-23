package chart_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestPodLabelPatchRBAC(t *testing.T) {
	for _, enabled := range []string{"false", "true"} {
		t.Run("monitor="+enabled, func(t *testing.T) {
			output, err := templateChart(t, "--set", "monitor.enabled="+enabled,
				"--show-only", "templates/rbac-kubeletplugin.yaml.yaml")
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}
			assertPodLabelPatchRBAC(t, output)
		})
	}
}

func TestDemoPodLabelPatchRBAC(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("..", "..", "demo", "yaml", "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	assertPodLabelPatchRBAC(t, manifest)
}

func assertPodLabelPatchRBAC(t *testing.T, manifest []byte) {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		var role rbacv1.ClusterRole
		if err := decoder.Decode(&role); err != nil {
			if err == io.EOF {
				t.Fatal("ClusterRole missing required Pod get/list/watch/patch permissions")
			}
			t.Fatal(err)
		}
		if role.Kind != "ClusterRole" {
			continue
		}
		for _, rule := range role.Rules {
			if !reflect.DeepEqual(rule.APIGroups, []string{""}) || !reflect.DeepEqual(rule.Resources, []string{"pods"}) {
				continue
			}
			if !reflect.DeepEqual(rule.Verbs, []string{"get", "list", "watch", "patch"}) {
				t.Fatalf("Pod permissions = %v, want read plus patch only", rule.Verbs)
			}
			return
		}
	}
}

func templateChart(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	baseArgs := []string{"template", "test", "../hami-dra-driver", "--namespace", "hami-system", "--kube-version", "1.34.0"}
	return exec.Command("helm", append(baseArgs, args...)...).CombinedOutput()
}

func renderDaemonSet(t *testing.T, args ...string) appsv1.DaemonSet {
	t.Helper()
	output, err := templateChart(t, append([]string{"--show-only", "templates/daemonset.yaml"}, args...)...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, output)
	}
	var ds appsv1.DaemonSet
	if err := yaml.Unmarshal(output, &ds); err != nil {
		t.Fatal(err)
	}
	if ds.Kind != "DaemonSet" {
		t.Fatalf("rendered kind = %q, want DaemonSet", ds.Kind)
	}
	return ds
}

func TestMonitorRuntimeInjectionAndCacheMount(t *testing.T) {
	for _, root := range []string{"/", "/run/nvidia/driver"} {
		for _, hook := range []string{"/usr/local", "/var/lib/hami"} {
			t.Run(root+hook, func(t *testing.T) {
				ds := renderDaemonSet(t, "--set", "monitor.enabled=true",
					"--set", "nvidiaDriverRoot="+root, "--set", "driver.hostHookPath="+hook)
				var monitor *corev1.Container
				for i := range ds.Spec.Template.Spec.Containers {
					if ds.Spec.Template.Spec.Containers[i].Name == "monitor" {
						monitor = &ds.Spec.Template.Spec.Containers[i]
					}
				}
				if monitor == nil {
					t.Fatal("monitor not rendered")
				}
				mounted := map[string]corev1.VolumeMount{}
				for _, mount := range monitor.VolumeMounts {
					mounted[mount.Name] = mount
				}
				if _, found := mounted["driver-root"]; found {
					t.Fatal("monitor must use runtime injection, not a driver-root mount")
				}
				if mounted["host-vgpu"].MountPath != "/usr/local/vgpu" {
					t.Fatal("monitor cache mount must remain fixed inside the container")
				}
				for _, volume := range ds.Spec.Template.Spec.Volumes {
					switch volume.Name {
					case "driver-root":
						if volume.HostPath.Path != root {
							t.Fatal("wrong NVIDIA driver root")
						}
					case "host-vgpu":
						if volume.HostPath.Path != filepath.Join(hook, "vgpu") {
							t.Fatal("wrong host cache root")
						}
					}
				}
				if !reflect.DeepEqual(monitor.Command, []string{"vGPUmonitor"}) || len(monitor.Args) != 0 {
					t.Fatal("monitor must run directly without a library preload wrapper")
				}
				variables := map[string]string{}
				for _, variable := range monitor.Env {
					variables[variable.Name] = variable.Value
				}
				for name, want := range map[string]string{
					"NVIDIA_VISIBLE_DEVICES": "all", "NVIDIA_DRIVER_CAPABILITIES": "utility",
					"HOOK_PATH": "/usr/local/vgpu",
				} {
					if variables[name] != want {
						t.Errorf("%s = %q, want %q", name, variables[name], want)
					}
				}
				for _, name := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "NVIDIA_DRIVER_ROOT", "DRA_MODE"} {
					if _, found := variables[name]; found {
						t.Errorf("unexpected monitor library override %s", name)
					}
				}
				// Runtime injection must stay disabled for the driver itself; its
				// existing explicit driver-root loading remains unchanged.
				for _, container := range ds.Spec.Template.Spec.Containers {
					if container.Name != "gpus" {
						continue
					}
					var visible, mountPath string
					for _, variable := range container.Env {
						if variable.Name == "NVIDIA_VISIBLE_DEVICES" {
							visible = variable.Value
						}
					}
					for _, mount := range container.VolumeMounts {
						if mount.Name == "driver-root" {
							mountPath = mount.MountPath
						}
					}
					if visible != "void" || mountPath != "/driver-root" {
						t.Fatal("driver library loading changed")
					}
				}
			})
		}
	}
}

func TestMonitorDisabled(t *testing.T) {
	ds := renderDaemonSet(t, "--set", "monitor.enabled=false")
	for _, container := range ds.Spec.Template.Spec.Containers {
		if container.Name == "monitor" {
			t.Fatal("disabled monitor was rendered")
		}
	}
}

func TestDeviceMemoryEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "default", want: "0"},
		{
			name: "missing from merged values",
			// A null override removes the key, as with reused defaults from an older chart.
			args: []string{"--set", "driver.preConfiguredDeviceMemory=null"},
			want: "0",
		},
		{
			name: "uppercase feature gate",
			args: []string{"--set-string", "featureGates.HAMiCoreSupport=TRUE,driver.preConfiguredDeviceMemory=64Gi"},
			want: "64Gi",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := renderDaemonSet(t, tc.args...)
			var budgets []string
			for _, container := range ds.Spec.Template.Spec.Containers {
				for _, env := range container.Env {
					if env.Name == "PRE_CONFIGURED_DEVICE_MEMORY" {
						if container.Name != "gpus" {
							t.Errorf("found PRE_CONFIGURED_DEVICE_MEMORY in %q, want gpus", container.Name)
						}
						budgets = append(budgets, env.Value)
					}
				}
			}
			if len(budgets) != 1 || budgets[0] != tc.want {
				t.Errorf("PRE_CONFIGURED_DEVICE_MEMORY values = %q, want [%q]", budgets, tc.want)
			}
		})
	}
}

func TestDeviceMemoryRejectsNonStringDefault(t *testing.T) {
	output, err := templateChart(t, "--set", "featureGates.HAMiCoreSupport=false,driver.preConfiguredDeviceMemory=0")
	if err == nil || !strings.Contains(string(output), "driver.preConfiguredDeviceMemory must be a Kubernetes Quantity string") {
		t.Fatalf("expected Helm to reject the numeric memory budget, got %v\n%s", err, output)
	}
}
