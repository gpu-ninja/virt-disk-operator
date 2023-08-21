/* SPDX-License-Identifier: Apache-2.0
 *
 * Copyright 2023 Damian Peckett <damian@pecke.tt>.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"log"
	"os"

	zaplogfmt "github.com/jsternberg/zap-logfmt"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"github.com/gpu-ninja/operator-utils/zaplogr"

	"github.com/adrg/xdg"
	"github.com/docker/go-units"
	virtdiskv1alpha1 "github.com/gpu-ninja/virt-disk-operator/api/v1alpha1"
	"github.com/gpu-ninja/virt-disk-operator/internal/controller"
	"github.com/gpu-ninja/virt-disk-operator/internal/disk"
	// +kubebuilder:scaffold:imports
)

var (
	scheme = runtime.NewScheme()
	logger *zap.Logger
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(virtdiskv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	defaultSocketPath, err := xdg.RuntimeFile("virt-disk.sock")
	if err != nil {
		log.Fatal(err)
	}

	app := &cli.App{
		Name:  "virt-disk-operator",
		Usage: "a Kubernetes operator for managing virtual disk devices",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "zap-log-level",
				Value: "info",
				Usage: "Zap Level to configure the verbosity of logging",
			},
			&cli.StringFlag{
				Name:  "metrics-bind-address",
				Value: ":8080",
				Usage: "The address the metric endpoint binds to",
			},
			&cli.StringFlag{
				Name:  "health-probe-bind-address",
				Value: ":8081",
				Usage: "The address the probe endpoint binds to",
			},
			&cli.BoolFlag{
				Name:  "leader-elect",
				Value: false,
				Usage: "Enable leader election for controller manager",
			},
		},
		Before: createLogger,
		Action: startManager,
		Commands: []*cli.Command{
			{
				Name:  "mount",
				Usage: "mount a virtual disk device",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "image",
						Usage:    "path to the backing image file",
						Required: true,
					},
					&cli.StringFlag{
						Name:     "size",
						Usage:    "disk size (e.g., 1G, 10G, 1T)",
						Required: true,
					},
					&cli.StringFlag{
						Name:  "socket",
						Usage: "path of the nbd socket",
						Value: defaultSocketPath,
					},
					&cli.StringFlag{
						Name:  "vg",
						Usage: "name of a volume group to create",
					},
					&cli.StringFlag{
						Name:  "lv",
						Usage: "name of a logical volume to create",
					},
				},
				Before: createLogger,
				Action: mountVirtualDisk,
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func createLogger(cCtx *cli.Context) error {
	lvl, err := zapcore.ParseLevel(cCtx.String("zap-log-level"))
	if err != nil {
		return fmt.Errorf("unable to parse log level: %w", err)
	}

	config := zap.NewProductionEncoderConfig()
	logger = zap.New(zapcore.NewCore(
		zaplogfmt.NewEncoder(config),
		os.Stdout,
		lvl,
	))

	ctrl.SetLogger(zaplogr.New(logger))

	return nil
}

func startManager(cCtx *cli.Context) error {
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     cCtx.String("metrics-bind-address"),
		Port:                   9443,
		HealthProbeBindAddress: cCtx.String("health-probe-bind-address"),
		LeaderElection:         cCtx.Bool("leader-elect"),
		LeaderElectionID:       "49e56cbc.gpu-ninja.com",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("unable to start manager: %w", err)
	}

	if err = (&controller.VirtualDiskReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("VirtualDisk-controller"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create controller: %w", err)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}

	logger.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("problem running manager: %w", err)
	}

	return nil
}

func mountVirtualDisk(cCtx *cli.Context) error {
	sizeBytes, err := units.FromHumanSize(cCtx.String("size"))
	if err != nil {
		return fmt.Errorf("unable to parse size: %w", err)
	}

	opts := &disk.MountOptions{
		SocketPath: cCtx.String("socket"),
		ImagePath:  cCtx.String("image"),
		Size:       sizeBytes,
	}

	if cCtx.IsSet("vg") || cCtx.IsSet("lv") {
		opts.LVM = &disk.LVMOptions{
			VolumeGroup:   cCtx.String("vg"),
			LogicalVolume: cCtx.String("lv"),
		}
	}

	return disk.MountVirtualDisk(ctrl.SetupSignalHandler(), logger, opts)
}
