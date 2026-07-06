// Package v1alpha1 define a API ScoringPipeline do zeedfai.
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
	// Imagem do serviço de scoring (consumidor Kafka).
	Image string `json:"image"`
	// Nome de um Secret docker-registry no mesmo namespace, para pull de
	// imagens privadas (ex.: GHCR). Opcional.
	// +optional
	ImagePullSecret string `json:"imagePullSecret,omitempty"`
}

type KafkaSpec struct {
	// Bootstrap servers, ex: "kafka:9092".
	Brokers string `json:"brokers"`
	Topic   string `json:"topic"`
	// +optional
	ConsumerGroup string `json:"consumerGroup,omitempty"`
}

type SLOSpec struct {
	// Latência p99.9 máxima em milissegundos. Default 250 (o SLA de referência).
	// +kubebuilder:default=250
	// +optional
	LatencyP999Ms int32 `json:"latencyP999Ms,omitempty"`
	// +optional
	ErrorRatePct string `json:"errorRatePct,omitempty"`
}

type ScalingSpec struct {
	// +kubebuilder:default=1
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`
	// +kubebuilder:default=10
	// +optional
	MaxReplicas int32 `json:"maxReplicas,omitempty"`
	// Lag alvo por réplica; acima disto o operator faz scale-out. (Fase 4)
	// +kubebuilder:default=1000
	// +optional
	TargetLagPerReplica int64 `json:"targetLagPerReplica,omitempty"`
}

type ScoringPipelineSpec struct {
	Model ModelSpec `json:"model"`
	Kafka KafkaSpec `json:"kafka"`
	// +optional
	SLO SLOSpec `json:"slo,omitempty"`
	// +optional
	Scaling ScalingSpec `json:"scaling,omitempty"`
}

type ScoringPipelineStatus struct {
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
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
