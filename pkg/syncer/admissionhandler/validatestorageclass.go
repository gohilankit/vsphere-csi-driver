package admissionhandler

import (
	"context"
	"encoding/json"

	admissionv1 "k8s.io/api/admission/v1"
	stroagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"
)

var (
	unSupportedParameters = parameterSet{
		common.CSIMigrationParams:                   struct{}{},
		common.DiskFormatMigrationParam:             struct{}{},
		common.HostFailuresToTolerateMigrationParam: struct{}{},
		common.ForceProvisioningMigrationParam:      struct{}{},
		common.CacheReservationMigrationParam:       struct{}{},
		common.DiskstripesMigrationParam:            struct{}{},
		common.ObjectspacereservationMigrationParam: struct{}{},
		common.IopslimitMigrationParam:              struct{}{},
	}
)

const (
	volumeExpansionErrorMessage = "AllowVolumeExpansion can not be set to true on the in-tree vSphere StorageClass"
	migrationParamErrorMessage  = "Invalid StorageClass Parameters. Migration specific parameters should not be used in the StorageClass"
)

// validateStorageClass helps validate AdmissionReview requests for StroageClass
func validateStorageClass(ctx context.Context, ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	log := logger.GetLogger(ctx)
	req := ar.Request

	var result *metav1.Status
	allowed := true

	switch req.Kind.Kind {
	case "StorageClass":
		sc := stroagev1.StorageClass{}
		log.Debugf("JSON req.Object.Raw: %v", string(req.Object.Raw))
		if err := json.Unmarshal(req.Object.Raw, &sc); err != nil {
			log.Error("error deserializing storage class")
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}
		log.Infof("Validating StorageClass: %q", sc.Name)
		// AllowVolumeExpansion check for kubernetes.io/vsphere-volume provisioner
		if sc.Provisioner == "kubernetes.io/vsphere-volume" {
			if sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion {
				allowed = false
				result = &metav1.Status{
					Reason: volumeExpansionErrorMessage,
				}
			}
		}
		// Migration parameters check for csi.vsphere.vmware.com provisioner
		if allowed && sc.Provisioner == "csi.vsphere.vmware.com" {
			for param := range sc.Parameters {
				if unSupportedParameters.Has(param) {
					allowed = false
					result = &metav1.Status{
						Reason: migrationParamErrorMessage,
					}
					break
				}
			}
		}
		if allowed {
			log.Infof("Validation of StorageClass: %q Passed", sc.Name)
		} else {
			log.Infof("Validation of StorageClass: %q Failed", sc.Name)
		}
	}
	// return AdmissionResponse result
	return &admissionv1.AdmissionResponse{
		Allowed: allowed,
		Result:  result,
	}
}
