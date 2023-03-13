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
	"fmt"
	"math/rand"
	"strings"
	"time"

	bootstrapv1beta1 "github.com/charmed-kubernetes/cluster-api-bootstrap-provider-charmed-k8s/api/v1beta1"
	"gopkg.in/yaml.v2"
	kcore "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	controlplanev1beta1 "github.com/charmed-kubernetes/cluster-api-control-plane-provider-charmed-k8s/api/v1beta1"
	juju "github.com/charmed-kubernetes/cluster-api-provider-juju/juju"
	"github.com/pkg/errors"
)

const controllerDataSecretName = "juju-controller-data"
const requeueTime = 30 * time.Second

type JujuConfig struct {
	Details struct {
		APIEndpoints []string `yaml:"api-endpoints"`
		CACert       string   `yaml:"ca-cert"`
	}
	Account struct {
		User     string `yaml:"user"`
		Password string `yaml:"password"`
	}
}

// ControlPlane holds business logic around control planes.
// It should never need to connect to a service, that responsibility lies outside of this struct.
type ControlPlane struct {
	KCP      *controlplanev1beta1.CharmedK8sControlPlane
	Cluster  *clusterv1.Cluster
	Machines []clusterv1.Machine
}

// CharmedK8sControlPlaneReconciler reconciles a CharmedK8sControlPlane object
type CharmedK8sControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io;bootstrap.cluster.x-k8s.io;controlplane.cluster.x-k8s.io,resources=*,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CharmedK8sControlPlane object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *CharmedK8sControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the CharmedK8sControlPlane instance.
	kcp := &controlplanev1beta1.CharmedK8sControlPlane{}
	if err := r.Client.Get(ctx, req.NamespacedName, kcp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get CharmedK8sControlPlane")
		return ctrl.Result{}, err
	}

	// Fetch the Cluster.
	cluster, err := util.GetOwnerCluster(ctx, r.Client, kcp.ObjectMeta)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("waiting for cluster owner to be found")
			return ctrl.Result{RequeueAfter: requeueTime}, nil
		}
		log.Error(err, "failed to get owner Cluster")
		return ctrl.Result{}, err
	}

	if cluster == nil {
		log.Info("waiting for cluster owner to be non-nil")
		return ctrl.Result{Requeue: true}, nil
	}

	if annotations.IsPaused(cluster, kcp) {
		log.Info("reconciliation is paused for this object")
		return ctrl.Result{Requeue: true}, nil
	}

	if !cluster.Status.InfrastructureReady {
		log.Info("cluster is not ready yet, requeueing")
		return ctrl.Result{Requeue: true}, nil
	}

	// Get config data from secret
	jujuConfig, err := getJujuConfigFromSecret(ctx, cluster, r.Client)
	if err != nil {
		log.Error(err, "failed to retrieve juju configuration data from secret")
		return ctrl.Result{}, err
	}
	if jujuConfig == nil {
		log.Info("juju controller configuration was nil, requeuing")
		return ctrl.Result{RequeueAfter: requeueTime}, nil
	}

	connectorConfig := juju.Configuration{
		ControllerAddresses: jujuConfig.Details.APIEndpoints,
		Username:            jujuConfig.Account.User,
		Password:            jujuConfig.Account.Password,
		CACert:              jujuConfig.Details.CACert,
	}
	jujuClient, err := juju.NewClient(connectorConfig)
	if err != nil {
		log.Error(err, "failed to create juju client")
		return ctrl.Result{}, err
	}

	clusterInfraRef := cluster.Spec.InfrastructureRef
	modelName := clusterInfraRef.Name
	modelUUID, err := jujuClient.Models.GetModelUUID(ctx, modelName)
	if err != nil {
		log.Error(err, "failed to retrieve modelUUID")
		return ctrl.Result{}, err
	}

	if modelUUID == "" {
		log.Info("model uuid was empty", "model", modelName)
		return ctrl.Result{RequeueAfter: requeueTime}, nil
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if kcp.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(kcp, controlplanev1beta1.CharmedK8sControlPlaneFinalizer) {
			controllerutil.AddFinalizer(kcp, controlplanev1beta1.CharmedK8sControlPlaneFinalizer)
			if err := r.Update(ctx, kcp); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("added finalizer")
			return ctrl.Result{}, nil
		}
	} else {
		// The control plane object is being deleted
		log.Info("deleting control plane")
		return r.reconcileDelete(ctx, cluster, kcp)
	}

	// Update ownerrefs on infra templates
	log.Info("updating owner references on infra templates")
	if err := r.reconcileExternalReference(ctx, kcp.Spec.MachineTemplate, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if !cluster.Spec.ControlPlaneEndpoint.IsValid() {
		log.Info("cluster does not yet have a ControlPlaneEndpoint defined")
		return ctrl.Result{}, nil
	}

	// TODO: handle proper adoption of Machines
	log.Info("Getting control plane machines")
	ownedMachines, err := r.getControlPlaneMachinesForCluster(ctx, util.ObjectKey(cluster))
	if err != nil {
		log.Error(err, "failed to retrieve control plane machines for cluster")
		return ctrl.Result{}, err
	}

	log.Info("setting MachinesReady condition based on aggregate status of owned machines")
	conditionGetters := make([]conditions.Getter, len(ownedMachines))
	for i, v := range ownedMachines {
		conditionGetters[i] = &v
	}
	conditions.SetAggregate(kcp, controlplanev1beta1.MachinesReadyCondition, conditionGetters, conditions.AddSourceRef(), conditions.WithStepCounterIf(false))

	var (
		errs        error
		result      ctrl.Result
		phaseResult ctrl.Result
	)

	// run all similar reconcile steps in the loop and pick the lowest RetryAfter, aggregate errors and check the requeue flags.
	for _, phase := range []func(context.Context, *clusterv1.Cluster, *controlplanev1beta1.CharmedK8sControlPlane, []clusterv1.Machine, *juju.Client, string) (ctrl.Result, error){
		r.reconcileKubeconfig,
		r.reconcileMachines,
	} {
		phaseResult, err = phase(ctx, cluster, kcp, ownedMachines, jujuClient, modelUUID)
		if err != nil {
			errs = kerrors.NewAggregate([]error{errs, err})
		}

		result = util.LowestNonZeroResult(result, phaseResult)
	}

	return result, errs
}

