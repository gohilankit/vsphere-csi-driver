package admissionhandler

import (
	"context"
	"encoding/json"
	"fmt"

	admissionv1 "k8s.io/api/admission/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"
	k8s "sigs.k8s.io/vsphere-csi-driver/pkg/kubernetes"

	cnsv1alpha1 "sigs.k8s.io/vsphere-csi-driver/pkg/apis/cnsoperator/cnsregistervolume/v1alpha1"
)

func validateRegisterVolume(ctx context.Context, ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	log := logger.GetLogger(ctx)
	req := ar.Request

	var result *metav1.Status
	allowed := true

	switch req.Kind.Kind {
	case "CnsRegisterVolume":
		log.Infof("Reached the handler")
		cns := cnsv1alpha1.CnsRegisterVolume{}
		if err := json.Unmarshal(req.Object.Raw, &cns); err != nil {
			log.Error("error deserializing CNSRegisterVolume object")
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}

		log.Infof("Validating CNSRegisterVolume: %q", cns.Name)
		k8sClient, err := k8s.NewClient(ctx)
		if err != nil {
			log.Errorf("Failed to initialize K8S client")
		}

		pvList, err := k8sClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Errorf("Failed to get persistent volume list from API server with error: %+v", err)
		}
		for _, pv := range pvList.Items {
			// Check if there exists a PV with vsphere volume attached and bound to a different PVC
			// than one passed in the spec
			if pv.Spec.CSI.VolumeHandle == cns.Spec.VolumeID && pv.Status.Phase == v1.VolumeBound &&
				(pv.Spec.ClaimRef.Name != cns.Spec.PvcName || pv.Spec.ClaimRef.Namespace != cns.Namespace) {
				var msg string
				msg = fmt.Sprintf("VolumeID: %s is already attached to PV: %s and bound to PVC: %s in namespace: %s",
					cns.Spec.VolumeID, pv.ObjectMeta.Name, pv.Spec.ClaimRef.Name, pv.Spec.ClaimRef.Namespace)

				allowed = false
				result = &metav1.Status{
					Message: msg,
					Reason:  metav1.StatusReason(msg),
				}
			}
		}

	}

	// return AdmissionResponse result
	return &admissionv1.AdmissionResponse{
		Allowed: allowed,
		Result:  result,
	}
}
