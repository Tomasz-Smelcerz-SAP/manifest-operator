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

package controllers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/kyma-project/manifest-operator/api/api/v1alpha1"
	"github.com/kyma-project/manifest-operator/operator/pkg/manifest"
	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/cli"
	"k8s.io/apimachinery/pkg/runtime"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Mode int

const (
	CreateMode Mode = iota
	DeletionMode
)

type DeployInfo struct {
	manifestKey client.ObjectKey
	*v1alpha1.ChartInfo
	Mode
}

// ManifestReconciler reconciles a Manifest object
type ManifestReconciler struct {
	client.Client
	Scheme                              *runtime.Scheme
	RestConfig                          *rest.Config
	RestMapper                          *restmapper.DeferredDiscoveryRESTMapper
	Workers                             *ManifestWorkers
	DeployChan                          chan DeployInfo
	ResponseChan                        chan error
	ReconciliationTargetKubeconfigFiles []string //Kubeconfigs for the target clusters to install/delete Helm releases.
}

//+kubebuilder:rbac:groups=component.kyma-project.io,resources=manifests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=component.kyma-project.io,resources=manifests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=component.kyma-project.io,resources=manifests/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ManifestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(req.NamespacedName.String())
	logger.Info("Reconciliation loop starting for", "resource", req.NamespacedName.String())

	// get manifest object
	manifestObj := v1alpha1.Manifest{}
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name, Namespace: req.Namespace}, &manifestObj); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		logger.Info(req.NamespacedName.String() + " got deleted!")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	manifestObj = *manifestObj.DeepCopy()

	// check if deletionTimestamp is set, retry until it gets fully deleted
	if !manifestObj.DeletionTimestamp.IsZero() && manifestObj.Status.State != v1alpha1.ManifestStateDeleting {
		// if the status is not yet set to deleting, also update the status
		return ctrl.Result{}, r.updateManifestStatus(ctx, &manifestObj, v1alpha1.ManifestStateDeleting, "deletion timestamp set")
	}

	// check finalizer
	if !controllerutil.ContainsFinalizer(&manifestObj, manifestFinalizer) {
		controllerutil.AddFinalizer(&manifestObj, manifestFinalizer)
		return ctrl.Result{}, r.updateManifest(ctx, &manifestObj)
	}

	// state handling
	switch manifestObj.Status.State {
	case "":
		return ctrl.Result{}, r.HandleInitialState(ctx, &logger, &manifestObj)
	case v1alpha1.ManifestStateProcessing:
		return ctrl.Result{}, r.HandleProcessingState(ctx, &logger, &manifestObj)
	case v1alpha1.ManifestStateDeleting:
		return ctrl.Result{}, r.HandleDeletingState(ctx, &logger, &manifestObj)
	case v1alpha1.ManifestStateError:
		return ctrl.Result{}, r.HandleErrorState(ctx, &logger, &manifestObj)
	case v1alpha1.ManifestStateReady:
		return ctrl.Result{}, r.HandleReadyState(ctx, &logger, &manifestObj)
	}

	// should not be reconciled again
	return ctrl.Result{}, nil
}

func (r *ManifestReconciler) HandleInitialState(ctx context.Context, _ *logr.Logger, manifestObj *v1alpha1.Manifest) error {
	return r.updateManifestStatus(ctx, manifestObj, v1alpha1.ManifestStateProcessing, "initial state")
}

func (r *ManifestReconciler) HandleProcessingState(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest) error {
	return r.jobAllocator(ctx, logger, manifestObj, CreateMode)
}

func (r *ManifestReconciler) HandleDeletingState(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest) error {
	return r.jobAllocator(ctx, logger, manifestObj, DeletionMode)
}

func (r *ManifestReconciler) jobAllocator(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest, mode Mode) error {
	chartCount := len(manifestObj.Spec.Charts)

	objKey := client.ObjectKey{Namespace: manifestObj.Namespace, Name: manifestObj.Name}
	go r.ResponseHandlerFunc(ctx, chartCount, logger, objKey)

	// send job to workers
	for _, chart := range manifestObj.Spec.Charts {
		r.DeployChan <- DeployInfo{objKey, &chart, mode}
	}

	return nil
}

func (r *ManifestReconciler) HandleErrorState(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest) error {
	if manifestObj.Status.ObservedGeneration == manifestObj.Generation {
		logger.Info("skipping reconciliation for " + manifestObj.Name + ", already reconciled!")
		return nil
	}
	return r.updateManifestStatus(ctx, manifestObj, v1alpha1.ManifestStateProcessing, "observed generation change")
}

func (r *ManifestReconciler) HandleReadyState(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest) error {
	if manifestObj.Status.ObservedGeneration == manifestObj.Generation {
		logger.Info("skipping reconciliation for " + manifestObj.Name + ", already reconciled!")
		return nil
	}
	return r.updateManifestStatus(ctx, manifestObj, v1alpha1.ManifestStateProcessing, "observed generation change")
}

func (r *ManifestReconciler) updateManifest(ctx context.Context, manifestObj *v1alpha1.Manifest) error {
	return r.Update(ctx, manifestObj)
}