// SetupWithManager sets up the controller with the Manager.
func (r *CharmedK8sControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&controlplanev1beta1.CharmedK8sControlPlane{}).
		Complete(r)
}

func (r *CharmedK8sControlPlaneReconciler) reconcileExternalReference(ctx context.Context, ref kcore.ObjectReference, cluster *clusterv1.Cluster) error {
	obj, err := external.Get(ctx, r.Client, &ref, cluster.Namespace)
	if err != nil {
		return err
	}

	objPatchHelper, err := patch.NewHelper(obj, r.Client)
	if err != nil {
		return err
	}

	obj.SetOwnerReferences(util.EnsureOwnerRef(obj.GetOwnerReferences(), metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       cluster.Name,
		UID:        cluster.UID,
	}))

	return objPatchHelper.Patch(ctx, obj)
}

func (r *CharmedK8sControlPlaneReconciler) getControlPlaneMachinesForCluster(ctx context.Context, cluster client.ObjectKey) ([]clusterv1.Machine, error) {
	selector := map[string]string{
		clusterv1.ClusterLabelName:             cluster.Name,
		clusterv1.MachineControlPlaneLabelName: "",
	}

	machineList := clusterv1.MachineList{}
	if err := r.Client.List(
		ctx,
		&machineList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels(selector),
	); err != nil {
		return nil, err
	}

	return machineList.Items, nil
}

