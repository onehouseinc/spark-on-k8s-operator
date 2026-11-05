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

package sparkapplication

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/golang/glog"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kubeflow/spark-operator/api/v1beta2"
	"github.com/kubeflow/spark-operator/internal/metrics"
	"github.com/kubeflow/spark-operator/internal/scheduler"
	"github.com/kubeflow/spark-operator/internal/scheduler/kubescheduler"
	"github.com/kubeflow/spark-operator/internal/scheduler/volcano"
	"github.com/kubeflow/spark-operator/internal/scheduler/yunikorn"
	"github.com/kubeflow/spark-operator/pkg/common"
	"github.com/kubeflow/spark-operator/pkg/util"
)

var (
	logger = log.Log.WithName("")
)

// DefaultSubmissionTimeout sets the budget measured from recorded submission
// intent. Preparation consumes this budget before spark-submit can start.
const (
	DefaultSubmissionTimeout  = 10 * time.Minute
	submissionRecheckInterval = 5 * time.Second
	activeRecheckInterval     = 5 * time.Minute
)

// Options defines the options of the controller.
type Options struct {
	// APIReader bypasses the informer cache for decisions to submit, recover or
	// delete resources. The regular client still handles writes and executor lists.
	APIReader client.Reader
	// SubmissionTimeout is measured from LastSubmissionAttemptTime, not process
	// launch. All controller replicas must use the same value during handoff.
	SubmissionTimeout     time.Duration
	Namespaces            []string
	EnableUIService       bool
	IngressClassName      string
	IngressURLFormat      string
	DefaultBatchScheduler string

	DriverPodCreationGracePeriod time.Duration

	KubeSchedulerNames []string

	SparkApplicationMetrics *metrics.SparkApplicationMetrics
	SparkExecutorMetrics    *metrics.SparkExecutorMetrics

	MaxTrackedExecutorPerApp int
}

// Reconciler reconciles a SparkApplication object.
type Reconciler struct {
	manager  ctrl.Manager
	scheme   *runtime.Scheme
	client   client.Client
	recorder record.EventRecorder
	options  Options
	registry *scheduler.Registry
}

// Reconciler implements reconcile.Reconciler.
var _ reconcile.Reconciler = &Reconciler{}

// NewReconciler creates a new Reconciler instance.
func NewReconciler(
	manager ctrl.Manager,
	scheme *runtime.Scheme,
	client client.Client,
	recorder record.EventRecorder,
	registry *scheduler.Registry,
	options Options,
) *Reconciler {
	if options.APIReader == nil {
		options.APIReader = client
	}
	if options.SubmissionTimeout == 0 {
		options.SubmissionTimeout = DefaultSubmissionTimeout
	}
	return &Reconciler{
		manager:  manager,
		scheme:   scheme,
		client:   client,
		recorder: recorder,
		registry: registry,
		options:  options,
	}
}

// +kubebuilder:rbac:groups=,resources=pods,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=,resources=configmaps,verbs=get;list;create;update;patch;delete
// +kubebuilder:rbac:groups=,resources=services,verbs=get;create;delete
// +kubebuilder:rbac:groups=,resources=nodes,verbs=get
// +kubebuilder:rbac:groups=,resources=events,verbs=create;update;patch
// +kubebuilder:rbac:groups=,resources=resourcequotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=extensions,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get
// +kubebuilder:rbac:groups=sparkoperator.k8s.io,resources=sparkapplications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sparkoperator.k8s.io,resources=sparkapplications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sparkoperator.k8s.io,resources=sparkapplications/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SparkApplication object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.2/pkg/reconcile

// Reconcile handles Create, Update and Delete events of the custom resource.
// State Machine for SparkApplication:
// +--------------------------------------------------------------------------------------------------------------------+
// |        +---------------------------------------------------------------------------------------------+             |
// |        |       +----------+                                                                          |             |
// |        |       |          |                                                                          |             |
// |        |       |          |                                                                          |             |
// |        |       |Submission|                                                                          |             |
// |        |  +---->  Failed  +----+------------------------------------------------------------------+  |             |
// |        |  |    |          |    |                                                                  |  |             |
// |        |  |    |          |    |                                                                  |  |             |
// |        |  |    +----^-----+    |  +-----------------------------------------+                     |  |             |
// |        |  |         |          |  |                                         |                     |  |             |
// |        |  |         |          |  |                                         |                     |  |             |
// |      +-+--+----+    |    +-----v--+-+          +----------+           +-----v-----+          +----v--v--+          |
// |      |         |    |    |          |          |          |           |           |          |          |          |
// |      |         |    |    |          |          |          |           |           |          |          |          |
// |      |   New   +---------> Submitted+----------> Running  +----------->  Failing  +---------->  Failed  |          |
// |      |         |    |    |          |          |          |           |           |          |          |          |
// |      |         |    |    |          |          |          |           |           |          |          |          |
// |      |         |    |    |          |          |          |           |           |          |          |          |
// |      +---------+    |    +----^-----+          +-----+----+           +-----+-----+          +----------+          |
// |                     |         |                      |                      |                                      |
// |                     |         |                      |                      |                                      |
// |    +------------+   |         |             +-------------------------------+                                      |
// |    |            |   |   +-----+-----+       |        |                +-----------+          +----------+          |
// |    |            |   |   |  Pending  |       |        |                |           |          |          |          |
// |    |            |   +---+   Rerun   <-------+        +---------------->Succeeding +---------->Completed |          |
// |    |Invalidating|       |           <-------+                         |           |          |          |          |
// |    |            +------->           |       |                         |           |          |          |          |
// |    |            |       |           |       |                         |           |          |          |          |
// |    |            |       +-----------+       |                         +-----+-----+          +----------+          |
// |    +------------+                           |                               |                                      |
// |                                             |                               |                                      |
// |                                             +-------------------------------+                                      |
// |                                                                                                                    |
// +--------------------------------------------------------------------------------------------------------------------+
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := req.NamespacedName
	app, err := r.getSparkApplication(ctx, key)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, err
	}
	logger.Info("Reconciling SparkApplication", "name", app.Name, "namespace", app.Namespace, "state", app.Status.AppState.State)
	defer logger.Info("Finished reconciling SparkApplication", "name", app.Name, "namespace", app.Namespace)

	// Check if the spark application is being deleted
	if !app.DeletionTimestamp.IsZero() {
		return r.handleSparkApplicationDeletion(ctx, req)
	}
	switch app.Status.AppState.State {
	case v1beta2.ApplicationStateNew:
		return r.reconcileNewSparkApplication(ctx, req)
	case v1beta2.ApplicationStateSubmitted:
		return r.reconcileSubmittedSparkApplication(ctx, req)
	case v1beta2.ApplicationStateFailedSubmission:
		return r.reconcileFailedSubmissionSparkApplication(ctx, req)
	case v1beta2.ApplicationStateRunning:
		return r.reconcileRunningSparkApplication(ctx, req)
	case v1beta2.ApplicationStatePendingRerun:
		return r.reconcilePendingRerunSparkApplication(ctx, req)
	case v1beta2.ApplicationStateInvalidating:
		return r.reconcileInvalidatingSparkApplication(ctx, req)
	case v1beta2.ApplicationStateSucceeding:
		return r.reconcileSucceedingSparkApplication(ctx, req)
	case v1beta2.ApplicationStateFailing:
		return r.reconcileFailingSparkApplication(ctx, req)
	case v1beta2.ApplicationStateCompleted:
		return r.reconcileCompletedSparkApplication(ctx, req)
	case v1beta2.ApplicationStateFailed:
		return r.reconcileFailedSparkApplication(ctx, req)
	case v1beta2.ApplicationStateUnknown:
		return r.reconcileUnknownSparkApplication(ctx, req)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, options controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("spark-application-controller").
		Watches(
			&corev1.Pod{},
			NewSparkPodEventHandler(mgr.GetClient(), r.options.SparkExecutorMetrics),
			builder.WithPredicates(newSparkPodEventFilter(r.options.Namespaces)),
		).
		Watches(
			&v1beta2.SparkApplication{},
			NewSparkApplicationEventHandler(r.options.SparkApplicationMetrics),
			builder.WithPredicates(
				NewSparkApplicationEventFilter(
					mgr.GetClient(),
					mgr.GetAPIReader(),
					mgr.GetEventRecorderFor("spark-application-event-handler"),
					r.options.Namespaces,
				),
			),
		).
		WithOptions(options).
		Complete(r)
}

