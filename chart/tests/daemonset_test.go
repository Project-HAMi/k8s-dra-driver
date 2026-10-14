package chart_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestMonitorDriverLibrariesAndCacheMount(t *testing.T) {
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
				if m := mounted["driver-root"]; m.MountPath != "/driver-root" || !m.ReadOnly || m.MountPropagation == nil || *m.MountPropagation != corev1.MountPropagationHostToContainer {
					t.Fatalf("monitor needs read-only, propagated driver-root: %+v", m)
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
				if len(monitor.Command) != 2 || monitor.Command[0] != "bash" || len(monitor.Args) != 1 {
					t.Fatal("monitor needs a library-discovery entrypoint")
				}
				// Execute the rendered entrypoint against a fixture driver root. Only
				// the final exec is substituted: this checks the actual selection logic.
				for _, dir := range []string{"usr/lib/x86_64-linux-gnu", "usr/lib/aarch64-linux-gnu", "usr/lib64", "usr/local/lib64"} {
					t.Run(dir, func(t *testing.T) {
						fixture := t.TempDir()
						lib := filepath.Join(fixture, dir, "libnvidia-ml.so.1")
						if err := os.MkdirAll(filepath.Dir(lib), 0755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(lib, nil, 0600); err != nil {
							t.Fatal(err)
						}
						script := strings.ReplaceAll(monitor.Args[0], "/driver-root", fixture)
						script = strings.ReplaceAll(script, "exec vGPUmonitor", `printf '%s' "$LD_PRELOAD"; exit 0`)
						cmd := exec.Command("bash", "-ec", script)
						cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LD_PRELOAD="}
						out, err := cmd.CombinedOutput()
						if err != nil || string(out) != lib {
							t.Fatalf("NVML selection: %q, %v", out, err)
						}
					})
				}
				script := strings.ReplaceAll(monitor.Args[0], "/driver-root", t.TempDir())
				if out, err := exec.Command("bash", "-ec", script).CombinedOutput(); err == nil || !strings.Contains(string(out), "libnvidia-ml.so.1") {
					t.Fatalf("missing NVML must fail with an actionable error: %s, %v", out, err)
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
