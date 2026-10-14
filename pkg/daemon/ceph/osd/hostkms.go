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
			name := strings.TrimPrefix(link, prefix)
			if !strings.HasPrefix(link, prefix) || strings.HasPrefix(name, "lvm-pv-uuid-") {
				return false
			}
			// of the device-mapper links only the WWID of a multipath device is stable
			return !strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "dm-uuid-mpath-")
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
	// IBM Key Protect keeps the alias of a deleted key, so PutSecret may keep the stale key, and
	// the key of the new OSD would be lost if storing it under the fsid failed
	if stored, err := kmsConfig.GetSecret(k.pendingName); err != nil || stored != key {
		return nil, errors.Errorf("failed to replace the stale pending key %q in the %q KMS for device %q; delete it from the KMS to prepare the device. %v", k.pendingName, kmsConfig.Provider, device, err)
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
// created the OSD. It also opens the closed ones, so that "ceph-volume raw list" reports an OSD
// that got no deployment before a reboot. Only OSDs of this cluster are opened, not purged ones.
// shortcut: one KMS read per encrypted OSD on every prepare run; skip OSDs that have a deployment
// if that load matters.
func recoverHostOSDKeys(context *clusterd.Context, clusterInfo *cephclient.ClusterInfo, nodeName string, clusterOSDs map[string]bool) error {
	if !hostKMSEnabled() {
		return nil
	}
	var kmsConfig *kms.Config
	for _, osd := range hostLUKSOSDs(context) {
		if !clusterOSDs[osd.uuid] {
			continue
		}
		if kmsConfig == nil {
			var err error
			if kmsConfig, err = kmsConfigFromEnv(context, clusterInfo); err != nil {
				return errors.Wrap(err, "failed to configure the KMS")
			}
		}
		key, err := recoverPendingKey(context, kmsConfig, nodeName, osd)
		if err != nil {
			return err
		}
		if osd.mapper == "" {
			openHostOSD(context, kmsConfig, osd, key)
		}
	}
	return nil
}

type hostLUKSOSD struct {
	disk   *sys.LocalDisk
	uuid   string
	mapper string // empty if the OSD is closed
}

// hostLUKSOSDs returns the devices of the encrypted raw OSDs on this node: closed ones by the OSD
// fsid in their LUKS header, and open ones by their mapper, since device discovery skips a disk
// with a child
func hostLUKSOSDs(context *clusterd.Context) []hostLUKSOSD {
	osds := []hostLUKSOSD{}
	seen := map[string]bool{}
	for _, disk := range context.Devices {
		osd := hostLUKSOSD{disk: disk}
		switch {
		case disk.Type == sys.CryptType:
			for _, link := range strings.Fields(disk.DevLinks) {
				if strings.HasPrefix(link, "/dev/mapper/ceph-") && strings.HasSuffix(link, "-"+oposd.DmcryptBlockType) {
					osd.mapper, osd.uuid = link, osdUUIDFromMapper(link)
				}
			}
			if osd.uuid == "" {
				continue
			}
			backing, err := GetBackingDeviceForEncryptedBlock(context, osd.mapper)
			if err != nil {
				logger.Warningf("failed to find the device of OSD %q. %v", osd.uuid, err)
				continue
			}
			name := path.Base(backing)
			if osd.disk, err = clusterd.PopulateDeviceUdevInfo(name, context.Executor, &sys.LocalDisk{Name: name, RealPath: backing}); err != nil {
				logger.Warningf("failed to get the udev links of device %q of OSD %q. %v", backing, osd.uuid, err)
				continue
			}
		case disk.Filesystem == "crypto_LUKS":
			dump, err := dumpLUKS(context, disk.RealPath)
			if err != nil {
				logger.Warningf("failed to check device %q for an encrypted OSD. %v", disk.RealPath, err)
				continue
			}
			if osd.uuid = osdUUIDFromLUKS(dump); osd.uuid == "" {
				continue
			}
		default:
			continue
		}
		// an open loop device is listed both as a LUKS device and through its mapper
		if !seen[osd.uuid] || osd.mapper != "" {
			if seen[osd.uuid] {
				osds = slices.DeleteFunc(osds, func(o hostLUKSOSD) bool { return o.uuid == osd.uuid })
			}
			seen[osd.uuid] = true
			osds = append(osds, osd)
		}
	}
	return osds
}

// recoverPendingKey moves the pending key of the device of an OSD to its fsid, and returns the key
// of the OSD if it read it
func recoverPendingKey(context *clusterd.Context, kmsConfig *kms.Config, nodeName string, osd hostLUKSOSD) (string, error) {
	pendingName := pendingKeyName(nodeName, osd.disk)
	key, err := kmsConfig.GetSecret(pendingName)
	if err != nil || key == "" {
		// no pending key, the usual case: the key was moved to the fsid on prepare
		return "", nil
	}
	// a pending key of an earlier failed attempt on this device, or of another device that had
	// this name, must not replace the key of this OSD
	if !keyOpensDevice(context, osd.disk.RealPath, key) {
		logger.Warningf("pending key %q does not open device %q of OSD %q, leaving it", pendingName, osd.disk.RealPath, osd.uuid)
		return "", nil
	}
	if stored, err := kmsConfig.GetSecret(osd.uuid); err != nil || stored == "" {
		if err := kmsConfig.PutSecret(osd.uuid, key); err != nil {
			return "", errors.Wrapf(err, "failed to store the pending encryption key %q as the key of OSD %q", pendingName, osd.uuid)
		}
		logger.Infof("stored the pending encryption key %q as the key of OSD %q", pendingName, osd.uuid)
	} else if stored != key {
		logger.Warningf("OSD %q has a key in the KMS that differs from the pending key %q that opens its device, leaving both", osd.uuid, pendingName)
		return "", nil
	}
	deleteKey(kmsConfig, pendingName)
	return key, nil
}

// openHostOSD opens a closed encrypted raw OSD with its key from the KMS, under the mapper name
// that ceph-volume gives it
func openHostOSD(context *clusterd.Context, kmsConfig *kms.Config, osd hostLUKSOSD, key string) {
	if key == "" {
		var err error
		if key, err = kmsConfig.GetSecret(osd.uuid); err != nil || key == "" {
			logger.Debugf("not opening OSD %q on device %q, the %q KMS has no key for it. %v", osd.uuid, osd.disk.RealPath, kmsConfig.Provider, err)
			return
		}
	}
	mapper := fmt.Sprintf("ceph-%s-%s-%s", osd.uuid, path.Base(osd.disk.RealPath), oposd.DmcryptBlockType)
	if err := openEncryptedDevice(context, osd.disk.RealPath, mapper, key); err != nil {
		logger.Warningf("failed to open OSD %q. %v", osd.uuid, err)
		return
	}
	logger.Infof("opened OSD %q on device %q as %q", osd.uuid, osd.disk.RealPath, mapper)
}

func keyOpensDevice(context *clusterd.Context, device, key string) bool {
	args := []string{"luksOpen", "--test-passphrase", device}
	return context.Executor.ExecuteCommandWithStdin(luksOpenCmdTimeOut, cryptsetupBinary, &key, args...) == nil
}

// clusterOSDUUIDs returns the fsids of the OSDs of this cluster
func clusterOSDUUIDs(context *clusterd.Context, clusterInfo *cephclient.ClusterInfo) (map[string]bool, error) {
	dump, err := cephclient.GetOSDDump(context, clusterInfo)
	if err != nil {
		return nil, err
	}
	uuids := map[string]bool{}
	for _, osd := range dump.OSDs {
		uuids[osd.UUID] = true
	}
	return uuids, nil
}

// osdUUIDFromLUKS returns the OSD fsid that ceph-volume writes to the LUKS2 subsystem of raw mode
// OSDs, "ceph_fsid=<osd fsid>"
func osdUUIDFromLUKS(dump string) string {
	match := luksLabelCephFSID.FindStringSubmatch(dump)
	if match == nil {
		return ""
	}
	id, err := uuid.Parse(strings.TrimSpace(match[1]))
	if err != nil {
		return ""
	}
	return id.String()
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