func (r *Reconciler) handleSparkApplicationDeletion(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := req.NamespacedName
	app, err := r.getSparkApplication(ctx, key)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, err
	}

	if err := r.deleteSparkResources(ctx, app); err != nil {
		logger.Error(err, "Failed to delete resources associated with SparkApplication", "name", app.Name, "namespace", app.Namespace)
		return ctrl.Result{Requeue: true}, err
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) reconcileNewSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.reconcileSubmission(ctx, req, v1beta2.ApplicationStateNew)
}

func (r *Reconciler) reconcileSubmittedSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := req.NamespacedName
	retryErr := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			old, err := r.getSparkApplication(ctx, key)
			if err != nil {
				return err
			}
			if old.Status.AppState.State != v1beta2.ApplicationStateSubmitted {
				return nil
			}
			app := old.DeepCopy()

			if err := r.updateSparkApplicationState(ctx, app); err != nil {
				return err
			}
			if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
				return err
			}
			return nil
		},
	)
	if retryErr != nil {
		logger.Error(retryErr, "Failed to reconcile SparkApplication", "name", key.Name, "namespace", key.Namespace)
		return ctrl.Result{}, retryErr
	}
	return ctrl.Result{RequeueAfter: activeRecheckInterval}, nil
}

func (r *Reconciler) reconcileFailedSubmissionSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.reconcileSubmission(ctx, req, v1beta2.ApplicationStateFailedSubmission)
}

func (r *Reconciler) reconcileRunningSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := req.NamespacedName
	retryErr := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			old, err := r.getSparkApplication(ctx, key)
			if err != nil {
				return err
			}
			if old.Status.AppState.State != v1beta2.ApplicationStateRunning {
				return nil
			}
			app := old.DeepCopy()

			if err := r.updateSparkApplicationState(ctx, app); err != nil {
				return err
			}

			if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
				return err
			}

			return nil
		},
	)
	if retryErr != nil {
		logger.Error(retryErr, "Failed to reconcile SparkApplication", "name", key.Name, "namespace", key.Namespace)
		return ctrl.Result{}, retryErr
	}
	return ctrl.Result{RequeueAfter: activeRecheckInterval}, nil
}

func (r *Reconciler) reconcilePendingRerunSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.reconcileSubmission(ctx, req, v1beta2.ApplicationStatePendingRerun)
}

func (r *Reconciler) reconcileInvalidatingSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := req.NamespacedName
	retryErr := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			old, err := r.getSparkApplication(ctx, key)
			if err != nil {
				return err
			}
			if old.Status.AppState.State != v1beta2.ApplicationStateInvalidating {
				return nil
			}
			app := old.DeepCopy()

			// Invalidate the current run and enqueue the SparkApplication for re-execution.
			if err := r.deleteSparkResources(ctx, app); err != nil {
				return err
			} else {
				r.resetSparkApplicationStatus(app)
				app.Status.AppState.State = v1beta2.ApplicationStatePendingRerun
			}
			if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
				return err
			}
			return nil
		},
	)
	if retryErr != nil {
		logger.Error(retryErr, "Failed to reconcile SparkApplication", "name", key.Name, "namespace", key.Namespace)
		return ctrl.Result{}, retryErr
	}
	return ctrl.Result{RequeueAfter: submissionRecheckInterval}, nil
}

func (r *Reconciler) reconcileSucceedingSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := req.NamespacedName
	retryErr := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			old, err := r.getSparkApplication(ctx, key)
			if err != nil {
				return err
			}
			if old.Status.AppState.State != v1beta2.ApplicationStateSucceeding {
				return nil
			}
			app := old.DeepCopy()

			if util.ShouldRetry(app) {
				if err := r.deleteSparkResources(ctx, app); err != nil {
					logger.Error(err, "failed to delete spark resources", "name", app.Name, "namespace", app.Namespace)
					return err
				}
				app.Status.AppState.State = v1beta2.ApplicationStatePendingRerun
			} else {
				app.Status.AppState.State = v1beta2.ApplicationStateCompleted
			}
			if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
				return err
			}
			return nil
		},
	)
	if retryErr != nil {
		logger.Error(retryErr, "Failed to reconcile SparkApplication", "name", key.Name, "namespace", key.Namespace)
		return ctrl.Result{}, retryErr
	}
	return ctrl.Result{RequeueAfter: submissionRecheckInterval}, nil
}

