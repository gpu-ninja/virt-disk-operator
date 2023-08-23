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

package disk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/go-units"
	"github.com/gpu-ninja/qcow2"
	"github.com/pojntfx/go-nbd/pkg/client"
	"github.com/pojntfx/go-nbd/pkg/server"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// LVMOptions are the options for setting up an LVM logical volume.
type LVMOptions struct {
	// VolumeGroup is the name of the LVM volume group to create.
	VolumeGroup string
	// LogicalVolume is the name of the LVM logical volume to create.
	// It will be allocated to use all available space in the volume group.
	LogicalVolume string
}

// MountOptions are the options for mounting a virtual disk device.
type MountOptions struct {
	// SocketPath is the path to host the NBD server on.
	SocketPath string
	// ImagePath is the path to the qcow2 backing image.
	ImagePath string
	// Size is the size of the qcow2 backing image in bytes.
	Size int64
	// LVM are the options for setting up an LVM logical volume.
	LVM *LVMOptions
}

var isReady bool

// MountVirtualDisk mounts a virtual disk device.
func MountVirtualDisk(ctx context.Context, logger *zap.Logger, opts *MountOptions) error {
	var createImage bool
	if _, err := os.Stat(opts.ImagePath); err != nil {
		createImage = true
	}

	readyForConnCh := make(chan struct{})

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return startReadyzServer(ctx, logger)
	})

	g.Go(func() error {
		return listenForConnections(ctx, logger, opts, createImage, readyForConnCh)
	})

	g.Go(func() error {
		<-readyForConnCh

		return connectToServer(ctx, logger, opts, createImage)
	})

	if err := g.Wait(); err != nil {
		logger.Error("Error while mounting virtual disk device", zap.Error(err))

		return err
	}

	return nil
}

func connectToServer(ctx context.Context, logger *zap.Logger, opts *MountOptions,
	createImage bool) error {
	devicePath, err := findNextFreeNBDDevice()
	if err != nil {
		return fmt.Errorf("could not find free nbd device: %w", err)
	}

	logger.Info("Using NBD device", zap.String("device", devicePath))

	f, err := os.Open(devicePath)
	if err != nil {
		return fmt.Errorf("could not open device file: %w", err)
	}
	defer f.Close()

	conn, err := net.Dial("unix", opts.SocketPath)
	if err != nil {
		return fmt.Errorf("could not connect to socket: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()

		logger.Info("Disconnecting from nbd server")

		if err := client.Disconnect(f); err != nil {
			logger.Error("Failed to disconnect from nbd server", zap.Error(err))
		}
	}()

	logger.Info("Connecting to nbd server", zap.String("socketPath", opts.SocketPath))

	if err := client.Connect(conn, f, &client.Options{
		ExportName: "virt-disk",
		BlockSize:  uint32(client.MaximumBlockSize),
		OnConnected: func() {
			defer func() {
				isReady = true
			}()

			logger.Info("Connected to nbd server")

			if opts.LVM != nil {
				if createImage {
					logger.Info("Setting up logical volume",
						zap.String("volumeGroup", opts.LVM.VolumeGroup),
						zap.String("logicalVolume", opts.LVM.LogicalVolume))

					if err := setupLogicalVolume(logger, devicePath, opts.LVM); err != nil {
						logger.Error("Failed to setup logical volume", zap.Error(err))
					}

					logger.Info("Logical volume setup complete")
				} else {
					logger.Info("Activating logical volume",
						zap.String("volumeGroup", opts.LVM.VolumeGroup),
						zap.String("logicalVolume", opts.LVM.LogicalVolume))

					if err := activateLogicalVolume(logger, opts.LVM); err != nil {
						logger.Error("Failed to activate logical volume", zap.Error(err))
					}

					logger.Info("Logical volume activation complete")
				}
			}
		},
	}); err != nil {
		return fmt.Errorf("could not connect to nbd server: %w", err)
	}

	return nil
}

func listenForConnections(ctx context.Context, logger *zap.Logger, opts *MountOptions,
	createImage bool, readyForConnCh chan struct{}) error {
	lis, err := net.Listen("unix", opts.SocketPath)
	if err != nil {
		return fmt.Errorf("could not listen on socket: %w", err)
	}
	defer lis.Close()

	logger.Info("Listening for connections", zap.String("socketPath", opts.SocketPath))

	var b *qcow2.Image
	if createImage {
		logger.Info("Creating image", zap.String("imagePath", opts.ImagePath),
			zap.String("size", units.HumanSize(float64(opts.Size))))

		b, err = qcow2.Create(opts.ImagePath, opts.Size)
		if err != nil {
			logger.Error("Could not create image", zap.Error(err))

			return fmt.Errorf("could not create image: %w", err)
		}
	} else {
		logger.Info("Opening image", zap.String("imagePath", opts.ImagePath))

		b, err = qcow2.Open(opts.ImagePath, false)
		if err != nil {
			logger.Error("Could not open image", zap.Error(err))

			return fmt.Errorf("could not open image: %w", err)
		}
	}

	ch := make(chan net.Conn)
	go func() {
		for {
			select {
			case <-ctx.Done():
				logger.Info("Shutting down server")

				if err := lis.Close(); err != nil {
					logger.Error("Could not close listener", zap.Error(err))
				}

				if err := b.Close(); err != nil {
					logger.Error("Could not close image", zap.Error(err))
				}

				return
			case conn := <-ch:
				go handleConnection(conn, b, logger)
			}
		}
	}()

	close(readyForConnCh)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			conn, err := lis.Accept()
			if err != nil && errors.Is(err, net.ErrClosed) {
				close(ch)
				return nil
			} else if err != nil {
				logger.Error("Could not accept connection, continuing", zap.Error(err))
				continue
			}

			ch <- conn
		}
	}
}

