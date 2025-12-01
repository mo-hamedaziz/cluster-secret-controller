/*
Copyright 2025.

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

package controller

import (
	"context"
	"fmt"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	clustersecretv1 "github.com/mo-hamedaziz/cluster-secret-controller/api/v1"
)

const (
	clusterSecretFinalizer = "clustersecret.pocteo.com/finalizer" // A finalizer to prevent deletion until cleanup is done
	ownerLabel             = "clustersecret.pocteo.com/owned-by" // Label to mark secrets owned by a ClusterSecret
)

// ClusterSecretReconciler reconciles a ClusterSecret object
type ClusterSecretReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=clustersecret.pocteo.com,resources=clustersecrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clustersecret.pocteo.com,resources=clustersecrets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=clustersecret.pocteo.com,resources=clustersecrets/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ClusterSecret object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *ClusterSecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// Fetch the ClusterSecret instance
	clusterSecret := &clustersecretv1.ClusterSecret{}
	if err := r.Get(ctx, req.NamespacedName, clusterSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // If the ClusterSecret CR is not found, do nothing
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !clusterSecret.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, clusterSecret)
	}

	// Add finalizer if not present
	// This will ensure cleanup runs before the resource is deleted
	if !controllerutil.ContainsFinalizer(clusterSecret, clusterSecretFinalizer) {
		controllerutil.AddFinalizer(clusterSecret, clusterSecretFinalizer)
		if err := r.Update(ctx, clusterSecret); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Get all namespaces
	namespaceList := &corev1.NamespaceList{}
	if err := r.List(ctx, namespaceList); err != nil {
		logger.Error(err, "Failed to list namespaces")
		return ctrl.Result{}, err
	}

	// Filter namespaces based on match patterns
	targetNamespaces := r.filterNamespaces(namespaceList.Items, clusterSecret.Spec.MatchNamespace)

	// Sync secrets to target namespaces => This will create or update a Secret in each targeted namespace
	syncedNamespaces := []string{}
	for _, ns := range targetNamespaces {
		if err := r.syncSecretToNamespace(ctx, clusterSecret, ns); err != nil {
			logger.Error(err, "Failed to sync secret to namespace", "namespace", ns)
			continue
		}
		syncedNamespaces = append(syncedNamespaces, ns)
	}

	// Clean up secrets in non-matching namespaces
	if err := r.cleanupOrphanedSecrets(ctx, clusterSecret, targetNamespaces); err != nil {
		logger.Error(err, "Failed to cleanup orphaned secrets")
	}

	// Update status
	clusterSecret.Status.Conditions = []metav1.Condition{
		{
			Type:               "Synced",
			Status:             metav1.ConditionTrue,
			Reason:             "SyncSuccessful",
			Message:            fmt.Sprintf("Synced to %d namespaces", len(syncedNamespaces)),
			LastTransitionTime: metav1.Now(),
		},
	}

	if err := r.Status().Update(ctx, clusterSecret); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil

}

func (r *ClusterSecretReconciler) handleDeletion(ctx context.Context, clusterSecret *clustersecretv1.ClusterSecret) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	if controllerutil.ContainsFinalizer(clusterSecret, clusterSecretFinalizer) {
		// Delete all owned secrets
		secretList := &corev1.SecretList{}
		if err := r.List(ctx, secretList, client.MatchingLabels{
			ownerLabel: clusterSecret.Name,
		}); err != nil {
			return ctrl.Result{}, err
		}

		for _, secret := range secretList.Items {
			if err := r.Delete(ctx, &secret); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "Failed to delete secret", "namespace", secret.Namespace, "name", secret.Name)
				return ctrl.Result{}, err
			}
		}

		// Remove finalizer
		controllerutil.RemoveFinalizer(clusterSecret, clusterSecretFinalizer)
		if err := r.Update(ctx, clusterSecret); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *ClusterSecretReconciler) filterNamespaces(namespaces []corev1.Namespace, matchPatterns []string) []string {
	var result []string

	for _, ns := range namespaces {
		// Include if matches any match pattern
		if matchesAnyPattern(ns.Name, matchPatterns) {
			result = append(result, ns.Name)
		}
	}

	return result
}

func matchesAnyPattern(name string, patterns []string) bool {
	for _, pattern := range patterns {
		// Convert wildcard pattern to regex
		regexPattern := wildcardToRegex(pattern)
		matched, _ := regexp.MatchString(regexPattern, name)
		if matched {
			return true
		}
	}
	return false
}

func wildcardToRegex(pattern string) string {
	// Convert shell-style wildcards to regex
	// * becomes .*
	// ? becomes .
	pattern = regexp.QuoteMeta(pattern)
	pattern = "^" + pattern + "$"
	pattern = regexp.MustCompile(`\\\*`).ReplaceAllString(pattern, ".*")
	pattern = regexp.MustCompile(`\\\?`).ReplaceAllString(pattern, ".")
	return pattern
}

func (r *ClusterSecretReconciler) syncSecretToNamespace(ctx context.Context, clusterSecret *clustersecretv1.ClusterSecret, namespace string) error {
	logger := logf.FromContext(ctx)

	// Prepare secret data
	secretData := make(map[string][]byte)
	for k, v := range clusterSecret.Spec.Data {
		secretData[k] = v
	}

	// Create or update secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterSecret.Name,
			Namespace: namespace,
			Labels: map[string]string{
				ownerLabel: clusterSecret.Name,
			},
		},
		Data: secretData,
		Type: corev1.SecretType(clusterSecret.Spec.Type),
	}

	// Check if secret exists
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: namespace}, existing)

	if err != nil && apierrors.IsNotFound(err) {
		// Create new secret
		logger.Info("Creating secret", "namespace", namespace, "name", secret.Name)
		return r.Create(ctx, secret)
	} else if err != nil {
		return err
	}

	// Update existing secret
	existing.Data = secretData
	existing.Type = secret.Type
	if existing.Labels == nil {
		existing.Labels = make(map[string]string)
	}
	existing.Labels[ownerLabel] = clusterSecret.Name

	logger.Info("Updating secret", "namespace", namespace, "name", secret.Name)
	return r.Update(ctx, existing)
}

func (r *ClusterSecretReconciler) cleanupOrphanedSecrets(ctx context.Context, clusterSecret *clustersecretv1.ClusterSecret, targetNamespaces []string) error {
	logger := logf.FromContext(ctx)

	// Get all secrets owned by this ClusterSecret
	secretList := &corev1.SecretList{}
	if err := r.List(ctx, secretList, client.MatchingLabels{
		ownerLabel: clusterSecret.Name,
	}); err != nil {
		return err
	}

	// Create a map of target namespaces for quick lookup
	targetNsMap := make(map[string]bool)
	for _, ns := range targetNamespaces {
		targetNsMap[ns] = true
	}

	// Delete secrets in namespaces that should no longer have them
	for _, secret := range secretList.Items {
		if !targetNsMap[secret.Namespace] {
			logger.Info("Deleting orphaned secret", "namespace", secret.Namespace, "name", secret.Name)
			if err := r.Delete(ctx, &secret); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterSecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clustersecretv1.ClusterSecret{}).
		Owns(&corev1.Secret{}).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceToClusterSecrets),
		).
		Complete(r)
}

// Watch for namespace changes and reconcile all ClusterSecrets
func (r *ClusterSecretReconciler) namespaceToClusterSecrets(ctx context.Context, obj client.Object) []reconcile.Request {
	clusterSecrets := &clustersecretv1.ClusterSecretList{}
	if err := r.List(ctx, clusterSecrets); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, len(clusterSecrets.Items))
	for i, cs := range clusterSecrets.Items {
		requests[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      cs.Name,
				Namespace: cs.Namespace,
			},
		}
	}
	return requests
}