func (r *Reconciler) reconcileFailingSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := req.NamespacedName

	result := ctrl.Result{RequeueAfter: submissionRecheckInterval}

	retryErr := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			old, err := r.getSparkApplication(ctx, key)
			if err != nil {
				return err
			}
			if old.Status.AppState.State != v1beta2.ApplicationStateFailing {
				return nil
			}
			app := old.DeepCopy()

			if util.ShouldRetry(app) {
				timeUntilNextRetryDue, err := util.TimeUntilNextRetryDue(app)
				if err != nil {
					return err
				}
				if timeUntilNextRetryDue <= 0 {
					if err := r.deleteSparkResources(ctx, app); err != nil {
						logger.Error(err, "failed to delete spark resources", "name", app.Name, "namespace", app.Namespace)
						return err
					}
					app.Status.AppState.State = v1beta2.ApplicationStatePendingRerun
				} else {
					// If we're waiting before retrying then reconcile will not modify anything, so we need to requeue.
					result.RequeueAfter = timeUntilNextRetryDue
				}
			} else {
				app.Status.AppState.State = v1beta2.ApplicationStateFailed
			}
			if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
				return err
			}
			return nil
		},
	)
	if retryErr != nil {
		logger.Error(retryErr, "Failed to reconcile SparkApplication", "name", key.Name, "namespace", key.Namespace)
		return result, retryErr
	}
	return result, nil
}

func (r *Reconciler) reconcileCompletedSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.reconcileTerminatedSparkApplication(ctx, req)
}

func (r *Reconciler) reconcileFailedSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.reconcileTerminatedSparkApplication(ctx, req)
}

func (r *Reconciler) reconcileTerminatedSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := req.NamespacedName
	old, err := r.getSparkApplication(ctx, key)
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	app := old.DeepCopy()
	if !util.IsTerminated(app) {
		return ctrl.Result{}, nil
	}

	if util.IsExpired(app) {
		logger.Info("Deleting expired SparkApplication", "name", app.Name, "namespace", app.Namespace, "state", app.Status.AppState.State)
		if err := r.client.Delete(ctx, app, client.Preconditions{UID: &app.UID}); err != nil {
			return ctrl.Result{Requeue: true}, err
		}
		return ctrl.Result{}, nil
	}

	if err := r.updateExecutorState(ctx, app); err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	if err := r.cleanUpOnTermination(old, app); err != nil {
		logger.Error(err, "Failed to clean up resources for SparkApplication", "name", old.Name, "namespace", old.Namespace, "state", old.Status.AppState.State)
		return ctrl.Result{Requeue: true}, err
	}

	// If termination time or TTL is not set, will not requeue this application.
	if app.Status.TerminationTime.IsZero() || app.Spec.TimeToLiveSeconds == nil || *app.Spec.TimeToLiveSeconds <= 0 {
		return ctrl.Result{}, nil
	}

	// Otherwise, requeue the application for subsequent deletion.
	now := time.Now()
	ttl := time.Duration(*app.Spec.TimeToLiveSeconds) * time.Second
	survival := now.Sub(app.Status.TerminationTime.Time)

	// If survival time is greater than TTL, requeue the application immediately.
	if survival >= ttl {
		return ctrl.Result{Requeue: true}, nil
	}
	// Otherwise, requeue the application after (TTL - survival) seconds.
	return ctrl.Result{RequeueAfter: ttl - survival}, nil
}

func (r *Reconciler) reconcileUnknownSparkApplication(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := req.NamespacedName
	retryErr := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			old, err := r.getSparkApplication(ctx, key)
			if err != nil {
				return err
			}
			if old.Status.AppState.State != v1beta2.ApplicationStateUnknown {
				return nil
			}
			app := old.DeepCopy()

			if err := r.updateSparkApplicationState(ctx, app); err != nil {
				return err
			}
			if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
				return err
			}
			return nil
		},
	)
	if retryErr != nil {
		logger.Error(retryErr, "Failed to reconcile SparkApplication", "name", key.Name, "namespace", key.Namespace)
		return ctrl.Result{}, retryErr
	}
	return ctrl.Result{RequeueAfter: activeRecheckInterval}, nil
}

// getSparkApplication bypasses the informer cache so intent checks and
// status-conflict retries use the latest persisted state. Re-reading a stale
// cache entry could repeat submission or exhaust retries without seeing a write.
func (r *Reconciler) getSparkApplication(ctx context.Context, key client.ObjectKey) (*v1beta2.SparkApplication, error) {
	app := &v1beta2.SparkApplication{}
	if err := r.options.APIReader.Get(ctx, key, app); err != nil {
		return nil, err
	}
	return app, nil
}

