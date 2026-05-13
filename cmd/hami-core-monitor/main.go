/*
 * Copyright 2025 The HAMi Authors.
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
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli/v2"

	"github.com/Project-HAMi/k8s-dra-driver/pkg/common"
	pkgflags "github.com/Project-HAMi/k8s-dra-driver/pkg/flags"
	"github.com/Project-HAMi/k8s-dra-driver/pkg/monitor"
	"github.com/Project-HAMi/k8s-dra-driver/pkg/version"

	"k8s.io/klog/v2"
)

type Flags struct {
	kubeClientConfig pkgflags.KubeClientConfig
	nodeName         string
	hookPath         string
	bindAddr         string
	feedbackInterval time.Duration
}

func main() {
	flags := &Flags{}

	app := &cli.App{
		Name:    "hami-core-monitor",
		Usage:   "HAMi-Core GPU monitor and metrics exporter",
		Version: version.Version(),
		Flags: append(flags.kubeClientConfig.Flags(),
			&cli.StringFlag{
				Name:        "node-name",
				Usage:       "Name of the node this monitor is running on",
				Destination: &flags.nodeName,
				EnvVars:     []string{"NODE_NAME"},
			},
			&cli.StringFlag{
				Name:        "hook-path",
				Usage:       "Host path where vGPU hooks and claim caches are mounted",
				Value:       "/usr/local/vgpu",
				Destination: &flags.hookPath,
			},
			&cli.StringFlag{
				Name:        "bind-address",
				Usage:       "The address the metric endpoint binds to",
				Value:       ":9394",
				Destination: &flags.bindAddr,
			},
			&cli.DurationFlag{
				Name:        "feedback-interval",
				Usage:       "Interval between soft-QoS feedback evaluations",
				Value:       5 * time.Second,
				Destination: &flags.feedbackInterval,
			},
		),
		Action: func(c *cli.Context) error {
			return run(c.Context, flags)
		},
	}

	if err := app.Run(os.Args); err != nil {
		klog.Fatalf("Failed to run monitor: %v", err)
	}
}

func run(ctx context.Context, flags *Flags) error {
	common.StartDebugSignalHandlers()

	if flags.nodeName == "" {
		return fmt.Errorf("--node-name or NODE_NAME must be set")
	}
	if flags.feedbackInterval <= 0 {
		return fmt.Errorf("feedback-interval must be positive")
	}

	claimLister := monitor.NewClaimLister(flags.hookPath)

	var mapper *ClaimMapper
	clientsets, err := flags.kubeClientConfig.NewClientSets()
	if err != nil {
		klog.ErrorS(err, "Failed to build Kubernetes clients; claim-to-pod mapping disabled")
	} else {
		mapper = NewClaimMapper(clientsets.Core, flags.nodeName)
		go mapper.Start(ctx)
	}

	reg := prometheus.NewRegistry()
	collector := newCollector(claimLister, mapper)
	reg.MustRegister(collector)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	srv := &http.Server{Addr: flags.bindAddr, Handler: mux}
	go func() {
		klog.InfoS("Starting metrics server", "addr", flags.bindAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.ErrorS(err, "Metrics server failed")
		}
	}()

	go watchAndFeedback(ctx, claimLister, flags.feedbackInterval)

	<-ctx.Done()
	klog.InfoS("Shutting down monitor")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
