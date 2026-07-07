package controllers

import (
	"context"
	"errors"
	"fmt"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	policyv1 "k8s.io/api/policy/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/bastian/zeedfai/operator/api/v1alpha1"
)

// reconcileObservability garante ServiceMonitor, PrometheusRule (com
// runbook_url) e PodDisruptionBudget para o pipeline. Requer o
// prometheus-operator instalado no cluster (ex.: via kube-prometheus-stack).
func (r *ScoringPipelineReconciler) reconcileObservability(ctx context.Context, sp *platformv1alpha1.ScoringPipeline, replicas int32) error {
	log := ctrl.LoggerFrom(ctx)
	labels := map[string]string{"app.kubernetes.io/name": "zeedfai-scorer", "zeedfai.io/pipeline": sp.Name}

	sm := &monitoringv1.ServiceMonitor{ObjectMeta: metav1.ObjectMeta{Name: sp.Name + "-scorer", Namespace: sp.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sm, func() error {
		sm.Labels = labels
		sm.Spec.Selector = metav1.LabelSelector{MatchLabels: labels}
		sm.Spec.Endpoints = []monitoringv1.Endpoint{{Port: "metrics", Interval: "15s"}}
		return controllerutil.SetControllerReference(sp, sm, r.Scheme())
	}); err != nil {
		// Cluster sem prometheus-operator (ou envtest): degradar com aviso em
		// vez de bloquear a gestão do pipeline por falta de monitoring.
		if isNoKindMatch(err) {
			log.Info("monitoring CRDs not installed; skipping ServiceMonitor/PrometheusRule")
			return r.reconcilePDB(ctx, sp, replicas)
		}
		return fmt.Errorf("servicemonitor: %w", err)
	}

	latencySLO := sp.Spec.SLO.LatencyP999Ms
	if latencySLO <= 0 {
		latencySLO = 250
	}
	minReplicas := replicas
	if minReplicas < 1 {
		minReplicas = 1
	}

	rule := &monitoringv1.PrometheusRule{ObjectMeta: metav1.ObjectMeta{Name: sp.Name + "-scorer", Namespace: sp.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rule, func() error {
		rule.Labels = labels
		rule.Spec.Groups = []monitoringv1.RuleGroup{{
			Name: sp.Name + ".rules",
			Rules: []monitoringv1.Rule{
				{
					Alert: "ZeedfaiSLOLatencyViolated",
					Expr:  intstr.FromString(fmt.Sprintf(`histogram_quantile(0.999, sum(rate(zeedfai_scorer_latency_seconds_bucket{pipeline="%s"}[5m])) by (le)) > %g`, sp.Name, float64(latencySLO)/1000)),
					For:   ptrDuration("5m"),
					Labels: map[string]string{"severity": "critical", "pipeline": sp.Name},
					Annotations: map[string]string{
						"summary":     fmt.Sprintf("p99.9 latency above %dms for %s", latencySLO, sp.Name),
						"runbook_url": runbookBaseURL + "/slo-latency-violated.md",
					},
				},
				{
					Alert: "ZeedfaiConsumerLagGrowing",
					Expr:  intstr.FromString(fmt.Sprintf(`deriv(kafka_consumergroup_lag{consumergroup="%s"}[5m]) > 0`, sp.Spec.Kafka.ConsumerGroup)),
					For:   ptrDuration("5m"),
					Labels: map[string]string{"severity": "warning", "pipeline": sp.Name},
					Annotations: map[string]string{
						"summary":     fmt.Sprintf("consumer lag growing for %s", sp.Name),
						"runbook_url": runbookBaseURL + "/consumer-lag-growing.md",
					},
				},
			},
		}}
		return controllerutil.SetControllerReference(sp, rule, r.Scheme())
	}); err != nil {
		return fmt.Errorf("prometheusrule: %w", err)
	}

	if err := r.reconcilePDB(ctx, sp, minReplicas); err != nil {
		return err
	}
	log.V(1).Info("observability reconciled", "pipeline", sp.Name)
	return nil
}

func (r *ScoringPipelineReconciler) reconcilePDB(ctx context.Context, sp *platformv1alpha1.ScoringPipeline, replicas int32) error {
	labels := map[string]string{"app.kubernetes.io/name": "zeedfai-scorer", "zeedfai.io/pipeline": sp.Name}
	minAvailable := intstr.FromInt(int(max32(replicas-1, 0)))
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: sp.Name + "-scorer", Namespace: sp.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		pdb.Spec.MinAvailable = &minAvailable
		return controllerutil.SetControllerReference(sp, pdb, r.Scheme())
	}); err != nil {
		return fmt.Errorf("pdb: %w", err)
	}
	return nil
}

// isNoKindMatch deteta "kind não registado na API server" mesmo se o erro
// vier embrulhado (IsNoMatchError faz type assertion simples, sem Unwrap).
func isNoKindMatch(err error) bool {
	var noMatch *apimeta.NoKindMatchError
	return errors.As(err, &noMatch)
}

func ptrDuration(s monitoringv1.Duration) *monitoringv1.Duration { return &s }

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
