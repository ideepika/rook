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
// ceph-volume reports once prepare succeeds. If the move fails or the prepare pod dies before it,
// the pending key stays in the KMS and recoverHostOSDKeys() moves it on the next run.

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

func pendingKeyName(nodeName, device string) string {
	return fmt.Sprintf("pending-%s-%s", nodeName, path.Base(device))
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
		pendingName: pendingKeyName(nodeName, device),
		key:         key,
	}
	// A pending key left by an earlier attempt belongs to no OSD on this device, since the device
	// is being prepared again. PutSecret does not overwrite, so delete it first.
	deleteKey(kmsConfig, k.pendingName)
	if err := kmsConfig.PutSecret(k.pendingName, key); err != nil {
		return nil, errors.Wrapf(err, "failed to store the encryption key for device %q in the %q KMS", device, kmsConfig.Provider)
	}
	// Some KMS keep an existing entry on PutSecret even after DeleteSecret (IBM Key Protect keeps the
	// alias of a deleted key), so make sure the pending key is the one ceph-volume will use
	if stored, err := kmsConfig.GetSecret(k.pendingName); err != nil || stored != key {
		return nil, errors.Errorf("failed to replace the stale pending key %q in the %q KMS for device %q. %v", k.pendingName, kmsConfig.Provider, device, err)
	}
	logger.Infof("stored the encryption key for device %q in the %q KMS as %q", device, kmsConfig.Provider, k.pendingName)
	return k, nil
}

// commit moves the key to the fsid of the OSD that "ceph-volume raw prepare" created, and returns
// the mapper of the OSD
func (k *hostOSDKey) commit(prepareOutput string) (string, error) {
	mapper := getEncryptedBlockPath(prepareOutput, oposd.DmcryptBlockType)
	osdUUID := osdUUIDFromMapper(mapper)
	if osdUUID == "" {
		return "", errors.Errorf("failed to find the OSD fsid in the ceph-volume output; the key is kept in the KMS as %q", k.pendingName)
	}
	if err := k.kmsConfig.PutSecret(osdUUID, k.key); err != nil {
		return "", errors.Wrapf(err, "failed to store the encryption key of OSD %q; the key is kept in the KMS as %q", osdUUID, k.pendingName)
	}
	logger.Infof("stored the encryption key of OSD %q in the %q KMS", osdUUID, k.kmsConfig.Provider)
	deleteKey(k.kmsConfig, k.pendingName)
	return mapper, nil
}

// recoverHostOSDKeys moves pending keys to the fsid of the encrypted raw OSDs on this node whose
// key the KMS does not hold yet, which happens when a prepare run failed after ceph-volume
// created the OSD
func recoverHostOSDKeys(context *clusterd.Context, clusterInfo *cephclient.ClusterInfo, nodeName string, osds []oposd.OSDInfo) error {
	if !hostKMSEnabled() {
		return nil
	}
	var kmsConfig *kms.Config
	for _, osd := range osds {
		if !osd.Encrypted || osd.CVMode != "raw" {
			continue
		}
		device := deviceFromMapper(osd.BlockPath)
		if device == "" {
			continue
		}
		if kmsConfig == nil {
			var err error
			if kmsConfig, err = kmsConfigFromEnv(context, clusterInfo); err != nil {
				return errors.Wrap(err, "failed to configure the KMS")
			}
		}
		pendingName := pendingKeyName(nodeName, device)
		key, err := kmsConfig.GetSecret(pendingName)
		if err != nil || key == "" {
			// no pending key: the key was moved to the fsid when the OSD was prepared
			continue
		}
		if stored, err := kmsConfig.GetSecret(osd.UUID); err != nil || stored == "" {
			if err := kmsConfig.PutSecret(osd.UUID, key); err != nil {
				return errors.Wrapf(err, "failed to store the pending encryption key %q as the key of OSD %d", pendingName, osd.ID)
			}
			logger.Infof("stored the pending encryption key %q as the key of OSD %d", pendingName, osd.ID)
		}
		deleteKey(kmsConfig, pendingName)
	}
	return nil
}

func deleteKey(kmsConfig *kms.Config, name string) {
	if err := kmsConfig.DeleteSecret(name); err != nil {
		logger.Debugf("failed to delete key %q from the KMS. %v", name, err)
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

// deviceFromMapper returns the kernel name of the device in a mapper name, see osdUUIDFromMapper()
func deviceFromMapper(mapper string) string {
	osdUUID := osdUUIDFromMapper(mapper)
	if osdUUID == "" {
		return ""
	}
	name := strings.TrimPrefix(path.Base(mapper), "ceph-"+osdUUID+"-")
	device, found := strings.CutSuffix(name, "-"+oposd.DmcryptBlockType)
	if !found {
		return ""
	}
	return device
}