func handleConnection(conn net.Conn, b *qcow2.Image, logger *zap.Logger) {
	defer func() {
		_ = conn.Close()

		if err := recover(); err != nil {
			logger.Error("Client disconnected with error", zap.Any("error", err))
		}
	}()

	logger.Info("Handling connection")

	if err := server.Handle(
		conn,
		[]*server.Export{
			{
				Name:        "virt-disk",
				Description: "Add virtual disk devices to any Kubernetes node.",
				Backend:     b,
			},
		},
		&server.Options{
			MinimumBlockSize:   uint32(client.MinimumBlockSize),
			PreferredBlockSize: uint32(client.MaximumBlockSize),
			MaximumBlockSize:   uint32(client.MaximumBlockSize),
		}); err != nil {
		logger.Error("Could not handle connection", zap.Error(err))
	}
}

func findNextFreeNBDDevice() (string, error) {
	devices, err := os.ReadDir("/sys/block")
	if err != nil {
		return "", fmt.Errorf("could not read /sys/block: %w", err)
	}

	for _, dev := range devices {
		if strings.HasPrefix(dev.Name(), "nbd") {
			pidFilePath := filepath.Join("/sys/block", dev.Name(), "pid")
			_, err := os.ReadFile(pidFilePath)
			if os.IsNotExist(err) {
				return filepath.Join("/dev/", dev.Name()), nil
			} else if err != nil {
				return "", err
			}
		}
	}

	return "", fmt.Errorf("no free NBD devices found")
}

func setupLogicalVolume(logger *zap.Logger, devicePath string, lvm *LVMOptions) error {
	logger.Info("Creating physical volume")

	cmd := exec.Command("/sbin/pvcreate", "-v", "-Zn", devicePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not run pvcreate: %w", err)
	}

	logger.Info("Creating volume group")

	cmd = exec.Command("/sbin/vgcreate", "-v", lvm.VolumeGroup, devicePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not run pvcreate: %w", err)
	}

	logger.Info("Creating logical volume")

	cmd = exec.Command("/sbin/lvcreate", "-v", "--noudevsync", "-Zn", "-n", lvm.LogicalVolume, "-l", "100%FREE", lvm.VolumeGroup)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not run pvcreate: %w", err)
	}

	logger.Info("Waiting for udev to settle")

	cmd = exec.Command("/usr/bin/udevadm", "settle")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not run udevadm settle: %w", err)
	}

	return nil
}

func activateLogicalVolume(logger *zap.Logger, lvm *LVMOptions) error {
	logger.Info("Activating logical volume")

	cmd := exec.Command("/sbin/lvchange", "-v", "--noudevsync", "-a", "y", lvm.VolumeGroup)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not run lvchange: %w", err)
	}

	return nil
}

func startReadyzServer(ctx context.Context, logger *zap.Logger) error {
	http.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if isReady {
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte("OK")); err != nil {
				logger.Warn("Could not write response", zap.Error(err))
			}
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			if _, err := w.Write([]byte("Service Unavailable")); err != nil {
				logger.Warn("Could not write response", zap.Error(err))
			}
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
