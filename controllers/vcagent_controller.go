/*
 * (c) Copyright IBM Corp. 2021
 * (c) Copyright VC Inc. 2021
 */

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-cmp/cmp"

	vcV1 "github.com/jhonmac666/agent-operator/api/v1"

	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/go-logr/logr"
	"github.com/jhonmac666/agent-operator/controllers/leaderelection"
	"github.com/jhonmac666/agent-operator/controllers/reconciliation"

	appV1 "k8s.io/api/apps/v1"
	coreV1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	vcAgentFinalizer    = "agent.vc.io/finalizer"
	crExpectedName      = "vcagent"
	crExpectedNamespace = "vcagent"
)

// Add will create a new VC Agent Controller and add this to the Manager for reconciling
func Add(mgr manager.Manager) error {
	return add(mgr, NewVcAgentReconciler(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		mgr.GetConfig(),
		logf.Log.WithName("agent.controller")))
}

// add sets up the controller with the Manager.
func add(mgr ctrl.Manager, r *VcAgentReconciler) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vcV1.VcAgent{}).
		Owns(&appV1.DaemonSet{}, builder.WithPredicates(filterPredicate())).
		Owns(&coreV1.Pod{}).
		Owns(&coreV1.Secret{}).
		Owns(&coreV1.ConfigMap{}).
		Owns(&coreV1.Service{}).
		Owns(&coreV1.ServiceAccount{}).
		Complete(r)
}

func filterPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// Evaluates to false if the object has been confirmed deleted.
			return !e.DeleteStateUnknown
		},
	}
}

// NewVcAgentReconciler initializes a new VcAgentReconciler instance
func NewVcAgentReconciler(client client.Client, apiReader client.Reader, scheme *runtime.Scheme, config *rest.Config, log logr.Logger) *VcAgentReconciler {
	return &VcAgentReconciler{
		client:              client,
		apiReader:           apiReader,
		scheme:              scheme,
		config:              config,
		log:                 log,
		agentReconciliation: reconciliation.New(scheme, log, crExpectedName, crExpectedNamespace),
		crAppName:           crExpectedName,
		crAppNamespace:      crExpectedNamespace,
	}
}

type VcAgentReconciler struct {
	client              client.Client
	apiReader           client.Reader
	scheme              *runtime.Scheme
	config              *rest.Config
	log                 logr.Logger
	agentReconciliation reconciliation.Reconciliation
	crAppName           string
	crAppNamespace      string
	// Uninitialized variables in NewVcAgentReconciler
	leaderElector *leaderelection.LeaderElector
}