// submitSparkApplication creates submission resources and runs spark-submit
// once. Its caller must persist the attempt identity first; this method must not
// be replayed merely because saving the outcome conflicts.
func (r *Reconciler) submitSparkApplication(app *v1beta2.SparkApplication) error {
	if util.PrometheusMonitoringEnabled(app) {
		logger.Info("Configure Prometheus monitoring for SparkApplication", "name", app.Name, "namespace", app.Namespace)
		if err := configPrometheusMonitoring(app, r.client); err != nil {
			return fmt.Errorf("failed to configure Prometheus monitoring: %v", err)
		}
	}

	// Use batch scheduler to perform scheduling task before submitting (before build command arguments).
	if needScheduling, scheduler := r.shouldDoBatchScheduling(app); needScheduling {
		logger.Info("Do batch scheduling for SparkApplication", "name", app.Name, "namespace", app.Namespace)
		if err := scheduler.Schedule(app); err != nil {
			return fmt.Errorf("failed to process batch scheduler: %v", err)
		}
	}

	// Create web UI service for spark applications if enabled.
	if r.options.EnableUIService {
		service, err := r.createWebUIService(app)
		if err != nil {
			return fmt.Errorf("failed to create web UI service: %v", err)
		}
		app.Status.DriverInfo.WebUIServiceName = service.serviceName
		app.Status.DriverInfo.WebUIPort = service.servicePort
		app.Status.DriverInfo.WebUIAddress = fmt.Sprintf("%s:%d", service.serviceIP, app.Status.DriverInfo.WebUIPort)
		logger.Info("Created web UI service for SparkApplication", "name", app.Name, "namespace", app.Namespace)

		// Create UI Ingress if ingress-format is set.
		if r.options.IngressURLFormat != "" {
			// We are going to want to use an ingress url.
			ingressURL, err := getDriverIngressURL(r.options.IngressURLFormat, app.Name, app.Namespace)
			if err != nil {
				return fmt.Errorf("failed to get ingress url: %v", err)
			}
			// need to ensure the spark.ui variables are configured correctly if a subPath is used.
			if ingressURL.Path != "" {
				if app.Spec.SparkConf == nil {
					app.Spec.SparkConf = make(map[string]string)
				}
				app.Spec.SparkConf[common.SparkUIProxyBase] = ingressURL.Path
				app.Spec.SparkConf[common.SparkUIProxyRedirectURI] = "/"
			}
			ingress, err := r.createWebUIIngress(app, *service, ingressURL, r.options.IngressClassName)
			if err != nil {
				return fmt.Errorf("failed to create web UI ingress: %v", err)
			}
			app.Status.DriverInfo.WebUIIngressAddress = ingress.ingressURL.String()
			app.Status.DriverInfo.WebUIIngressName = ingress.ingressName
			logger.Info("Created web UI ingress for SparkApplication", "name", app.Name, "namespace", app.Namespace)
		}
	}

	for _, driverIngressConfiguration := range app.Spec.DriverIngressOptions {
		logger.Info("Creating driver ingress service for SparkApplication", "name", app.Name, "namespace", app.Namespace)
		service, err := r.createDriverIngressServiceFromConfiguration(app, &driverIngressConfiguration)
		if err != nil {
			return fmt.Errorf("failed to create driver ingress service for SparkApplication: %v", err)
		}
		// Create ingress if ingress-format is set.
		if driverIngressConfiguration.IngressURLFormat != "" {
			// We are going to want to use an ingress url.
			ingressURL, err := getDriverIngressURL(driverIngressConfiguration.IngressURLFormat, app.Name, app.Namespace)
			if err != nil {
				return fmt.Errorf("failed to get driver ingress url: %v", err)
			}
			ingress, err := r.createDriverIngress(app, &driverIngressConfiguration, *service, ingressURL, r.options.IngressClassName)
			if err != nil {
				return fmt.Errorf("failed to create driver ingress: %v", err)
			}
			logger.V(1).Info("Created driver ingress for SparkApplication", "name", app.Name, "namespace", app.Namespace, "ingressName", ingress.ingressName, "ingressURL", ingress.ingressURL)
		}
	}

	defer func() {
		if err := r.cleanUpPodTemplateFiles(app); err != nil {
			logger.Error(fmt.Errorf("failed to clean up pod template files: %v", err), "name", app.Name, "namespace", app.Namespace)
		}
	}()

	sparkSubmitArgs, err := buildSparkSubmitArgs(app)
	if err != nil {
		return fmt.Errorf("failed to build spark-submit arguments: %v", err)
	}

	// Try submitting the application by running spark-submit.
	logger.Info("Running spark-submit for SparkApplication", "name", app.Name, "namespace", app.Namespace, "arguments", sparkSubmitArgs)
	if err := runSparkSubmit(newSubmission(sparkSubmitArgs, app), r.options.SubmissionTimeout); err != nil {
		r.recordSparkApplicationEvent(app)
		return fmt.Errorf("failed to run spark-submit: %v", err)
	}
	return nil
}

// updateDriverState derives application status from a live driver read after
// checking ownership, spec generation and submission identity. It updates only
// the in-memory app; the caller persists it with resource-version protection.
func (r *Reconciler) updateDriverState(ctx context.Context, app *v1beta2.SparkApplication) error {
	// Either the driver pod doesn't exist yet or its name has not been updated.
	if app.Status.DriverInfo.PodName == "" {
		return fmt.Errorf("empty driver pod name with application state %s", app.Status.AppState.State)
	}

	driverPod, err := r.getDriverPod(ctx, app)
	if err != nil {
		return err
	}

	if driverPod == nil {
		if app.Status.AppState.State != v1beta2.ApplicationStateSubmitted || metav1.Now().Sub(app.Status.LastSubmissionAttemptTime.Time) > r.options.DriverPodCreationGracePeriod {
			glog.Warningf("Expected driver pod %s/%s was not found for SparkApplication %s (state=%s, submissionID=%s)",
				app.Namespace, app.Status.DriverInfo.PodName, app.Name, app.Status.AppState.State, app.Status.SubmissionID)
			app.Status.AppState.State = v1beta2.ApplicationStateFailing
			app.Status.AppState.ErrorMessage = "driver pod not found"
			app.Status.TerminationTime = metav1.Now()
			return nil
		}
		return fmt.Errorf("driver pod not found, while inside the grace period. Grace period of %v expires at %v", r.options.DriverPodCreationGracePeriod, app.Status.LastSubmissionAttemptTime.Add(r.options.DriverPodCreationGracePeriod))
	}

	if !resourceBelongsToApp(app, driverPod) {
		return fmt.Errorf("driver pod %s does not belong to SparkApplication %s", driverPod.Name, app.Name)
	}
	if driverGenerationChanged(app, driverPod) {
		app.Status.AppState = v1beta2.ApplicationState{State: v1beta2.ApplicationStateInvalidating}
		return nil
	}
	if driverPod.Labels[common.LabelSubmissionID] != app.Status.SubmissionID {
		return fmt.Errorf("driver pod %s has an unrecorded submission ID", driverPod.Name)
	}

	app.Status.SparkApplicationID = util.GetSparkApplicationID(driverPod)
	driverState := util.GetDriverState(driverPod)
	if util.IsDriverTerminated(driverState) {
		if app.Status.TerminationTime.IsZero() {
			app.Status.TerminationTime = metav1.Now()
		}
		if driverState == v1beta2.DriverStateFailed {
			if state := util.GetDriverContainerTerminatedState(driverPod); state != nil {
				if state.ExitCode != 0 {
					app.Status.AppState.ErrorMessage = fmt.Sprintf("driver container failed with ExitCode: %d, Reason: %s", state.ExitCode, state.Reason)
				}
			} else {
				app.Status.AppState.ErrorMessage = "driver container status missing"
			}
		}
	}

	newState := util.DriverStateToApplicationState(driverState)
	// Only record a driver event if the application state (derived from the driver pod phase) has changed.
	if newState != app.Status.AppState.State {
		r.recordDriverEvent(app, driverState, driverPod.Name)
		app.Status.AppState.State = newState
	}

	return nil
}

