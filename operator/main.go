package main

import (
	"os"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	platformv1alpha1 "github.com/bastian/zeedfai/operator/api/v1alpha1"
	"github.com/bastian/zeedfai/operator/controllers"
)

func main() {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = platformv1alpha1.AddToScheme(scheme)
	_ = monitoringv1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: ":8083"},
		HealthProbeBindAddress: ":8082",
		// Leader election só faz sentido in-cluster; em dev local (make run)
		// fica desligada por omissão.
		LeaderElection:   os.Getenv("ENABLE_LEADER_ELECTION") == "true",
		LeaderElectionID: "zeedfai-operator",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controllers.ScoringPipelineReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("zeedfai-operator"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	setupLog.Info("starting zeedfai operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}
