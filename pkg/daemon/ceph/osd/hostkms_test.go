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

func TestDeviceFromMapper(t *testing.T) {
	uuid := "8f3d6a1e-2b7c-4e59-9a0d-1c2b3d4e5f60"
	assert.Equal(t, "loop0", deviceFromMapper("/dev/mapper/ceph-"+uuid+"-loop0-block-dmcrypt"))
	assert.Equal(t, "nvme0n1p2", deviceFromMapper("/dev/mapper/ceph-"+uuid+"-nvme0n1p2-block-dmcrypt"))
	assert.Equal(t, "", deviceFromMapper("/dev/mapper/ceph-"+uuid+"-loop0"))
	assert.Equal(t, "", deviceFromMapper("/dev/loop0"))
	assert.Equal(t, "pending-node1-loop0", pendingKeyName("node1", "/dev/loop0"))
}