// updateExecutorState lists the executor pods of the application
// and updates the executor state based on the current phase of the pods.
func (r *Reconciler) updateExecutorState(_ context.Context, app *v1beta2.SparkApplication) error {
	podList, err := r.getExecutorPods(app)
	if err != nil {
		return err
	}
	pods := podList.Items

	executorStateMap := make(map[string]v1beta2.ExecutorState)
	var executorApplicationID string
	for _, pod := range pods {
		if util.IsExecutorPod(&pod) {
			// If the executor number is higher than the `MaxTrackedExecutorPerApp` we want to stop persisting executors
			if executorID, _ := strconv.Atoi(util.GetSparkExecutorID(&pod)); executorID > r.options.MaxTrackedExecutorPerApp {
				continue
			}
			newState := util.GetExecutorState(&pod)
			oldState, exists := app.Status.ExecutorState[pod.Name]
			// Only record an executor event if the executor state is new or it has changed.
			if !exists || newState != oldState {
				if newState == v1beta2.ExecutorStateFailed {
					execContainerState := util.GetExecutorContainerTerminatedState(&pod)
					if execContainerState != nil {
						r.recordExecutorEvent(app, newState, pod.Name, execContainerState.ExitCode, execContainerState.Reason)
					} else {
						// If we can't find the container state,
						// we need to set the exitCode and the Reason to unambiguous values.
						r.recordExecutorEvent(app, newState, pod.Name, -1, "Unknown (Container not Found)")
					}
				} else {
					r.recordExecutorEvent(app, newState, pod.Name)
				}
			}
			executorStateMap[pod.Name] = newState

			if executorApplicationID == "" {
				executorApplicationID = util.GetSparkApplicationID(&pod)
			}
		}
	}

	// ApplicationID label can be different on driver/executors. Prefer executor ApplicationID if set.
	// Refer https://issues.apache.org/jira/projects/SPARK/issues/SPARK-25922 for details.
	if executorApplicationID != "" {
		app.Status.SparkApplicationID = executorApplicationID
	}

	if app.Status.ExecutorState == nil {
		app.Status.ExecutorState = make(map[string]v1beta2.ExecutorState)
	}
	for name, state := range executorStateMap {
		app.Status.ExecutorState[name] = state
	}

	// Handle missing/deleted executors.
	for name, oldStatus := range app.Status.ExecutorState {
		_, exists := executorStateMap[name]
		if !util.IsExecutorTerminated(oldStatus) && !exists {
			if !util.IsDriverRunning(app) {
				// If ApplicationState is COMPLETED, in other words, the driver pod has been completed
				// successfully. The executor pods terminate and are cleaned up, so we could not found
				// the executor pod, under this circumstances, we assume the executor pod are completed.
				if app.Status.AppState.State == v1beta2.ApplicationStateCompleted {
					app.Status.ExecutorState[name] = v1beta2.ExecutorStateCompleted
				} else {
					glog.Infof("Executor pod %s not found, assuming it was deleted.", name)
					app.Status.ExecutorState[name] = v1beta2.ExecutorStateFailed
				}
			} else {
				app.Status.ExecutorState[name] = v1beta2.ExecutorStateUnknown
			}
		}
	}

	return nil
}

func (r *Reconciler) getExecutorPods(app *v1beta2.SparkApplication) (*corev1.PodList, error) {
	matchLabels := util.GetResourceLabels(app)
	matchLabels[common.LabelSparkRole] = common.SparkRoleExecutor
	pods := &corev1.PodList{}
	if err := r.client.List(context.TODO(), pods, client.InNamespace(app.Namespace), client.MatchingLabels(matchLabels)); err != nil {
		return nil, fmt.Errorf("failed to get pods for SparkApplication %s/%s: %v", app.Namespace, app.Name, err)
	}
	return pods, nil
}

// getDriverPod reads the recorded driver name, falling back to the name
// derived from the spec when status is incomplete. A nil pod without an error
// means NotFound; API failures must not be treated as permission to submit again.
// The caller decides whether absence is expected or warrants a warning.
func (r *Reconciler) getDriverPod(ctx context.Context, app *v1beta2.SparkApplication) (*corev1.Pod, error) {
	name := app.Status.DriverInfo.PodName
	if name == "" {
		name = util.GetDriverPodName(app)
	}
	pod := &corev1.Pod{}
	if err := r.options.APIReader.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: name}, pod); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return pod, nil
}

func (r *Reconciler) updateSparkApplicationState(ctx context.Context, app *v1beta2.SparkApplication) error {
	if err := r.updateDriverState(ctx, app); err != nil {
		return err
	}

	if err := r.updateExecutorState(ctx, app); err != nil {
		return err
	}

	return nil
}

// updateSparkApplicationStatus updates the status of the SparkApplication.
func (r *Reconciler) updateSparkApplicationStatus(ctx context.Context, app *v1beta2.SparkApplication) error {
	if err := r.client.Status().Update(ctx, app); err != nil {
		// Log failure with details
		logger.Error(err, "FAILED to update SparkApplication status",
			"name", app.Name,
			"namespace", app.Namespace,
			"state", app.Status.AppState.State,
			"submissionID", app.Status.SubmissionID,
			"driverPodName", app.Status.DriverInfo.PodName,
			"errorType", fmt.Sprintf("%T", err))
		return err
	}
	// Log before status update
	logger.Info("Updated SparkApplication status to API server",
		"name", app.Name,
		"namespace", app.Namespace,
		"state", app.Status.AppState.State,
		"submissionID", app.Status.SubmissionID,
		"driverPodName", app.Status.DriverInfo.PodName,
		"resourceVersion", app.ResourceVersion)

	return nil
}

// getSparkResources reads the driver and operator-managed UI
// resources that must disappear before another attempt. Recorded names survive
// invalidation; derived names cover a crash before older operators saved them.
// Live reads include resources that the cache has not observed or does not watch.
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

