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

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/sparsefile"
)

const testDiskSize = 1 << 30

func testActorDirs(t *testing.T) *ateworkerpb.ActorDirs {
	t.Helper()
	return &ateworkerpb.ActorDirs{RootDir: t.TempDir()}
}

func allocated(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return sparsefile.AllocatedBytes(info)
}

// A disk keeps what the guest wrote across checkpoint and restore, into
// another actor, and stays sparse on the way.
func TestDiskVolumeCheckpointRestore(t *testing.T) {
	disks := []*ateworkerpb.DiskVolume{{Name: "docker", SizeBytes: testDiskSize}}
	src := testActorDirs(t)
	if err := createDiskVolumes(src, disks); err != nil {
		t.Fatal(err)
	}
	img := diskImagePath(src, "docker")
	if info, err := os.Stat(img); err != nil || info.Size() != testDiskSize {
		t.Fatalf("fresh disk: %v, %v; want %d bytes", info, err, testDiskSize)
	}
	if a := allocated(t, img); a > 1<<20 {
		t.Fatalf("fresh disk allocates %d bytes; want it sparse", a)
	}

	// The guest writes a block in the middle.
	payload := bytes.Repeat([]byte("ate"), 4096)
	f, err := os.OpenFile(img, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(payload, 512<<20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	checkpointDir := t.TempDir()
	files, err := checkpointDiskVolumes(t.Context(), src, checkpointDir, disks)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(files, []string{"disk-docker.img"}) {
		t.Fatalf("snapshot files = %v", files)
	}
	snap := filepath.Join(checkpointDir, "disk-docker.img")
	if a := allocated(t, snap); a > 1<<20 {
		t.Fatalf("snapshot copy allocates %d bytes; want holes kept", a)
	}

	dst := testActorDirs(t)
	if err := stageDiskVolumes(dst, checkpointDir, disks); err != nil {
		t.Fatal(err)
	}
	restored := diskImagePath(dst, "docker")
	got := make([]byte, len(payload))
	rf, err := os.Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if _, err := rf.ReadAt(got, 512<<20); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("restored disk lost the guest's write (err %v)", err)
	}
	if info, _ := rf.Stat(); info.Size() != testDiskSize {
		t.Fatalf("restored disk is %d bytes, want %d", info.Size(), testDiskSize)
	}
	if a := allocated(t, restored); a > 1<<20 {
		t.Fatalf("restored disk allocates %d bytes; want holes kept", a)
	}
	// The snapshot file is untouched by later writes to the live disk.
	if err := os.WriteFile(restored, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(snap); info.Size() != testDiskSize {
		t.Fatal("writing the restored disk changed the snapshot file")
	}

	if err := removeDiskVolumes(dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(diskVolumesDir(dst)); !os.IsNotExist(err) {
		t.Fatalf("disk volumes dir still there: %v", err)
	}
}

// A snapshot without a disk's image (taken before the volume was declared)
// restores it empty, and a stale image from an earlier activation is gone.
func TestStageDiskVolumesWithoutSnapshotImage(t *testing.T) {
	dirs := testActorDirs(t)
	if err := os.MkdirAll(diskVolumesDir(dirs), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskImagePath(dirs, "stale"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	disks := []*ateworkerpb.DiskVolume{{Name: "docker", SizeBytes: testDiskSize}}
	if err := stageDiskVolumes(dirs, t.TempDir(), disks); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(diskImagePath(dirs, "docker")); err != nil || info.Size() != testDiskSize {
		t.Fatalf("disk: %v, %v", info, err)
	}
	if _, err := os.Stat(diskImagePath(dirs, "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale image survived: %v", err)
	}
}

func TestDiskSnapshotFileNames(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b"} {
		if _, err := diskSnapshotFile(bad); err == nil {
			t.Errorf("diskSnapshotFile(%q) succeeded", bad)
		}
	}
	name, _ := diskSnapshotFile("docker")
	if !isDiskSnapshotFile(name) || !isSnapshotDataFile(name, false) {
		t.Errorf("%q is not recognized as a snapshot data file", name)
	}
}

func TestBlockDeviceWiring(t *testing.T) {
	devs := []*ateworkerpb.VolumeDevice{{Name: "docker", DevicePath: "/dev/docker"}}
	got, err := kataBlockDevices(devs, map[string]string{"docker": "06"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GetType() != "blk" || got[0].GetVmPath() != "06" ||
		got[0].GetContainerPath() != "/dev/docker" || got[0].GetId() != "06" {
		t.Fatalf("kata devices = %v", got)
	}
	if _, err := kataBlockDevices([]*ateworkerpb.VolumeDevice{{Name: "nope", DevicePath: "/dev/x"}}, nil); err == nil {
		t.Fatal("kataBlockDevices accepted a device with no disk")
	}

	spec := &specs.Spec{}
	addBlockDeviceNodes(spec, devs)
	if len(spec.Linux.Devices) != 1 || spec.Linux.Devices[0].Path != "/dev/docker" || spec.Linux.Devices[0].Type != "b" {
		t.Fatalf("spec devices = %+v", spec.Linux.Devices)
	}
	d := spec.Linux.Devices[0]
	rules := spec.Linux.Resources.Devices
	if len(rules) != 1 || !rules[0].Allow || *rules[0].Major != d.Major || *rules[0].Minor != d.Minor {
		t.Fatalf("cgroup rules %+v do not allow the node %+v the kata-agent rewrites", rules, d)
	}
}

func TestRewriteSnapshotDiskPaths(t *testing.T) {
	dirs := testActorDirs(t)
	write := func(cfg map[string]any) string {
		dir := t.TempDir()
		b, _ := json.Marshal(cfg)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	read := func(dir string) map[string]any {
		b, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		_ = json.Unmarshal(b, &cfg)
		return cfg
	}

	dir := write(map[string]any{"disks": []any{map[string]any{"id": "ate-disk-docker", "path": "/old/actor/disks/docker.img"}}, "cpus": map[string]any{"boot_vcpus": 2}})
	if err := rewriteSnapshotDiskPaths(dir, dirs); err != nil {
		t.Fatal(err)
	}
	cfg := read(dir)
	if got := cfg["disks"].([]any)[0].(map[string]any)["path"]; got != diskImagePath(dirs, "docker") {
		t.Fatalf("disk path = %v, want %s", got, diskImagePath(dirs, "docker"))
	}
	if cfg["cpus"] == nil {
		t.Fatal("rewrite dropped other config")
	}

	// No disks: the file is left alone.
	dir = write(map[string]any{"cpus": map[string]any{}})
	before, _ := os.Stat(filepath.Join(dir, "config.json"))
	if err := rewriteSnapshotDiskPaths(dir, dirs); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(dir, "config.json"))
	if !os.SameFile(before, after) {
		t.Fatal("config without disks was rewritten")
	}

	dir = write(map[string]any{"disks": []any{map[string]any{"id": "_disk0", "path": "/x"}}})
	if err := rewriteSnapshotDiskPaths(dir, dirs); err == nil {
		t.Fatal("accepted a disk this worker never configures")
	}
}
