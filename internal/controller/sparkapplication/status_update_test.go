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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubeflow/spark-operator/api/v1beta2"
)

func TestUpdateSparkApplicationStatusAfterContextCancelled(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	seed := &v1beta2.SparkApplication{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(seed).
		WithStatusSubresource(seed).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if err := ctx.Err(); err != nil {
					return err // what the real client's rate limiter does
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &Reconciler{client: c}

	app := &v1beta2.SparkApplication{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(seed), app); err != nil {
		t.Fatal(err)
	}
	app.Status.SubmissionID = "new-id"

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the manager cancels the reconcile context on shutdown

	if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
		t.Fatalf("status save failed after cancellation: %v", err)
	}
	got := &v1beta2.SparkApplication{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(seed), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.SubmissionID != "new-id" {
		t.Fatalf("SubmissionID = %q, want new-id", got.Status.SubmissionID)
	}
}
