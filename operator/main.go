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

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"go.uber.org/zap/zapcore"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	manifestv1alpha1 "github.com/kyma-project/manifest-operator/api/api/v1alpha1"
	"github.com/kyma-project/manifest-operator/operator/controllers"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(manifestv1alpha1.AddToScheme(scheme))

	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var clientQps float64
	var clientBurst int
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":2020", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":2021", "The address the probe endpoint binds to.")
	flag.Float64Var(&clientQps, "in-cluster-client-qps", 150.0, "in-cluster K8s client QPS")
	flag.IntVar(&clientBurst, "in-cluster-client-burst", 150, "in-cluster K8s client burst for throttle")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
		TimeEncoder: CustomTimeEncoder,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	reconciliationTargetKubeconfigs := []string{}

	for _, e := range os.Environ() {
		pair := strings.SplitN(e, "=", 2)
		if strings.HasPrefix(pair[0], "RECONCILIATION_TARGET_KUBECONFIG") {
			reconciliationTargetKubeconfigs = append(reconciliationTargetKubeconfigs, pair[1])
		}
	}

	if len(reconciliationTargetKubeconfigs) < 1 {
		panic("No \"RECONCILIATION_TARGET_KUBECONFIG_<INDEX>\" variables defined")
	}

	fmt.Println("----------------------------------------")
	fmt.Println("Configured KUBECONFIG files:")
	fmt.Println(reconciliationTargetKubeconfigs)
	fmt.Println("----------------------------------------")

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	clientConfig := ctrl.GetConfigOrDie()
	clientConfig.QPS = float32(clientQps)
	clientConfig.Burst = clientBurst

	mgr, err := ctrl.NewManager(clientConfig, ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "7f5e28d0.kyma-project.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	workersLogger := ctrl.Log.WithName("workers")
	manifestWorkers := controllers.NewManifestWorkers(&workersLogger)
	context := ctrl.SetupSignalHandler()

	if err = (&controllers.ManifestReconciler{
		Client:                              mgr.GetClient(),
		Scheme:                              mgr.GetScheme(),
		Workers:                             manifestWorkers,
		ReconciliationTargetKubeconfigFiles: reconciliationTargetKubeconfigs,
	}).SetupWithManager(context, mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Manifest")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(context); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func CustomTimeEncoder(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
	//based on time.RFC3339Nano
	enc.AppendString(t.Format("2006-01-02T15:04:05.999Z07:00"))
}
