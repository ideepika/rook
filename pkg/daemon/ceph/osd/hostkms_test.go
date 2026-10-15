/*
Copyright 2026 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package osd

import (
	"testing"

	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/rook/rook/pkg/util/sys"
	"github.com/stretchr/testify/assert"
)

func TestOSDUUIDFromMapper(t *testing.T) {
	uuid := "8f3d6a1e-2b7c-4e59-9a0d-1c2b3d4e5f60"
	assert.Equal(t, uuid, osdUUIDFromMapper("/dev/mapper/ceph-"+uuid+"-loop0-block-dmcrypt"))
	assert.Equal(t, uuid, osdUUIDFromMapper("/dev/mapper/ceph-"+uuid+"-nvme0n1p2-block-dmcrypt"))
	assert.Equal(t, "", osdUUIDFromMapper(""))
	assert.Equal(t, "", osdUUIDFromMapper("/dev/mapper/ceph-short-block-dmcrypt"))
	assert.Equal(t, "", osdUUIDFromMapper("/dev/mapper/set1-data-0abcd-block-dmcrypt-and-more-text"))
}

func TestGetEncryptedBlockPathRawPrepare(t *testing.T) {
	op := "Running command: /usr/sbin/cryptsetup --key-size 512 --key-file - --allow-discards luksOpen /dev/loop0 ceph-8f3d6a1e-2b7c-4e59-9a0d-1c2b3d4e5f60-loop0-block-dmcrypt"
	assert.Equal(t, "/dev/mapper/ceph-8f3d6a1e-2b7c-4e59-9a0d-1c2b3d4e5f60-loop0-block-dmcrypt", getEncryptedBlockPath(op, "block-dmcrypt"))
}

func TestPendingKeyName(t *testing.T) {
	disk := &sys.LocalDisk{Name: "sdb", DevLinks: "/dev/disk/by-path/pci-0000:00:05.0-scsi-0:0:0:1 /dev/disk/by-id/wwn-0x5000c500a1b2c3d4 /dev/disk/by-uuid/a4ed20f4 /dev/disk/by-id/lvm-pv-uuid-TYz5Lw"}
	name := pendingKeyName("node1", disk)
	assert.Len(t, name, len("pending-node1-")+16)

	// a reboot that renames the device keeps the name
	renamed := *disk
	renamed.Name = "sdc"
	assert.Equal(t, name, pendingKeyName("node1", &renamed))

	// another device has another name
	other := &sys.LocalDisk{Name: "sdb", DevLinks: "/dev/disk/by-id/wwn-0x5000c500ffffffff"}
	assert.NotEqual(t, name, pendingKeyName("node1", other))

	// a multipath device is named by its WWID, not by its dm-N kernel name
	mpath := &sys.LocalDisk{Name: "dm-3", DevLinks: "/dev/disk/by-id/dm-name-mpatha /dev/disk/by-id/dm-uuid-mpath-3600508b4000156d7"}
	renamedMpath := &sys.LocalDisk{Name: "dm-5", DevLinks: mpath.DevLinks}
	assert.Equal(t, pendingKeyName("node1", mpath), pendingKeyName("node1", renamedMpath))

	// a loop device has no by-id link
	loop := &sys.LocalDisk{Name: "loop0", DevLinks: "/dev/disk/by-diskseq/12 /dev/disk/by-loop-ref/\\x2fvar\\x2flib\\x2fosd0.img"}
	assert.NotEqual(t, pendingKeyName("node1", &sys.LocalDisk{Name: "loop0"}), pendingKeyName("node1", loop))
}

func TestOSDUUIDFromLUKS(t *testing.T) {
	assert.Equal(t, "64f279de-45a7-4372-aa11-d52d13db22c6", osdUUIDFromLUKS("LUKS header information\nVersion:       \t2\nLabel:         \t(no label)\nSubsystem:     \tceph_fsid=64f279de-45a7-4372-aa11-d52d13db22c6\n"))
	assert.Equal(t, "", osdUUIDFromLUKS("Label:  \t(no label)\nSubsystem:     \t(no subsystem)\n"))
	assert.Equal(t, "", osdUUIDFromLUKS("Subsystem: ceph_fsid=not-a-uuid\n"))
	// only the subsystem holds the OSD fsid
	assert.Equal(t, "", osdUUIDFromLUKS("Label:         \tceph_fsid=64f279de-45a7-4372-aa11-d52d13db22c6\nSubsystem:     \t(no subsystem)\n"))
}

func TestHostLUKSOSDs(t *testing.T) {
	openUUID := "8f3d6a1e-2b7c-4e59-9a0d-1c2b3d4e5f60"
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			switch {
			case command == "cryptsetup" && args[0] == "status":
				return "type:    LUKS2\n  device:  /dev/sdb\n", nil
			case command == "udevadm":
				return "DEVLINKS=/dev/disk/by-id/wwn-0x5000c500a1b2c3d4\n", nil
			}
			return "", errors.Errorf("unexpected %s %v", command, args)
		},
		MockExecuteCommandWithCombinedOutput: func(command string, args ...string) (string, error) {
			if command == "cryptsetup" && args[0] == "luksDump" && args[1] == "/dev/sdc" {
				return "Subsystem:     \tceph_fsid=64f279de-45a7-4372-aa11-d52d13db22c6\n", nil
			}
			return "Subsystem:     \t(no subsystem)\n", nil
		},
	}
	context := &clusterd.Context{Executor: executor, Devices: []*sys.LocalDisk{
		// an open OSD: discovery lists its mapper but not sdb, which has a child
		{Name: "dm-2", RealPath: "/dev/dm-2", Type: sys.CryptType, DevLinks: "/dev/mapper/ceph-" + openUUID + "-sdb-block-dmcrypt"},
		// a closed OSD
		{Name: "sdc", RealPath: "/dev/sdc", Type: sys.DiskType, Filesystem: "crypto_LUKS"},
		// LUKS that is no raw mode OSD
		{Name: "sdd", RealPath: "/dev/sdd", Type: sys.DiskType, Filesystem: "crypto_LUKS"},
	}}

	osds := hostLUKSOSDs(context)
	assert.Len(t, osds, 2)
	assert.Equal(t, openUUID, osds[0].uuid)
	assert.Equal(t, "/dev/sdb", osds[0].disk.RealPath)
	assert.Equal(t, "/dev/disk/by-id/wwn-0x5000c500a1b2c3d4", osds[0].disk.DevLinks)
	assert.NotEmpty(t, osds[0].mapper)
	assert.Equal(t, "64f279de-45a7-4372-aa11-d52d13db22c6", osds[1].uuid)
	assert.Empty(t, osds[1].mapper)
}