//+kubebuilder:rbac:groups=agents.vc.io,resources=vcagent,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods;secrets;configmaps;services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=agents.vc.io,resources=vcagent/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.vc.io,resources=vcagent/finalizers,verbs=update
func (r *VcAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.log.WithValues("namespace", req.Namespace, "name", req.Name)
	log.Info("Reconciling VC Agent")

	crdInstance, err := r.fetchAgentCrdInstance(ctx, req)
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			r.log.Info("VC Agent CRD instance not found, please install the VcAgent CustomResource")
			return ctrl.Result{}, nil
		} else {
			r.log.Error(err, "Failed to get VC Agent CustomResource or invalid")
			return ctrl.Result{}, err
		}
	}

	isVcAgentDeleted := crdInstance.GetDeletionTimestamp() != nil
	if isVcAgentDeleted {
		r.log.Info("VC Agent Operator CustomResource is deleted. Cleanup Agent.")

		if controllerutil.ContainsFinalizer(crdInstance, vcAgentFinalizer) {
			// This is a kind of work-around. Normally should just directly execute the clean-up logic. But when the user removes
			// the entire vcagent Namespace the Operator runtime will get deleted before having a chance to clean up.
			// Try to detect this and remove the Finalizer. Otherwise the user needs to manually remove the Finalizer to get
			// all garbage collected.
			// The proper way of cleaning up would be:
			// 1) remove the Operator Custom Resource
			// 2) remove everything else
			if egNamespace, err := r.fetchVCNamespace(ctx); err == nil && egNamespace.GetDeletionTimestamp() != nil {
				r.log.Info("Seems like the VC namespace got deleted. Skip running the finalizer logic and try to remove finalizer.\n" +
					" Please delete the VC Agent Operator CustomResource _first_!")
			} else {
				r.log.V(1).Info("Running the finalizer...")
				if err := r.finalizeAgent(req, crdInstance); err != nil {
					return ctrl.Result{}, err
				}
			}

			controllerutil.RemoveFinalizer(crdInstance, vcAgentFinalizer)
			if err := r.client.Update(ctx, crdInstance); err != nil {
				return ctrl.Result{}, err
			}
			r.log.V(1).Info("Removed Finalizer from VC Agent Operator CustomResource")
		}
		return ctrl.Result{}, nil
	}

	// Validate the Custom Resource object (configuration) before we're taking any other actions
	r.log.V(1).Info("Validating the CRD")
	if err := r.validateAgentCrd(crdInstance); err != nil {
		r.log.Error(err, "Unrecoverable error validating the VC Agent CRD for deployment")
		return ctrl.Result{}, err
	}

	if !crdInstance.Status.OldVersionsUpdated {
		// If something got deleted, give the Operator another reconcile loop to clean up before continuing. So return immediately
		if deleted, err := r.purgeOldResources(ctx); err != nil {
			return ctrl.Result{}, err
		} else if deleted {
			return ctrl.Result{RequeueAfter: time.Second * 1}, nil
		}

		if err := r.upsertCrdStatusFields(ctx, req, func(status *vcV1.VcAgentStatus) vcV1.VcAgentStatus {
			status.OldVersionsUpdated = true
			return *status
		}); err != nil {
			if k8sErrors.IsConflict(err) {
				// do manual retry without error
				return ctrl.Result{RequeueAfter: time.Second * 1}, nil
			}
			r.log.Error(err, "Failed to update VC Agent CRD Status field - old versions purged")
			return ctrl.Result{}, err
		}
	}

	//
	// Potential Old Operator resources removed, start installation of (new) Operator
	//

	r.log.V(1).Info("Injecting finalizer into CRD, for cleanup when CRD gets removed")
	if err := r.injectFinalizer(ctx, req, crdInstance); err != nil {
		if k8sErrors.IsConflict(err) {
			// do manual retry without error
			return ctrl.Result{RequeueAfter: time.Second * 1}, nil
		}
		r.log.Error(err, "Failure adding finalizer into CRD")
		return ctrl.Result{}, err
	}

	//// First try to start Leader Election Coordination so to return error if we cannot get it started
	//if r.leaderElector == nil || !r.leaderElector.IsLeaderElectionScheduled() {
		//if r.leaderElector != nil {
			//// As we'll replace the Leader Elector instance make sure to properly clean up old one
			//r.leaderElector.CancelLeaderElection()
		//}

		//r.leaderElector = leaderelection.NewLeaderElection(r.client, req.NamespacedName)
		//if err := r.leaderElector.StartCoordination(r.crAppNamespace); err != nil {
			//r.log.Error(err, "Failure starting Leader Election Coordination")
			//return ctrl.Result{}, err
		//}
	//}

	if err := r.agentReconciliation.CreateOrUpdate(req, crdInstance); err != nil {
		return ctrl.Result{}, err
	}
	r.log.Info("Agent installed/upgraded successfully")

	if err := r.updateStatusFields(ctx, req, crdInstance); err != nil {
		if k8sErrors.IsConflict(err) {
			// do manual retry without error
			return ctrl.Result{RequeueAfter: time.Second * 1}, nil
		}
		r.log.Error(err, "Failed to update VC Agent CRD Status field - resource references")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *VcAgentReconciler) finalizeAgent(req ctrl.Request, crdInstance *vcV1.VcAgent) error {
	if err := r.agentReconciliation.Delete(req, crdInstance); err != nil {
		return err
	}
	if r.leaderElector != nil {
		r.leaderElector.CancelLeaderElection()
	}
	r.log.Info("Successfully finalized vc agent")
	return nil
}

func (r *VcAgentReconciler) injectFinalizer(ctx context.Context, req ctrl.Request, crdInstance *vcV1.VcAgent) error {
	if !controllerutil.ContainsFinalizer(crdInstance, vcAgentFinalizer) {
		// Pull the CR object again, so we're sure to have the latest version including changes
		if err := r.client.Get(ctx, req.NamespacedName, crdInstance); err != nil {
			return err
		}

		controllerutil.AddFinalizer(crdInstance, vcAgentFinalizer)
		return r.client.Update(ctx, crdInstance)
	}
	return nil
}

func (r *VcAgentReconciler) fetchAgentCrdInstance(ctx context.Context, req ctrl.Request) (*vcV1.VcAgent, error) {
	crdInstance := &vcV1.VcAgent{}
	if err := r.client.Get(ctx, req.NamespacedName, crdInstance); err != nil {
		return nil, err
	}

	// Verify if the CR has the expected Name / Namespace set. At a later time we could really make this configurable and install
	// our Agent in the given Namespace. For now, we only support the fixed value.
	if crExpectedName != crdInstance.Name || crExpectedNamespace != crdInstance.Namespace {
		err := fmt.Errorf("VC Agent CustomResource Name (%v) or Namespace (%v) don't match currently mandatory Name='%v' and Namespace='%v'. Please adjust the CustomResource",
			crdInstance.Name, crdInstance.Namespace, crExpectedName, crExpectedNamespace)
		return nil, err
	}

	r.log.V(1).Info(fmt.Sprintf("Found VC CustomResource: %v", crdInstance))
	return crdInstance, nil
}

// fetchVCNamespace will get the Namespace instance for ourselves
func (r *VcAgentReconciler) fetchVCNamespace(ctx context.Context) (*coreV1.Namespace, error) {
	egNamespace := &coreV1.Namespace{}
	if err := r.client.Get(ctx, client.ObjectKey{
		Namespace: "",
		Name:      crExpectedNamespace,
	}, egNamespace); err != nil {
		return nil, err
	}

	r.log.V(1).Info(fmt.Sprintf("Found vcagent Namespace: %v", egNamespace))
	return egNamespace, nil
}

