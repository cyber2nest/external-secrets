/*
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

package clusterexternalsecret

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	esv1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	"github.com/external-secrets/external-secrets/pkg/controllers/clusterexternalsecret/cesmetrics"
	ctrlmetrics "github.com/external-secrets/external-secrets/pkg/controllers/metrics"
	"github.com/external-secrets/external-secrets/pkg/utils"
)

// Reconciler reconciles a ClusterExternalSecret object.
type Reconciler struct {
	client.Client
	Log             logr.Logger
	Scheme          *runtime.Scheme
	RequeueInterval time.Duration
}

const (
	errGetCES               = "could not get ClusterExternalSecret"
	errPatchStatus          = "unable to patch status"
	errConvertLabelSelector = "unable to convert labelselector"
	errGetExistingES        = "could not get existing ExternalSecret"
	errNamespacesFailed     = "one or more namespaces failed"
)

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("ClusterExternalSecret", req.NamespacedName)

	resourceLabels := ctrlmetrics.RefineNonConditionMetricLabels(map[string]string{"name": req.Name, "namespace": req.Namespace})
	start := time.Now()

	externalSecretReconcileDuration := cesmetrics.GetGaugeVec(cesmetrics.ClusterExternalSecretReconcileDurationKey)
	defer func() { externalSecretReconcileDuration.With(resourceLabels).Set(float64(time.Since(start))) }()

	var clusterExternalSecret esv1.ClusterExternalSecret
	err := r.Get(ctx, req.NamespacedName, &clusterExternalSecret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			cesmetrics.RemoveMetrics(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}

		log.Error(err, errGetCES)
		return ctrl.Result{}, err
	}

	// skip reconciliation if deletion timestamp is set on cluster external secret
	if clusterExternalSecret.DeletionTimestamp != nil {
		log.Info("skipping as it is in deletion")
		return ctrl.Result{}, nil
	}

	p := client.MergeFrom(clusterExternalSecret.DeepCopy())
	defer r.deferPatch(ctx, log, &clusterExternalSecret, p)

	return r.reconcile(ctx, log, &clusterExternalSecret)
}

func (r *Reconciler) reconcile(ctx context.Context, log logr.Logger, clusterExternalSecret *esv1.ClusterExternalSecret) (ctrl.Result, error) {
	refreshInt := r.RequeueInterval
	if clusterExternalSecret.Spec.RefreshInterval != nil {
		refreshInt = clusterExternalSecret.Spec.RefreshInterval.Duration
	}

	esName := clusterExternalSecret.Spec.ExternalSecretName
	if esName == "" {
		esName = clusterExternalSecret.ObjectMeta.Name
	}
	if prevName := clusterExternalSecret.Status.ExternalSecretName; prevName != esName {
		// ExternalSecretName has changed, so remove the old ones
		if err := r.removeOldSecrets(ctx, log, clusterExternalSecret, prevName); err != nil {
			return ctrl.Result{}, err
		}
	}
	clusterExternalSecret.Status.ExternalSecretName = esName

	selectors := []*metav1.LabelSelector{}
	if s := clusterExternalSecret.Spec.NamespaceSelector; s != nil {
		selectors = append(selectors, s)
	}
	selectors = append(selectors, clusterExternalSecret.Spec.NamespaceSelectors...)

	namespaces, err := utils.GetTargetNamespaces(ctx, r.Client, clusterExternalSecret.Spec.Namespaces, selectors)
	if err != nil {
		log.Error(err, "failed to get target Namespaces")
		failedNamespaces := map[string]error{
			"unknown": err,
		}
		condition := NewClusterExternalSecretCondition(failedNamespaces)
		SetClusterExternalSecretCondition(clusterExternalSecret, *condition)

		clusterExternalSecret.Status.FailedNamespaces = toNamespaceFailures(failedNamespaces)

		return ctrl.Result{}, err
	}

	failedNamespaces := r.deleteOutdatedExternalSecrets(ctx, namespaces, esName, clusterExternalSecret.Name, clusterExternalSecret.Status.ProvisionedNamespaces)

	provisionedNamespaces := r.gatherProvisionedNamespaces(ctx, log, clusterExternalSecret, namespaces, esName, failedNamespaces)

	condition := NewClusterExternalSecretCondition(failedNamespaces)
	SetClusterExternalSecretCondition(clusterExternalSecret, *condition)

	clusterExternalSecret.Status.FailedNamespaces = toNamespaceFailures(failedNamespaces)
	sort.Strings(provisionedNamespaces)
	clusterExternalSecret.Status.ProvisionedNamespaces = provisionedNamespaces

	return ctrl.Result{RequeueAfter: refreshInt}, nil
}

func (r *Reconciler) gatherProvisionedNamespaces(
	ctx context.Context,
	log logr.Logger,
	clusterExternalSecret *esv1.ClusterExternalSecret,
	namespaces []v1.Namespace,
	esName string,
	failedNamespaces map[string]error,
) []string {
	var provisionedNamespaces []string //nolint:prealloc // we don't know the size
	for _, namespace := range namespaces {
		var existingES esv1.ExternalSecret
		err := r.Get(ctx, types.NamespacedName{
			Name:      esName,
			Namespace: namespace.Name,
		}, &existingES)
		if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, errGetExistingES)
			failedNamespaces[namespace.Name] = err
			continue
		}

		if err == nil && !isExternalSecretOwnedBy(&existingES, clusterExternalSecret.Name) {
			failedNamespaces[namespace.Name] = errors.New("external secret already exists in namespace")
			continue
		}

		if err := r.createOrUpdateExternalSecret(ctx, clusterExternalSecret, namespace, esName, clusterExternalSecret.Spec.ExternalSecretMetadata); err != nil {
			log.Error(err, "failed to create or update external secret")
			failedNamespaces[namespace.Name] = err
			continue
		}

		provisionedNamespaces = append(provisionedNamespaces, namespace.Name)
	}
	return provisionedNamespaces
}

func (r *Reconciler) removeOldSecrets(ctx context.Context, log logr.Logger, clusterExternalSecret *esv1.ClusterExternalSecret, prevName string) error {
	var (
		failedNamespaces = map[string]error{}
		lastErr          error
	)
	for _, ns := range clusterExternalSecret.Status.ProvisionedNamespaces {
		if err := r.deleteExternalSecret(ctx, prevName, clusterExternalSecret.Name, ns); err != nil {
			log.Error(err, "could not delete ExternalSecret")
			failedNamespaces[ns] = err
			lastErr = err
		}
	}
	if len(failedNamespaces) > 0 {
		condition := NewClusterExternalSecretCondition(failedNamespaces)
		SetClusterExternalSecretCondition(clusterExternalSecret, *condition)
		clusterExternalSecret.Status.FailedNamespaces = toNamespaceFailures(failedNamespaces)
		return lastErr
	}

	return nil
}

func (r *Reconciler) createOrUpdateExternalSecret(ctx context.Context, clusterExternalSecret *esv1.ClusterExternalSecret, namespace v1.Namespace, esName string, esMetadata esv1.ExternalSecretMetadata) error {
	externalSecret := &esv1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace.Name,
			Name:      esName,
		},
	}

	mutateFunc := func() error {
		externalSecret.Labels = esMetadata.Labels
		externalSecret.Annotations = esMetadata.Annotations
		externalSecret.Spec = clusterExternalSecret.Spec.ExternalSecretSpec

		if err := controllerutil.SetControllerReference(clusterExternalSecret, externalSecret, r.Scheme); err != nil {
			return fmt.Errorf("could not set the controller owner reference %w", err)
		}

		return nil
	}

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, externalSecret, mutateFunc); err != nil {
		return fmt.Errorf("could not create or update ExternalSecret: %w", err)
	}

	return nil
}

func (r *Reconciler) deleteExternalSecret(ctx context.Context, esName, cesName, namespace string) error {
	var existingES esv1.ExternalSecret
	err := r.Get(ctx, types.NamespacedName{
		Name:      esName,
		Namespace: namespace,
	}, &existingES)
	if err != nil {
		// If we can't find it then just leave
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if !isExternalSecretOwnedBy(&existingES, cesName) {
		return nil
	}

	err = r.Delete(ctx, &existingES, &client.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("external secret in non matching namespace could not be deleted: %w", err)
	}

	return nil
}

func (r *Reconciler) deferPatch(ctx context.Context, log logr.Logger, clusterExternalSecret *esv1.ClusterExternalSecret, p client.Patch) {
	if err := r.Status().Patch(ctx, clusterExternalSecret, p); err != nil {
		log.Error(err, errPatchStatus)
	}
}

func (r *Reconciler) deleteOutdatedExternalSecrets(ctx context.Context, namespaces []v1.Namespace, esName, cesName string, provisionedNamespaces []string) map[string]error {
	failedNamespaces := map[string]error{}
	// Loop through existing namespaces first to make sure they still have our labels
	for _, namespace := range getRemovedNamespaces(namespaces, provisionedNamespaces) {
		err := r.deleteExternalSecret(ctx, esName, cesName, namespace)
		if err != nil {
			r.Log.Error(err, "unable to delete external secret")
			failedNamespaces[namespace] = err
		}
	}

	return failedNamespaces
}

func isExternalSecretOwnedBy(es *esv1.ExternalSecret, cesName string) bool {
	owner := metav1.GetControllerOf(es)
	return owner != nil && schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind).GroupKind().String() == esv1.ClusterExtSecretGroupKind && owner.Name == cesName
}

func getRemovedNamespaces(currentNSs []v1.Namespace, provisionedNSs []string) []string {
	currentNSSet := map[string]struct{}{}
	for _, currentNs := range currentNSs {
		currentNSSet[currentNs.Name] = struct{}{}
	}

	var removedNSs []string
	for _, ns := range provisionedNSs {
		if _, ok := currentNSSet[ns]; !ok {
			removedNSs = append(removedNSs, ns)
		}
	}

	return removedNSs
}

func toNamespaceFailures(failedNamespaces map[string]error) []esv1.ClusterExternalSecretNamespaceFailure {
	namespaceFailures := make([]esv1.ClusterExternalSecretNamespaceFailure, len(failedNamespaces))

	i := 0
	for namespace, err := range failedNamespaces {
		namespaceFailures[i] = esv1.ClusterExternalSecretNamespaceFailure{
			Namespace: namespace,
			Reason:    err.Error(),
		}
		i++
	}
	sort.Slice(namespaceFailures, func(i, j int) bool { return namespaceFailures[i].Namespace < namespaceFailures[j].Namespace })
	return namespaceFailures
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, opts controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(opts).
		For(&esv1.ClusterExternalSecret{}).
		Owns(&esv1.ExternalSecret{}).
		Watches(
			&v1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForNamespace),
			builder.WithPredicates(utils.NamespacePredicate()),
		).
		Complete(r)
}

func (r *Reconciler) findObjectsForNamespace(ctx context.Context, namespace client.Object) []reconcile.Request {
	var clusterExternalSecrets esv1.ClusterExternalSecretList
	if err := r.List(ctx, &clusterExternalSecrets); err != nil {
		r.Log.Error(err, errGetCES)
		return []reconcile.Request{}
	}

	return r.queueRequestsForItem(&clusterExternalSecrets, namespace)
}

func (r *Reconciler) queueRequestsForItem(clusterExternalSecrets *esv1.ClusterExternalSecretList, namespace client.Object) []reconcile.Request {
	var requests []reconcile.Request
	for i := range clusterExternalSecrets.Items {
		clusterExternalSecret := clusterExternalSecrets.Items[i]
		var selectors []*metav1.LabelSelector
		if s := clusterExternalSecret.Spec.NamespaceSelector; s != nil {
			selectors = append(selectors, s)
		}
		selectors = append(selectors, clusterExternalSecret.Spec.NamespaceSelectors...)

		var selected bool
		for _, selector := range selectors {
			labelSelector, err := metav1.LabelSelectorAsSelector(selector)
			if err != nil {
				r.Log.Error(err, errConvertLabelSelector)
				continue
			}

			if labelSelector.Matches(labels.Set(namespace.GetLabels())) {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      clusterExternalSecret.GetName(),
						Namespace: clusterExternalSecret.GetNamespace(),
					},
				})
				selected = true
				break
			}
		}

		// Prevent the object from being added twice if it happens to be listed
		// by Namespaces selector as well.
		if selected {
			continue
		}

		if slices.Contains(clusterExternalSecret.Spec.Namespaces, namespace.GetName()) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      clusterExternalSecret.GetName(),
					Namespace: clusterExternalSecret.GetNamespace(),
				},
			})
		}
	}

	return requests
}
