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
	_ "embed"

	"github.com/rook/rook/pkg/daemon/ceph/osd/kms"
	"github.com/rook/rook/pkg/operator/ceph/controller"
	v1 "k8s.io/api/core/v1"
)

const (
	hostEncryptionOpenInitContainer = "encryption-open-host"
	hostEncryptionKeyVolumeName     = "osd-encryption-key"
	hostEncryptionKeyDir            = "/var/lib/rook-encryption-key"
)

//go:embed open-host-encrypted-block.sh
var openHostEncryptedBlockCode string

// hostOSDUsesKMS reports whether the OSD is an encrypted host-based raw mode OSD whose key is in
// the KMS
func (c *Cluster) hostOSDUsesKMS(osd OSDInfo, osdProps osdProperties) bool {
	return !osdProps.onPVC() && osd.Encrypted && osd.CVMode == "raw" && c.spec.Security.KeyManagementService.IsEnabled()
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