// validateAgentCrd does some basic validation as otherwise Helm may not deploy the Agent DaemonSet but silently skip it if
// certain fields are omitted. In the future we should prevent this by adding a Validation WebHook.
func (r *VcAgentReconciler) validateAgentCrd(crd *vcV1.VcAgent) error {
	if len(crd.Spec.Agent.EndpointHost) == 0 || len(crd.Spec.Agent.EndpointPort) == 0 || crd.Spec.Agent.EndpointPort == "0" {
		r.log.Info(`
##############################################################################
####    ERROR: You did not specify a correct Endpoint (host and/or port)  ####
##############################################################################
`)
		return fmt.Errorf("CRD Agent Spec should contain valid EndpointHost and EndpointPort")
	}

	if len(crd.Spec.Cluster.Name) == 0 && len(crd.Spec.Zone.Name) == 0 {
		r.log.Info(`
##############################################################################
####    ERROR: You did not specify a zone or name for this cluster.       ####
##############################################################################
`)
		return fmt.Errorf("CRD Agent Spec should contain either Zone or Cluster name")
	}

	if len(crd.Spec.Agent.Key) == 0 && len(crd.Spec.Agent.KeysSecret) == 0 {
		r.log.Info(`
##############################################################################
####    ERROR: You did not specify your secret agent key.                 ####
##############################################################################
`)
		return fmt.Errorf("CRD Agent Spec should contain either Key or KeySecret")
	}

	return nil
}

func (r *VcAgentReconciler) purgeOldResources(ctx context.Context) (bool, error) {
	r.log.V(1).Info("Checking for old Agent Operator installations and purging / upgrading them")

	if deleted, err := r.getAndDeleteOldOperator(ctx); err != nil {
		r.log.Error(err, "Unrecoverable error removing the old Operator Deployment spec. Cannot continue Agent installation")
		return false, err
	} else if deleted {
		return true, nil
	}

	if deleted, err := r.getAndDeleteOldOperatorResources(ctx); err != nil {
		r.log.Error(err, "Unrecoverable error updating old resources for Helm-based installation. Cannot continue Agent installation")
		return false, err
	} else if deleted {
		return true, nil
	}

	return false, nil
}

func (r *VcAgentReconciler) upsertCrdStatusFields(ctx context.Context, req ctrl.Request, statusFn func(status *vcV1.VcAgentStatus) vcV1.VcAgentStatus) error {
	// Pull the CR object again, so we're sure to have the latest version including changes
	crdInstance := &vcV1.VcAgent{}
	if err := r.client.Get(ctx, req.NamespacedName, crdInstance); err != nil {
		return err
	}

	crdInstance.Status = statusFn(&crdInstance.Status)

	return r.client.Status().Update(ctx, crdInstance)
}

func (r *VcAgentReconciler) updateStatusFields(ctx context.Context, req ctrl.Request, crdInstance *vcV1.VcAgent) error {
	r.log.V(1).Info("Updating Agent CRD Status field with references to DaemonSet and ConfigMap")

	configMaps := &coreV1.ConfigMapList{}
	if err := r.client.List(ctx, configMaps, client.InNamespace(r.crAppNamespace)); err != nil {
		r.log.Error(err, "Failed getting ConfigMap to update VC Agent CRD Status field")
		return err
	}
	var configMapResource vcV1.ResourceInfo
	for _, val := range configMaps.Items {
		if val.Name == "vcagent" {
			configMapResource = vcV1.ResourceInfo{
				Name: val.Name,
				UID:  string(val.UID),
			}
		}
	}

	daemonSets := &appV1.DaemonSetList{}
	if err := r.client.List(ctx, daemonSets, client.InNamespace(r.crAppNamespace)); err != nil {
		r.log.Error(err, "Failed getting DaemonSet to update VC Agent CRD Status field")
		return err
	}
	var daemonSetResource vcV1.ResourceInfo
	for _, val := range daemonSets.Items {
		if val.Name == "vcagent" {
			daemonSetResource = vcV1.ResourceInfo{
				Name: val.Name,
				UID:  string(val.UID),
			}
		}
	}

	if !cmp.Equal(configMapResource, crdInstance.Status.ConfigMap) ||
		!cmp.Equal(daemonSetResource, crdInstance.Status.DaemonSet) {

		return r.upsertCrdStatusFields(ctx, req, func(status *vcV1.VcAgentStatus) vcV1.VcAgentStatus {
			status.ConfigMap = configMapResource
			status.DaemonSet = daemonSetResource
			// Reset other statuses because we don't use them for the v1 Agent
			status.Secret = vcV1.ResourceInfo{}
			status.ClusterRole = vcV1.ResourceInfo{}
			status.ClusterRoleBinding = vcV1.ResourceInfo{}
			status.ServiceAccount = vcV1.ResourceInfo{}
			return *status
		})
	}

	return nil
}