// deleteSparkResources reads and requests deletion of the driver and UI
// resources. A successful request does not mean deletion has finished; the next
// submission must confirm their absence before allocating a replacement ID.
func (r *Reconciler) deleteSparkResources(ctx context.Context, app *v1beta2.SparkApplication) error {
	objects, err := r.getSparkResources(ctx, app)
	if err != nil {
		return err
	}
	return r.deleteObservedResources(ctx, app, objects)
}

// deleteObservedResources deletes only the owned object versions that
// were read. UID and resourceVersion preconditions prevent deleting a replacement
// or a resource whose ownership changed between inspection and deletion.
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

func (r *Reconciler) recordSparkApplicationEvent(app *v1beta2.SparkApplication) {
	switch app.Status.AppState.State {
	case v1beta2.ApplicationStateNew:
		r.recorder.Eventf(
			app,
			corev1.EventTypeNormal,
			common.EventSparkApplicationAdded,
			"SparkApplication %s was added, enqueuing it for submission",
			app.Name,
		)
	case v1beta2.ApplicationStateSubmitted:
		r.recorder.Eventf(
			app,
			corev1.EventTypeNormal,
			common.EventSparkApplicationSubmitted,
			"SparkApplication %s was submitted successfully",
			app.Name,
		)
	case v1beta2.ApplicationStateFailedSubmission:
		r.recorder.Eventf(
			app,
			corev1.EventTypeWarning,
			common.EventSparkApplicationSubmissionFailed,
			"failed to submit SparkApplication %s: %s",
			app.Name,
			app.Status.AppState.ErrorMessage,
		)
	case v1beta2.ApplicationStateCompleted:
		r.recorder.Eventf(
			app,
			corev1.EventTypeNormal,
			common.EventSparkApplicationCompleted,
			"SparkApplication %s completed",
			app.Name,
		)
	case v1beta2.ApplicationStateFailed:
		r.recorder.Eventf(
			app,
			corev1.EventTypeWarning,
			common.EventSparkApplicationFailed,
			"SparkApplication %s failed: %s",
			app.Name,
			app.Status.AppState.ErrorMessage,
		)
	case v1beta2.ApplicationStatePendingRerun:
		r.recorder.Eventf(
			app,
			corev1.EventTypeWarning,
			common.EventSparkApplicationPendingRerun,
			"SparkApplication %s is pending rerun",
			app.Name,
		)
	}
}

func (r *Reconciler) recordDriverEvent(app *v1beta2.SparkApplication, state v1beta2.DriverState, name string) {
	switch state {
	case v1beta2.DriverStatePending:
		r.recorder.Eventf(app, corev1.EventTypeNormal, common.EventSparkDriverPending, "Driver %s is pending", name)
	case v1beta2.DriverStateRunning:
		r.recorder.Eventf(app, corev1.EventTypeNormal, common.EventSparkDriverRunning, "Driver %s is running", name)
	case v1beta2.DriverStateCompleted:
		r.recorder.Eventf(app, corev1.EventTypeNormal, common.EventSparkDriverCompleted, "Driver %s completed", name)
	case v1beta2.DriverStateFailed:
		r.recorder.Eventf(app, corev1.EventTypeWarning, common.EventSparkDriverFailed, "Driver %s failed", name)
	case v1beta2.DriverStateUnknown:
		r.recorder.Eventf(app, corev1.EventTypeWarning, common.EventSparkDriverUnknown, "Driver %s in unknown state", name)
	}
}

func (r *Reconciler) recordExecutorEvent(app *v1beta2.SparkApplication, state v1beta2.ExecutorState, args ...interface{}) {
	switch state {
	case v1beta2.ExecutorStatePending:
		r.recorder.Eventf(app, corev1.EventTypeNormal, common.EventSparkExecutorPending, "Executor %s is pending", args)
	case v1beta2.ExecutorStateRunning:
		r.recorder.Eventf(app, corev1.EventTypeNormal, common.EventSparkExecutorRunning, "Executor %s is running", args)
	case v1beta2.ExecutorStateCompleted:
		r.recorder.Eventf(app, corev1.EventTypeNormal, common.EventSparkExecutorCompleted, "Executor %s completed", args)
	case v1beta2.ExecutorStateFailed:
		r.recorder.Eventf(app, corev1.EventTypeWarning, common.EventSparkExecutorFailed, "Executor %s failed with ExitCode: %d, Reason: %s", args)
	case v1beta2.ExecutorStateUnknown:
		r.recorder.Eventf(app, corev1.EventTypeWarning, common.EventSparkExecutorUnknown, "Executor %s in unknown state", args)
	}
}

func (r *Reconciler) resetSparkApplicationStatus(app *v1beta2.SparkApplication) {
	status := &app.Status
	switch status.AppState.State {
	case v1beta2.ApplicationStateInvalidating:
		status.SparkApplicationID = ""
		status.SubmissionAttempts = 0
		status.ExecutionAttempts = 0
		status.LastSubmissionAttemptTime = metav1.Time{}
		status.TerminationTime = metav1.Time{}
		status.AppState.ErrorMessage = ""
		status.ExecutorState = nil
	case v1beta2.ApplicationStatePendingRerun:
		status.SparkApplicationID = ""
		status.SubmissionAttempts = 0
		status.LastSubmissionAttemptTime = metav1.Time{}
		status.DriverInfo = v1beta2.DriverInfo{}
		status.AppState.ErrorMessage = ""
		status.ExecutorState = nil
	}
}

func (r *Reconciler) shouldDoBatchScheduling(app *v1beta2.SparkApplication) (bool, scheduler.Interface) {
	// If batch scheduling isn't enabled
	if r.registry == nil {
		return false, nil
	}

	schedulerName := r.options.DefaultBatchScheduler
	if app.Spec.BatchScheduler != nil && *app.Spec.BatchScheduler != "" {
		schedulerName = *app.Spec.BatchScheduler
	}

	// If both the default and app batch scheduler are unspecified or empty
	if schedulerName == "" {
		return false, nil
	}

	var err error
	var scheduler scheduler.Interface

	switch schedulerName {
	case common.VolcanoSchedulerName:
		config := &volcano.Config{
			RestConfig: r.manager.GetConfig(),
		}
		scheduler, err = r.registry.GetScheduler(schedulerName, config)
	case yunikorn.SchedulerName:
		scheduler, err = r.registry.GetScheduler(schedulerName, nil)
	}

	for _, name := range r.options.KubeSchedulerNames {
		if schedulerName == name {
			config := &kubescheduler.Config{
				SchedulerName: name,
				Client:        r.manager.GetClient(),
			}
			scheduler, err = r.registry.GetScheduler(name, config)
		}
	}

	if err != nil || scheduler == nil {
		logger.Error(err, "Failed to get scheduler for SparkApplication", "name", app.Name, "namespace", app.Namespace, "scheduler", schedulerName)
		return false, nil
	}
	return scheduler.ShouldSchedule(app), scheduler
}

