// +kubebuilder:object:generate=true
// +groupName=controlplane.cluster.x-k8s.io
package v1beta1

import clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

const (
	// MachinesReadyCondition reports an aggregate of current status of the machines controlled by the CharmedK8sControlPlane.
	MachinesReadyCondition clusterv1.ConditionType = "MachinesReady"
)

const (
	// ResizedCondition documents a MicroK8sControlPlane that is resizing the set of controlled machines.
	ResizedCondition clusterv1.ConditionType = "Resized"

	// ScalingUpReason (Severity=Info) documents a MicroK8sControlPlane that is increasing the number of replicas.
	ScalingUpReason = "ScalingUp"

	// ScalingDownReason (Severity=Info) documents a MicroK8sControlPlane that is decreasing the number of replicas.
	ScalingDownReason = "ScalingDown"
)
