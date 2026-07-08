package controllers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	platformv1alpha1 "github.com/bastian/zeedfai/operator/api/v1alpha1"
)

// Integration tests with envtest (real kube-apiserver + etcd, no
// kubelet). Kafka and Prometheus don't exist here: the autoscaler
// degrades (lag unavailable → keeps minReplicas) and observability
// skips ServiceMonitor/PrometheusRule (CRDs absent) but still creates
// the PDB — both degraded paths are part of what's being tested.
//
// Requires the envtest binaries: make envtest (or setup-envtest use).

var k8sClient client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("SKIP: KUBEBUILDER_ASSETS not set (run 'make test-integration')")
		os.Exit(0)
	}
	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "config", "crd")},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Println("envtest start:", err)
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	// Espelha o scheme do main.go — monitoringv1 registado mas sem os CRDs
	// instalados no envtest, para exercer o caminho degradado (NoKindMatch).
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = platformv1alpha1.AddToScheme(scheme)
	_ = monitoringv1.AddToScheme(scheme)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		fmt.Println("manager:", err)
		os.Exit(1)
	}
	if err := (&ScoringPipelineReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("test"),
	}).SetupWithManager(mgr); err != nil {
		fmt.Println("setup:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = mgr.Start(ctx) }()
	k8sClient = mgr.GetClient()

	code := m.Run()
	cancel()
	_ = testEnv.Stop()
	os.Exit(code)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", what)
}

func newPipeline(name string) *platformv1alpha1.ScoringPipeline {
	return &platformv1alpha1.ScoringPipeline{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: platformv1alpha1.ScoringPipelineSpec{
			Model:   platformv1alpha1.ModelSpec{Image: "example.com/scorer:test", ImagePullSecret: "ghcr-pull"},
			Kafka:   platformv1alpha1.KafkaSpec{Brokers: "kafka.invalid:9092", Topic: "tx", ConsumerGroup: "g-" + name},
			Scaling: platformv1alpha1.ScalingSpec{MinReplicas: 2, MaxReplicas: 5, TargetLagPerReplica: 1000},
		},
	}
}

func TestReconcileCreatesWorkload(t *testing.T) {
	ctx := context.Background()
	sp := newPipeline("p1")
	if err := k8sClient.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}

	var dep appsv1.Deployment
	eventually(t, "deployment created", func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Name: "p1-scorer", Namespace: "default"}, &dep) == nil
	})
	if *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, expected 2 (minReplicas; lag unavailable)", *dep.Spec.Replicas)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "example.com/scorer:test" {
		t.Errorf("image = %s", c.Image)
	}
	envs := map[string]string{}
	for _, e := range c.Env {
		envs[e.Name] = e.Value
	}
	for k, want := range map[string]string{
		"KAFKA_BROKERS": "kafka.invalid:9092", "KAFKA_TOPIC": "tx",
		"KAFKA_GROUP": "g-p1", "PIPELINE_NAME": "p1",
	} {
		if envs[k] != want {
			t.Errorf("env %s = %q, expected %q", k, envs[k], want)
		}
	}
	if len(dep.Spec.Template.Spec.ImagePullSecrets) != 1 || dep.Spec.Template.Spec.ImagePullSecrets[0].Name != "ghcr-pull" {
		t.Errorf("imagePullSecrets = %v", dep.Spec.Template.Spec.ImagePullSecrets)
	}
	if dep.Spec.Template.Spec.SecurityContext == nil || dep.Spec.Template.Spec.SecurityContext.RunAsNonRoot == nil || !*dep.Spec.Template.Spec.SecurityContext.RunAsNonRoot {
		t.Errorf("pod securityContext should require non-root: %#v", dep.Spec.Template.Spec.SecurityContext)
	}
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("container securityContext should disable privilege escalation: %#v", c.SecurityContext)
	}
	if c.Resources.Requests.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
		t.Errorf("resources should set cpu/memory requests and limits: %#v", c.Resources)
	}

	var svc corev1.Service
	eventually(t, "service created", func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Name: "p1-scorer", Namespace: "default"}, &svc) == nil
	})
	if svc.Labels["zeedfai.io/pipeline"] != "p1" {
		t.Errorf("service missing pipeline label (regression of the ServiceMonitor bug): %v", svc.Labels)
	}

	var pdb policyv1.PodDisruptionBudget
	eventually(t, "pdb created (even without monitoring CRDs)", func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Name: "p1-scorer", Namespace: "default"}, &pdb) == nil
	})
}

func TestSelfHealingRecreatesDeployment(t *testing.T) {
	ctx := context.Background()
	sp := newPipeline("p2")
	if err := k8sClient.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	key := types.NamespacedName{Name: "p2-scorer", Namespace: "default"}
	eventually(t, "deployment created", func() bool { return k8sClient.Get(ctx, key, &dep) == nil })

	uid := dep.UID
	if err := k8sClient.Delete(ctx, &dep); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deployment recreated after delete", func() bool {
		var d appsv1.Deployment
		if err := k8sClient.Get(ctx, key, &d); err != nil {
			return false
		}
		return d.UID != uid && d.DeletionTimestamp == nil
	})
}

func TestCanaryDeploymentLifecycle(t *testing.T) {
	ctx := context.Background()
	sp := newPipeline("p3")
	sp.Spec.Canary = platformv1alpha1.CanarySpec{Enabled: true, Image: "example.com/scorer:candidate", StepPercent: 50}
	if err := k8sClient.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}

	key := types.NamespacedName{Name: "p3-scorer-canary", Namespace: "default"}
	var canary appsv1.Deployment
	eventually(t, "canary deployment created", func() bool { return k8sClient.Get(ctx, key, &canary) == nil })
	if got := canary.Spec.Template.Spec.Containers[0].Image; got != "example.com/scorer:candidate" {
		t.Errorf("canary image = %s", got)
	}
	// 50% of 2 stable replicas = 1 canary replica
	if *canary.Spec.Replicas != 1 {
		t.Errorf("canary replicas = %d, expected 1", *canary.Spec.Replicas)
	}
	// env ROLE=canary and same consumer group as stable (partition split)
	envs := map[string]string{}
	for _, e := range canary.Spec.Template.Spec.Containers[0].Env {
		envs[e.Name] = e.Value
	}
	if envs["ROLE"] != "canary" || envs["KAFKA_GROUP"] != "g-p3" {
		t.Errorf("canary env = %v", envs)
	}
	if canary.Spec.Template.Spec.SecurityContext == nil || canary.Spec.Template.Spec.Containers[0].SecurityContext == nil {
		t.Errorf("canary should include pod and container security contexts")
	}
	if canary.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().IsZero() {
		t.Errorf("canary should include resource requests: %#v", canary.Spec.Template.Spec.Containers[0].Resources)
	}

	// disabling the canary removes the deployment
	eventually(t, "patch canary disabled", func() bool {
		var cur platformv1alpha1.ScoringPipeline
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "p3", Namespace: "default"}, &cur); err != nil {
			return false
		}
		cur.Spec.Canary.Enabled = false
		return k8sClient.Update(ctx, &cur) == nil
	})
	eventually(t, "canary deployment removed", func() bool {
		var d appsv1.Deployment
		err := k8sClient.Get(ctx, key, &d)
		return apierrors.IsNotFound(err) || (err == nil && d.DeletionTimestamp != nil)
	})
}
