/*
Copyright 2026.

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

package wake

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNamespace = "sandbox-system"

func TestSystemKeyReader_WaitForKey(t *testing.T) {
	tests := []struct {
		name      string
		secret    *corev1.Secret
		expectKey string
	}{
		{
			name: "non-empty key",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: SystemKeySecretName, Namespace: testNamespace},
				Data:       map[string][]byte{SystemKeyDataKey: []byte(" system-key \n")},
			},
			expectKey: " system-key \n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &SystemKeyReader{Reader: fakeClient(t, tt.secret), Namespace: testNamespace, Backoff: time.Millisecond}

			key, err := reader.WaitForKey(context.Background())

			require.NoError(t, err)
			assert.Equal(t, tt.expectKey, key)
		})
	}
}

func TestSystemKeyReader_WaitForKeyRetriesUntilReady(t *testing.T) {
	fc := fakeClient(t)
	reader := &SystemKeyReader{Reader: fc, Namespace: testNamespace, Backoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan string, 1)
	errs := make(chan error, 1)
	go func() {
		key, err := reader.WaitForKey(ctx)
		if err != nil {
			errs <- err
			return
		}
		result <- key
	}()

	time.Sleep(5 * time.Millisecond)
	require.NoError(t, fc.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SystemKeySecretName, Namespace: testNamespace},
		Data:       map[string][]byte{SystemKeyDataKey: []byte(" \n")},
	}))
	time.Sleep(5 * time.Millisecond)
	var stored corev1.Secret
	require.NoError(t, fc.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: SystemKeySecretName}, &stored))
	stored.Data[SystemKeyDataKey] = []byte("ready")
	require.NoError(t, fc.Update(context.Background(), &stored))

	select {
	case key := <-result:
		assert.Equal(t, "ready", key)
	case err := <-errs:
		t.Fatalf("EnsureKey returned error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EnsureKey")
	}
}

func TestSystemKeyReader_WaitForKeyHonorsDeadline(t *testing.T) {
	reader := &SystemKeyReader{
		Reader:    readOnlyMissingClient{},
		Namespace: testNamespace,
		Backoff:   time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	// readOnlyMissingClient implements only Reader (Get/List): WaitForKey returning
	// DeadlineExceeded rather than panicking confirms it issues only Get and never
	// attempts a Create/Update.
	_, err := reader.WaitForKey(ctx)

	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

type readOnlyMissingClient struct{}

func (readOnlyMissingClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return apierrors.NewNotFound(corev1.Resource("secrets"), SystemKeySecretName)
}

func (readOnlyMissingClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return nil
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

var _ client.Reader = readOnlyMissingClient{}
