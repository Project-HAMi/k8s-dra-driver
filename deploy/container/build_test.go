package container_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
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
		{"HAMI_VGPUMONITOR_SOURCE=invalid"},
	} {
		args = append([]string{"-f", "deploy/container/Makefile", "validate-monitor-image"}, args...)
		if output, err := makeCommand(args...).CombinedOutput(); err == nil {
			t.Fatalf("invalid configuration accepted: %v: %s", args, output)
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
	}
	output, err := makeCommand("-n", "image", "HAMI_VGPUMONITOR_SOURCE=image", "HAMI_VGPUMONITOR_IMAGE=example.com/hami:release").CombinedOutput()
	if err != nil || !strings.Contains(string(output), `--build-arg HAMI_VGPUMONITOR_SOURCE="image"`) || !strings.Contains(string(output), `--build-arg HAMI_VGPUMONITOR_IMAGE="example.com/hami:release"`) {
		t.Fatalf("release extraction arguments not forwarded: %v: %s", err, output)
	}
}

func TestPublishingWorkflowsForwardMonitorImage(t *testing.T) {
	for _, name := range []string{"image.yml", "release-image.yml"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../..", ".github/workflows", name))
			if err != nil {
				t.Fatal(err)
			}
			var workflow struct {
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
			job := workflow.Jobs["image"]
			if job.Env["HAMI_VGPUMONITOR_IMAGE"] != "${{ inputs.monitor_image || vars.HAMI_VGPUMONITOR_IMAGE || 'scratch' }}" {
				t.Fatal("monitor image must come from dispatch input or repository variable")
			}
			if job.Env["HAMI_VGPUMONITOR_SOURCE"] != "${{ inputs.monitor_source || vars.HAMI_VGPUMONITOR_SOURCE || 'source' }}" || job.Env["HAMI_VGPUMONITOR_REF"] != "${{ inputs.monitor_ref || vars.HAMI_VGPUMONITOR_REF || 'master' }}" {
				t.Fatal("workflow must default to the HAMi development branch")
			}
			var loaded, forwarded bool
			for _, step := range job.Steps {
				if step.ID == "args" {
					loaded = strings.Contains(step.Run, "validate-monitor-image") && strings.Contains(step.Run, "hami_vgpumonitor_image=$(make print-HAMI_VGPUMONITOR_IMAGE)")
					for _, argument := range []string{"source", "ref"} {
						loaded = loaded && strings.Contains(step.Run, "hami_vgpumonitor_"+argument+"=$(make print-HAMI_VGPUMONITOR_"+strings.ToUpper(argument)+")")
					}
				}
				if strings.HasPrefix(step.Uses, "docker/build-push-action@") {
					forwarded = strings.Contains(step.With["build-args"], "HAMI_VGPUMONITOR_IMAGE=${{ steps.args.outputs.hami_vgpumonitor_image }}")
					for _, argument := range []string{"source", "ref"} {
						forwarded = forwarded && strings.Contains(step.With["build-args"], "HAMI_VGPUMONITOR_"+strings.ToUpper(argument)+"=${{ steps.args.outputs.hami_vgpumonitor_"+argument+" }}")
					}
				}
			}
			if !loaded || !forwarded {
				t.Fatalf("monitor build-argument chain incomplete: loaded=%t forwarded=%t", loaded, forwarded)
			}
		})
	}
}
