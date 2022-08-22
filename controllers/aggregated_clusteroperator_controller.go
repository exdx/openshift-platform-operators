/*
Copyright 2022.

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

package controllers

import (
	"context"
	"errors"
	"fmt"

	openshiftconfigv1 "github.com/openshift/api/config/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	utilerror "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logr "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	platformv1alpha1 "github.com/openshift/api/platform/v1alpha1"
	platformtypes "github.com/openshift/platform-operators/api/v1alpha1"
	aggregatedco "github.com/openshift/platform-operators/internal/aggregated-co"
	"github.com/openshift/platform-operators/internal/util"
)

type AggregatedClusterOperatorReconciler struct {
	client.Client
	DiscoveryClient discovery.DiscoveryInterface
	Configv1Client  configv1client.ConfigV1Interface
}

const aggregateCOName = "platform-operators-aggregated"

//+kubebuilder:rbac:groups=platform.openshift.io,resources=platformoperators,verbs=get;list;watch
//+kubebuilder:rbac:groups=platform.openshift.io,resources=platformoperators/status,verbs=get
//+kubebuilder:rbac:groups=config.openshift.io,resources=clusteroperators,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.10.0/pkg/reconcile
func (a *AggregatedClusterOperatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContext(ctx)
	log.Info("reconciling request", "req", req.NamespacedName)
	defer log.Info("finished reconciling request", "req", req.NamespacedName)

	// Create a CO Builder to build the CO status
	coBuilder := aggregatedco.NewBuilder()
	// Create a CO Writer to write to the CO status
	coWriter := aggregatedco.NewWriter(a.Configv1Client)

	aggregatedCO := &openshiftconfigv1.ClusterOperator{}
	if err := a.Get(ctx, req.NamespacedName, aggregatedCO); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	defer func() {
		if err := coWriter.UpdateStatus(aggregatedCO, coBuilder.GetStatus()); err != nil {
			log.Error(err, "error updating CO status")
		}
	}()

	// Set the default CO status conditions: Progressing True, Degraded False, Available False
	coBuilder.WithProgressing(openshiftconfigv1.ConditionTrue, "")
	coBuilder.WithDegraded(openshiftconfigv1.ConditionFalse)
	coBuilder.WithAvailable(openshiftconfigv1.ConditionFalse, "", "")

	poList := &platformv1alpha1.PlatformOperatorList{}
	if err := a.List(ctx, poList); err != nil {
		log.Error(err, "error listing platformoperators")
		return ctrl.Result{}, err
	}

	if len(poList.Items) == 0 {
		// No POs on cluster, everything is fine
		coBuilder.WithAvailable(openshiftconfigv1.ConditionTrue, "", "No POs Found")
		return ctrl.Result{}, nil
	}

	statusErrorCheck := a.inspectPlatformOperators(poList)
	if statusErrorCheck != nil {
		// One of the POs is in an error state
		// Update the Aggregated CO with the information on the failed PO
		coBuilder.WithDegraded(openshiftconfigv1.ConditionTrue)
		coBuilder.WithAvailable(openshiftconfigv1.ConditionFalse, utilerror.NewAggregate(statusErrorCheck.FailingErrors).Error(), "PO In An Error State")
		return ctrl.Result{}, nil
	}

	coBuilder.WithAvailable(openshiftconfigv1.ConditionTrue, "All POs in a successful state", "POs Are Healthy")

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (a *AggregatedClusterOperatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openshiftconfigv1.ClusterOperator{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
			return object.GetName() == aggregateCOName
		}))).
		Watches(&source.Kind{Type: &platformv1alpha1.PlatformOperator{}}, handler.EnqueueRequestsFromMapFunc(util.RequeueBundleDeployment(mgr.GetClient()))).
		Complete(a)
}

type POStatusErrors struct {
	FailingPOs    []*platformv1alpha1.PlatformOperator
	FailingErrors []error
}

// inspectPlatformOperators iterates over all the POs on the cluster
// and determines whether a PO is in a failing state by inspecting its status.
// A nil return value indicates no errors were found with the POs provided.
func (a *AggregatedClusterOperatorReconciler) inspectPlatformOperators(POList *platformv1alpha1.PlatformOperatorList) *POStatusErrors {
	POstatuses := new(POStatusErrors)

	for _, po := range POList.Items {
		po := po.DeepCopy()
		status := po.Status

		for _, condition := range status.Conditions {
			if condition.Reason == platformtypes.ReasonSourceFailed || condition.Reason == platformtypes.ReasonApplyFailed {
				POstatuses.FailingPOs = append(POstatuses.FailingPOs, po)
				POstatuses.FailingErrors = append(POstatuses.FailingErrors, errors.New(fmt.Sprintf("%s is failing: %q", po.GetName(), condition.Reason)))
			}
		}
	}

	// check if any POs were populated in the POStatusErrors type
	if len(POstatuses.FailingPOs) > 0 || len(POstatuses.FailingErrors) > 0 {
		return POstatuses
	}

	return nil
}