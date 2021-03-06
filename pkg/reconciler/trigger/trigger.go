/*
Copyright 2020 Google LLC

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

package trigger

import (
	"context"
	"fmt"

	"github.com/google/knative-gcp/pkg/reconciler/celltenant"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"

	"github.com/google/knative-gcp/pkg/logging"
	"knative.dev/eventing/pkg/duck"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/resolver"

	brokerv1beta1 "github.com/google/knative-gcp/pkg/apis/broker/v1beta1"
	triggerreconciler "github.com/google/knative-gcp/pkg/client/injection/reconciler/broker/v1beta1/trigger"
	brokerlisters "github.com/google/knative-gcp/pkg/client/listers/broker/v1beta1"
	"github.com/google/knative-gcp/pkg/reconciler"
	reconcilerutils "github.com/google/knative-gcp/pkg/reconciler/utils"
	"knative.dev/eventing/pkg/apis/eventing/v1beta1"
)

const (
	// Name of the corev1.Events emitted from the Trigger reconciliation process.
	triggerReconciled = "TriggerReconciled"
	triggerFinalized  = "TriggerFinalized"
)

// Reconciler implements controller.Reconciler for Trigger resources.
type Reconciler struct {
	*reconciler.Base
	targetReconciler *celltenant.TargetReconciler

	brokerLister brokerlisters.BrokerLister

	// Dynamic tracker to track sources. It tracks the dependency between Triggers and Sources.
	sourceTracker duck.ListableTracker

	// Dynamic tracker to track AddressableTypes. It tracks Trigger subscribers.
	addressableTracker duck.ListableTracker
	uriResolver        *resolver.URIResolver
}

// Check that TriggerReconciler implements Interface
var _ triggerreconciler.Interface = (*Reconciler)(nil)
var _ triggerreconciler.Finalizer = (*Reconciler)(nil)

// ReconcileKind implements Interface.ReconcileKind.
func (r *Reconciler) ReconcileKind(ctx context.Context, t *brokerv1beta1.Trigger) pkgreconciler.Event {
	b, err := r.brokerLister.Brokers(t.Namespace).Get(t.Spec.Broker)

	if err != nil && !apierrs.IsNotFound(err) {
		// Unknown error. genreconciler will record an `InternalError` event and keep retrying.
		return err
	}

	if apierrs.IsNotFound(err) {
		logging.FromContext(ctx).Error("Trigger does not have broker", zap.String("namespace", t.Namespace), zap.String("trigger", t.Name), zap.String("broker", t.Spec.Broker))
		t.Status.MarkBrokerFailed("BrokerDoesNotExist", "Broker %q does not exist", t.Spec.Broker)
	}

	// If the broker has been or is being deleted, we clean up resources created by this controller
	// for the given trigger.
	if apierrs.IsNotFound(err) || !b.GetDeletionTimestamp().IsZero() {
		return r.FinalizeKind(ctx, t)
	}

	if !reconcilerutils.BrokerClassFilter(b) {
		// Call Finalizer anyway in case the Trigger still holds GCP Broker related resources.
		// If a Trigger used to point to a GCP Broker but now has a Broker with a different brokerclass,
		// we should clean up resources related to GCP Broker.
		event := r.FinalizeKind(ctx, t)

		// If a trigger has never pointed to a GCP broker, topic/subscription readiness shouldn't block this
		// trigger's readiness. However, without a reliable way to tell if the trigger has previously pointed
		// to a GCP broker FinalizeKind called above and other code could potentially change the topic/subscription
		// readiness to UNKNOWN even when it has never pointed to a GCP broker. Always mark the topic/subscription
		// ready here to unblock trigger readiness.
		// This code can potentially cause problems in cases where the trigger did refer to a GCP
		// broker which got deleted and recreated with a new non GCP broker. It's necessary to do best
		// effort GC but the topic/subscription is going to be marked ready even when GC fails. This can result in
		// dangling topic/subscription without matching status.
		// This line should be deleted once the following TODO is finished.
		// TODO(https://github.com/knative/pkg/issues/1149) Add a FilterKind to genreconciler so it will
		// skip a trigger if it's not pointed to a gcp broker and doesn't have googlecloud finalizer string.
		t.Status.MarkTopicReady()
		t.Status.MarkSubscriptionReady()
		var reconcilerEvent *pkgreconciler.ReconcilerEvent
		switch {
		case event == nil:
			return nil
		case pkgreconciler.EventAs(event, &reconcilerEvent):
			return event
		default:
			return fmt.Errorf("Error won't be retried, please manually delete PubSub resources:: %w", event)
		}
	}

	return r.reconcile(ctx, t, b)
}

// reconciles the Trigger given that its Broker exists and is not being deleted.
func (r *Reconciler) reconcile(ctx context.Context, t *brokerv1beta1.Trigger, b *brokerv1beta1.Broker) pkgreconciler.Event {
	t.Status.InitializeConditions()
	t.Status.PropagateBrokerStatus(&b.Status)

	if err := r.resolveSubscriber(ctx, t, b); err != nil {
		return err
	}

	if b.Spec.Delivery == nil {
		b.SetDefaults(ctx)
	}

	ct := celltenant.TargetFromTrigger(t, b.Spec.Delivery)
	if err := r.targetReconciler.ReconcileRetryTopicAndSubscription(ctx, r.Recorder, ct); err != nil {
		return err
	}

	if err := r.checkDependencyAnnotation(ctx, t); err != nil {
		return err
	}

	return pkgreconciler.NewEvent(corev1.EventTypeNormal, triggerReconciled, "Trigger reconciled: \"%s/%s\"", t.Namespace, t.Name)
}

// FinalizeKind frees GCP Broker related resources for this Trigger if applicable. It's called when:
// 1) the Trigger is being deleted;
// 2) the Broker of this Trigger is deleted;
// 3) the Broker of this Trigger is updated with one that is not a GCP broker.
func (r *Reconciler) FinalizeKind(ctx context.Context, t *brokerv1beta1.Trigger) pkgreconciler.Event {
	// Don't care if the Trigger doesn't have the GCP Broker specific finalizer string.
	// Right now all triggers have the finalizer because genreconciler automatically adds it.
	// TODO(https://github.com/knative/pkg/issues/1149) Add a FilterKind to genreconciler so it will
	// skip a trigger if it's not pointed to a gcp broker and doesn't have googlecloud finalizer string.
	if !hasGCPBrokerFinalizer(t) {
		return nil
	}
	ct := celltenant.TargetFromTrigger(t, nil)
	if err := r.targetReconciler.DeleteRetryTopicAndSubscription(ctx, r.Recorder, ct); err != nil {
		return err
	}
	return pkgreconciler.NewEvent(corev1.EventTypeNormal, triggerFinalized, "Trigger finalized: \"%s/%s\"", t.Namespace, t.Name)
}

func (r *Reconciler) resolveSubscriber(ctx context.Context, t *brokerv1beta1.Trigger, b *brokerv1beta1.Broker) error {
	if t.Spec.Subscriber.Ref != nil && t.Spec.Subscriber.Ref.Namespace == "" {
		// To call URIFromDestination(dest apisv1alpha1.Destination, parent interface{}), dest.Ref must have a Namespace
		// We will use the Namespace of Trigger as the Namespace of dest.Ref
		t.Spec.Subscriber.Ref.Namespace = t.GetNamespace()
	}

	subscriberURI, err := r.uriResolver.URIFromDestinationV1(ctx, t.Spec.Subscriber, b)
	if err != nil {
		logging.FromContext(ctx).Error("Unable to get the Subscriber's URI", zap.Error(err))
		t.Status.MarkSubscriberResolvedFailed("Unable to get the Subscriber's URI", "%v", err)
		t.Status.SubscriberURI = nil
		return err
	}
	t.Status.SubscriberURI = subscriberURI
	t.Status.MarkSubscriberResolvedSucceeded()

	return nil
}

// hasGCPBrokerFinalizer checks if the Trigger object has a finalizer matching the one added by this controller.
func hasGCPBrokerFinalizer(t *brokerv1beta1.Trigger) bool {
	for _, f := range t.Finalizers {
		if f == finalizerName {
			return true
		}
	}
	return false
}

func (r *Reconciler) checkDependencyAnnotation(ctx context.Context, t *brokerv1beta1.Trigger) error {
	if dependencyAnnotation, ok := t.GetAnnotations()[v1beta1.DependencyAnnotation]; ok {
		dependencyObjRef, err := v1beta1.GetObjRefFromDependencyAnnotation(dependencyAnnotation)
		if err != nil {
			t.Status.MarkDependencyFailed("ReferenceError", "Unable to unmarshal objectReference from dependency annotation of trigger: %v", err)
			return fmt.Errorf("getting object ref from dependency annotation %q: %v", dependencyAnnotation, err)
		}
		trackSource := r.sourceTracker.TrackInNamespace(ctx, t)
		// Trigger and its dependent source are in the same namespace, we already did the validation in the webhook.
		if err := trackSource(dependencyObjRef); err != nil {
			t.Status.MarkDependencyUnknown("TrackingError", "Unable to track dependency: %v", err)
			return fmt.Errorf("tracking dependency: %v", err)
		}
		if err := r.propagateDependencyReadiness(ctx, t, dependencyObjRef); err != nil {
			return fmt.Errorf("propagating dependency readiness: %v", err)
		}
	} else {
		t.Status.MarkDependencySucceeded()
	}
	return nil
}

func (r *Reconciler) propagateDependencyReadiness(ctx context.Context, t *brokerv1beta1.Trigger, dependencyObjRef corev1.ObjectReference) error {
	lister, err := r.sourceTracker.ListerFor(dependencyObjRef)
	if err != nil {
		t.Status.MarkDependencyUnknown("ListerDoesNotExist", "Failed to retrieve lister: %v", err)
		return fmt.Errorf("retrieving lister: %v", err)
	}
	dependencyObj, err := lister.ByNamespace(t.GetNamespace()).Get(dependencyObjRef.Name)
	if err != nil {
		if apierrs.IsNotFound(err) {
			t.Status.MarkDependencyFailed("DependencyDoesNotExist", "Dependency does not exist: %v", err)
		} else {
			t.Status.MarkDependencyUnknown("DependencyGetFailed", "Failed to get dependency: %v", err)
		}
		return fmt.Errorf("getting the dependency: %v", err)
	}
	dependency := dependencyObj.(*duckv1.Source)

	// The dependency hasn't yet reconciled our latest changes to
	// its desired state, so its conditions are outdated.
	if dependency.GetGeneration() != dependency.Status.ObservedGeneration {
		logging.FromContext(ctx).Info("The ObjectMeta Generation of dependency is not equal to the observedGeneration of status",
			zap.Any("objectMetaGeneration", dependency.GetGeneration()),
			zap.Any("statusObservedGeneration", dependency.Status.ObservedGeneration))
		t.Status.MarkDependencyUnknown("GenerationNotEqual", "The dependency's metadata.generation, %q, is not equal to its status.observedGeneration, %q.", dependency.GetGeneration(), dependency.Status.ObservedGeneration)
		return nil
	}
	t.Status.PropagateDependencyStatus(dependency)
	return nil
}
