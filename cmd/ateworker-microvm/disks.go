// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package main

// Disk volumes (ActorTemplate volumes[].disk).
//
// Each disk is a sparse file under the actor's own directory, attached to the
// micro-VM as a virtio-blk device (hot-plugged once the kata-agent runs) and
// handed to the containers that attach it
// (volume_devices) as a block device node, through the kata-agent's "blk"
// device handler. The guest owns its contents: Substrate never formats or
// mounts it.
//
// Lifecycle, like the rootfs upper: RunWorkload creates each disk empty;
// CheckpointWorkload copies it (holes kept) into the snapshot as
// diskSnapshotFile, at every fidelity, while the guest is paused; RestoreWorkload
// copies it back before the VM is created or restored; teardown removes the
// directory. The snapshot copy is taken with the guest paused but not flushed,
// so it holds what the guest had written to the device: a filesystem on it sees
// what an abrupt power cut would leave, which a journaling filesystem recovers
// from on its next mount.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"
	"go.ate.dev/substrate/cmd/ateworker-microvm/internal/ch"
	"go.ate.dev/substrate/cmd/ateworker-microvm/internal/third_party/kata/agentpb"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/sparsefile"
)

// kataBlockPCIDevice is the kata-agent device type for a virtio-blk PCI
// device located by its PCI path (DRIVER_BLK_PCI_TYPE).
const kataBlockPCIDevice = "blk"

// diskVolumesDir holds the actor's disk images, one file per disk volume.
// Local to this binary, like rootfsUpperDir.
func diskVolumesDir(actorDirs *ateworkerpb.ActorDirs) string {
	return filepath.Join(actorDirs.GetRootDir(), "disks")
}

// diskImagePath is the host file backing one disk volume.
func diskImagePath(actorDirs *ateworkerpb.ActorDirs, name string) string {
	return filepath.Join(diskVolumesDir(actorDirs), name+".img")
}

// diskDeviceID names a disk volume's virtio-blk device in the VM config and in
// vm.info's device tree.
func diskDeviceID(name string) string { return "ate-disk-" + name }

// diskSnapshotFile is the snapshot file holding a disk volume's image.
func diskSnapshotFile(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\x00") || name == "." || name == ".." {
		return "", fmt.Errorf("invalid disk volume name %q", name)
	}
	return "disk-" + name + ".img", nil
}

// isDiskSnapshotFile reports whether name is a snapshot file diskSnapshotFile
// made.
func isDiskSnapshotFile(name string) bool {
	return strings.HasPrefix(name, "disk-") && strings.HasSuffix(name, ".img")
}

// createDiskVolumes replaces the actor's disk images with empty ones of the
// declared sizes. Sparse, so a disk takes host space only as it is written.
func createDiskVolumes(actorDirs *ateworkerpb.ActorDirs, disks []*ateworkerpb.DiskVolume) error {
	return stageDiskVolumes(actorDirs, "", disks)
}

// stageDiskVolumes puts the actor's disk images in place: copied from
// snapshotDir when it holds them, else empty. The directory is rebuilt from
// scratch, so nothing from an earlier activation survives.
func stageDiskVolumes(actorDirs *ateworkerpb.ActorDirs, snapshotDir string, disks []*ateworkerpb.DiskVolume) error {
	dir := diskVolumesDir(actorDirs)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("while clearing disk volumes dir %q: %w", dir, err)
	}
	if len(disks) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("while creating disk volumes dir %q: %w", dir, err)
	}
	for _, d := range disks {
		dst := diskImagePath(actorDirs, d.GetName())
		if snapshotDir != "" {
			name, err := diskSnapshotFile(d.GetName())
			if err != nil {
				return err
			}
			src := filepath.Join(snapshotDir, name)
			// A copy, not a link: the snapshot file may be kept as a local
			// cache, and the VM writes to the image.
			_, err = sparsefile.CopyFile(src, dst)
			switch {
			case err == nil:
				if err := growTo(dst, d.GetSizeBytes()); err != nil {
					return err
				}
				continue
			case errors.Is(err, fs.ErrNotExist):
				// A snapshot taken before the volume existed: start empty.
				slog.Warn("Snapshot has no image for disk volume; starting it empty", slog.String("volume", d.GetName()))
			default:
				return fmt.Errorf("while restoring disk volume %q: %w", d.GetName(), err)
			}
		}
		f, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("while creating disk volume %q: %w", d.GetName(), err)
		}
		err = f.Truncate(d.GetSizeBytes())
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("while sizing disk volume %q: %w", d.GetName(), err)
		}
	}
	return nil
}

