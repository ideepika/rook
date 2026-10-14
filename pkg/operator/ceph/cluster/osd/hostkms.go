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
	"context"
	_ "embed"
	"fmt"
	"syscall"

	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/daemon/ceph/osd/kms"
	"github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"github.com/rook/rook/pkg/util/exec"
	"github.com/rook/rook/pkg/util/log"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	hostEncryptionOpenInitContainer = "encryption-open-host"
	hostEncryptionKeyVolumeName     = "osd-encryption-key"
	hostEncryptionKeyDir            = "/var/lib/rook-encryption-key"
)

//go:embed open-host-encrypted-block.sh
var openHostEncryptedBlockCode string

// hostOSDUsesKMS reports whether the OSD is an encrypted host-based raw mode OSD, whose key is in
// the KMS. Encrypted host OSDs only use raw mode with a KMS, so this does not depend on the
// current KMS settings: an OSD whose KMS settings were removed fails to open, and says why.
func hostOSDUsesKMS(osd OSDInfo, onPVC bool) bool {
	return !onPVC && osd.Encrypted && osd.CVMode == "raw"
}

// getHostEncryptionOpenInitContainer returns the init container that fetches the key of an
// encrypted host-based OSD from the KMS and opens its device, and the volumes it needs
func (c *Cluster) getHostEncryptionOpenInitContainer(osd OSDInfo, osdProps osdProperties) ([]v1.Volume, v1.Container) {
	keyVolume := v1.Volume{
		Name:         hostEncryptionKeyVolumeName,
		VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{Medium: v1.StorageMediumMemory}},
	}
	volumes := append([]v1.Volume{keyVolume}, c.kmsVolumes()...)

	envVars := c.getConfigEnvVars(osdProps, "", false)
	envVars = append(envVars, kms.ConfigToEnvVar(c.spec)...)
	envVars = append(envVars,
		blockPathEnvVariable(osd.BlockPath),
		v1.EnvVar{Name: "ROOK_OSD_UUID", Value: osd.UUID},
		v1.EnvVar{Name: "ROOK_ENCRYPTION_KEY_DIR", Value: hostEncryptionKeyDir},
	)

	mounts := []v1.VolumeMount{
		{Name: hostEncryptionKeyVolumeName, MountPath: hostEncryptionKeyDir},
		{Name: "devices", MountPath: "/dev"},
	}
	mounts = append(mounts, c.kmsVolumeMounts()...)

	return volumes, v1.Container{
		Name:            hostEncryptionOpenInitContainer,
		Image:           c.rookVersion,
		ImagePullPolicy: controller.GetContainerImagePullPolicy(c.spec.CephVersion.ImagePullPolicy),
		Command:         []string{"/bin/bash", "-c", openHostEncryptedBlockCode},
		Env:             envVars,
		VolumeMounts:    mounts,
		SecurityContext: controller.PrivilegedContext(true),
		Resources:       osdProps.resources,
	}
}

// removeMonCopyOfHostOSDKey removes the copy of the encryption key that "ceph-volume raw prepare"
// stores in the mons, so that the KMS holds the only copy. ceph-volume sends the key with
// "ceph osd new", which stores it as a config-key and creates a lockbox user that can read it.
// Raw mode OSDs never read it, since the OSD pod opens the device with the key from the KMS.
// The prepare pod's bootstrap-osd key may not delete either, so the operator does it.
func (c *Cluster) removeMonCopyOfHostOSDKey(osd OSDInfo) error {
	configKey := fmt.Sprintf("dm-crypt/osd/%s/luks", osd.UUID)
	// "config-key rm" writes to the mons even when the key is missing, so check first
	_, err := cephclient.NewCephCommand(c.context, c.clusterInfo, []string{"config-key", "exists", configKey}).Run()
	if code, ok := exec.ExitStatus(err); ok && code == int(syscall.ENOENT) {
		return nil
	} else if err != nil {
		return errors.Wrapf(err, "failed to check for the encryption key of OSD %d in the mons", osd.ID)
	}

	// The mons may hold the only copy stored under the fsid, if the prepare job failed to move the
	// key in the KMS to the fsid
	kmsConfig, err := kms.NewOperatorConfig(c.clusterInfo.Context, c.context, &c.spec, c.clusterInfo)
	if err != nil {
		return errors.Wrapf(err, "failed to check for the encryption key of OSD %d in the KMS", osd.ID)
	}
	if key, err := kmsConfig.GetSecret(osd.UUID); err != nil || key == "" {
		return errors.Errorf("not deleting the encryption key of OSD %d from the mons, the %q KMS does not hold it. %v", osd.ID, kmsConfig.Provider, err)
	}

	// Delete the user first: if the config-key went first and this failed, the next reconcile
	// would find no config-key and leave the user behind
	if err := cephclient.AuthDelete(c.context, c.clusterInfo, "client.osd-lockbox."+osd.UUID); err != nil {
		return errors.Wrapf(err, "failed to delete the lockbox user of OSD %d", osd.ID)
	}
	if _, err := cephclient.NewCephCommand(c.context, c.clusterInfo, []string{"config-key", "rm", configKey}).Run(); err != nil {
		return errors.Wrapf(err, "failed to delete the encryption key of OSD %d from the mons", osd.ID)
	}
	log.NamespacedInfo(c.clusterInfo.Namespace, logger, "deleted the copy of the encryption key of OSD %d from the mons; the KMS holds the only copy", osd.ID)
	return nil
}

// HostOSDKeyNames returns the names of the KMS keys of the encrypted host-based OSDs that have a
// deployment in the namespace. shortcut: OSDs whose deployment is already gone, and pending keys
// of failed prepare runs, are not listed; list the keys from the KMS if they turn out to remain.
func HostOSDKeyNames(ctx context.Context, clusterdContext *clusterd.Context, namespace string) ([]string, error) {
	selector := fmt.Sprintf("%s=%s,%s=true", k8sutil.AppAttr, AppName, encrypted)
	deployments, err := clusterdContext.Clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list encrypted osd deployments")
	}
	names := []string{}
	for _, d := range deployments.Items {
		container, err := findOSDContainer(d.Spec.Template.Spec.Containers)
		if err != nil {
			log.NamespacedWarning(namespace, logger, "not deleting the encryption key of OSD deployment %q. %v", d.Name, err)
			continue
		}
		// the env parsing of getOSDInfo() fails on more than this needs, and queries the nodes
		osd := OSDInfo{Encrypted: true}
		for _, env := range container.Env {
			switch env.Name {
			case "ROOK_OSD_UUID":
				osd.UUID = env.Value
			case "ROOK_CV_MODE":
				osd.CVMode = env.Value
			}
		}
		_, onPVC := d.Labels[OSDOverPVCLabelKey]
		if hostOSDUsesKMS(osd, onPVC) && osd.UUID != "" {
			names = append(names, osd.UUID)
		}
	}
	return names, nil
}