func (r *CharmedK8sControlPlaneReconciler) reconcileMachines(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1beta1.CharmedK8sControlPlane, machines []clusterv1.Machine, jujuClient *juju.Client, modelUUID string) (res ctrl.Result, err error) {
	log := log.FromContext(ctx)
	log.Info("reconciling machines")
	// If we've made it this far, we can assume that all ownedMachines are up to date
	numMachines := len(machines)
	desiredReplicas := int(*kcp.Spec.Replicas)

	controlPlane := r.newControlPlane(cluster, kcp, machines)

	switch {
	// We are creating the first replica
	case numMachines < desiredReplicas && numMachines == 0:
		// Create new Machine
		log.Info("initializing control plane")

		return r.bootControlPlane(ctx, cluster, kcp, controlPlane)

	// We are scaling up
	case numMachines < desiredReplicas && numMachines > 0:
		conditions.MarkFalse(kcp, controlplanev1beta1.ResizedCondition, controlplanev1beta1.ScalingUpReason, clusterv1.ConditionSeverityWarning,
			"Scaling up control plane to %d replicas (actual %d)", desiredReplicas, numMachines)

		// Create a new Machine
		log.Info("scaling up control plane")
		return r.bootControlPlane(ctx, cluster, kcp, controlPlane)

	// We are scaling down
	case numMachines > desiredReplicas:
		conditions.MarkFalse(kcp, controlplanev1beta1.ResizedCondition, controlplanev1beta1.ScalingDownReason, clusterv1.ConditionSeverityWarning,
			"Scaling down control plane to %d replicas (actual %d)",
			desiredReplicas, numMachines)

		log.Info("scaling down control plane")
		res, err = r.scaleDownControlPlane(ctx, kcp, util.ObjectKey(cluster), controlPlane.KCP.Name, machines)
		if err != nil {
			if res.Requeue || res.RequeueAfter > 0 {
				log.Error(err, "failed to scale down control plane")
				return res, nil
			}
		}

		return res, err

	default:
		log.Info("updating conditions")
		if conditions.Has(kcp, clusterv1.MachinesReadyCondition) {
			log.Info("marking resized condition true")
			conditions.MarkTrue(kcp, clusterv1.ResizedCondition)
		}
		log.Info("marking machines created condition true")
		conditions.MarkTrue(kcp, clusterv1.MachinesCreatedCondition)
	}

	return ctrl.Result{}, nil

}

func (r *CharmedK8sControlPlaneReconciler) newControlPlane(cluster *clusterv1.Cluster, kcp *controlplanev1beta1.CharmedK8sControlPlane, machines []clusterv1.Machine) *ControlPlane {
	return &ControlPlane{
		KCP:      kcp,
		Cluster:  cluster,
		Machines: machines,
	}
}

func (r *CharmedK8sControlPlaneReconciler) bootControlPlane(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1beta1.CharmedK8sControlPlane, controlPlane *ControlPlane) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Since the cloned resource should eventually have a controller ref for the Machine, we create an
	// OwnerReference here without the Controller field set
	infraCloneOwner := &metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "CharmedK8sControlPlane",
		Name:       kcp.Name,
		UID:        kcp.UID,
	}

	// Clone the infrastructure template
	infraRef, err := external.CloneTemplate(ctx, &external.CloneTemplateInput{
		Client:      r.Client,
		TemplateRef: &kcp.Spec.MachineTemplate,
		Namespace:   kcp.Namespace,
		OwnerRef:    infraCloneOwner,
		ClusterName: cluster.Name,
	})
	if err != nil {
		conditions.MarkFalse(kcp, clusterv1.MachinesCreatedCondition,
			clusterv1.InfrastructureTemplateCloningFailedReason,
			clusterv1.ConditionSeverityError, err.Error())

		return ctrl.Result{}, err
	}

	// Clone the bootstrap configuration
	bootstrapConfig := &kcp.Spec.ControlPlaneConfig
	bootstrapRef, err := r.generateBootstrapConfig(ctx, kcp, bootstrapConfig)
	if err != nil {
		conditions.MarkFalse(kcp, clusterv1.MachinesCreatedCondition,
			clusterv1.BootstrapTemplateCloningFailedReason,
			clusterv1.ConditionSeverityError, err.Error())

		return ctrl.Result{}, err
	}
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.SimpleNameGenerator.GenerateName(kcp.Name + "-"),
			Namespace: kcp.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterLabelName:             cluster.Name,
				clusterv1.MachineControlPlaneLabelName: "",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kcp, clusterv1.GroupVersion.WithKind("CharmedK8sControlPlane")),
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:       cluster.Name,
			InfrastructureRef: *infraRef,
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: bootstrapRef,
			},
			//WARNING: This is a work around, I dont know how this is supposed to be set
		},
	}

	failureDomains := r.getFailureDomain(ctx, cluster)
	if len(failureDomains) > 0 {
		machine.Spec.FailureDomain = &failureDomains[rand.Intn(len(failureDomains))]
	}

	if err := r.Client.Create(ctx, machine); err != nil {
		conditions.MarkFalse(kcp, clusterv1.MachinesCreatedCondition,
			clusterv1.MachineCreationFailedReason,
			clusterv1.ConditionSeverityError, err.Error())

		return ctrl.Result{}, errors.Wrap(err, "Failed to create machine")
	}

	log.Info("created machine", "machine", machine)
	return ctrl.Result{Requeue: true}, nil
}

