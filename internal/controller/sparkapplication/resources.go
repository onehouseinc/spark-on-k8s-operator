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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/pkg/util"
)

func (r *Reconciler) getSparkResources(ctx context.Context, app *v1beta2.SparkApplication) ([]client.Object, error) {
	driverName := app.Status.DriverInfo.PodName
	if driverName == "" {
		driverName = util.GetDriverPodName(app)
	}
	objects := []client.Object{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: driverName, Namespace: app.Namespace}}}
	serviceName := app.Status.DriverInfo.WebUIServiceName
	if serviceName == "" && r.options.EnableUIService {
		serviceName = util.GetDefaultUIServiceName(app)
	}
	if serviceName != "" {
		objects = append(objects, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: app.Namespace}})
	}
	ingressName := app.Status.DriverInfo.WebUIIngressName
	if ingressName == "" && r.options.EnableUIService && r.options.IngressURLFormat != "" {
		ingressName = util.GetDefaultUIIngressName(app)
	}
	if ingressName != "" {
		meta := metav1.ObjectMeta{Name: ingressName, Namespace: app.Namespace}
		if util.IngressCapabilities.Has("extensions/v1beta1") && !util.IngressCapabilities.Has("networking.k8s.io/v1") {
			objects = append(objects, &extensionsv1beta1.Ingress{ObjectMeta: meta})
		} else {
			objects = append(objects, &networkingv1.Ingress{ObjectMeta: meta})
		}
	}
	var existing []client.Object
	for _, object := range objects {
		if err := r.options.APIReader.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		existing = append(existing, object)
	}
	return existing, nil
}

func (r *Reconciler) deleteSparkResources(ctx context.Context, app *v1beta2.SparkApplication) error {
	objects, err := r.getSparkResources(ctx, app)
	if err != nil {
		return err
	}
	return r.deleteObservedResources(ctx, app, objects)
}

func (r *Reconciler) deleteObservedResources(ctx context.Context, app *v1beta2.SparkApplication, objects []client.Object) error {
	for _, object := range objects {
		if !resourceBelongsToApp(app, object) {
			return fmt.Errorf("resource %s/%s does not belong to SparkApplication %s", object.GetNamespace(), object.GetName(), app.Name)
		}
		if !object.GetDeletionTimestamp().IsZero() {
			continue
		}
		uid := object.GetUID()
		version := object.GetResourceVersion()
		if err := r.client.Delete(ctx, object, client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
