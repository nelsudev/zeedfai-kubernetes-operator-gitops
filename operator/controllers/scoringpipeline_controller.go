package controllers

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/bastian/zeedfai/operator/api/v1alpha1"
)

const runbookBaseURL = "https://github.com/bastian/zeedfai/blob/main/runbooks"

// ScoringPipelineReconciler reconcilia ScoringPipelines: Deployment + Service
// hoje; autoscaling por consumer lag e self-healing por SLO na Fase 4.
type ScoringPipelineReconciler struct {
	client.Client
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=platform.zeedfai.io,resources=scoringpipelines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.zeedfai.io,resources=scoringpipelines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;events,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors;prometheusrules,verbs=get;list;watch;create;update;patch

func (r *ScoringPipelineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var sp platformv1alpha1.ScoringPipeline
	if err := r.Get(ctx, req.NamespacedName, &sp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	replicas := sp.Spec.Scaling.MinReplicas
	if replicas < 1 {
		replicas = 1
	}
	// TODO(fase 4): substituir por decisão de autoscaling baseada em consumer
	// lag (targetLagPerReplica) e latência p99.9 vinda do Prometheus, com
	// cooldown para evitar flapping.

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: sp.Name + "-scorer", Namespace: sp.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		labels := map[string]string{"app.kubernetes.io/name": "zeedfai-scorer", "zeedfai.io/pipeline": sp.Name}
		dep.Labels = labels
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Template.Labels = labels
		group := sp.Spec.Kafka.ConsumerGroup
		if group == "" {
			group = "zeedfai-" + sp.Name
		}
		dep.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  "scorer",
			Image: sp.Spec.Model.Image,
			Env: []corev1.EnvVar{
				{Name: "KAFKA_BROKERS", Value: sp.Spec.Kafka.Brokers},
				{Name: "KAFKA_TOPIC", Value: sp.Spec.Kafka.Topic},
				{Name: "KAFKA_GROUP", Value: group},
				{Name: "PIPELINE_NAME", Value: sp.Name},
			},
			Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8080}},
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstrFromInt(8080)},
			}},
		}}
		if sp.Spec.Model.ImagePullSecret != "" {
			dep.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: sp.Spec.Model.ImagePullSecret}}
		}
		return controllerutil.SetControllerReference(&sp, dep, r.Scheme())
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile deployment: %w", err)
	}
	if op != controllerutil.OperationResultNone {
		log.Info("deployment reconciled", "operation", op)
		r.Recorder.Eventf(&sp, corev1.EventTypeNormal, "Reconciled", "deployment %s: %s", dep.Name, op)
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sp.Name + "-scorer", Namespace: sp.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.Selector = map[string]string{"zeedfai.io/pipeline": sp.Name}
		svc.Spec.Ports = []corev1.ServicePort{{Name: "metrics", Port: 8080}}
		return controllerutil.SetControllerReference(&sp, svc, r.Scheme())
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
	}
	if err := r.reconcileObservability(ctx, &sp, replicas); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile observability: %w", err)
	}

	// Status
	var current appsv1.Deployment
	available := metav1.ConditionFalse
	reason, msg := "Progressing", "waiting for ready replicas"
	if err := r.Get(ctx, types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &current); err == nil {
		sp.Status.Replicas = current.Status.ReadyReplicas
		if current.Status.ReadyReplicas >= replicas {
			available, reason, msg = metav1.ConditionTrue, "AllReplicasReady", "all replicas ready"
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	meta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{
		Type: "Available", Status: available, Reason: reason, Message: msg,
		ObservedGeneration: sp.Generation,
	})
	if err := r.Status().Update(ctx, &sp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

func (r *ScoringPipelineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.ScoringPipeline{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