// getFailureDomain will return a slice of failure domains from the cluster status.
func (r *CharmedK8sControlPlaneReconciler) getFailureDomain(ctx context.Context, cluster *clusterv1.Cluster) []string {
	if cluster.Status.FailureDomains == nil {
		return nil
	}

	retList := []string{}
	for key := range cluster.Status.FailureDomains {
		retList = append(retList, key)
	}
	return retList
}

func (r *CharmedK8sControlPlaneReconciler) scaleDownControlPlane(ctx context.Context, kcp *controlplanev1beta1.CharmedK8sControlPlane, cluster client.ObjectKey, cpName string, machines []clusterv1.Machine) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	if len(machines) == 0 {
		return ctrl.Result{}, fmt.Errorf("no machines found")
	}
	log.WithValues("machines", len(machines)).Info("found control plane machines")
	deleteMachine := machines[len(machines)-1]
	machine := machines[len(machines)-1]
	for i := len(machines) - 1; i >= 0; i-- {
		machine = machines[i]
		logger := log.WithValues("machineName", machine.Name)
		if !machine.ObjectMeta.DeletionTimestamp.IsZero() {
			logger.Info("machine is in process of deletion")
		}
		// mark the oldest machine to be deleted first
		if machine.CreationTimestamp.Before(&deleteMachine.CreationTimestamp) {
			deleteMachine = machine
		}
	}

	log.WithValues("machineName", deleteMachine.Name).Info("deleting machine")

	err := r.Client.Delete(ctx, &deleteMachine)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Requeue so that we handle any additional scaling.
	return ctrl.Result{Requeue: true}, nil
}

func (r *CharmedK8sControlPlaneReconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1beta1.CharmedK8sControlPlane) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	kubeConfigSecret := &kcore.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace,
			Name:      cluster.Name + "-kubeconfig",
		},
	}
	if err := r.Client.Delete(ctx, kubeConfigSecret); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "failed to delete kubeconfig secret", "secret", kubeConfigSecret.Name)
		return ctrl.Result{}, err
	}

	// Get list of all control plane machines
	ownedMachines, err := r.getControlPlaneMachinesForCluster(ctx, util.ObjectKey(cluster))
	if err != nil {
		return ctrl.Result{}, err
	}

	// If no control plane machines remain, remove the finalizer
	if len(ownedMachines) == 0 {
		log.Info("no machines exist")
		if controllerutil.ContainsFinalizer(kcp, controlplanev1beta1.CharmedK8sControlPlaneFinalizer) {
			log.Info("removing finalizer and stopping reconciliation")
			controllerutil.RemoveFinalizer(kcp, controlplanev1beta1.CharmedK8sControlPlaneFinalizer)
			return ctrl.Result{}, r.Client.Update(ctx, kcp)
		}
	}

	for _, ownedMachine := range ownedMachines {
		// Already deleting this machine
		if !ownedMachine.ObjectMeta.DeletionTimestamp.IsZero() {
			continue
		}
		// Submit deletion request
		if err := r.Client.Delete(ctx, &ownedMachine); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	conditions.MarkFalse(kcp, clusterv1.ResizedCondition, clusterv1.DeletingReason, clusterv1.ConditionSeverityInfo, "")
	// Requeue the deletion so we can check to make sure machines got cleaned up
	return ctrl.Result{RequeueAfter: requeueTime}, nil
}