// growTo extends path to size bytes if it is smaller. It never shrinks: the
// guest's data would be cut off.
func growTo(path string, size int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() < size {
		return os.Truncate(path, size)
	}
	return nil
}

// checkpointDiskVolumes copies each disk image into checkpointDir, keeping
// holes, and returns the snapshot files it wrote. The guest must be paused.
func checkpointDiskVolumes(ctx context.Context, actorDirs *ateworkerpb.ActorDirs, checkpointDir string, disks []*ateworkerpb.DiskVolume) ([]string, error) {
	var files []string
	for _, d := range disks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, err := diskSnapshotFile(d.GetName())
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(checkpointDir, name)
		if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if _, err := sparsefile.CopyFile(diskImagePath(actorDirs, d.GetName()), dst); err != nil {
			return nil, fmt.Errorf("while copying disk volume %q into the snapshot: %w", d.GetName(), err)
		}
		files = append(files, name)
	}
	return files, nil
}

// removeDiskVolumes deletes the actor's disk images.
func removeDiskVolumes(actorDirs *ateworkerpb.ActorDirs) error {
	return os.RemoveAll(diskVolumesDir(actorDirs))
}

// attachDiskVolumes hot-plugs each disk volume into the running VM and returns
// where each sits on the guest's PCI bus, in the kata-agent's PCI path form.
// The kata-agent must be up (it watches for the devices' uevents). A MEMORY
// snapshot's VM config keeps the disks, so a restored VM has them already.
func attachDiskVolumes(ctx context.Context, client *ch.Client, actorDirs *ateworkerpb.ActorDirs, disks []*ateworkerpb.DiskVolume) (map[string]string, error) {
	out := make(map[string]string, len(disks))
	for _, d := range disks {
		bdf, err := client.AddDisk(ctx, ch.DiskConfig{Path: diskImagePath(actorDirs, d.GetName()), ID: diskDeviceID(d.GetName()), ImageType: "Raw"})
		if err != nil {
			return nil, fmt.Errorf("while attaching disk volume %q: %w", d.GetName(), err)
		}
		path, err := ch.KataPCIPath(bdf)
		if err != nil {
			return nil, fmt.Errorf("while attaching disk volume %q: %w", d.GetName(), err)
		}
		out[d.GetName()] = path
	}
	return out, nil
}

// kataBlockDevices returns the kata-agent devices for a container's
// volume_devices: the agent finds each disk by its PCI path and creates its
// node at the container path, fixing the spec entry addBlockDeviceNodes made.
func kataBlockDevices(devices []*ateworkerpb.VolumeDevice, pciPaths map[string]string) ([]*agentpb.Device, error) {
	var out []*agentpb.Device
	for _, dev := range devices {
		pci, ok := pciPaths[dev.GetName()]
		if !ok {
			return nil, fmt.Errorf("volume device %q names no disk volume of the actor", dev.GetName())
		}
		// The kata-agent's blk handler parses the PCI path from Id (older
		// agents read VmPath), so both carry it.
		out = append(out, &agentpb.Device{
			Id:            pci,
			Type:          kataBlockPCIDevice,
			VmPath:        pci,
			ContainerPath: dev.GetDevicePath(),
		})
	}
	return out, nil
}

// placeholderBlockMajor is the major number the spec carries for a disk
// volume's device node until the kata-agent replaces it with the guest's
// (the agent matches spec entries by path, then cgroup rules by these
// numbers). 4095 is the largest major and is not assigned to a driver.
const placeholderBlockMajor = 4095

// addBlockDeviceNodes adds a device node and a device-cgroup allow rule to spec
// for each of the container's volume devices. The numbers are placeholders the
// kata-agent rewrites.
func addBlockDeviceNodes(spec *specs.Spec, devices []*ateworkerpb.VolumeDevice) {
	if len(devices) == 0 {
		return
	}
	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}
	if spec.Linux.Resources == nil {
		spec.Linux.Resources = &specs.LinuxResources{}
	}
	mode := os.FileMode(0o660)
	var uid, gid uint32
	for i, dev := range devices {
		major, minor := int64(placeholderBlockMajor), int64(i)
		spec.Linux.Devices = append(spec.Linux.Devices, specs.LinuxDevice{
			Path:     dev.GetDevicePath(),
			Type:     "b",
			Major:    major,
			Minor:    minor,
			FileMode: &mode,
			UID:      &uid,
			GID:      &gid,
		})
		spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, specs.LinuxDeviceCgroup{
			Allow:  true,
			Type:   "b",
			Major:  &major,
			Minor:  &minor,
			Access: "rwm",
		})
	}
}
