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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/kyma-project/manifest-operator/api/api/v1alpha1"
	"github.com/kyma-project/manifest-operator/operator/pkg/manifest"
	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/cli"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Mode int

const (
	CreateMode Mode = iota
	DeletionMode
)

const (
	DefaultWorkersCount               = 4
	WaitTimeout         time.Duration = 5 * time.Minute
)

type RequestErrChan chan *RequestError

type DeployInfo struct {
	v1alpha1.ChartInfo
	manifestKey client.ObjectKey
	Mode
	client.ObjectKey
	RequestErrChan
}

// ManifestReconciler reconciles a Manifest object
type ManifestReconciler struct {
	client.Client
	Scheme                              *runtime.Scheme
	RestConfig                          *rest.Config
	RestMapper                          *restmapper.DeferredDiscoveryRESTMapper
	DeployChan                          chan DeployInfo
	ReconciliationTargetKubeconfigFiles []string //Kubeconfigs for the target clusters to install/delete Helm releases.
	Workers                             *ManifestWorkers
}

type RequestError struct {
	ResNamespacedName client.ObjectKey
	Err               error
}

func (r *RequestError) Error() string {
	return r.Err.Error()
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
		return ctrl.Result{RequeueAfter: time.Second * 60}, r.HandleProcessingState(ctx, &logger, &manifestObj)
	case v1alpha1.ManifestStateDeleting:
		return ctrl.Result{}, r.HandleDeletingState(ctx, &logger, &manifestObj)
	case v1alpha1.ManifestStateError:
		return ctrl.Result{RequeueAfter: time.Second * 600}, r.HandleErrorState(ctx, &logger, &manifestObj)
	case v1alpha1.ManifestStateReady:
		return ctrl.Result{RequeueAfter: time.Second * 3600}, r.HandleReadyState(ctx, &logger, &manifestObj)
	}

	// should not be reconciled again
	return ctrl.Result{}, nil
}

func (r *ManifestReconciler) HandleInitialState(ctx context.Context, _ *logr.Logger, manifestObj *v1alpha1.Manifest) error {
	return r.updateManifestStatus(ctx, manifestObj, v1alpha1.ManifestStateProcessing, "initial state")
}

func (r *ManifestReconciler) HandleProcessingState(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest) error {

	//Because deletion condition is handled further down in the jobAllocator call chain, this redundant check here is necessary
	if manifestObj.DeletionTimestamp.IsZero() {
		//Check here if the target object is ready
		//If so, set the Manifest CR status to Ready and exit from here
		targetObjectReady := r.checkTargetObject(ctx, logger, manifestObj)
		if targetObjectReady {
			if manifestObj.Status.State != v1alpha1.ManifestStateReady {
				if err := r.updateManifestStatus(ctx, manifestObj, v1alpha1.ManifestStateReady, "target object is ready"); err != nil {
					objKey := client.ObjectKey{Namespace: manifestObj.Namespace, Name: manifestObj.Name}
					logger.Error(err, "error updating status", "resource", objKey)
				}
			}
			return nil
		}
	}
	return r.jobAllocator(ctx, logger, manifestObj, CreateMode)
}

