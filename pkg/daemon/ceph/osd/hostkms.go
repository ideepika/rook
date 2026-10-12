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
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/daemon/ceph/osd/kms"
	oposd "github.com/rook/rook/pkg/operator/ceph/cluster/osd"
)

// Encryption keys of host-based OSDs, kept in a KMS.
//
// "ceph-volume raw prepare" generates the OSD fsid itself, so the key cannot be stored under the
// fsid before prepare runs. It is stored under a pending name first, and moved to the fsid that
// ceph-volume reports once prepare succeeds. A prepare pod that dies in between leaves only a
// pending key behind, never an OSD whose key is missing from the KMS.

// hostKMSEnabled reports whether encrypted host-based OSDs keep their keys in a KMS. The operator
// passes the KMS settings to host-based prepare pods only when a KMS is configured.
func hostKMSEnabled() bool {
	return !isOnPVC && os.Getenv(kms.Provider) != ""
}

type hostOSDKey struct {
	kmsConfig   *kms.Config
	pendingName string
	key         string
}

// newHostOSDKey generates a key for a new OSD on the given device and stores it in the KMS under a
// pending name
func newHostOSDKey(context *clusterd.Context, clusterInfo *cephclient.ClusterInfo, nodeName, device string) (*hostOSDKey, error) {
	if !hostKMSEnabled() {
		return nil, errors.Errorf("encrypted device %q needs a KMS to use raw mode", device)
	}
	kmsConfig, err := kmsConfigFromEnv(context, clusterInfo)
	if err != nil {
		return nil, errors.Wrap(err, "failed to configure the KMS")
	}
	key, err := oposd.GenerateDmCryptKey()
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate the encryption key")
	}

	k := &hostOSDKey{
		kmsConfig:   kmsConfig,
		pendingName: fmt.Sprintf("pending-%s-%s", nodeName, path.Base(device)),
		key:         key,
	}
	// A pending key left by an earlier attempt must not be reused, and PutSecret does not overwrite
	k.abort()
	if err := kmsConfig.PutSecret(k.pendingName, key); err != nil {
		return nil, errors.Wrapf(err, "failed to store the encryption key for device %q in the %q KMS", device, kmsConfig.Provider)
	}
	logger.Infof("stored the encryption key for device %q in the %q KMS as %q", device, kmsConfig.Provider, k.pendingName)
	return k, nil
}

// commit moves the key to the fsid of the OSD that "ceph-volume raw prepare" created
func (k *hostOSDKey) commit(prepareOutput string) error {
	mapper := getEncryptedBlockPath(prepareOutput, oposd.DmcryptBlockType)
	osdUUID := osdUUIDFromMapper(mapper)
	if osdUUID == "" {
		return errors.Errorf("failed to find the OSD fsid in the ceph-volume output; the key is kept in the KMS as %q", k.pendingName)
	}
	if err := k.kmsConfig.PutSecret(osdUUID, k.key); err != nil {
		return errors.Wrapf(err, "failed to store the encryption key of OSD %q; the key is kept in the KMS as %q", osdUUID, k.pendingName)
	}
	logger.Infof("stored the encryption key of OSD %q in the %q KMS", osdUUID, k.kmsConfig.Provider)
	k.abort()
	return nil
}

// abort deletes the pending key
func (k *hostOSDKey) abort() {
	if err := k.kmsConfig.DeleteSecret(k.pendingName); err != nil {
		logger.Debugf("failed to delete pending key %q from the KMS. %v", k.pendingName, err)
	}
}

// osdUUIDFromMapper returns the OSD fsid from the mapper that ceph-volume opens on prepare,
// "/dev/mapper/ceph-<osd fsid>-<kernel name>-block-dmcrypt"
func osdUUIDFromMapper(mapper string) string {
	name := strings.TrimPrefix(path.Base(mapper), "ceph-")
	if len(name) < 36 {
		return ""
	}
	id, err := uuid.Parse(name[:36])
	if err != nil {
		return ""
	}
	return id.String()
}
