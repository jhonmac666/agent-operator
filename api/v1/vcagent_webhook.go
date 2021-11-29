/*
 * (c) Copyright IBM Corp. 2021
 * (c) Copyright VC Inc. 2021
 */

package v1

import (
	ctrl "sigs.k8s.io/controller-runtime"
)

// log is for logging in this package.
//var vcagentlog = logf.Log.WithName("vcagent-resource")

func (r *VcAgent) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}
