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
	"strings"
	"syscall"
	"testing"

	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/daemon/ceph/osd/kms"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRemoveMonCopyOfHostOSDKey(t *testing.T) {
	osd := OSDInfo{ID: 3, UUID: "9d1b4c0e-6f7a-4f53-9a8e-1f2b3c4d5e6f"}

	run := func(existsErr, delErr error, keyInKMS bool) ([]string, error) {
		var ran []string
		clientset := fake.NewSimpleClientset()
		if keyInKMS {
			// the default KMS keeps keys in Kubernetes Secrets
			_, err := clientset.CoreV1().Secrets("ns").Create(context.TODO(), &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: kms.GenerateOSDEncryptionSecretName(osd.UUID), Namespace: "ns"},
				Data:       map[string][]byte{kms.OsdEncryptionSecretNameKeyName: []byte("key")},
			}, metav1.CreateOptions{})
			assert.NoError(t, err)
		}
		executor := &exectest.MockExecutor{
			MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
				cmd := strings.Join(args[:2], " ")
				ran = append(ran, cmd+" "+args[2])
				switch cmd {
				case "config-key exists":
					return "", existsErr
				case "auth del":
					return "", delErr
				}
				return "", nil
			},
		}
		c := &Cluster{
			context:     &clusterd.Context{Executor: executor, Clientset: clientset},
			clusterInfo: cephclient.AdminTestClusterInfo("ns"),
			spec:        cephv1.ClusterSpec{},
		}
		return ran, c.removeMonCopyOfHostOSDKey(osd)
	}

	t.Run("key in the mons is deleted, user first", func(t *testing.T) {
		ran, err := run(nil, nil, true)
		assert.NoError(t, err)
		assert.Equal(t, []string{
			"config-key exists dm-crypt/osd/" + osd.UUID + "/luks",
			"auth del client.osd-lockbox." + osd.UUID,
			"config-key rm dm-crypt/osd/" + osd.UUID + "/luks",
		}, ran)
	})

	t.Run("no key in the mons writes nothing", func(t *testing.T) {
		ran, err := run(syscall.ENOENT, nil, true)
		assert.NoError(t, err)
		assert.Len(t, ran, 1)
	})

	t.Run("failed check is an error", func(t *testing.T) {
		ran, err := run(syscall.EIO, nil, true)
		assert.Error(t, err)
		assert.Len(t, ran, 1)
	})

	t.Run("key missing from the KMS keeps the mon copy", func(t *testing.T) {
		ran, err := run(nil, nil, false)
		assert.Error(t, err)
		assert.Len(t, ran, 1)
	})

	t.Run("failed user delete keeps the config-key for the retry", func(t *testing.T) {
		ran, err := run(nil, syscall.EIO, true)
		assert.Error(t, err)
		assert.Len(t, ran, 2)
	})
}
