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
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/util/wait"
)

// MountOptions are the options for mounting a virtual disk device.
type MountOptions struct {
	// ImagePath is the path to the qcow2 backing image.
	ImagePath string
	// Size is the size of the qcow2 backing image in bytes.
	Size int64
	// VolumeGroup is the name of the optional LVM volume group to create.
	VolumeGroup string
	// LogicalVolume is the name of the optional LVM logical volume to create.
	LogicalVolume string
}

var isReady bool

// MountVirtualDisk mounts a virtual disk device.
func MountVirtualDisk(ctx context.Context, logger *zap.Logger, opts *MountOptions) error {
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return startReadyzServer(ctx, logger)
	})

	g.Go(func() error {
		if _, err := os.Stat(opts.ImagePath); os.IsNotExist(err) {
			logger.Info("Backing image does not exist, creating it")

			if err := execCommand(ctx, "/usr/bin/qemu-img", "create", "-f", "qcow2", opts.ImagePath, strconv.FormatInt(opts.Size, 10)); err != nil {
				return fmt.Errorf("could not create backing image: %w", err)
			}
		}

		devicePath, err := findNextFreeNBDDevice()
		if err != nil {
			return fmt.Errorf("could not find free nbd device: %w", err)
		}

		logger.Info("Using NBD device", zap.String("device", devicePath))

		pidFilePath := "/run/qemu-nbd.pid"
		socketPath := "/run/qemu-nbd.sock"

		if err := execCommand(ctx, "/usr/bin/qemu-nbd", "-k", socketPath, "--pid-file", pidFilePath, "-f", "qcow2", "-c", devicePath, opts.ImagePath); err != nil {
			return fmt.Errorf("could not run qemu-nbd: %w", err)
		}

		defer func() {
			if err := execCommand(ctx, "/usr/bin/qemu-nbd", "-k", socketPath, "-d", devicePath); err != nil {
				logger.Warn("Could not stop qemu-nbd", zap.Error(err))
			}

			if err := os.Remove(pidFilePath); err != nil {
				logger.Warn("Could not remove qemu-nbd pid file", zap.Error(err))
			}

			if err := os.Remove(socketPath); err != nil {
				logger.Warn("Could not remove qemu-nbd socket", zap.Error(err))
			}
		}()

		err = wait.PollUntilContextTimeout(ctx, time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
			size, err := blockDeviceSize(devicePath)
			if err != nil {
				return false, fmt.Errorf("could not get block device size: %w", err)
			}

			return size > 0, nil
		})
		if err != nil {
			return fmt.Errorf("failed to wait for block device to appear: %w", err)
		}

		if opts.VolumeGroup != "" {
			logger.Info("Checking if device already contains an lvm volume")

			err = execCommand(ctx, "/sbin/pvck", "-v", devicePath)
			if err != nil {
				logger.Info("No lvm volume found, creating",
					zap.String("volumeGroup", opts.VolumeGroup),
					zap.String("logicalVolume", opts.LogicalVolume))

				if err := createLVMVolume(ctx, logger, devicePath, opts); err != nil {
					return fmt.Errorf("could not create lvm volume: %w", err)
				}
			}

			logger.Info("Activating volume group",
				zap.String("volumeGroup", opts.VolumeGroup))

			if err := activateVolumeGroup(ctx, logger, opts); err != nil {
				return fmt.Errorf("could not activate volume group: %w", err)
			}

			logger.Info("LVM setup complete")
		}

		isReady = true

		pidStr, err := os.ReadFile(pidFilePath)
		if err != nil {
			return fmt.Errorf("could not read qemu-nbd pid file: %w", err)
		}

		pid, err := strconv.Atoi(strings.TrimSpace(string(pidStr)))
		if err != nil {
			return fmt.Errorf("could not parse qemu-nbd pid: %w", err)
		}

		logger.Info("Waiting for qemu-nbd to exit")

		_ = wait.PollUntilContextTimeout(ctx, time.Second, time.Duration(math.MaxInt64), true, func(ctx context.Context) (bool, error) {
			if _, err := os.FindProcess(pid); err != nil {
				return true, nil
			}

			return false, nil
		})

		// context has already been cancelled.
		if opts.VolumeGroup != "" {
			if err := deactivateVolumeGroup(context.Background(), logger, opts); err != nil {
				logger.Warn("Failed to deactivate logical volume", zap.Error(err))
			}
		}

		return nil
	})

	if err := g.Wait(); err != nil {
		logger.Error("Error while mounting virtual disk device", zap.Error(err))

		return err
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

	return "", fmt.Errorf("no free nbd devices found")
}

func createLVMVolume(ctx context.Context, logger *zap.Logger, devicePath string, opts *MountOptions) error {
	logger.Info("Creating physical volume")

	err := execCommand(ctx, "/sbin/pvcreate", "-v", devicePath)
	if err != nil {
		return fmt.Errorf("could not run pvcreate: %w", err)
	}

	logger.Info("Creating volume group")

	err = execCommand(ctx, "/sbin/vgcreate", "-v", opts.VolumeGroup, devicePath)
	if err != nil {
		return fmt.Errorf("could not run vgcreate: %w", err)
	}

	if opts.LogicalVolume != "" {
		logger.Info("Creating logical volume")

		err := execCommand(ctx, "/sbin/lvcreate", "-v", "-an", "-n", opts.LogicalVolume, "-l", "100%FREE", opts.VolumeGroup)
		if err != nil {
			return fmt.Errorf("could not run lvcreate: %w", err)
		}
	}

	return nil
}

func activateVolumeGroup(ctx context.Context, logger *zap.Logger, opts *MountOptions) error {
	logger.Info("Activating logical volume")

	err := retry.Do(func() error {
		err := execCommand(ctx, "/sbin/lvchange", "-v", "-ay", opts.VolumeGroup)
		if err != nil {
			logger.Warn("Could not activate logical volume", zap.Error(err))

			if err := execCommand(ctx, "/usr/sbin/dmsetup", "remove", lvmName(opts)); err != nil {
				logger.Warn("Could not reset devmapper device", zap.Error(err))
			}

			return err
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("could not run lvchange: %w", err)
	}

	return nil
}

func deactivateVolumeGroup(ctx context.Context, logger *zap.Logger, opts *MountOptions) error {
	logger.Info("Deactivating logical volume")

	err := execCommand(ctx, "/sbin/lvchange", "-v", "-an", opts.VolumeGroup)
	if err != nil {
		return fmt.Errorf("could not run lvchange: %w", err)
	}

	return nil
}

func blockDeviceSize(devicePath string) (int64, error) {
	sizeStr, err := os.ReadFile(filepath.Join("/sys/block", filepath.Base(devicePath), "size"))
	if err != nil {
		return 0, fmt.Errorf("could not read block device size: %w", err)
	}

	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeStr)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("could not parse block device size: %w", err)
	}

	return size, nil
}

func execCommand(ctx context.Context, name string, arg ...string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, name, arg...)
	cmd.Env = os.Environ()

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start command: %w", err)
	}

	go func() {
		<-cmdCtx.Done()

		if _, err := os.FindProcess(cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
		}
	}()

	return cmd.Wait()
}

func lvmName(opts *MountOptions) string {
	lvmEscape := func(input string) string {
		return strings.ReplaceAll(input, "-", "--")
	}

	return fmt.Sprintf("%s-%s", lvmEscape(opts.VolumeGroup), lvmEscape(opts.LogicalVolume))
}
