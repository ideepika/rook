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

	// a loop device has no by-id link
	loop := &sys.LocalDisk{Name: "loop0", DevLinks: "/dev/disk/by-diskseq/12 /dev/disk/by-loop-ref/\\x2fvar\\x2flib\\x2fosd0.img"}
	assert.NotEqual(t, pendingKeyName("node1", &sys.LocalDisk{Name: "loop0"}), pendingKeyName("node1", loop))
}

func TestOSDUUIDFromLUKS(t *testing.T) {
	assert.Equal(t, "64f279de-45a7-4372-aa11-d52d13db22c6", osdUUIDFromLUKS("LUKS header information\nVersion:       \t2\nLabel:         \t(no label)\nSubsystem:     \tceph_fsid=64f279de-45a7-4372-aa11-d52d13db22c6\n"))
	assert.Equal(t, "", osdUUIDFromLUKS("Label:  \t(no label)\nSubsystem:     \t(no subsystem)\n"))
	assert.Equal(t, "", osdUUIDFromLUKS("Subsystem: ceph_fsid=not-a-uuid\n"))
}