func (r *ManifestReconciler) checkTargetObject(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest) bool {

	targetNamespace, objectSuffix, clusterIndexBase, err := loadTestingNameParsing(manifestObj.Name)
	if err != nil {
		logger.Error(err, "error checking target object state for manifest", "manifest", manifestObj.Namespace+"/"+manifestObj.Name)
		return false
	}

	clusterIndex := clusterIndexBase % len(r.ReconciliationTargetKubeconfigFiles)

	//TODO: Ensure it's consistent with what Helm is actually generating.
	targetName := fmt.Sprintf("%s-%s-%s", manifestObj.Spec.Charts[0].ReleaseName, objectSuffix, manifestObj.Spec.Charts[0].ChartName)

	targetObjKey := client.ObjectKey{Namespace: targetNamespace, Name: targetName}
	targetObj := unstructured.Unstructured{}

	targetObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "kyma.kyma-project.io", Version: "v1alpha1", Kind: "LongOperation"})
	targetObj.SetName(targetName)
	targetObj.SetNamespace(targetNamespace)

	kubeconfigFile := r.ReconciliationTargetKubeconfigFiles[clusterIndex]

	// TODO: This function is best invoked with the target cluster client injected
	restConfig, err := clientcmd.BuildConfigFromFlags(
		"", kubeconfigFile,
	)
	if err != nil {
		logger.Error(fmt.Errorf(
			"unable to load kubeconfig from %s: %v",
			kubeconfigFile, err,
		), "error checking target object state", "resource", targetObjKey)
		return false
	}

	//TODO: Another client!
	kc, err := client.New(restConfig, client.Options{})
	if err != nil {
		logger.Error(err, "error checking target object state", "resource", targetObjKey)
		return false
	}

	err = kc.Get(ctx, targetObjKey, &targetObj)
	if err != nil {
		logger.Error(err, "error checking target object state", "resource", targetObjKey)
		return false
	}

	logger.Info("target object exists", "resource", targetObjKey)
	status, statusExists, err := unstructured.NestedString(targetObj.Object, "status", "state")
	if err != nil {
		logger.Error(err, "error checking target object state", "resource", targetObjKey)
		return false
	}

	if statusExists {
		logger.Info("target object status is:"+status, "resource", targetObjKey)
		return status == "Ready"
	}

	logger.Info("target object has no status", "resource", targetObjKey)
	return false
}

func (r *ManifestReconciler) HandleDeletingState(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest) error {
	return r.jobAllocator(ctx, logger, manifestObj, DeletionMode)
}

func (r *ManifestReconciler) jobAllocator(ctx context.Context, logger *logr.Logger, manifestObj *v1alpha1.Manifest, mode Mode) error {
	namespacedName := client.ObjectKeyFromObject(manifestObj)
	responseChan := make(RequestErrChan)
	chartCount := len(manifestObj.Spec.Charts)

	go r.ResponseHandlerFunc(ctx, logger, chartCount, responseChan, namespacedName)

	// send job to workers
	for _, chart := range manifestObj.Spec.Charts {
		r.DeployChan <- DeployInfo{
			ChartInfo:      chart,
			Mode:           mode,
			ObjectKey:      namespacedName,
			RequestErrChan: responseChan,
		}
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

func (r *ManifestReconciler) HandleCharts(deployInfo DeployInfo, logger *logr.Logger) *RequestError {
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

		err error
	)

	wErr := wrapErr(deployInfo.ObjectKey)

	// evaluate create or delete chart
	create := deployInfo.Mode == CreateMode

	targetNamespace, objectSuffix, clusterIndexBase, err := loadTestingNameParsing(deployInfo.manifestKey.Name)
	if err != nil {
		return wErr(err)
	}

	clusterIndex := clusterIndexBase % len(r.ReconciliationTargetKubeconfigFiles)
	// TODO: Fetch it's contents from a secret in the current cluster instead of pointing to a local file
	kubeconfigFile := r.ReconciliationTargetKubeconfigFiles[clusterIndex]

	restConfig, err := clientcmd.BuildConfigFromFlags(
		"", kubeconfigFile,
	)
	if err != nil {
		return wErr(fmt.Errorf(
			"unable to load kubeconfig from %s: %v",
			kubeconfigFile, err,
		))
	}

	indexedReleaseName := fmt.Sprintf("%s-%s", releaseName, objectSuffix)

	// Enforcing target namespace
	args["flags"] = args["flags"] + ",Namespace=" + targetNamespace + ",CreateNamespace=true"

	logger.Info(fmt.Sprintf("Install flags: %s", args["flags"]))
	logger.Info(fmt.Sprintf("Kubeconfig file: %s", kubeconfigFile))

	manifestOperations := manifest.NewOperations(logger, restConfig, cli.New(), WaitTimeout)

	if create {
		err = manifestOperations.Install("", indexedReleaseName, fmt.Sprintf("%s/%s", repoName, chartName), repoName, url, args)
	} else {
		err = manifestOperations.Uninstall("", fmt.Sprintf("%s/%s", repoName, chartName), indexedReleaseName, args)
	}

	return &RequestError{
		ResNamespacedName: deployInfo.ObjectKey,
		Err:               err,
	}
}

func (r *ManifestReconciler) ResponseHandlerFunc(ctx context.Context, logger *logr.Logger, chartCount int, responseChan RequestErrChan, namespacedName client.ObjectKey) {
	errorState := false
	for a := 1; a <= chartCount; a++ {
		select {
		case <-ctx.Done():
			logger.Error(ctx.Err(), fmt.Sprintf("context closed, error occured while handling response for %s", namespacedName.String()))
			return
		case response := <-responseChan:
			if response.Err != nil {
				logger.Error(response.Err, fmt.Sprintf("chart installation failure for %s!!!", response.ResNamespacedName.String()))
				errorState = true
				break
			}
		}
	}

	latestManifestObj := &v1alpha1.Manifest{}
	if err := r.Get(ctx, namespacedName, latestManifestObj); err != nil {
		logger.Error(err, "error while locating", "resource", namespacedName)
		return
	}

	// handle deletion if no previous error occurred
	if !errorState && !latestManifestObj.DeletionTimestamp.IsZero() {
		// remove finalizer
		controllerutil.RemoveFinalizer(latestManifestObj, manifestFinalizer)
		if err := r.updateManifest(ctx, latestManifestObj); err != nil {
			// finalizer removal failure
			logger.Error(err, "unexpected error while deleting", "resource", namespacedName)
			errorState = true
		} else {
			// finalizer successfully removed
			return
		}
	}

	var endState v1alpha1.ManifestState
	if errorState {
		endState = v1alpha1.ManifestStateError
	} else {
		endState = v1alpha1.ManifestStateReady
	}

	// update status for non-deletion scenarios
	if err := r.updateManifestStatus(ctx, latestManifestObj, endState, "manifest charts installed!"); err != nil {
		logger.Error(err, "error updating status", "resource", namespacedName)
	}
	return
}

// SetupWithManager sets up the controller with the Manager.
func (r *ManifestReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	r.DeployChan = make(chan DeployInfo)
	r.Workers.StartWorkers(ctx, r.DeployChan, r.HandleCharts)

	// default config from kubebuilder
	r.RestConfig = mgr.GetConfig()

	// TODO: Uncomment below lines to get your custom kubeconfig
	//var err error
	//r.RestConfig, err = util.GetConfig("")
	//if err != nil {
	//	return err
	//}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Manifest{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: DefaultWorkersCount,
		}).
		Complete(r)
}

