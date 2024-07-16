/*
Copyright 2024 The Kubernetes Authors.

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

package cnsunregistervolume

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	cnstypes "github.com/vmware/govmomi/cns/types"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	apis "sigs.k8s.io/vsphere-csi-driver/v3/pkg/apis/cnsoperator"
	cnsunregistervolumev1alpha1 "sigs.k8s.io/vsphere-csi-driver/v3/pkg/apis/cnsoperator/cnsunregistervolume/v1alpha1"
	volumes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/volume"
	commonconfig "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/config"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
	k8s "sigs.k8s.io/vsphere-csi-driver/v3/pkg/kubernetes"
)

const (
	defaultMaxWorkerThreadsForUnregisterVolume = 40
)

var (
	// backOffDuration is a map of cnsunregistervolume name's to the time after which
	// a request for this instance will be requeued.
	// Initialized to 1 second for new instances and for instances whose latest
	// reconcile operation succeeded.
	// If the reconcile fails, backoff is incremented exponentially.
	backOffDuration         map[string]time.Duration
	backOffDurationMapMutex = sync.Mutex{}
)

// Add creates a new CnsUnregisterVolume Controller and adds it to the Manager,
// ConfigurationInfo and VirtualCenterTypes. The Manager will set fields on
// the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager, clusterFlavor cnstypes.CnsClusterFlavor,
	configInfo *commonconfig.ConfigurationInfo, volumeManager volumes.Manager) error {
	ctx, log := logger.GetNewContextWithLogger()
	if clusterFlavor != cnstypes.CnsClusterFlavorWorkload {
		log.Debug("Not initializing the CnsUnregisterVolume Controller as its a non-WCP CSI deployment")
		return nil
	}

	// Initializes kubernetes client.
	k8sclient, err := k8s.NewClient(ctx)
	if err != nil {
		log.Errorf("Creating Kubernetes client failed. Err: %v", err)
		return err
	}

	// eventBroadcaster broadcasts events on cnsunregistervolume instances to the
	// event sink.
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(
		&typedcorev1.EventSinkImpl{
			Interface: k8sclient.CoreV1().Events(""),
		},
	)
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: apis.GroupName})
	return add(mgr, newReconciler(mgr, configInfo, volumeManager, recorder))
}

// newReconciler returns a new reconcile.Reconciler.
func newReconciler(mgr manager.Manager, configInfo *commonconfig.ConfigurationInfo,
	volumeManager volumes.Manager, recorder record.EventRecorder) reconcile.Reconciler {
	return &ReconcileCnsUnregisterVolume{client: mgr.GetClient(), scheme: mgr.GetScheme(),
		configInfo: configInfo, volumeManager: volumeManager, recorder: recorder}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler.
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	ctx, log := logger.GetNewContextWithLogger()

	maxWorkerThreads := getMaxWorkerThreadsToReconcileCnsUnregisterVolume(ctx)
	// Create a new controller.
	c, err := controller.New("cnsunregistervolume-controller", mgr,
		controller.Options{Reconciler: r, MaxConcurrentReconciles: maxWorkerThreads})
	if err != nil {
		log.Errorf("Failed to create new CnsUnregisterVolume controller with error: %+v", err)
		return err
	}

	backOffDuration = make(map[string]time.Duration)

	// Watch for changes to primary resource CnsUnregisterVolume.
	err = c.Watch(source.Kind(mgr.GetCache(), &cnsunregistervolumev1alpha1.CnsUnregisterVolume{}),
		&handler.EnqueueRequestForObject{})
	if err != nil {
		log.Errorf("Failed to watch for changes to CnsUnregisterVolume resource with error: %+v", err)
		return err
	}
	return nil
}

// blank assignment to verify that ReconcileCnsUnregisterVolume implements
// reconcile.Reconciler.
var _ reconcile.Reconciler = &ReconcileCnsUnregisterVolume{}

// ReconcileCnsUnregisterVolume reconciles a CnsUnregisterVolume object.
type ReconcileCnsUnregisterVolume struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver.
	client        client.Client
	scheme        *runtime.Scheme
	configInfo    *commonconfig.ConfigurationInfo
	volumeManager volumes.Manager
	recorder      record.EventRecorder
}

// Reconcile reads that state of the cluster for a ReconcileCnsUnregisterVolume object
// and makes changes based on the state read and what is in the
// ReconcileCnsUnregisterVolume.Spec.
// Note:
// The Controller will requeue the Request to be processed again if the
// returned error is non-nil or Result.Requeue is true. Otherwise, upon
// completion it will remove the work from the queue.
func (r *ReconcileCnsUnregisterVolume) Reconcile(ctx context.Context,
	request reconcile.Request) (reconcile.Result, error) {
	log := logger.GetLogger(ctx)
	// Fetch the ReconcileCnsUnregisterVolume instance.
	instance := &cnsunregistervolumev1alpha1.CnsUnregisterVolume{}
	err := r.client.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Infof("CnsUnregisterVolume resource not found. Ignoring since object must be deleted.")
			return reconcile.Result{}, nil
		}
		log.Errorf("Error reading the CnsUnregisterVolume with name: %q on namespace: %q. Err: %+v",
			request.Name, request.Namespace, err)
		// Error reading the object - return with err.
		return reconcile.Result{}, err
	}
	// Initialize backOffDuration for the instance, if required.
	backOffDurationMapMutex.Lock()
	var timeout time.Duration
	if _, exists := backOffDuration[instance.Name]; !exists {
		backOffDuration[instance.Name] = time.Second
	}
	timeout = backOffDuration[instance.Name]
	backOffDurationMapMutex.Unlock()

	// If the CnsRegistereVolume instance is already unregistered, remove the
	// instance from the queue.
	if instance.Status.Unregistered {
		backOffDurationMapMutex.Lock()
		delete(backOffDuration, instance.Name)
		backOffDurationMapMutex.Unlock()
		return reconcile.Result{}, nil
	}

	log.Infof("Reconciling CnsUnregisterVolume with instance: %q from namespace: %q. timeout %q seconds",
		instance.Name, request.Namespace, timeout)
	// Validate CnsUnregisterVolume spec to check for valid entries.
	err = validateCnsUnregisterVolumeSpec(ctx, instance)
	if err != nil {
		log.Errorf(err.Error())
		setInstanceError(ctx, r, instance, err.Error())
		return reconcile.Result{RequeueAfter: timeout}, nil
	}

	// 1. Fetch the PV corresponding to the volume and set on it the ReclaimPolicy to Retain.
	// 2. Delete PVC, wait for it to get deleted.
	// 3. Delete PV.
	// 4. Set the CnsUnregisterVolumeStatus.Unregistered to true.
	// 5. Upon PV deletion, CSI metadata syncer should delete the volume from CNS without deleting the underlying FCD

	k8sclient, err := k8s.NewClient(ctx)
	if err != nil {
		log.Errorf("Failed to initialize K8S client when reconciling the CnsUnregisterVolume "+
			"instance: %s on namespace: %s. Error: %+v", instance.Name, instance.Namespace, err)
		setInstanceError(ctx, r, instance, "Failed to init K8S client for volume unregistration")
		return reconcile.Result{RequeueAfter: timeout}, nil
	}

	pvc, err := k8sclient.CoreV1().PersistentVolumeClaims(instance.Namespace).Get(ctx, instance.Spec.PvcName, metav1.GetOptions{})
	if err != nil {
		log.Errorf("Unable to get PVC object %q in namespace %q", instance.Spec.PvcName, instance.Namespace)
		return reconcile.Result{}, err
	}

	pvName := pvc.Spec.VolumeName
	pv, err := k8sclient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		log.Errorf("Unable to get PV %q", pvName)
		return reconcile.Result{}, err
	}

	//Change PV ReclaimPolicy to retain so that underlying FCD doesn't get deleted when deleting Pv,PVC
	pv.Spec.PersistentVolumeReclaimPolicy = v1.PersistentVolumeReclaimRetain
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, updateErr := k8sclient.CoreV1().PersistentVolumes().Update(context.TODO(), pv, metav1.UpdateOptions{})
		return updateErr
	})
	if retryErr != nil {
		log.Errorf("Unable to get PV %q", pvName)
		return reconcile.Result{}, err
	}

	// Delete PVC on source cluster
	err = k8sclient.CoreV1().PersistentVolumeClaims(instance.Namespace).Delete(ctx, instance.Spec.PvcName, *metav1.NewDeleteOptions(0))
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Errorf("Failed to delete PVC %q in namespace %q with error - %s", instance.Spec.PvcName, instance.Namespace, err.Error())
			return reconcile.Result{}, err
		}
	}

	// Delete PV on source cluster.
	// Since reclaimPolicy was set to Retain, we need to explicitly delete it.
	err = k8sclient.CoreV1().PersistentVolumes().Delete(ctx, pvName, *metav1.NewDeleteOptions(0))
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Errorf("Failed to delete PV %q", pvName)
			return reconcile.Result{}, err
		}
	}

	// Update the instance to indicate the volume unregistration is successful.
	msg := fmt.Sprintf("Successfully unregistered the volume on namespace: %s", instance.Namespace)
	err = setInstanceSuccess(ctx, r, instance, msg)
	if err != nil {
		msg := fmt.Sprintf("Failed to update CnsUnregistered instance with error: %+v", err)
		log.Error(msg)
		setInstanceError(ctx, r, instance, msg)
		return reconcile.Result{RequeueAfter: timeout}, nil
	}
	backOffDurationMapMutex.Lock()
	delete(backOffDuration, instance.Name)
	backOffDurationMapMutex.Unlock()
	log.Info(msg)
	return reconcile.Result{}, nil
}

// validateCnsUnregisterVolumeSpec validates the input params of
// CnsUnregisterVolume instance.
func validateCnsUnregisterVolumeSpec(ctx context.Context, instance *cnsunregistervolumev1alpha1.CnsUnregisterVolume) error {
	var msg string
	if instance.Spec.PvcName == "" {
		msg = "Pvc name not specified in the spec"
	}
	if msg != "" {
		return errors.New(msg)
	}
	return nil
}

// setInstanceError sets error and records an event on the CnsUnregisterVolume
// instance.
func setInstanceError(ctx context.Context, r *ReconcileCnsUnregisterVolume,
	instance *cnsunregistervolumev1alpha1.CnsUnregisterVolume, errMsg string) {
	log := logger.GetLogger(ctx)
	instance.Status.Error = errMsg
	err := updateCnsUnregisterVolume(ctx, r.client, instance)
	if err != nil {
		log.Errorf("updateCnsUnregisterVolume failed. err: %v", err)
	}
	recordEvent(ctx, r, instance, v1.EventTypeWarning, errMsg)
}

// setInstanceSuccess sets instance to success and records an event on the
// CnsUnregisterVolume instance.
func setInstanceSuccess(ctx context.Context, r *ReconcileCnsUnregisterVolume,
	instance *cnsunregistervolumev1alpha1.CnsUnregisterVolume, msg string) error {
	instance.Status.Unregistered = true
	instance.Status.Error = ""
	err := updateCnsUnregisterVolume(ctx, r.client, instance)
	if err != nil {
		return err
	}
	recordEvent(ctx, r, instance, v1.EventTypeNormal, msg)
	return nil
}

// recordEvent records the event, sets the backOffDuration for the instance
// appropriately and logs the message.
// backOffDuration is reset to 1 second on success and doubled on failure.
func recordEvent(ctx context.Context, r *ReconcileCnsUnregisterVolume,
	instance *cnsunregistervolumev1alpha1.CnsUnregisterVolume, eventtype string, msg string) {
	log := logger.GetLogger(ctx)
	log.Debugf("Event type is %s", eventtype)
	switch eventtype {
	case v1.EventTypeWarning:
		// Double backOff duration.
		backOffDurationMapMutex.Lock()
		backOffDuration[instance.Name] = backOffDuration[instance.Name] * 2
		r.recorder.Event(instance, v1.EventTypeWarning, "CnsUnregisterVolumeFailed", msg)
		backOffDurationMapMutex.Unlock()
	case v1.EventTypeNormal:
		// Reset backOff duration to one second.
		backOffDurationMapMutex.Lock()
		backOffDuration[instance.Name] = time.Second
		r.recorder.Event(instance, v1.EventTypeNormal, "CnsUnregisterVolumeSucceeded", msg)
		backOffDurationMapMutex.Unlock()
	}
}

// updateCnsUnregisterVolume updates the CnsUnregisterVolume instance in K8S.
func updateCnsUnregisterVolume(ctx context.Context, client client.Client,
	instance *cnsunregistervolumev1alpha1.CnsUnregisterVolume) error {
	log := logger.GetLogger(ctx)
	err := client.Update(ctx, instance)
	if err != nil {
		log.Errorf("Failed to update CnsUnregisterVolume instance: %q on namespace: %q. Error: %+v",
			instance.Name, instance.Namespace, err)
	}
	return err
}
