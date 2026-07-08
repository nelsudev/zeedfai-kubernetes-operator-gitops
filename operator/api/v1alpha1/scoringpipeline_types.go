// Package v1alpha1 defines the zeedfai ScoringPipeline API.
// +kubebuilder:object:generate=true
// +groupName=platform.zeedfai.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "platform.zeedfai.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

type ModelSpec struct {
	// Image of the scoring service (Kafka consumer).
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// Name of a docker-registry Secret in the same namespace, for pulling
	// private images (e.g. GHCR). Optional.
	// +optional
	ImagePullSecret string `json:"imagePullSecret,omitempty"`
}

type KafkaSpec struct {
	// Bootstrap servers, e.g. "kafka:9092".
	// +kubebuilder:validation:MinLength=1
	Brokers string `json:"brokers"`
	// +kubebuilder:validation:MinLength=1
	Topic string `json:"topic"`
	// +optional
	ConsumerGroup string `json:"consumerGroup,omitempty"`
}

type SLOSpec struct {
	// Maximum p99.9 latency in milliseconds. Default 250 (the reference SLA).
	// +kubebuilder:default=250
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60000
	// +optional
	LatencyP999Ms int32 `json:"latencyP999Ms,omitempty"`
	// +optional
	ErrorRatePct string `json:"errorRatePct,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.maxReplicas == 0 || self.minReplicas == 0 || self.maxReplicas >= self.minReplicas",message="maxReplicas must be greater than or equal to minReplicas"
type ScalingSpec struct {
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxReplicas int32 `json:"maxReplicas,omitempty"`
	// Target lag per replica; above this the operator scales out.
	// +kubebuilder:default=1000
	// +kubebuilder:validation:Minimum=1
	// +optional
	TargetLagPerReplica int64 `json:"targetLagPerReplica,omitempty"`
	// Minimum time between scaling decisions, to avoid flapping.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +optional
	CooldownSeconds int32 `json:"cooldownSeconds,omitempty"`
}

type CanarySpec struct {
	// Enables a canary analysis for spec.canary.image.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Candidate image to validate before it becomes spec.model.image.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Image string `json:"image,omitempty"`
	// % of total replicas assigned to the canary (it shares the consumer
	// group with stable, so it receives that fraction of traffic via rebalance).
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	StepPercent int32 `json:"stepPercent,omitempty"`
	// Error rate (%) above which automatic rollback is triggered.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	ErrorRateThresholdPct int32 `json:"errorRateThresholdPct,omitempty"`
	// Evaluation window before marking the canary safe for promotion.
	// +kubebuilder:default=120
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +optional
	EvaluationSeconds int32 `json:"evaluationSeconds,omitempty"`
}

type ScoringPipelineSpec struct {
	Model ModelSpec `json:"model"`
	Kafka KafkaSpec `json:"kafka"`
	// +optional
	SLO SLOSpec `json:"slo,omitempty"`
	// +optional
	Scaling ScalingSpec `json:"scaling,omitempty"`
	// +optional
	Canary CanarySpec `json:"canary,omitempty"`
}

type ScoringPipelineStatus struct {
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// Last scaling decision applied by the autoscaler (consumer lag / SLO).
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`
	// Consumer lag observed at the last evaluation.
	// +optional
	ConsumerLag int64 `json:"consumerLag,omitempty"`
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`
	// +optional
	CanaryStartedAt *metav1.Time `json:"canaryStartedAt,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="Lag",type=integer,JSONPath=`.status.consumerLag`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
type ScoringPipeline struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScoringPipelineSpec   `json:"spec,omitempty"`
	Status ScoringPipelineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ScoringPipelineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ScoringPipeline `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ScoringPipeline{}, &ScoringPipelineList{})
}