//Expects objName in the format: <name>-<number1>-<number2>
//Returns "<name>-<number1>", "<number2>", <error>
func splitName(objName string) (name string, index string, err error) {
	parts := strings.SplitN(objName, "-", 3)
	if len(parts) != 3 {
		return "", "", errors.Errorf("name not in <name>-<number>-<number> format: \"%s\"", objName)
	}

	if len(parts[0]) < 2 {
		return "", "", errors.Errorf("name prefix too short: \"%s\"", parts[0])
	}

	if len(parts[2]) < 1 {
		return "", "", errors.New("suffix is empty")
	}

	return parts[0] + "-" + parts[1], parts[2], nil
}

func parseNumber(value string) (int, error) {
	res, err := strconv.Atoi(value)

	if err != nil {
		return 0, errors.Wrap(err, "error converting value to an int")
	}

	if res < 0 || res > 9999999 {
		return 0, errors.Errorf("Invalid range for a value: %s", value)
	}

	return res, nil
}

func wrapErr(namespacedName client.ObjectKey) func(error) *RequestError {
	return func(err error) *RequestError {
		return &RequestError{
			ResNamespacedName: namespacedName,
			Err:               err,
		}
	}
}

func loadTestingNameParsing(manifestCRName string) (targetNamespace string, objectSuffix string, clusterIndex int, err error) {

	//example: "manifest11-for-ltkyma-12-13" => targetNamespace: "kyma-12-13", objectSuffix: "11", clusterIndex: 13
	r, err := regexp.Compile("([^0-9]+)([0-9]+)([^0-9]+lt)(kyma[-][0-9]+[-])([0-9]+)$")
	if err != nil {
		return
	}

	matches := r.FindAllStringSubmatch(manifestCRName, -1)

	objectSuffix = matches[0][2]
	clusterIndexValue := matches[0][5]
	targetNamespace = matches[0][4] + clusterIndexValue
	clusterIndex, err = parseNumber(objectSuffix)
	return
}
