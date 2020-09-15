/*
Copyright 2020 The Kubernetes Authors.
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

package admissionhandler

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"
)

type (
	parameterSet map[string]struct{}
)

// Has checks if specified paramName present in the paramSet
func (paramSet parameterSet) Has(paramName string) bool {
	_, ok := paramSet[paramName]
	return ok
}

var (
	webhookServerPort = 9443
	server            *http.Server
)

// StartWebhookServer starts the webhook server
func StartWebhookServer(ctx context.Context, cert string, key string, port string) error {
	log := logger.GetLogger(ctx)

	// define http server and server handler
	mux := http.NewServeMux()
	mux.HandleFunc("/validate-storageclass", validationHandler)
	mux.HandleFunc("/validate-registervolume", validationHandler)
	server = &http.Server{
		Addr:    fmt.Sprintf(":%v", port),
		Handler: mux,
	}

	if port == "" {
		port = fmt.Sprintf("%v", webhookServerPort)
	}
	if cert == "" || key == "" {
		log.Debugf("starting web hook server insecurely on port: %v", port)
		if err := server.ListenAndServe(); err != nil {
			log.Errorf("failed to listen and serve webhook server: %v", err)
			return err
		}
	} else {
		certs, err := tls.X509KeyPair([]byte(cert), []byte(key))
		if err != nil {
			log.Errorf("failed to load key pair: %v", err)
			return err
		}
		server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{certs}}

		// start webhook server
		log.Debugf("starting secure webhook server on port: %v", port)
		if err := server.ListenAndServeTLS("", ""); err != nil {
			log.Errorf("failed to listen and serve webhook server: %v", err)
			return err
		}
	}
	return nil
}

// validationHandler is the handler for webhook http multiplexer to help validate resources
// depending on the URL validation of AdmissionReview will be redirected to appropriate validation function
func validationHandler(w http.ResponseWriter, r *http.Request) {
	var body []byte
	ctx, log := logger.GetNewContextWithLogger()
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		log.Error("received empty request body")
		http.Error(w, "received empty request body", http.StatusBadRequest)
		return
	}
	log.Debugf("Received request")
	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Errorf("Content-Type=%s, expect application/json", contentType)
		http.Error(w, "invalid Content-Type, expect `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	var admissionResponse *admissionv1.AdmissionResponse
	ar := admissionv1.AdmissionReview{}
	codecs := serializer.NewCodecFactory(runtime.NewScheme())
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		log.Errorf("Can't decode body: %v", err)
		admissionResponse = &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	} else {
		switch r.URL.Path {
		case "/validate-storageclass":
			log.Debugf("request URL path is /validate-storageclass")
			log.Debugf("admissionReview: %+v", ar)
			admissionResponse = validateStorageClass(ctx, &ar)
			log.Debugf("admissionResponse: %+v", admissionResponse)

		case "/validate-registervolume":
			log.Infof("request URL path is /validate-registervolume")
			log.Infof("admissionReview: %+v", ar)
			admissionResponse = validateRegisterVolume(ctx, &ar)
			log.Infof("admissionResponse: %+v", admissionResponse)
		}
	}

	admissionReview := admissionv1.AdmissionReview{}
	admissionReview.APIVersion = "admission.k8s.io/v1"
	admissionReview.Kind = "AdmissionReview"
	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		if ar.Request != nil {
			admissionReview.Response.UID = ar.Request.UID
		}
	}
	resp, err := json.Marshal(admissionReview)
	if err != nil {
		log.Errorf("Can't encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	log.Debugf("Ready to write response")
	if _, err := w.Write(resp); err != nil {
		log.Errorf("Can't write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}