// Clean up when the spark application is terminated.
func (r *Reconciler) cleanUpOnTermination(_, newApp *v1beta2.SparkApplication) error {
	if needScheduling, scheduler := r.shouldDoBatchScheduling(newApp); needScheduling {
		if err := scheduler.Cleanup(newApp); err != nil {
			return err
		}
	}
	return nil
}

// cleanUpPodTemplateFiles cleans up the driver and executor pod template files.
func (r *Reconciler) cleanUpPodTemplateFiles(app *v1beta2.SparkApplication) error {
	if app.Spec.Driver.Template == nil && app.Spec.Executor.Template == nil {
		return nil
	}
	path := fmt.Sprintf("/tmp/spark/%s", app.Status.SubmissionID)
	if err := os.RemoveAll(path); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	logger.V(1).Info("Deleted pod template files", "path", path)
	return nil
}

// reconcileSubmission handles NEW, SUBMISSION_FAILED and PENDING_RERUN.
// It first recovers any recorded attempt, then applies retry policy and cleanup
// before starting another one. The expected state prevents a retry from acting on
// an application that another writer has already advanced or invalidated.
func (r *Reconciler) reconcileSubmission(ctx context.Context, req ctrl.Request, expectedState v1beta2.ApplicationStateType) (ctrl.Result, error) {
	result := ctrl.Result{RequeueAfter: submissionRecheckInterval}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := r.getSparkApplication(ctx, req.NamespacedName)
		if err != nil {
			return client.IgnoreNotFound(err)
		}
		if app.Status.AppState.State != expectedState || !app.DeletionTimestamp.IsZero() {
			return nil
		}
		if handled, err := r.recoverSubmission(ctx, app); handled || err != nil {
			return err
		}
		// A previous operator may still be submitting after leadership changes.
		// Keep its identity until the bounded submit process has had time to exit.
		if app.Status.SubmissionID != "" && !app.Status.LastSubmissionAttemptTime.IsZero() &&
			app.Status.TerminationTime.IsZero() && app.Status.AppState.ErrorMessage == "" {
			if time.Since(app.Status.LastSubmissionAttemptTime.Time) < r.options.SubmissionTimeout {
				return nil
			}
			// Save the expired attempt before applying retry policy on the next pass;
			// otherwise a lost outcome could bypass Never or exhaust the budget unnoticed.
			app.Status.SubmissionAttempts++
			app.Status.AppState = v1beta2.ApplicationState{
				State:        v1beta2.ApplicationStateFailedSubmission,
				ErrorMessage: "submission timed out before driver creation",
			}
			return r.updateSparkApplicationStatus(ctx, app)
		}
		if expectedState == v1beta2.ApplicationStateFailedSubmission {
			if !util.ShouldRetry(app) {
				app.Status.AppState.State = v1beta2.ApplicationStateFailed
				app.Status.TerminationTime = metav1.Now()
				return r.updateSparkApplicationStatus(ctx, app)
			}
			delay, err := util.TimeUntilNextRetryDue(app)
			if err != nil {
				return err
			}
			if delay > 0 {
				result.RequeueAfter = delay
				return nil
			}
		}
		resources, err := r.getSparkResources(ctx, app)
		if err != nil {
			return err
		}
		if len(resources) != 0 {
			if expectedState == v1beta2.ApplicationStateNew && r.options.SparkApplicationMetrics != nil {
				r.options.SparkApplicationMetrics.IncOrphanedResourceCleanupCount(app)
			}
			return r.deleteObservedResources(ctx, app, resources)
		}
		if expectedState == v1beta2.ApplicationStatePendingRerun {
			r.resetSparkApplicationStatus(app)
		}
		return r.startSubmissionAttempt(ctx, app)
	})
	return result, err
}

// recoverSubmission uses the live driver to recover a lost submit outcome.
// A true result means this pass recovered, is waiting for, or rejected that driver;
// the caller must not start another submission. False permits normal retry/cleanup
// only when the error is nil. Retired and completed attempts are never revived.
func (r *Reconciler) recoverSubmission(ctx context.Context, app *v1beta2.SparkApplication) (bool, error) {
	pod, err := r.getDriverPod(ctx, app)
	if err != nil || pod == nil {
		return false, err
	}
	if pod.Labels[common.LabelSparkAppName] != app.Name || !resourceBelongsToApp(app, pod) {
		return true, fmt.Errorf("driver pod %s does not belong to SparkApplication %s", pod.Name, app.Name)
	}
	if !pod.DeletionTimestamp.IsZero() {
		return true, nil
	}
	if driverGenerationChanged(app, pod) {
		app.Status.AppState = v1beta2.ApplicationState{State: v1beta2.ApplicationStateInvalidating}
		return true, r.updateSparkApplicationStatus(ctx, app)
	}
	if pod.Labels[common.LabelSubmissionID] != "" && pod.Labels[common.LabelSubmissionID] != app.Status.SubmissionID {
		return true, fmt.Errorf("driver pod %s has an unrecorded submission ID; refusing to adopt or delete it", pod.Name)
	}
	// Invalidation leaves the old ID but clears the attempt time. A finished run
	// retains its termination time. Neither is an interrupted new submission.
	if app.Status.SubmissionID == "" || app.Status.LastSubmissionAttemptTime.IsZero() || !app.Status.TerminationTime.IsZero() {
		return false, nil
	}
	missingConfigMapName, err := r.missingDriverConfigMap(ctx, pod)
	if err != nil {
		return true, err
	}
	if missingConfigMapName != "" && time.Since(app.Status.LastSubmissionAttemptTime.Time) < r.options.SubmissionTimeout {
		return true, nil
	}
	// A saved failure already counted its attempt. A new intent clears the error
	// even when it starts from SUBMISSION_FAILED, so its lost outcome still counts.
	if app.Status.AppState.State != v1beta2.ApplicationStateFailedSubmission || app.Status.AppState.ErrorMessage == "" {
		app.Status.SubmissionAttempts++
	}
	if missingConfigMapName != "" {
		app.Status.AppState = v1beta2.ApplicationState{
			State:        v1beta2.ApplicationStateFailedSubmission,
			ErrorMessage: fmt.Sprintf("submission timed out waiting for driver ConfigMap %s", missingConfigMapName),
		}
		// Persist failure before deleting its evidence. A failed status write must
		// leave the partial driver available so the next pass can count the attempt.
		if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
			return true, err
		}
		return true, r.deleteObservedResources(ctx, app, []client.Object{pod})
	}
	app.Status.ExecutionAttempts++
	app.Status.AppState.ErrorMessage = ""
	app.Status.ExecutorState = nil
	app.Status.DriverInfo.PodName = pod.Name
	if err := r.updateSparkApplicationState(ctx, app); err != nil {
		return true, err
	}
	return true, r.updateSparkApplicationStatus(ctx, app)
}