func (r *CharmedK8sControlPlaneReconciler) generateBootstrapConfig(ctx context.Context, kcp *controlplanev1beta1.CharmedK8sControlPlane, spec *bootstrapv1beta1.CharmedK8sConfigSpec) (*kcore.ObjectReference, error) {
	log := log.FromContext(ctx)
	log.Info("generating bootstrap config", "spec", spec)
	owner := metav1.OwnerReference{
		APIVersion:         clusterv1.GroupVersion.String(),
		Kind:               "CharmedK8sControlPlane",
		Name:               kcp.Name,
		UID:                kcp.UID,
		BlockOwnerDeletion: pointer.BoolPtr(true),
	}

	bootstrapConfig := &bootstrapv1beta1.CharmedK8sConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:            names.SimpleNameGenerator.GenerateName(kcp.Name + "-"),
			Namespace:       kcp.Namespace,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: *spec,
	}

	if err := r.Client.Create(ctx, bootstrapConfig); err != nil {
		return nil, errors.Wrap(err, "Failed to create bootstrap configuration")
	}

	bootstrapRef := &kcore.ObjectReference{
		APIVersion: bootstrapv1beta1.GroupVersion.String(),
		Kind:       "CharmedK8sConfig",
		Name:       bootstrapConfig.GetName(),
		Namespace:  bootstrapConfig.GetNamespace(),
		UID:        bootstrapConfig.GetUID(),
	}

	return bootstrapRef, nil
}

