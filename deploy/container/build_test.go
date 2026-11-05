package container_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func makeCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("make", args...)
	cmd.Dir = "../.."
	return cmd
}

func TestMonitorImageValidation(t *testing.T) {
	for _, image := range []string{"", "scratch", "projecthami/hami:v2.9.0", "docker.io/projecthami/hami:v2.9.0"} {
		t.Run(image, func(t *testing.T) {
			output, err := makeCommand("-f", "deploy/container/Makefile", "build", "DOCKER=true", "HAMI_VGPUMONITOR_SOURCE=image", "HAMI_VGPUMONITOR_IMAGE="+image).CombinedOutput()
			if err == nil || !strings.Contains(string(output), "unsupported") {
				t.Fatalf("expected actionable monitor-image error, got %v: %s", err, output)
			}
		})
	}
	output, err := makeCommand("-f", "deploy/container/Makefile", "validate-monitor-image", "HAMI_VGPUMONITOR_SOURCE=image", "HAMI_VGPUMONITOR_IMAGE=example.com/hami:verified").CombinedOutput()
	if err != nil {
		t.Fatalf("explicit monitor image rejected: %v: %s", err, output)
	}
	output, err = makeCommand("-f", "deploy/container/Makefile", "validate-monitor-image", "HAMI_VGPUMONITOR_SOURCE=source", "HAMI_VGPUMONITOR_REF=master", "HAMI_VGPUMONITOR_IMAGE=").CombinedOutput()
	if err != nil {
		t.Fatalf("development source rejected: %v: %s", err, output)
	}
	for _, args := range [][]string{
		{"HAMI_VGPUMONITOR_SOURCE=source", "HAMI_VGPUMONITOR_REF="},
		{"HAMI_VGPUMONITOR_SOURCE=source", "HAMI_VGPUMONITOR_REPO="},
		{"HAMI_VGPUMONITOR_SOURCE=source", "HAMI_VGPUMONITOR_REPO=   "},
		{"HAMI_VGPUMONITOR_SOURCE=invalid"},
	} {
		args = append([]string{"-f", "deploy/container/Makefile", "validate-monitor-image"}, args...)
		if output, err := makeCommand(args...).CombinedOutput(); err == nil {
			t.Fatalf("invalid configuration accepted: %v: %s", args, output)
		}
	}
}

func TestMonitorSourceRepository(t *testing.T) {
	for _, args := range [][]string{
		{"HAMI_VGPUMONITOR_SOURCE=source", "HAMI_VGPUMONITOR_REPO=https://github.com/example/HAMi.git", "HAMI_VGPUMONITOR_REF=monitor-dev"},
		{"HAMI_VGPUMONITOR_SOURCE=image", "HAMI_VGPUMONITOR_REPO=", "HAMI_VGPUMONITOR_REF=", "HAMI_VGPUMONITOR_IMAGE=example.com/hami:verified"},
	} {
		args = append([]string{"-f", "deploy/container/Makefile", "build", "DOCKER=true"}, args...)
		if output, err := makeCommand(args...).CombinedOutput(); err != nil {
			t.Fatalf("valid repository configuration rejected: %v: %s", err, output)
		}
	}
}

func TestMonitorImageMakePropagation(t *testing.T) {
	for _, args := range [][]string{
		{"-n", "image"},
		{"-n", "-f", "deploy/container/Makefile", "build"},
		{"-n", "-f", "deploy/container/Makefile", "build", "BUILD_MULTI_ARCH_IMAGES=true", "PUSH_ON_BUILD=true"},
	} {
		args = append(args, "HAMI_VGPUMONITOR_IMAGE=example.com/hami:verified")
		output, err := makeCommand(args...).CombinedOutput()
		if err != nil {
			t.Fatalf("make %v: %v: %s", args, err, output)
		}
		if !strings.Contains(string(output), `--build-arg HAMI_VGPUMONITOR_IMAGE="example.com/hami:verified"`) {
			t.Fatalf("monitor image not forwarded for %v: %s", args, output)
		}
		if !strings.Contains(string(output), `--build-arg HAMI_VGPUMONITOR_SOURCE="source"`) || !strings.Contains(string(output), `--build-arg HAMI_VGPUMONITOR_REF="master"`) {
			t.Fatalf("development source defaults not forwarded: %s", output)
		}
		if !strings.Contains(string(output), `--build-arg HAMI_VGPUMONITOR_REPO="https://github.com/Project-HAMi/HAMi.git"`) {
			t.Fatalf("default source repository not forwarded: %s", output)
		}
		customArgs := append(append([]string{}, args...), "HAMI_VGPUMONITOR_REPO=https://github.com/example/HAMi.git", "HAMI_VGPUMONITOR_REF=monitor-dev")
		output, err = makeCommand(customArgs...).CombinedOutput()
		if err != nil || !strings.Contains(string(output), `--build-arg HAMI_VGPUMONITOR_REPO="https://github.com/example/HAMi.git"`) || !strings.Contains(string(output), `--build-arg HAMI_VGPUMONITOR_REF="monitor-dev"`) {
			t.Fatalf("custom source repository/ref not forwarded for %v: %v: %s", args, err, output)
		}
	}
	output, err := makeCommand("-n", "image", "HAMI_VGPUMONITOR_SOURCE=image", "HAMI_VGPUMONITOR_IMAGE=example.com/hami:release").CombinedOutput()
	if err != nil || !strings.Contains(string(output), `--build-arg HAMI_VGPUMONITOR_SOURCE="image"`) || !strings.Contains(string(output), `--build-arg HAMI_VGPUMONITOR_IMAGE="example.com/hami:release"`) {
		t.Fatalf("release extraction arguments not forwarded: %v: %s", err, output)
	}
}

