/*
Copyright 2026 The Kubeflow authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sparkapplication

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/pkg/util"
)

func TestSpecUpdatePersistsInvalidation(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict=%t", conflict), func(t *testing.T) {
			oldApp, app := specUpdateApplications()
			before := app.DeepCopy()
			c := specUpdateClient(t, app)
			updates, apiReads := 0, 0
			writer := interceptor.NewClient(c, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, underlying client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					updates++
					if conflict && updates == 1 {
						// Advance the stored resource version before forwarding the stale status write.
						latest := app.DeepCopy()
						latest.Annotations = map[string]string{"external-update": "preserved"}
						require.NoError(t, c.Update(ctx, latest))
					}
					return underlying.SubResource(subresource).Update(ctx, obj, opts...)
				},
			})
			reader := interceptor.NewClient(c, interceptor.Funcs{
				Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					apiReads++
					return underlying.Get(ctx, key, obj, opts...)
				},
			})
			filter := NewSparkApplicationEventFilter(writer, reader, record.NewFakeRecorder(10), nil)

			require.True(t, filter.Update(event.UpdateEvent{ObjectOld: oldApp, ObjectNew: app}))
			require.Equal(t, before, app, "watch objects must remain immutable")
			stored := &v1beta2.SparkApplication{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(app), stored))
			require.Equal(t, v1beta2.ApplicationStateInvalidating, stored.Status.AppState.State)
			if conflict {
				require.Equal(t, 1, apiReads, "only conflicts need a live read")
				require.Equal(t, 2, updates)
				require.Equal(t, "preserved", stored.Annotations["external-update"])
			} else {
				require.Zero(t, apiReads)
				require.Equal(t, 1, updates)
			}
		})
	}
}

func TestSpecUpdateStillEnqueuesAfterWriteFailure(t *testing.T) {
	oldApp, app := specUpdateApplications()
	c := specUpdateClient(t, app)
	writer := interceptor.NewClient(c, interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return errors.New("API unavailable")
		},
	})
	filter := NewSparkApplicationEventFilter(writer, c, record.NewFakeRecorder(10), nil)

	require.True(t, filter.Update(event.UpdateEvent{ObjectOld: oldApp, ObjectNew: app}))
}

func TestSpecUpdateIgnoresDeletedApplication(t *testing.T) {
	oldApp, app := specUpdateApplications()
	c := specUpdateClient(t, app)
	require.NoError(t, c.Delete(context.Background(), app))
	recorder := record.NewFakeRecorder(10)
	filter := NewSparkApplicationEventFilter(c, c, recorder, nil)

	require.True(t, filter.Update(event.UpdateEvent{ObjectOld: oldApp, ObjectNew: app}))
	require.Empty(t, recorder.Events, "deletion is not an invalidation failure")
}

func TestSpecUpdateIgnoresSupersededEvents(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*v1beta2.SparkApplication)
	}{
		{
			name: "recreated application",
			change: func(app *v1beta2.SparkApplication) {
				app.UID = "replacement-uid"
			},
		},
		{
			name: "newer generation",
			change: func(app *v1beta2.SparkApplication) {
				app.Generation++
				app.Spec.Driver.CoreLimit = util.StringPtr("8")
			},
		},
		{
			name: "newer submission",
			change: func(app *v1beta2.SparkApplication) {
				app.Status.SubmissionID = "run-b"
			},
		},
		{
			name: "deleting application",
			change: func(app *v1beta2.SparkApplication) {
				now := metav1.Now()
				app.DeletionTimestamp = &now
				app.Finalizers = []string{"test-finalizer"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldApp, app := specUpdateApplications()
			latest := app.DeepCopy()
			test.change(latest)
			latest.ResourceVersion = "3"
			c := specUpdateClient(t, latest)
			filter := NewSparkApplicationEventFilter(c, c, record.NewFakeRecorder(10), nil)

			require.True(t, filter.Update(event.UpdateEvent{ObjectOld: oldApp, ObjectNew: app}))
			stored := &v1beta2.SparkApplication{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(app), stored))
			require.Equal(t, latest.Status, stored.Status)
			require.Equal(t, latest.ResourceVersion, stored.ResourceVersion)
		})
	}
}

// specUpdateApplications models a CPU-limit update while the recorded run stays the
// same, letting tests distinguish spec changes from a replacement application or run.
func specUpdateApplications() (*v1beta2.SparkApplication, *v1beta2.SparkApplication) {
	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "spec-update", Namespace: "default", UID: "app-uid", Generation: 2, ResourceVersion: "2"},
		Spec:       v1beta2.SparkApplicationSpec{Driver: v1beta2.DriverSpec{SparkPodSpec: v1beta2.SparkPodSpec{CoreLimit: util.StringPtr("4")}}},
		Status:     v1beta2.SparkApplicationStatus{SubmissionID: "run-a", AppState: v1beta2.ApplicationState{State: v1beta2.ApplicationStateRunning}},
	}
	oldApp := app.DeepCopy()
	oldApp.Generation = 1
	oldApp.ResourceVersion = "1"
	oldApp.Spec.Driver.CoreLimit = util.StringPtr("2")
	return oldApp, app
}

// specUpdateClient keeps status writes separate from spec updates, as the CRD does.
func specUpdateClient(t *testing.T, app *v1beta2.SparkApplication) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1beta2.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(app).WithStatusSubresource(&v1beta2.SparkApplication{}).Build()
}
