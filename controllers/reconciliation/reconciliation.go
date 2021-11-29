/*
 * (c) Copyright IBM Corp. 2021
 * (c) Copyright VC Inc. 2021
 */

package reconciliation

import (
	"github.com/go-logr/logr"
	vcV1 "github.com/jhonmac666/agent-operator/api/v1"
	"github.com/jhonmac666/agent-operator/controllers/reconciliation/helm"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
)

type Reconciliation interface {
	// CreateOrUpdate creates a new Agent installation or updates to the latest defined configuration
	CreateOrUpdate(req ctrl.Request, crdInstance *vcV1.VcAgent) error
	// Delete removes the Agent installation from the cluster
	Delete(req ctrl.Request, crdInstance *vcV1.VcAgent) error
}

func New(scheme *runtime.Scheme, log logr.Logger, crAppName string, crAppNamespace string) Reconciliation {
	return helm.NewHelmReconciliation(scheme, log, crAppName, crAppNamespace)
}