// resourceBelongsToApp requires evidence tied to this application's UID,
// not just its reusable name. Owner references support older resources; a UID
// annotation supports drivers created without the webhook. Conflicting stamps veto both.
func resourceBelongsToApp(app *v1beta2.SparkApplication, object client.Object) bool {
	if uid, stamped := object.GetAnnotations()[common.AnnotationAppUID]; stamped && uid != string(app.UID) {
		return false
	}
	if owner := metav1.GetControllerOf(object); owner != nil {
		return owner.Kind == "SparkApplication" && owner.Name == app.Name && owner.UID == app.UID
	}
	return object.GetAnnotations()[common.AnnotationAppUID] == string(app.UID) && app.UID != ""
}

// missingDriverConfigMap checks the generated ConfigMap needed by a Pending
// driver. It returns its name only after a live NotFound response; an empty name
// does not prove general pod readiness, and API failures remain errors.
func (r *Reconciler) missingDriverConfigMap(ctx context.Context, pod *corev1.Pod) (string, error) {
	if pod.Status.Phase != corev1.PodPending {
		return "", nil
	}
	// Spark 3.5 creates this ConfigMap after the driver pod. An interrupted
	// submit can leave a Pending pod that cannot start without it.
	for _, volume := range pod.Spec.Volumes {
		if volume.Name != "spark-conf-volume-driver" || volume.ConfigMap == nil {
			continue
		}
		name := volume.ConfigMap.Name
		err := r.options.APIReader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: name}, &corev1.ConfigMap{})
		if errors.IsNotFound(err) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", nil
}

// driverGenerationChanged detects a stamped driver from a different spec.
// An unstamped legacy driver provides no generation evidence; absence alone must
// not invalidate it.
func driverGenerationChanged(app *v1beta2.SparkApplication, pod *corev1.Pod) bool {
	generation, stamped := pod.Annotations[common.AnnotationAppGeneration]
	return stamped && generation != strconv.FormatInt(app.Generation, 10)
}

// startSubmissionAttempt saves identity and start time before creating resources
// or running spark-submit. Once submission begins, an outcome-write failure is
// left for recovery instead of escaping to the caller's retry loop and launching twice.
func (r *Reconciler) startSubmissionAttempt(ctx context.Context, app *v1beta2.SparkApplication) error {
	app.Status.SubmissionID = uuid.New().String()
	app.Status.DriverInfo = v1beta2.DriverInfo{PodName: util.GetDriverPodName(app)}
	if r.options.EnableUIService {
		app.Status.DriverInfo.WebUIServiceName = util.GetDefaultUIServiceName(app)
		if r.options.IngressURLFormat != "" {
			app.Status.DriverInfo.WebUIIngressName = util.GetDefaultUIIngressName(app)
		}
	}
	app.Status.LastSubmissionAttemptTime = metav1.Now()
	app.Status.TerminationTime = metav1.Time{}
	app.Status.AppState.ErrorMessage = ""
	app.Status.ExecutorState = nil
	app.Status.SparkApplicationID = ""
	if err := r.updateSparkApplicationStatus(ctx, app); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	intent := app.DeepCopy()
	app.Status.SubmissionAttempts++
	if err := r.submitSparkApplication(app); err != nil {
		app.Status.AppState = v1beta2.ApplicationState{
			State: v1beta2.ApplicationStateFailedSubmission, ErrorMessage: err.Error(),
		}
	} else {
		app.Status.AppState = v1beta2.ApplicationState{State: v1beta2.ApplicationStateSubmitted}
		app.Status.ExecutionAttempts++
	}
	if err := r.saveSubmissionOutcome(ctx, intent, app); err != nil {
		logger.Error(err, "Failed to save the submission outcome", "name", app.Name,
			"namespace", app.Namespace, "submissionID", app.Status.SubmissionID)
	}
	// A lost outcome is recovered from the recorded intent on the next reconcile.
	// It must not retry the caller's submission side effects on a status conflict.
	return nil
}

// saveSubmissionOutcome retries the result of one launch without rerunning it.
// The saved intent identifies the application and attempt whose result may be
// written; a replacement or newer run keeps its status, while a changed spec
// retires this attempt through INVALIDATING.
func (r *Reconciler) saveSubmissionOutcome(ctx context.Context, intent, outcome *v1beta2.SparkApplication) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := r.getSparkApplication(ctx, client.ObjectKeyFromObject(intent))
		if err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.UID != intent.UID || latest.Status.SubmissionID != intent.Status.SubmissionID ||
			!latest.DeletionTimestamp.IsZero() || latest.Status.AppState.State != intent.Status.AppState.State {
			return nil
		}
		if latest.Generation != intent.Generation {
			latest.Status.AppState = v1beta2.ApplicationState{State: v1beta2.ApplicationStateInvalidating}
		} else {
			if outcome.Status.AppState.State == v1beta2.ApplicationStateFailedSubmission {
				// spark-submit can fail after creating a usable driver. Recover it
				// before exposing a false submission failure to status consumers.
				if handled, err := r.recoverSubmission(ctx, latest); handled || err != nil {
					return err
				}
			}
			latest.Status = outcome.Status
		}
		if err := r.updateSparkApplicationStatus(ctx, latest); err != nil {
			return err
		}
		r.recordSparkApplicationEvent(latest)
		return nil
	})
}