func TestWorkflowsForwardMonitorArguments(t *testing.T) {
	output, err := makeCommand("-s", "print-HAMI_VGPUMONITOR_RELEASE_IMAGE").CombinedOutput()
	if err != nil {
		t.Fatalf("read shared release image: %v: %s", err, output)
	}
	releaseImage := strings.TrimSpace(string(output))
	if !strings.HasPrefix(releaseImage, "projecthami/hami:v2.11.0@sha256:") {
		t.Fatalf("expected a pinned HAMi v2.11.0 release image, got %q", releaseImage)
	}
	defaults := map[string]string{
		"source": "image",
		"repo":   "https://github.com/Project-HAMi/HAMi.git",
		"ref":    "master",
		"image":  "", // Resolved from versions.mk by the argument-loading step.
	}
	for _, tc := range []struct {
		file     string
		job      string
		dispatch bool
	}{
		{"image.yml", "image", true},
		{"release-image.yml", "image", true},
		{"ci.yml", "monitor-build", false},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../..", ".github/workflows", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			var workflow struct {
				On struct {
					WorkflowDispatch struct {
						Inputs map[string]struct {
							Default string
							Options []string
						}
					} `yaml:"workflow_dispatch"`
				}
				Jobs map[string]struct {
					Env   map[string]string
					Steps []struct {
						ID   string
						Run  string
						Uses string
						With map[string]string
					}
				}
			}
			if err := yaml.Unmarshal(data, &workflow); err != nil {
				t.Fatal(err)
			}
			inputPrefix := ""
			if tc.dispatch {
				inputPrefix = "inputs.monitor_"
				input := workflow.On.WorkflowDispatch.Inputs["monitor_source"]
				if input.Default != "image" || !slices.Contains(input.Options, "image") || !slices.Contains(input.Options, "source") {
					t.Fatal("manual dispatch must default to image and retain the source option")
				}
			}
			job := workflow.Jobs[tc.job]
			for argument, fallback := range defaults {
				variable := "HAMI_VGPUMONITOR_" + strings.ToUpper(argument)
				expression := "${{ "
				if inputPrefix != "" {
					expression += inputPrefix + argument + " || "
				}
				expression += "vars." + variable
				if fallback != "" {
					expression += " || '" + fallback + "'"
				}
				expression += " }}"
				if job.Env[variable] != expression {
					t.Fatalf("%s override/default expression: got %s, want %s", variable, job.Env[variable], expression)
				}
			}
			var argumentScript string
			var forwarded, qemu bool
			for _, step := range job.Steps {
				if step.ID == "args" {
					argumentScript = step.Run
				}
				if strings.HasPrefix(step.Uses, "docker/setup-qemu-action@") {
					qemu = slices.Contains(strings.Split(step.With["platforms"], ","), "arm64")
				}
				if strings.HasPrefix(step.Uses, "docker/build-push-action@") {
					forwarded = true
					for argument := range defaults {
						variable := "HAMI_VGPUMONITOR_" + strings.ToUpper(argument)
						value := "${{ steps.args.outputs.hami_vgpumonitor_" + argument + " }}"
						forwarded = forwarded && strings.Contains(step.With["build-args"], variable+"="+value)
					}
					if !tc.dispatch && (step.With["target"] != "vgpumonitor-smoke" || step.With["platforms"] != "linux/amd64,linux/arm64") {
						t.Fatal("PR CI must run the monitor smoke stage for both architectures")
					}
				}
			}
			if argumentScript == "" || !forwarded {
				t.Fatal("monitor argument loading/forwarding is incomplete")
			}
			if !tc.dispatch && !qemu {
				t.Fatal("PR CI needs arm64 emulation for the monitor version command")
			}
			testMonitorArgumentLoading(t, argumentScript, releaseImage)
		})
	}
}

func testMonitorArgumentLoading(t *testing.T, script, releaseImage string) {
	t.Helper()
	for _, tc := range []struct {
		name   string
		source string
		image  string
	}{
		{"default-image", "image", ""},
		{"custom-image", "image", "example.com/hami:verified"},
		{"custom-source", "source", ""},
		{"invalid-image", "image", "scratch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputPath := filepath.Join(t.TempDir(), "outputs")
			cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", script)
			cmd.Dir = "../.."
			cmd.Env = append(os.Environ(),
				"GITHUB_OUTPUT="+outputPath,
				"RELEASE_TAG=monitor-test",
				"HAMI_VGPUMONITOR_SOURCE="+tc.source,
				"HAMI_VGPUMONITOR_IMAGE="+tc.image,
				"HAMI_VGPUMONITOR_REPO=https://github.com/example/HAMi.git",
				"HAMI_VGPUMONITOR_REF=monitor-dev",
			)
			output, err := cmd.CombinedOutput()
			if tc.name == "invalid-image" {
				if err == nil || !strings.Contains(string(output), "unsupported") {
					t.Fatalf("invalid monitor image accepted: %v: %s", err, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("load monitor arguments: %v: %s", err, output)
			}
			data, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]string{}
			for _, line := range strings.Split(string(data), "\n") {
				key, value, ok := strings.Cut(line, "=")
				if ok {
					values[key] = value
				}
			}
			image := tc.image
			if image == "" {
				image = releaseImage
			}
			for key, want := range map[string]string{
				"source": tc.source,
				"image":  image,
				"repo":   "https://github.com/example/HAMi.git",
				"ref":    "monitor-dev",
			} {
				if got := values["hami_vgpumonitor_"+key]; got != want {
					t.Errorf("monitor %s: got %q, want %q", key, got, want)
				}
			}
		})
	}
}
