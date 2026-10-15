package chart_test

import (
	"bytes"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func renderDaemonSet(t *testing.T, enabled, driverRoot, hookRoot string) appsv1.DaemonSet {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	output, err := exec.Command("helm", "template", "test", "../hami-dra-driver", "--namespace", "hami-system", "--kube-version", "1.34.0",
		"--set", "monitor.enabled="+enabled, "--set", "nvidiaDriverRoot="+driverRoot,
		"--set", "driver.hostHookPath="+hookRoot).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, output)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	for {
		var ds appsv1.DaemonSet
		if err := decoder.Decode(&ds); err != nil {
			if err == io.EOF {
				t.Fatal("DaemonSet not rendered")
			}
			t.Fatal(err)
		}
		if ds.Kind == "DaemonSet" {
			return ds
		}
	}
}

func TestMonitorRuntimeInjectionAndCacheMount(t *testing.T) {
	for _, root := range []string{"/", "/run/nvidia/driver"} {
		for _, hook := range []string{"/usr/local", "/var/lib/hami"} {
			t.Run(root+hook, func(t *testing.T) {
				ds := renderDaemonSet(t, "true", root, hook)
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
					"HOOK_PATH": "/usr/local/vgpu", "DRA_MODE": "true",
				} {
					if variables[name] != want {
						t.Errorf("%s = %q, want %q", name, variables[name], want)
					}
				}
				for _, name := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "NVIDIA_DRIVER_ROOT"} {
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
	ds := renderDaemonSet(t, "false", "/", "/usr/local")
	for _, container := range ds.Spec.Template.Spec.Containers {
		if container.Name == "monitor" {
			t.Fatal("disabled monitor was rendered")
		}
	}
}
