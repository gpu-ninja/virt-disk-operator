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

package virtualdisk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/wait"
)

// AttachOptions are the options for attaching a virtual disk device.
type AttachOptions struct {
	// Image is the path to the qcow2 backing image.
	Image string
	// Size is the size of the qcow2 backing image in bytes.
	Size int64
	// VolumeGroup is the name of the LVM volume group to create.
	VolumeGroup string
	// LogicalVolume is the name of the optional LVM logical volume to create.
	LogicalVolume string
	// EncryptionKeyFilePath is the optional encryption key file to use for LUKS.
	EncryptionKeyFilePath string
	// PIDFilePath is the optional path to the qemu-nbd pid file.
	// If not specified, /run/qemu-nbd.pid is used.
	PIDFilePath string
	// SocketPath is the optional path to the qemu-nbd socket.
	// If not specified, /run/qemu-nbd.sock is used.
	SocketPath string
}

// Attach attachs a virtual disk device.
func Attach(ctx context.Context, logger *zap.Logger, opts *AttachOptions, readyCh chan struct{}) error {
	if _, err := os.Stat(opts.Image); errors.Is(err, os.ErrNotExist) {
		logger.Info("Backing image does not exist, creating it")

		if err := execCommand(ctx, time.Minute, "/usr/bin/qemu-img", "create", "-f", "qcow2", opts.Image, strconv.FormatInt(opts.Size, 10)); err != nil {
			return fmt.Errorf("could not create backing image: %w", err)
		}
	}

	devicePath, err := findNextFreeNBDDevice()
	if err != nil {
		return fmt.Errorf("could not find free nbd device: %w", err)
	}

	logger.Info("Using NBD device", zap.String("device", devicePath))

	pidFilePath := opts.PIDFilePath
	if pidFilePath == "" {
		pidFilePath = "/run/qemu-nbd.pid"
	}

	socketPath := opts.SocketPath
	if socketPath == "" {
		socketPath = "/run/qemu-nbd.sock"
	}

	if err := execCommand(ctx, 10*time.Second, "/usr/bin/qemu-nbd", "-k", socketPath, "--pid-file", pidFilePath, "-f", "qcow2", "-c", devicePath, opts.Image); err != nil {
		return fmt.Errorf("could not run qemu-nbd: %w", err)
	}

	go func() {
		<-ctx.Done()

		shutdownCtx := context.Background()

		if err := deactivateVolume(shutdownCtx, logger, opts); err != nil {
			logger.Warn("Failed to deactivate volume", zap.Error(err))
		}

		logger.Info("Stopping qemu-nbd")

		if err := execCommand(shutdownCtx, 10*time.Second, "/usr/bin/qemu-nbd", "-k", socketPath, "-d", devicePath); err != nil {
			logger.Warn("Could not stop qemu-nbd", zap.Error(err))
		}

		if err := os.Remove(pidFilePath); err != nil {
			logger.Warn("Could not remove qemu-nbd pid file", zap.Error(err))
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

	logger.Info("Checking if device already contains an lvm / luks volume")

	var needsCreation bool
	if opts.EncryptionKeyFilePath != "" {
		if err := execCommand(ctx, 10*time.Second, "/sbin/cryptsetup", "isLuks", "--debug", devicePath); err != nil {
			needsCreation = true
		}
	} else {
		if err := execCommand(ctx, 10*time.Second, "/sbin/pvck", "-v", devicePath); err != nil {
			needsCreation = true
		}
	}

	if needsCreation {
		logger.Info("No volume found, creating",
			zap.String("volumeGroup", opts.VolumeGroup),
			zap.String("logicalVolume", opts.LogicalVolume))

		if err := createVolume(ctx, logger, devicePath, opts); err != nil {
			if err := os.RemoveAll(opts.Image); err != nil {
				logger.Warn("Could not remove backing image", zap.Error(err))
			}

			return fmt.Errorf("could not create volume: %w", err)
		}
	} else {
		logger.Info("Found existing volume")
	}

	logger.Info("Activating volume",
		zap.String("volumeGroup", opts.VolumeGroup))

	if err := activateVolume(ctx, logger, devicePath, opts); err != nil {
		return fmt.Errorf("could not activate volume: %w", err)
	}

	readyCh <- struct{}{}

	pidBytes, err := os.ReadFile(pidFilePath)
	if err != nil {
		return fmt.Errorf("could not read qemu-nbd pid: %w", err)
	}

	qemuNBDPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		return fmt.Errorf("could not parse qemu-nbd pid: %w", err)
	}

	logger.Info("Waiting for qemu-nbd to exit",
		zap.Int("pid", qemuNBDPID))

	for {
		p, err := os.FindProcess(qemuNBDPID)
		if err != nil {
			break
		}

		if err := p.Signal(os.Signal(syscall.Signal(0))); err != nil {
			break
		}

		time.Sleep(time.Second)
	}

	// Bit of a hack, but for some reason it can take a while for qemu-nbd file locks to be released
	time.Sleep(5 * time.Second)

	return nil
}

func findNextFreeNBDDevice() (string, error) {
	dir, err := os.Open("/sys/block")
	if err != nil {
		return "", fmt.Errorf("could not open /sys/block: %w", err)
	}
	defer dir.Close()

	devices, err := dir.Readdirnames(-1)
	if err != nil {
		return "", fmt.Errorf("could not read /sys/block: %w", err)
	}

	var nbdDevices []string
	for _, dev := range devices {
		if strings.HasPrefix(dev, "nbd") {
			nbdDevices = append(nbdDevices, dev)
		}
	}

	for _, dev := range nbdDevices {
		pidFilePath := filepath.Join("/sys/block", dev, "pid")
		_, err := os.ReadFile(pidFilePath)
		if errors.Is(err, os.ErrNotExist) {
			return filepath.Join("/dev/", dev), nil
		} else if err != nil {
			return "", err
		}
	}

	return "", fmt.Errorf("no free nbd devices found")
}

func createVolume(ctx context.Context, logger *zap.Logger, devicePath string, opts *AttachOptions) error {
	if opts.LogicalVolume != "" {
		devMapperDevicePath := filepath.Join("/dev/mapper", lvmName(opts))
		if _, err := os.Stat(devMapperDevicePath); err == nil {
			logger.Info("Cleaning up existing devmapper device",
				zap.String("device", devMapperDevicePath))

			err := execCommand(ctx, 10*time.Second, "/usr/sbin/dmsetup", "remove", "-v", lvmName(opts))
			if err != nil {
				return fmt.Errorf("could not reset devmapper device: %w", err)
			}
		}
	}

	if opts.EncryptionKeyFilePath != "" {
		logger.Info("Creating luks volume",
			zap.String("device", devicePath))

		err := execCommand(ctx, 5*time.Minute, "/sbin/cryptsetup", "luksFormat", "--debug", "-q", "--type", "luks2",
			// We use a very strong random keyfile, so we can lower the KDF iterations and memory usage.
			// This helps prevent the consumption of excessive CPU and memory resources.
			"--pbkdf", "argon2id", "--pbkdf-force-iterations", "4", "--pbkdf-memory", "64000",
			"--key-file", opts.EncryptionKeyFilePath, devicePath)
		if err != nil {
			return fmt.Errorf("could not create luks volume: %w", err)
		}

		cryptoDevicePath := "/dev/mapper/crypto_" + lvmEscape(opts.VolumeGroup)

		if _, err := os.Stat(cryptoDevicePath); err == nil {
			logger.Info("Cleaning up existing crypto devmapper device",
				zap.String("device", cryptoDevicePath))

			err := execCommand(ctx, 10*time.Second, "/usr/sbin/dmsetup", "remove", "-v", filepath.Base(cryptoDevicePath))
			if err != nil {
				return fmt.Errorf("could not reset devmapper device: %w", err)
			}
		}

		logger.Info("Opening encrypted luks volume",
			zap.String("device", devicePath),
			zap.String("cryptoDevice", cryptoDevicePath))

		err = execCommand(ctx, time.Minute, "/sbin/cryptsetup", "open", "--debug", "--type", "luks2",
			"--key-file", opts.EncryptionKeyFilePath,
			devicePath, filepath.Base(cryptoDevicePath))
		if err != nil {
			return fmt.Errorf("could not open luks volume: %w", err)
		}

		logger.Info("Zeroing first 16MB of crypto device so it will be detected as empty",
			zap.String("device", cryptoDevicePath))

		err = execCommand(ctx, 10*time.Second, "/usr/bin/dd", "if=/dev/zero", "of="+cryptoDevicePath, "bs=1M", "count=16")
		if err != nil {
			return fmt.Errorf("could not wipe crypto device: %w", err)
		}

		devicePath = cryptoDevicePath
	}

	logger.Info("Creating physical volume",
		zap.String("device", devicePath))

	err := execCommand(ctx, 10*time.Second, "/sbin/pvcreate", "-v", devicePath)
	if err != nil {
		return fmt.Errorf("failed to create physical volume: %w", err)
	}

	logger.Info("Creating volume group",
		zap.String("device", devicePath))

	err = execCommand(ctx, 10*time.Second, "/sbin/vgcreate", "-v", opts.VolumeGroup, devicePath)
	if err != nil {
		return fmt.Errorf("failed to create volume group: %w", err)
	}

	if opts.LogicalVolume != "" {
		logger.Info("Creating logical volume")

		err := execCommand(ctx, 10*time.Second, "/sbin/lvcreate", "-v", "-an", "-n", opts.LogicalVolume, "-l", "100%FREE", opts.VolumeGroup)
		if err != nil {
			return fmt.Errorf("failed to create logical volume: %w", err)
		}
	}

	if opts.EncryptionKeyFilePath != "" {
		cryptoDevicePath := "/dev/mapper/crypto_" + lvmEscape(opts.VolumeGroup)

		logger.Info("Closing luks volume",
			zap.String("device", devicePath))

		err := execCommand(ctx, time.Minute, "/sbin/cryptsetup", "close", "--debug", filepath.Base(cryptoDevicePath))
		if err != nil {
			return fmt.Errorf("could not close luks volume: %w", err)
		}
	}

	return nil
}

func activateVolume(ctx context.Context, logger *zap.Logger, devicePath string, opts *AttachOptions) error {
	devMapperDevicePath := filepath.Join("/dev/mapper", lvmName(opts))
	if _, err := os.Stat(devMapperDevicePath); err == nil {
		logger.Info("Cleaning up existing devmapper device",
			zap.String("device", devMapperDevicePath))

		err := execCommand(ctx, 10*time.Second, "/usr/sbin/dmsetup", "remove", "-v", lvmName(opts))
		if err != nil {
			return fmt.Errorf("could not reset devmapper device: %w", err)
		}
	}

	if opts.EncryptionKeyFilePath != "" {
		cryptoDevicePath := "/dev/mapper/crypto_" + lvmEscape(opts.VolumeGroup)

		if _, err := os.Stat(cryptoDevicePath); err == nil {
			logger.Info("Cleaning up existing crypto devmapper device",
				zap.String("device", cryptoDevicePath))

			err := execCommand(ctx, 10*time.Second, "/usr/sbin/dmsetup", "remove", "-v", filepath.Base(cryptoDevicePath))
			if err != nil {
				return fmt.Errorf("could not reset devmapper device: %w", err)
			}
		}

		logger.Info("Opening encrypted luks volume",
			zap.String("device", devicePath),
			zap.String("cryptoDevice", cryptoDevicePath))

		err := execCommand(ctx, time.Minute, "/sbin/cryptsetup", "open", "--debug", "--type", "luks2",
			"--key-file", opts.EncryptionKeyFilePath, devicePath, filepath.Base(cryptoDevicePath))
		if err != nil {
			return fmt.Errorf("could not open luks volume: %w", err)
		}
	}

	logger.Info("Activating volume group")

	err := execCommand(ctx, 10*time.Second, "/sbin/lvchange", "-v", "-ay", opts.VolumeGroup)
	if err != nil {
		return fmt.Errorf("failed to activate volume group: %w", err)
	}

	return nil
}

func deactivateVolume(ctx context.Context, logger *zap.Logger, opts *AttachOptions) error {
	logger.Info("Deactivating volume group")

	err := execCommand(ctx, 10*time.Second, "/sbin/lvchange", "-v", "-an", opts.VolumeGroup)
	if err != nil {
		return fmt.Errorf("failed to deactivate volume group: %w", err)
	}

	if opts.EncryptionKeyFilePath != "" {
		cryptoDevicePath := "/dev/mapper/crypto_" + lvmEscape(opts.VolumeGroup)

		logger.Info("Closing luks volume",
			zap.String("device", cryptoDevicePath))

		err := execCommand(ctx, 10*time.Second, "/sbin/cryptsetup", "close", "--debug", filepath.Base(cryptoDevicePath))
		if err != nil {
			return fmt.Errorf("could not close luks volume: %w", err)
		}
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

func execCommand(ctx context.Context, timeout time.Duration, name string, arg ...string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, name, arg...)
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

func lvmName(opts *AttachOptions) string {
	return fmt.Sprintf("%s-%s", lvmEscape(opts.VolumeGroup), lvmEscape(opts.LogicalVolume))
}

func lvmEscape(input string) string {
	return strings.ReplaceAll(input, "-", "--")
}
