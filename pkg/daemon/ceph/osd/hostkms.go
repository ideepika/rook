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
	"crypto/sha256"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/daemon/ceph/osd/kms"
	oposd "github.com/rook/rook/pkg/operator/ceph/cluster/osd"
	"github.com/rook/rook/pkg/util/sys"
)

// Encryption keys of host-based OSDs, kept in a KMS.
//
// "ceph-volume raw prepare" generates the OSD fsid itself, so the key cannot be stored under the
// fsid before prepare runs. It is stored under a pending name first, and moved to the fsid that
// ceph-volume reports once prepare succeeds. If the move fails or the prepare pod dies before it,
// the pending key stays in the KMS and recoverHostOSDKeys() moves it on the next run. The pending
// name is derived from a name of the device that survives reboots, since the kernel name may not.

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

// pendingKeyName returns the name of the pending key of a device, from the first of its udev links
// that does not change with reboots or with the device content
func pendingKeyName(nodeName string, disk *sys.LocalDisk) string {
	id := disk.Name
	links := strings.Fields(disk.DevLinks)
	slices.Sort(links)
	for _, prefix := range []string{"/dev/disk/by-id/", "/dev/disk/by-path/", "/dev/disk/by-loop-ref/"} {
		i := slices.IndexFunc(links, func(link string) bool {
			return strings.HasPrefix(link, prefix) && !strings.HasPrefix(link, prefix+"lvm-pv-uuid-") && !strings.HasPrefix(link, prefix+"dm-")
		})
		if i >= 0 {
			id = links[i]
			break
		}
	}
	// KMS key names must be valid Kubernetes Secret names for some providers, links are not
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("pending-%s-%x", nodeName, sum[:8])
}