func (r *ManifestReconciler) updateManifestStatus(ctx context.Context, manifestObj *v1alpha1.Manifest, state v1alpha1.ManifestState, message string) error {
	manifestObj.Status.State = state
	switch state {
	case v1alpha1.ManifestStateReady:
		addReadyConditionForObjects(manifestObj, []string{v1alpha1.ManifestKind}, v1alpha1.ConditionStatusTrue, message)
	case "":
		addReadyConditionForObjects(manifestObj, []string{v1alpha1.ManifestKind}, v1alpha1.ConditionStatusUnknown, message)
	default:
		addReadyConditionForObjects(manifestObj, []string{v1alpha1.ManifestKind}, v1alpha1.ConditionStatusFalse, message)
	}
	return r.Status().Update(ctx, manifestObj.SetObservedGeneration())
}

func (r *ManifestReconciler) HandleCharts(deployInfo DeployInfo, logger logr.Logger) error {
	var (
		args = map[string]string{
			// check --set flags parameter from manifest
			"set": "",
			// comma seperated values of manifest command line flags
			"flags": deployInfo.ClientConfig,
		}
		repoName    = deployInfo.RepoName
		url         = deployInfo.Url
		chartName   = deployInfo.ChartName
		releaseName = deployInfo.ReleaseName
	)

	// evaluate create or delete chart
	create := deployInfo.Mode == CreateMode

	// TODO: implement better settings handling
	targetNamespace, objectNumber, err := splitName(deployInfo.manifestKey.Name)

	clusterIndex := objectNumber % len(r.ReconciliationTargetKubeconfigFiles)
	// TODO: Fetch it's contents from a secret in the current cluster instead of pointing to a local file
	kubeconfigFile := r.ReconciliationTargetKubeconfigFiles[clusterIndex]

	restConfig, err := clientcmd.BuildConfigFromFlags(
		"", kubeconfigFile,
	)
	if err != nil {
		return fmt.Errorf(
			"unable to load kubeconfig from %s: %v",
			kubeconfigFile, err,
		)
	}

	indexedReleaseName := fmt.Sprintf("%s-%02d", releaseName, objectNumber)
	// Enforcing target namespace
	args["flags"] = args["flags"] + ",Namespace=" + targetNamespace + ",CreateNamespace=true"

	logger.Info(fmt.Sprintf("Install flags: %s", args["flags"]))
	logger.Info(fmt.Sprintf("Kubeconfig file: %s", kubeconfigFile))

	manifestOperations := manifest.NewOperations(logger, restConfig, cli.New())
	if create {
		if err := manifestOperations.Install("", indexedReleaseName, fmt.Sprintf("%s/%s", repoName, chartName), repoName, url, args); err != nil {
			return err
		}
	} else {
		if err := manifestOperations.Uninstall("", fmt.Sprintf("%s/%s", repoName, chartName), indexedReleaseName, args); err != nil {
			return err
		}
	}

	return nil
}

func (r *ManifestReconciler) ResponseHandlerFunc(ctx context.Context, chartCount int, logger *logr.Logger, namespacedName client.ObjectKey) {
	endState := v1alpha1.ManifestStateReady
	var err error
	for a := 1; a <= chartCount; a++ {
		err = <-r.ResponseChan
		if err != nil {
			logger.Error(err, "chart installation failure!!!")
			endState = v1alpha1.ManifestStateError
			break
		}
	}

	latestManifestObj := &v1alpha1.Manifest{}
	if err := r.Get(ctx, namespacedName, latestManifestObj); err != nil {
		logger.Error(err, "error while locating", "resource", namespacedName)
		return
	}

	switch endState {
	case v1alpha1.ManifestStateReady:
		// handle deletion if no previous error occurred
		if !latestManifestObj.DeletionTimestamp.IsZero() {

			// remove finalizer
			controllerutil.RemoveFinalizer(latestManifestObj, manifestFinalizer)
			if err = r.updateManifest(ctx, latestManifestObj); err != nil {
				// finalizer removal failure
				logger.Error(err, "unexpected error while deleting", "resource", namespacedName)
				endState = v1alpha1.ManifestStateError
			} else {
				// finalizer successfully removed
				return
			}

		}
	default:
	}

	// update status for non-deletion scenarios
	if err := r.updateManifestStatus(ctx, latestManifestObj, endState, "manifest charts installed!"); err != nil {
		logger.Error(err, "error updating status", "resource", namespacedName)
	}
	return
}

// SetupWithManager sets up the controller with the Manager.
func (r *ManifestReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	r.DeployChan = make(chan DeployInfo, DefaultWorkersCount)
	r.ResponseChan = make(chan error, DefaultWorkersCount)

	r.Workers.StartWorkers(ctx, r.DeployChan, r.ResponseChan, r.HandleCharts)
	r.RestConfig = mgr.GetConfig()

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Manifest{}).
		Complete(r)
}

//Expects objName in the format: <name>-<number>
//Returns <name>, <number>, <error>
func splitName(objName string) (name string, index int, err error) {
	parts := strings.SplitN(objName, "-", 3)
	if len(parts) != 3 {
		return "", 0, errors.Errorf("name not in <name>-<number>-<number> format: \"%s\"", objName)
	}

	if len(parts[0]) < 2 {
		return "", 0, errors.Errorf("name prefix too short: \"%s\"", parts[0])
	}

	if len(parts[2]) < 1 {
		return "", 0, errors.New("index is empty")
	}

	index, err = strconv.Atoi(parts[2])
	if err != nil {
		return "", 0, errors.Wrap(err, "error converting name suffix to an int")
	}

	if index < 0 || index > 9999999 {
		return "", 0, errors.Errorf("Invalid index range: %d", index)
	}

	return parts[0] + "-" + parts[1], index, nil
}
