//go:build privileged
// +build privileged

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

package virtualdisk_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/go-units"
	"github.com/gpu-ninja/virt-disk-operator/internal/virtualdisk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

type LsblkResponse struct {
	BlockDevices []struct {
		Name string `json:"name"`
		Size string `json:"size"`
		Type string `json:"type"`
	} `json:"blockdevices"`
}

func TestAttach(t *testing.T) {
	logger := zaptest.NewLogger(t)

	parentCtx := context.Background()

	err := os.MkdirAll("/run/qemu-nbd", 0o755)
	require.NoError(t, err)

	t.Run("New", func(t *testing.T) {
		id := fmt.Sprintf("test_attach_%d", time.Now().Unix())

		imagePath := filepath.Join(t.TempDir(), "image.qcow2")

		opts := virtualdisk.AttachOptions{
			Image:         imagePath,
			Size:          64 * units.MB,
			VolumeGroup:   id + "_vg",
			LogicalVolume: id + "_lv",
			PIDFilePath:   filepath.Join("/run/qemu-nbd", id+".pid"),
			SocketPath:    filepath.Join("/run/qemu-nbd", id+".sock"),
		}

		ctx, cancel := context.WithCancel(parentCtx)
		defer cancel()

		readyCh := make(chan struct{}, 1)

		go func() {
			if err := virtualdisk.Attach(ctx, logger, &opts, readyCh); err != nil {
				logger.Error("failed to attach", zap.Error(err))
				close(readyCh)
			}
		}()

		_, ok := <-readyCh
		require.True(t, ok, "failed to attach virtual disk")

		output, err := exec.CommandContext(ctx, "lsblk", "-o", "NAME,SIZE,TYPE", "-J",
			filepath.Join("/dev/mapper", fmt.Sprintf("%s-%s", opts.VolumeGroup, opts.LogicalVolume))).CombinedOutput()
		require.NoError(t, err, "failed to run lsblk on devmapper device: "+string(output))

		t.Log("Detaching virtual disk")

		cancel()
		for {
			_, err := os.Stat(opts.PIDFilePath)
			if err != nil {
				break
			}

			time.Sleep(100 * time.Millisecond)
		}

		var resp LsblkResponse
		err = json.Unmarshal(output, &resp)
		require.NoError(t, err)

		assert.Len(t, resp.BlockDevices, 1)
		assert.Equal(t, "lvm", resp.BlockDevices[0].Type)

		size, err := units.FromHumanSize(resp.BlockDevices[0].Size)
		require.NoError(t, err)

		assert.NotZero(t, size)
	})

	t.Run("New Encrypted", func(t *testing.T) {
		id := fmt.Sprintf("test_attach_enc_%d", time.Now().Unix())

		imagePath := filepath.Join(t.TempDir(), "image.qcow2")

		keyFilePath, err := generateEncryptionKey(t)
		require.NoError(t, err)

		opts := virtualdisk.AttachOptions{
			Image:                 imagePath,
			Size:                  64 * units.MB,
			VolumeGroup:           id + "_vg",
			LogicalVolume:         id + "_lv",
			EncryptionKeyFilePath: keyFilePath,
			PIDFilePath:           filepath.Join("/run/qemu-nbd", id+".pid"),
			SocketPath:            filepath.Join("/run/qemu-nbd", id+".sock"),
		}

		ctx, cancel := context.WithCancel(parentCtx)
		defer cancel()

		readyCh := make(chan struct{}, 1)

		go func() {
			if err := virtualdisk.Attach(ctx, logger, &opts, readyCh); err != nil {
				logger.Error("failed to attach", zap.Error(err))
				close(readyCh)
			}
		}()

		_, ok := <-readyCh
		require.True(t, ok, "failed to attach virtual disk")

		output, err := exec.CommandContext(ctx, "lsblk", "-o", "NAME,SIZE,TYPE", "-J",
			filepath.Join("/dev/mapper", fmt.Sprintf("%s-%s", opts.VolumeGroup, opts.LogicalVolume))).CombinedOutput()
		require.NoError(t, err, "failed to run lsblk on devmapper device: "+string(output))

		t.Log("Detaching virtual disk")

		cancel()
		for {
			_, err := os.Stat(opts.PIDFilePath)
			if err != nil {
				break
			}

			time.Sleep(100 * time.Millisecond)
		}

		var resp LsblkResponse
		err = json.Unmarshal(output, &resp)
		require.NoError(t, err)

		assert.Len(t, resp.BlockDevices, 1)
		assert.Equal(t, "lvm", resp.BlockDevices[0].Type)

		size, err := units.FromHumanSize(resp.BlockDevices[0].Size)
		require.NoError(t, err)

		assert.NotZero(t, size)
	})

	t.Run("Existing", func(t *testing.T) {
		id := fmt.Sprintf("test_reattach_%d", time.Now().Unix())

		imagePath := filepath.Join(t.TempDir(), "image.qcow2")

		opts := virtualdisk.AttachOptions{
			Image:         imagePath,
			Size:          64 * units.MB,
			VolumeGroup:   id + "_vg",
			LogicalVolume: id + "_lv",
			PIDFilePath:   filepath.Join("/run/qemu-nbd", id+".pid"),
			SocketPath:    filepath.Join("/run/qemu-nbd", id+".sock"),
		}

		ctx, cancel := context.WithCancel(parentCtx)
		defer cancel()

		readyCh := make(chan struct{}, 1)

		go func() {
			if err := virtualdisk.Attach(ctx, logger, &opts, readyCh); err != nil {
				logger.Error("failed to attach", zap.Error(err))
				close(readyCh)
			}
		}()

		_, ok := <-readyCh
		require.True(t, ok, "failed to attach virtual disk")

		t.Log("Detaching virtual disk")

		cancel()
		for {
			_, err := os.Stat(opts.PIDFilePath)
			if err != nil {
				break
			}

			time.Sleep(100 * time.Millisecond)
		}

		err = exec.CommandContext(ctx, "lsblk",
			filepath.Join("/dev/mapper", fmt.Sprintf("%s-%s", opts.VolumeGroup, opts.LogicalVolume))).Run()
		require.Error(t, err)

		t.Log("Reattaching virtual disk")

		ctx, cancel = context.WithCancel(parentCtx)
		defer cancel()

		readyCh = make(chan struct{}, 1)

		go func() {
			if err := virtualdisk.Attach(ctx, logger, &opts, readyCh); err != nil {
				logger.Error("failed to attach", zap.Error(err))
				close(readyCh)
			}
		}()

		_, ok = <-readyCh
		require.True(t, ok, "failed to reattach virtual disk")

		output, err := exec.CommandContext(ctx, "lsblk", "-o", "NAME,SIZE,TYPE", "-J",
			filepath.Join("/dev/mapper", fmt.Sprintf("%s-%s", opts.VolumeGroup, opts.LogicalVolume))).CombinedOutput()
		require.NoError(t, err)

		t.Log("Detaching virtual disk")

		cancel()
		for {
			_, err := os.Stat(opts.PIDFilePath)
			if err != nil {
				break
			}

			time.Sleep(100 * time.Millisecond)
		}

		var resp LsblkResponse
		err = json.Unmarshal(output, &resp)
		require.NoError(t, err)

		assert.Len(t, resp.BlockDevices, 1)
		assert.Equal(t, "lvm", resp.BlockDevices[0].Type)

		size, err := units.FromHumanSize(resp.BlockDevices[0].Size)
		require.NoError(t, err)

		assert.NotZero(t, size)
	})

	t.Run("Existing Encrypted", func(t *testing.T) {
		id := fmt.Sprintf("test_reattach_enc_%d", time.Now().Unix())

		imagePath := filepath.Join(t.TempDir(), "image.qcow2")

		keyFilePath, err := generateEncryptionKey(t)
		require.NoError(t, err)

		opts := virtualdisk.AttachOptions{
			Image:                 imagePath,
			Size:                  64 * units.MB,
			VolumeGroup:           id + "_vg",
			LogicalVolume:         id + "_lv",
			EncryptionKeyFilePath: keyFilePath,
			PIDFilePath:           filepath.Join("/run/qemu-nbd", id+".pid"),
			SocketPath:            filepath.Join("/run/qemu-nbd", id+".sock"),
		}

		ctx, cancel := context.WithCancel(parentCtx)
		defer cancel()

		readyCh := make(chan struct{}, 1)

		go func() {
			if err := virtualdisk.Attach(ctx, logger, &opts, readyCh); err != nil {
				logger.Error("failed to attach", zap.Error(err))
				close(readyCh)
			}
		}()

		_, ok := <-readyCh
		require.True(t, ok, "failed to attach virtual disk")

		t.Log("Detaching virtual disk")

		cancel()
		for {
			_, err := os.Stat(opts.PIDFilePath)
			if err != nil {
				break
			}

			time.Sleep(100 * time.Millisecond)
		}

		err = exec.CommandContext(ctx, "lsblk",
			filepath.Join("/dev/mapper", fmt.Sprintf("%s-%s", opts.VolumeGroup, opts.LogicalVolume))).Run()
		require.Error(t, err)

		t.Log("Reattaching virtual disk")

		ctx, cancel = context.WithCancel(parentCtx)
		defer cancel()

		readyCh = make(chan struct{}, 1)

		go func() {
			if err := virtualdisk.Attach(ctx, logger, &opts, readyCh); err != nil {
				logger.Error("failed to attach", zap.Error(err))
				close(readyCh)
			}
		}()

		_, ok = <-readyCh
		require.True(t, ok, "failed to reattach virtual disk")

		output, err := exec.CommandContext(ctx, "lsblk", "-o", "NAME,SIZE,TYPE", "-J",
			filepath.Join("/dev/mapper", fmt.Sprintf("%s-%s", opts.VolumeGroup, opts.LogicalVolume))).CombinedOutput()
		require.NoError(t, err)

		t.Log("Detaching virtual disk")

		cancel()
		for {
			_, err := os.Stat(opts.PIDFilePath)
			if err != nil {
				break
			}

			time.Sleep(100 * time.Millisecond)
		}

		var resp LsblkResponse
		err = json.Unmarshal(output, &resp)
		require.NoError(t, err)

		assert.Len(t, resp.BlockDevices, 1)
		assert.Equal(t, "lvm", resp.BlockDevices[0].Type)

		size, err := units.FromHumanSize(resp.BlockDevices[0].Size)
		require.NoError(t, err)

		assert.NotZero(t, size)
	})
}

func generateEncryptionKey(t *testing.T) (string, error) {
	keyData := make([]byte, 64)
	n, err := rand.Read(keyData)
	if err != nil || n != 64 {
		return "", fmt.Errorf("failed to generate encryption key: %w", err)
	}

	keyFilePath := filepath.Join(t.TempDir(), "key")

	err = os.WriteFile(keyFilePath, keyData, 0o600)
	if err != nil {
		return "", fmt.Errorf("failed to write encryption key to file: %w", err)
	}

	return keyFilePath, nil
}