// newHostOSDKey generates a key for a new OSD on the given device and stores it in the KMS under a
// pending name
func newHostOSDKey(context *clusterd.Context, clusterInfo *cephclient.ClusterInfo, nodeName string, disk *sys.LocalDisk) (*hostOSDKey, error) {
	device := disk.Name
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
		pendingName: pendingKeyName(nodeName, disk),
		key:         key,
	}
	// A pending key left by an earlier attempt belongs to no OSD, since recoverHostOSDKeys() ran
	// first and the device is blank. PutSecret does not overwrite, so delete it first.
	deleteKey(kmsConfig, k.pendingName)
	if err := kmsConfig.PutSecret(k.pendingName, key); err != nil {
		return nil, errors.Wrapf(err, "failed to store the encryption key for device %q in the %q KMS", device, kmsConfig.Provider)
	}
	// IBM Key Protect keeps the alias of a deleted key, so PutSecret may keep the stale key. The OSD
	// still works, but a failed move to the fsid cannot be recovered, since recoverHostOSDKeys()
	// only moves a pending key that opens the device.
	if stored, err := kmsConfig.GetSecret(k.pendingName); err != nil || stored != key {
		logger.Warningf("failed to replace the stale pending key %q in the %q KMS for device %q; if storing the key under the OSD fsid fails, the OSD must be prepared again. %v", k.pendingName, kmsConfig.Provider, device, err)
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
// created the OSD. It finds the OSDs by their LUKS header, and opens those that are closed, so
// that "ceph-volume raw list" reports an OSD that got no deployment before a reboot.
// shortcut: one KMS read per encrypted OSD on every prepare run; skip OSDs that have a deployment
// if that load matters.
func recoverHostOSDKeys(context *clusterd.Context, clusterInfo *cephclient.ClusterInfo, nodeName string) error {
	if !hostKMSEnabled() {
		return nil
	}
	var kmsConfig *kms.Config
	for _, disk := range context.Devices {
		if disk.Filesystem != "crypto_LUKS" {
			continue
		}
		dump, err := dumpLUKS(context, disk.RealPath)
		if err != nil {
			logger.Warningf("failed to check device %q for an encrypted OSD. %v", disk.RealPath, err)
			continue
		}
		osdUUID := osdUUIDFromLUKS(dump)
		if osdUUID == "" {
			continue
		}
		if kmsConfig == nil {
			if kmsConfig, err = kmsConfigFromEnv(context, clusterInfo); err != nil {
				return errors.Wrap(err, "failed to configure the KMS")
			}
		}
		if err := recoverPendingKey(context, kmsConfig, nodeName, disk, osdUUID); err != nil {
			return err
		}
		openHostOSD(context, kmsConfig, disk, osdUUID)
	}
	return nil
}

func recoverPendingKey(context *clusterd.Context, kmsConfig *kms.Config, nodeName string, disk *sys.LocalDisk, osdUUID string) error {
	pendingName := pendingKeyName(nodeName, disk)
	key, err := kmsConfig.GetSecret(pendingName)
	if err != nil || key == "" {
		// no pending key, the usual case: the key was moved to the fsid on prepare
		return nil
	}
	// a pending key of an earlier failed attempt on this device, or of another device that had
	// this name, must not replace the key of this OSD
	if !keyOpensDevice(context, disk.RealPath, key) {
		logger.Warningf("pending key %q does not open device %q of OSD %q, leaving it", pendingName, disk.RealPath, osdUUID)
		return nil
	}
	if stored, err := kmsConfig.GetSecret(osdUUID); err != nil || stored == "" {
		if err := kmsConfig.PutSecret(osdUUID, key); err != nil {
			return errors.Wrapf(err, "failed to store the pending encryption key %q as the key of OSD %q", pendingName, osdUUID)
		}
		logger.Infof("stored the pending encryption key %q as the key of OSD %q", pendingName, osdUUID)
	} else if stored != key {
		logger.Warningf("OSD %q has a key in the KMS that differs from the pending key %q that opens its device, leaving both", osdUUID, pendingName)
		return nil
	}
	deleteKey(kmsConfig, pendingName)
	return nil
}

// openHostOSD opens a closed encrypted raw OSD with its key from the KMS, under the mapper name
// that ceph-volume gives it
func openHostOSD(context *clusterd.Context, kmsConfig *kms.Config, disk *sys.LocalDisk, osdUUID string) {
	mapper := fmt.Sprintf("ceph-%s-%s-%s", osdUUID, path.Base(disk.RealPath), oposd.DmcryptBlockType)
	if _, err := os.Stat(path.Join("/dev/mapper", mapper)); err == nil {
		return
	}
	key, err := kmsConfig.GetSecret(osdUUID)
	if err != nil || key == "" {
		// also the case for LUKS devices of lvm mode OSDs, and of other clusters
		logger.Debugf("not opening OSD %q on device %q, the %q KMS has no key for it. %v", osdUUID, disk.RealPath, kmsConfig.Provider, err)
		return
	}
	args := []string{"luksOpen", "--allow-discards", "--key-file", "-", disk.RealPath, mapper}
	if err := context.Executor.ExecuteCommandWithStdin(luksOpenCmdTimeOut, cryptsetupBinary, &key, args...); err != nil {
		logger.Warningf("failed to open OSD %q on device %q. %v", osdUUID, disk.RealPath, err)
		return
	}
	logger.Infof("opened OSD %q on device %q as %q", osdUUID, disk.RealPath, mapper)
}

// osdUUIDFromLUKS returns the OSD fsid that ceph-volume writes to the LUKS2 subsystem of raw mode
// OSDs, "ceph_fsid=<osd fsid>"
func osdUUIDFromLUKS(dump string) string {
	for _, line := range strings.Split(dump, "\n") {
		value, found := strings.CutPrefix(strings.TrimSpace(line), "Subsystem:")
		if !found {
			continue
		}
		id, err := uuid.Parse(strings.TrimPrefix(strings.TrimSpace(value), "ceph_fsid="))
		if err != nil || !strings.Contains(value, "ceph_fsid=") {
			return ""
		}
		return id.String()
	}
	return ""
}

func keyOpensDevice(context *clusterd.Context, device, key string) bool {
	args := []string{"luksOpen", "--test-passphrase", "--key-file", "-", device}
	return context.Executor.ExecuteCommandWithStdin(luksOpenCmdTimeOut, cryptsetupBinary, &key, args...) == nil
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
