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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

func TestSubmissionDeadlineIncludesPreparation(t *testing.T) {
	t.Setenv(common.EnvKubernetesServiceHost, "localhost")
	t.Setenv(common.EnvKubernetesServicePort, "443")
	invocation := filepath.Join(t.TempDir(), "invoked")
	t.Setenv("SUBMISSION_TEST_INVOCATION", invocation)
	installSparkSubmit(t, "touch \"$SUBMISSION_TEST_INVOCATION\"\n")

	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "deadline", Namespace: "default", UID: "app-uid", Generation: 1},
		Spec: v1beta2.SparkApplicationSpec{
			Image:        util.StringPtr("spark:test"),
			SparkVersion: "3.5.3",
		},
		Status: v1beta2.SparkApplicationStatus{SubmissionID: "recorded-attempt", LastSubmissionAttemptTime: metav1.Now()},
	}
	v1beta2.SetSparkApplicationDefaults(app)
	timeout := 5 * time.Second
	deadline, cancel := context.WithDeadline(context.Background(), app.Status.LastSubmissionAttemptTime.Add(timeout))
	defer cancel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	prepared := false
	slow := interceptor.NewClient(c, interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Service); ok {
				<-deadline.Done()
				prepared = true
			}
			return underlying.Create(ctx, obj, opts...)
		},
	})
	r := NewReconciler(nil, scheme, slow, record.NewFakeRecorder(10), nil, Options{
		APIReader: c, EnableUIService: true, SparkSubmitTimeout: timeout,
	})

	err := r.runSubmission(app)

	require.True(t, prepared)
	require.ErrorContains(t, err, "spark-submit timed out after 5s")
	_, err = os.Stat(invocation)
	require.ErrorIs(t, err, os.ErrNotExist, "an expired intent must not launch a new submit process")
}
