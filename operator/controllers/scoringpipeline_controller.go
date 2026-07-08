package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

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

const runbookBaseURL = "https://github.com/nelsudev/zeedfai-kubernetes-operator-gitops/blob/main/runbooks"

// ScoringPipelineReconciler reconciles ScoringPipelines: Deployment +
// Service, autoscaling by consumer lag, SLO self-healing, canary analysis,
// and self-generated observability.
type ScoringPipelineReconciler struct {
	client.Client
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=platform.zeedfai.io,resources=scoringpipelines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.zeedfai.io,resources=scoringpipelines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;events,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors;prometheusrules,verbs=get;list;watch;create;update;patch

func (r *ScoringPipelineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var sp platformv1alpha1.ScoringPipeline
	if err := r.Get(ctx, req.NamespacedName, &sp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	minReplicas := sp.Spec.Scaling.MinReplicas
	if minReplicas < 1 {
		minReplicas = 1
	}
	maxReplicas := sp.Spec.Scaling.MaxReplicas
	if maxReplicas < minReplicas {
		maxReplicas = minReplicas
	}
	group := sp.Spec.Kafka.ConsumerGroup
	if group == "" {
		group = "zeedfai-" + sp.Name
	}

	// Autoscaling by consumer lag, with self-healing on SLO violation,
	// subject to a cooldown to avoid flapping under bursty traffic.
	replicas := minReplicas
	if sp.Status.DesiredReplicas > 0 {
		replicas = sp.Status.DesiredReplicas
	}
	cooldown := time.Duration(sp.Spec.Scaling.CooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	var lastScale *time.Time
	if sp.Status.LastScaleTime != nil {
		t := sp.Status.LastScaleTime.Time
		lastScale = &t
	}

	lag, lagErr := consumerLag(ctx, strings.Split(sp.Spec.Kafka.Brokers, ","), group, sp.Spec.Kafka.Topic)
	if lagErr != nil {
		log.Info("consumer lag unavailable, skipping autoscale evaluation", "error", lagErr.Error())
	} else {
		sp.Status.ConsumerLag = lag
		consumerLagGauge.WithLabelValues(sp.Name).Set(float64(lag))
		desired := desiredReplicasFromLag(lag, sp.Spec.Scaling.TargetLagPerReplica, minReplicas, maxReplicas)

		sloMs := sp.Spec.SLO.LatencyP999Ms
		if sloMs <= 0 {
			sloMs = 250
		}
		if sloLatencyViolated(ctx, sp.Name, sloMs) && desired < maxReplicas {
			desired++
			log.Info("SLO latency violated, forcing scale-out", "pipeline", sp.Name, "desired", desired)
		}

		if desired != replicas && cooldownElapsed(lastScale, cooldown) {
			log.Info("autoscale decision", "pipeline", sp.Name, "lag", lag, "from", replicas, "to", desired)
			replicas = desired
			now := metav1.Now()
			sp.Status.LastScaleTime = &now
		}
	}
	sp.Status.DesiredReplicas = replicas
	desiredReplicasGauge.WithLabelValues(sp.Name).Set(float64(replicas))

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: sp.Name + "-scorer", Namespace: sp.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		labels := map[string]string{"app.kubernetes.io/name": "zeedfai-scorer", "zeedfai.io/pipeline": sp.Name}
		dep.Labels = labels
		dep.Spec.Replicas = &replicas
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Template.Labels = labels
		dep.Spec.Template.Spec.SecurityContext = hardenedPodSecurityContext()
		dep.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  "scorer",
			Image: sp.Spec.Model.Image,
			Env: []corev1.EnvVar{
				{Name: "KAFKA_BROKERS", Value: sp.Spec.Kafka.Brokers},
				{Name: "KAFKA_TOPIC", Value: sp.Spec.Kafka.Topic},
				{Name: "KAFKA_GROUP", Value: group},
				{Name: "PIPELINE_NAME", Value: sp.Name},
			},
			Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8080}},
			Resources:       scorerResources(),
			SecurityContext: hardenedContainerSecurityContext(),
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
		// Labels on the Service itself: that's what the ServiceMonitor
		// selects on (the Service's spec.selector only selects pods).
		svc.Labels = map[string]string{"app.kubernetes.io/name": "zeedfai-scorer", "zeedfai.io/pipeline": sp.Name}
		svc.Spec.Selector = map[string]string{"zeedfai.io/pipeline": sp.Name}
		svc.Spec.Ports = []corev1.ServicePort{{Name: "metrics", Port: 8080}}
		return controllerutil.SetControllerReference(&sp, svc, r.Scheme())
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
	}
	if err := r.reconcileObservability(ctx, &sp, replicas); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile observability: %w", err)
	}
	if err := r.reconcileCanary(ctx, &sp, group, replicas); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile canary: %w", err)
	}

	// Status
	var current appsv1.Deployment
	available := metav1.ConditionFalse
	reason, msg := "Progressing", "waiting for ready replicas"
	if err := r.Get(ctx, types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &current); err == nil {
		sp.Status.Replicas = current.Status.ReadyReplicas
		readyReplicasGauge.WithLabelValues(sp.Name).Set(float64(current.Status.ReadyReplicas))
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
	// Periodic requeue: the autoscaler needs to re-evaluate lag even without
	// spec changes (e.g. during a traffic burst).
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *ScoringPipelineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.ScoringPipeline{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
