package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/bastian/zeedfai/operator/api/v1alpha1"
)

// reconcileCanary manages a secondary Deployment that runs the candidate
// image and shares the consumer group with stable — Kafka distributes
// partitions across all members of the group, so the canary naturally
// receives a fraction of traffic proportional to its replica count.
//
// Design decision: rollback is automatic (the risky side); promotion is
// not — it's left as a status/Event recommendation, to avoid conflicting
// with GitOps (the operator never writes spec.model.image back; that's an
// auditable Git change).
func (r *ScoringPipelineReconciler) reconcileCanary(ctx context.Context, sp *platformv1alpha1.ScoringPipeline, group string, stableReplicas int32) error {
	log := ctrl.LoggerFrom(ctx)
	canaryName := sp.Name + "-scorer-canary"

	active := sp.Spec.Canary.Enabled && sp.Spec.Canary.Image != "" && sp.Spec.Canary.Image != sp.Spec.Model.Image

	// Anti-loop guard: if this same spec (same Generation) already had a
	// rollback, don't recreate the canary — otherwise the create→fail→rollback
	// cycle would repeat forever. Any spec change (new image, disabling the
	// canary) bumps the Generation and clears the guard.
	if cond := meta.FindStatusCondition(sp.Status.Conditions, "CanaryHealthy"); active &&
		cond != nil && cond.Reason == "RolledBack" && cond.ObservedGeneration == sp.Generation {
		active = false
	}

	if !active {
		if sp.Status.CanaryStartedAt != nil {
			sp.Status.CanaryStartedAt = nil
		}
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: canaryName, Namespace: sp.Namespace}}
		if err := r.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete canary deployment: %w", err)
		}
		return nil
	}

	if sp.Status.CanaryStartedAt == nil {
		now := metav1.Now()
		sp.Status.CanaryStartedAt = &now
		log.Info("canary started", "pipeline", sp.Name, "image", sp.Spec.Canary.Image)
		r.Recorder.Eventf(sp, corev1.EventTypeNormal, "CanaryStarted", "canary %s started with image %s", sp.Name, sp.Spec.Canary.Image)
	}

	step := sp.Spec.Canary.StepPercent
	if step <= 0 {
		step = 20
	}
	canaryReplicas := int32(1)
	if r := (stableReplicas*step + 99) / 100; r > canaryReplicas {
		canaryReplicas = r
	}

	labels := map[string]string{"app.kubernetes.io/name": "zeedfai-scorer", "zeedfai.io/pipeline": sp.Name, "zeedfai.io/role": "canary"}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: canaryName, Namespace: sp.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		dep.Spec.Replicas = &canaryReplicas
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Template.Labels = labels
		dep.Spec.Template.Spec.SecurityContext = hardenedPodSecurityContext()
		dep.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  "scorer",
			Image: sp.Spec.Canary.Image,
			Env: []corev1.EnvVar{
				{Name: "KAFKA_BROKERS", Value: sp.Spec.Kafka.Brokers},
				{Name: "KAFKA_TOPIC", Value: sp.Spec.Kafka.Topic},
				{Name: "KAFKA_GROUP", Value: group}, // same group as stable: shares the traffic
				{Name: "PIPELINE_NAME", Value: sp.Name},
				{Name: "ROLE", Value: "canary"},
			},
			Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8080}},
			Resources:       scorerResources(),
			SecurityContext: hardenedContainerSecurityContext(),
		}}
		if sp.Spec.Model.ImagePullSecret != "" {
			dep.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: sp.Spec.Model.ImagePullSecret}}
		}
		return controllerutil.SetControllerReference(sp, dep, r.Scheme())
	}); err != nil {
		return fmt.Errorf("reconcile canary deployment: %w", err)
	}

	threshold := float64(sp.Spec.Canary.ErrorRateThresholdPct)
	if threshold <= 0 {
		threshold = 5
	}
	errRate, ok := canaryErrorRatePct(ctx, sp.Name)
	if ok && errRate > threshold {
		log.Info("canary error rate above threshold, rolling back", "pipeline", sp.Name, "errorRatePct", errRate, "threshold", threshold)
		r.Recorder.Eventf(sp, corev1.EventTypeWarning, "CanaryRolledBack",
			"canary %s error rate %.2f%% > %.2f%%, rolled back automatically", sp.Name, errRate, threshold)
		if err := r.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("rollback canary: %w", err)
		}
		sp.Status.CanaryStartedAt = nil
		meta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "CanaryHealthy", ObservedGeneration: sp.Generation, Status: metav1.ConditionFalse, Reason: "RolledBack", Message: fmt.Sprintf("error rate %.2f%% exceeded threshold %.2f%%", errRate, threshold)})
		return nil
	}

	evalWindow := time.Duration(sp.Spec.Canary.EvaluationSeconds) * time.Second
	if evalWindow <= 0 {
		evalWindow = 120 * time.Second
	}
	if time.Since(sp.Status.CanaryStartedAt.Time) >= evalWindow {
		meta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "CanaryHealthy", ObservedGeneration: sp.Generation, Status: metav1.ConditionTrue, Reason: "EvaluationPassed",
			Message: "canary survived the evaluation window without exceeding the error threshold; safe to promote via a Git commit (spec.model.image)"})
	} else {
		meta.SetStatusCondition(&sp.Status.Conditions, metav1.Condition{Type: "CanaryHealthy", ObservedGeneration: sp.Generation, Status: metav1.ConditionUnknown, Reason: "Evaluating", Message: "canary analysis in progress"})
	}
	return nil
}

// canaryErrorRatePct queries Prometheus: % of events with an error over the
// last 2 minutes, for the pipeline's role=canary pods.
func canaryErrorRatePct(ctx context.Context, pipeline string) (float64, bool) {
	// Denominator = processed + errored events, because an event that errors
	// doesn't increment events_total — otherwise the percentage could exceed 100%.
	q := fmt.Sprintf(`100 * sum(rate(zeedfai_scorer_errors_total{pipeline=%q,role="canary"}[2m])) / clamp_min(sum(rate(zeedfai_scorer_events_total{pipeline=%q,role="canary"}[2m])) + sum(rate(zeedfai_scorer_errors_total{pipeline=%q,role="canary"}[2m])), 0.001)`, pipeline, pipeline, pipeline)
	u := fmt.Sprintf("%s/api/v1/query?query=%s", strings.TrimRight(prometheusURL, "/"), url.QueryEscape(q))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, false
	}
	resp, err := prometheusHTTPClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()

	var body struct {
		Data struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Data.Result) == 0 {
		return 0, false
	}
	s, ok := body.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
