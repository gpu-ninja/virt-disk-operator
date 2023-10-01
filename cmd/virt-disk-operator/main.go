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
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

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

	"github.com/docker/go-units"
	virtdiskv1alpha1 "github.com/gpu-ninja/virt-disk-operator/api/v1alpha1"
	"github.com/gpu-ninja/virt-disk-operator/internal/controller"
	"github.com/gpu-ninja/virt-disk-operator/internal/virtualdisk"
	disk "github.com/gpu-ninja/virt-disk-operator/internal/virtualdisk"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
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
	app := &cli.App{
		Name:  "virt-disk-operator",
		Usage: "A Kubernetes operator for creating virtual disk devices.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "zap-log-level",
				Value: "info",
				Usage: "Zap Level to configure the verbosity of logging.",
			},
			&cli.StringFlag{
				Name:  "metrics-bind-address",
				Value: ":8080",
				Usage: "The address the metric endpoint binds to.",
			},
			&cli.StringFlag{
				Name:  "health-probe-bind-address",
				Value: ":8081",
				Usage: "The address the probe endpoint binds to.",
			},
			&cli.BoolFlag{
				Name:  "leader-elect",
				Value: false,
				Usage: "Enable leader election for controller manager.",
			},
		},
		Before: createLogger,
		Action: startManager,
		Commands: []*cli.Command{
			{
				Name:  "attach",
				Usage: "Attach a virtual disk device.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "image",
						Usage:    "Path to the qcow2 backing image.",
						Required: true,
					},
					&cli.StringFlag{
						Name:     "size",
						Usage:    "Disk size (e.g., 1G, 10G, 1T).",
						Required: true,
					},
					&cli.StringFlag{
						Name:     "vg-name",
						Usage:    "Name of the volume group to create.",
						Required: true,
					},
					&cli.StringFlag{
						Name:  "lv-name",
						Usage: "Name of the optional logical volume to create.",
					},
					&cli.StringFlag{
						Name:  "key-file",
						Usage: "Path to the encryption key file.",
					},
				},
				Before: createLogger,
				Action: attachVirtualDisk,
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
		Metrics:                metricsserver.Options{BindAddress: cCtx.String("metrics-bind-address")},
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

func attachVirtualDisk(cCtx *cli.Context) error {
	ctx := ctrl.SetupSignalHandler()

	sizeBytes, err := units.FromHumanSize(cCtx.String("size"))
	if err != nil {
		return fmt.Errorf("unable to parse size: %w", err)
	}

	opts := &disk.AttachOptions{
		Image:                 cCtx.String("image"),
		Size:                  sizeBytes,
		VolumeGroup:           cCtx.String("vg-name"),
		LogicalVolume:         cCtx.String("lv-name"),
		EncryptionKeyFilePath: cCtx.String("key-file"),
	}

	readyCh := make(chan struct{}, 1)
	defer close(readyCh)

	go func() {
		if _, ok := <-readyCh; ok {
			if err := startReadyzServer(ctx, logger); err != nil {
				logger.Error("failed to start readyz server", zap.Error(err))
			}
		}
	}()

	if err := virtualdisk.Attach(ctx, logger, opts, readyCh); err != nil {
		return fmt.Errorf("unable to attach virtual disk: %w", err)
	}

	return nil
}

func startReadyzServer(ctx context.Context, logger *zap.Logger) error {
	http.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
			logger.Warn("Could not write response", zap.Error(err))
		}
	})

	server := &http.Server{
		Addr: ":8081",
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("could not start readyz server: %w", err)
	}

	return nil
}