func (r *CharmedK8sControlPlaneReconciler) reconcileKubeconfig(ctx context.Context, cluster *clusterv1.Cluster, kcp *controlplanev1beta1.CharmedK8sControlPlane, machines []clusterv1.Machine, jujuClient *juju.Client, modelUUID string) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("reconciling kubeconfig")
	endpoint := cluster.Spec.ControlPlaneEndpoint
	if endpoint.IsZero() {
		return ctrl.Result{}, nil
	}

	kubeConfigSecret := &kcore.Secret{}
	kubeConfigSecret.Name = cluster.Name + "-kubeconfig"
	kubeConfigSecret.Namespace = cluster.Namespace

	err := r.Get(ctx, types.NamespacedName{Name: kubeConfigSecret.Name, Namespace: kubeConfigSecret.Namespace}, kubeConfigSecret)
	if err != nil {
		// If the error was a not found error we want to go through the creation process
		// otherwise it was a real error and we will log and return it
		if apierrors.IsNotFound(err) {
			readInput := juju.ReadApplicationInput{
				ModelUUID:       modelUUID,
				ApplicationName: "kubernetes-control-plane",
			}
			activeIdle, err := jujuClient.Applications.AreApplicationUnitsActiveIdle(ctx, readInput)
			if err != nil {
				log.Error(err, "error reading kubernetes-control-plane application")
				return ctrl.Result{}, err
			}
			if activeIdle {
				if kcp.Spec.GetKubeConfigOperationID == nil {
					log.Info("operationID was nil, enqueuing action to get kubeconfig from the control plane leader")
					enqueueInput := juju.EnqueueOperationInput{
						Receiver: "kubernetes-control-plane/leader",
						Name:     "get-kubeconfig",
					}
					enqueuedActions, err := jujuClient.Actions.EnqueueOperation(ctx, enqueueInput, modelUUID)
					if err != nil {
						log.Error(err, "failed to enqueue action using input", "input", enqueueInput)
						return ctrl.Result{}, err
					}

					kcp.Spec.GetKubeConfigOperationID = &enqueuedActions.OperationID
					if err := r.Update(ctx, kcp); err != nil {
						log.Error(err, "error updating operation id")
						return ctrl.Result{}, err
					}
					log.Info("successfully updated control plane", "Spec.OperationID", &kcp.Spec.GetKubeConfigOperationID)
					// object will re reconcile upon update of the spec
					return ctrl.Result{}, nil
				} else {
					// OperationID is set, which means we need to check for completion
					operation, err := jujuClient.Actions.GetOperation(ctx, *kcp.Spec.GetKubeConfigOperationID, modelUUID)
					if err != nil {
						log.Error(err, "error getting operation", "ID", *kcp.Spec.GetKubeConfigOperationID)
						return ctrl.Result{}, err
					}

					if operation.Fail != "" {
						log.Error(nil, fmt.Sprintf("operation %s failed with message: %s", operation.ID, operation.Fail))
						log.Info("Clearing operation ID so new operation can be queued")
						kcp.Spec.GetKubeConfigOperationID = nil
						if err := r.Update(ctx, kcp); err != nil {
							log.Error(err, "error updating operation id")
							return ctrl.Result{}, err
						}
						// object will re reconcile upon update of the spec
						return ctrl.Result{}, nil
					}
					// check for completion
					if !(operation.Status == "completed") {
						log.Info("operation is not complete, requeueing", "operation", operation)
						return ctrl.Result{RequeueAfter: requeueTime}, nil
					} else {
						log.Info("operation is complete", "operation", operation)
						if len(operation.Actions) != 1 {
							log.Error(nil, "expected 1 action", "got", len(operation.Actions))
							return ctrl.Result{}, errors.New("invalid action length")
						} else {
							actionResult := operation.Actions[0]
							log.Info("action output", "output", actionResult.Output)
							kubeconfig, keyExists := actionResult.Output["kubeconfig"]
							if !keyExists {
								log.Error(nil, "action result missing key kubeconfig")
								return ctrl.Result{}, errors.New("invalid action result format")
							}

							kubeConfigSecret.Type = kcore.SecretTypeOpaque
							kubeConfigSecret.Data = map[string][]byte{}
							kubeConfigSecret.Data["value"] = []byte(kubeconfig.(string))

							if err := r.Create(ctx, kubeConfigSecret); err != nil {
								log.Error(err, "failed to create kubeconfig secret")
								return ctrl.Result{}, err
							}
							log.Info(fmt.Sprintf("created kubeconfig secret %s", kubeConfigSecret.Name))
							return ctrl.Result{}, nil
						}
					}
				}
			} else {
				log.Info("kubernetes-control-plane units are not active/idle, requeueing")
				return ctrl.Result{RequeueAfter: requeueTime}, nil
			}
		} else {
			log.Error(err, "error getting kubeconfig secret", "secret", kubeConfigSecret.Name)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func getJujuConfigFromSecret(ctx context.Context, cluster *clusterv1.Cluster, c client.Client) (*JujuConfig, error) {
	log := log.FromContext(ctx)

	configSecret := &kcore.Secret{}
	objectKey := client.ObjectKey{
		Namespace: cluster.Namespace,
		Name:      controllerDataSecretName,
	}
	if err := c.Get(ctx, objectKey, configSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		} else {
			return nil, err
		}
	}
	data := string(configSecret.Data["controller-data"][:])
	split := strings.Split(data, fmt.Sprintf("%s:\n", cluster.Name+"-k8s-cloud"))
	yam := split[1]
	config := JujuConfig{}
	err := yaml.Unmarshal([]byte(yam), &config)
	if err != nil {
		log.Error(err, "error unmarshalling YAML data into config struct")
		return nil, err
	}

	return &config, nil

}
