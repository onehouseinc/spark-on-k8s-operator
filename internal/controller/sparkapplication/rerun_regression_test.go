/*
Copyright 2024 The Kubeflow authors.

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

package sparkapplication_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/internal/controller/sparkapplication"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

func rerunFixture(t *testing.T, state v1beta2.ApplicationStateType) (*v1beta2.SparkApplication, client.WithWatch) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, v1beta2.AddToScheme(scheme))
	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "rerun", Namespace: "default", UID: "app-uid", Generation: 1},
		Spec:       v1beta2.SparkApplicationSpec{MainApplicationFile: util.StringPtr("local:///dummy.jar"), Image: util.StringPtr("spark:test")},
	}
	v1beta2.SetSparkApplicationDefaults(app)
	app.Spec.RestartPolicy = v1beta2.RestartPolicy{Type: v1beta2.RestartPolicyAlways, OnSubmissionFailureRetryInterval: util.Int64Ptr(1)}
	app.Status.AppState.State = state
	app.Status.SubmissionID = "old-id"
	app.Status.DriverInfo.PodName = util.GetDriverPodName(app)
	if state == v1beta2.ApplicationStateFailedSubmission {
		app.Status.SubmissionAttempts = 1
		app.Status.LastSubmissionAttemptTime = metav1.NewTime(time.Now().Add(-time.Hour))
	}
	return app, fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(app).WithObjects(app).Build()
}

func reconcileRerun(t *testing.T, c client.Client, app *v1beta2.SparkApplication) ctrl.Result {
	t.Helper()
	r := sparkapplication.NewReconciler(nil, c.Scheme(), c, record.NewFakeRecorder(100), nil, sparkapplication.Options{})
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	require.NoError(t, err)
	return result
}

func TestRerunWaitsForResourceDeletion(t *testing.T) {
	for _, resource := range []string{"driver", "service", "ingress"} {
		t.Run(resource, func(t *testing.T) {
			app, c := rerunFixture(t, v1beta2.ApplicationStatePendingRerun)
			var leftover client.Object
			switch resource {
			case "driver":
				leftover = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: app.Status.DriverInfo.PodName, Namespace: app.Namespace, Finalizers: []string{"test/hold"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			case "service":
				app.Status.DriverInfo.WebUIServiceName = "rerun-ui"
				leftover = &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "rerun-ui", Namespace: app.Namespace}}
			case "ingress":
				app.Status.DriverInfo.WebUIIngressName = "rerun-ui"
				leftover = &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "rerun-ui", Namespace: app.Namespace}}
			}
			require.NoError(t, c.Status().Update(context.Background(), app))
			require.NoError(t, c.Create(context.Background(), leftover))
			if resource == "driver" {
				require.NoError(t, c.Delete(context.Background(), leftover))
			}
			writes := 0
			counted := interceptor.NewClient(c, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				writes++
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}})
			for i := 0; i < 15; i++ {
				require.Equal(t, 5*time.Second, reconcileRerun(t, counted, app).RequeueAfter)
			}
			require.Zero(t, writes, "waiting must not write unchanged status every five seconds")
			if resource == "driver" {
				require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(leftover), leftover))
				leftover.SetFinalizers(nil)
				require.NoError(t, c.Update(context.Background(), leftover))
			} else {
				require.NoError(t, c.Delete(context.Background(), leftover))
			}
			// No pod event is sent: the scheduled reconciliation must attempt submission.
			t.Setenv(common.EnvSparkHome, t.TempDir())
			require.Zero(t, reconcileRerun(t, counted, app).RequeueAfter)
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(app), app))
			require.NotEqual(t, "old-id", app.Status.SubmissionID)
			require.EqualValues(t, 1, app.Status.SubmissionAttempts)
		})
	}
}

func TestRerunAdoptsDriverSubmissionID(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodRunning, corev1.PodPending} {
		t.Run(string(phase), func(t *testing.T) {
			app, c := rerunFixture(t, v1beta2.ApplicationStatePendingRerun)
			pod := createDriverPod(app.Name, app.Namespace)
			pod.Name = app.Status.DriverInfo.PodName
			pod.Labels[common.LabelSubmissionID] = "live-id"
			pod.Status.Phase = phase
			require.NoError(t, c.Create(context.Background(), pod))
			reconcileRerun(t, c, app)
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(app), app))
			require.Equal(t, "live-id", app.Status.SubmissionID)
		})
	}
}

func TestSubmissionConflictDoesNotResubmit(t *testing.T) {
	for _, state := range []v1beta2.ApplicationStateType{v1beta2.ApplicationStateNew, v1beta2.ApplicationStateFailedSubmission, v1beta2.ApplicationStatePendingRerun} {
		for _, outcome := range []string{"success", "failure", "new-generation", "new-id", "new-uid", "invalidating", "deleting"} {
			t.Run(fmt.Sprintf("%s/%s", state, outcome), func(t *testing.T) {
				app, c := rerunFixture(t, state)
				sparkHome := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(sparkHome, "bin"), 0755))
				command := filepath.Join(sparkHome, "bin", "spark-submit")
				script := "#!/bin/sh\nprintf 'launch\\n' >> \"$0.calls\"\n"
				if outcome == "failure" {
					script += "exit 1\n"
				}
				require.NoError(t, os.WriteFile(command, []byte(script), 0755))
				t.Setenv(common.EnvSparkHome, sparkHome)
				t.Setenv(common.EnvKubernetesServiceHost, "localhost")
				t.Setenv(common.EnvKubernetesServicePort, "443")
				writes, submissionID := 0, ""
				var superseding *v1beta2.SparkApplication
				conflicting := interceptor.NewClient(c, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					updated := obj.(*v1beta2.SparkApplication)
					writes++
					if writes == 1 {
						submissionID = updated.Status.SubmissionID
						if outcome != "success" && outcome != "failure" {
							latest := &v1beta2.SparkApplication{}
							require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(app), latest))
							switch outcome {
							case "new-generation":
								latest.Generation++
								require.NoError(t, c.Update(ctx, latest))
							case "new-id":
								latest.Status.SubmissionID = "newer-run"
								require.NoError(t, c.Status().Update(ctx, latest))
							case "new-uid":
								require.NoError(t, c.Delete(ctx, latest))
								latest.UID = "replacement-app-uid"
								latest.ResourceVersion = ""
								require.NoError(t, c.Create(ctx, latest))
							case "invalidating":
								latest.Status.AppState.State = v1beta2.ApplicationStateInvalidating
								require.NoError(t, c.Status().Update(ctx, latest))
							case "deleting":
								latest.Finalizers = []string{"test/hold"}
								require.NoError(t, c.Update(ctx, latest))
								require.NoError(t, c.Delete(ctx, latest))
								require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(app), latest))
							}
							superseding = latest.DeepCopy()
						}
					}
					if writes <= 2 {
						return apierrors.NewConflict(schema.GroupResource{Resource: "sparkapplications"}, app.Name, fmt.Errorf("cache still has pre-submit status"))
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				}})
				reconcileRerun(t, conflicting, app)
				calls, err := os.ReadFile(command + ".calls")
				require.NoError(t, err)
				require.Equal(t, 1, strings.Count(string(calls), "launch\n"))
				require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(app), app))
				if superseding != nil {
					require.Equal(t, superseding.Status, app.Status, "the submission must not overwrite a changed application")
					return
				}
				require.Equal(t, submissionID, app.Status.SubmissionID)
				attempts := 1
				if state == v1beta2.ApplicationStateFailedSubmission {
					attempts = 2
				}
				require.EqualValues(t, attempts, app.Status.SubmissionAttempts)
				if outcome == "success" {
					require.Equal(t, v1beta2.ApplicationStateSubmitted, app.Status.AppState.State)
				} else {
					require.Equal(t, v1beta2.ApplicationStateFailedSubmission, app.Status.AppState.State)
				}
			})
		}
	}
}
