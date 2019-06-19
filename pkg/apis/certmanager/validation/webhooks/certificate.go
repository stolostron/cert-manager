/*
Copyright 2019 The Jetstack cert-manager contributors.

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

package webhooks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/klog"

	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	authorizationv1 "k8s.io/api/authorization/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	authclientv1beta1 "k8s.io/client-go/kubernetes/typed/authorization/v1beta1"
	restclient "k8s.io/client-go/rest"

	"github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
	"github.com/jetstack/cert-manager/pkg/apis/certmanager/validation"
)

type CertificateAdmissionHook struct {
	authClient *authclientv1beta1.AuthorizationV1beta1Client
}

func (c *CertificateAdmissionHook) Initialize(kubeClientConfig *restclient.Config, stopCh <-chan struct{}) error {
	c.authClient, _ = authclientv1beta1.NewForConfig(kubeClientConfig)
	return nil
}

func (c *CertificateAdmissionHook) ValidatingResource() (plural schema.GroupVersionResource, singular string) {
	gv := v1alpha1.SchemeGroupVersion
	gv.Group = "admission." + gv.Group
	// override version to be the version of the admissionresponse resource
	gv.Version = "v1beta1"
	return gv.WithResource("certificates"), "certificate"
}

func (c *CertificateAdmissionHook) Validate(admissionSpec *admissionv1beta1.AdmissionRequest) *admissionv1beta1.AdmissionResponse {
	status := &admissionv1beta1.AdmissionResponse{}

	obj := &v1alpha1.Certificate{}
	err := json.Unmarshal(admissionSpec.Object.Raw, obj)
	if err != nil {
		status.Allowed = false
		status.Result = &metav1.Status{
			Status: metav1.StatusFailure, Code: http.StatusBadRequest, Reason: metav1.StatusReasonBadRequest,
			Message: err.Error(),
		}
		return status
	}
	c.test(obj, admissionSpec)
	authorized := allowed(admissionSpec, obj)
	if !authorized {
		timeStamp := time.Now()
		klog.Errorf("[UNAUTHORIZED] %s\nUser: %s (not a cluster administrator) tried to create Certificate %s using a ClusterIssuer.", timeStamp.String(), admissionSpec.UserInfo.Username, obj.ObjectMeta.Name)
		message := fmt.Sprintf("User is unauthorized to create the Certificate %s using the ClusterIssuer %s.", obj.ObjectMeta.Name, obj.Spec.IssuerRef.Name)
		status.Allowed = false
		status.Result = &metav1.Status{
			Status: metav1.StatusFailure, Code: http.StatusForbidden, Reason: metav1.StatusReasonForbidden,
			Message: message,
		}
		return status
	}

	err = validation.ValidateCertificate(obj).ToAggregate()
	if err != nil {
		status.Allowed = false
		status.Result = &metav1.Status{
			Status: metav1.StatusFailure, Code: http.StatusNotAcceptable, Reason: metav1.StatusReasonNotAcceptable,
			Message: err.Error(),
		}
		return status
	}

	status.Allowed = true

	return status
}
func (c *CertificateAdmissionHook) test(obj *v1alpha1.Certificate, admissionSpec *admissionv1beta1.AdmissionRequest) {
	klog.Infof("AUTH CLIENT: %v", *c.authClient)
	klog.Infof("AUTH CLIENT subject access review: %v", c.authClient.SubjectAccessReviews())
	klog.Infof("Rest client: %v", c.authClient.RESTClient())
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: obj.ObjectMeta.Namespace,
				Verb:      "use",
				Group:     "certmanager.k8s.io",
				Resource:  "ClusterIssuer",
			},
			User:   admissionSpec.UserInfo.Username,
			Groups: admissionSpec.UserInfo.Groups,
			UID:    admissionSpec.UserInfo.UID,
		},
	}
	client := c.authClient.SubjectAccessReviews()
	res, err := client.Create(sar)
	if err != nil {
		klog.Infof("Error occurred using subject access review client to create %v", sar)
	}
	klog.Infof("The res %v", res)
	//client := c.authClient.RESTClient()
	//klog.Infof("BASE: %v\nVERSIONPATH: %v\nCONFIG: %v\nSERIALIZERS: %v\nCREATEBACKOFF: %v\nTHROTTLE: %v\nCLIENT: %v\n", client.base, client.versionedAPIPath, client.contentConfig, client.serializers, client.createBackoffMgr, client.Throttle, client.Client)
}
func allowed(request *admissionv1beta1.AdmissionRequest, crt *v1alpha1.Certificate) bool {
	issuerKind := crt.Spec.IssuerRef.Kind
	username := request.UserInfo.Username
	uid, err := url.Parse(username)
	if err != nil {
		klog.Infof("An error occurred parsing the username %s to a url: %s", username, err.Error())
		return false
	}
	if issuerKind == "ClusterIssuer" {
		klog.Info("ClusterIssuer")
		if uid.Fragment != "" {
			// Check if this user is the default cluster admin
			if admin, ok := os.LookupEnv("DEFAULT_ADMIN"); ok {
				if oidcUrl, exists := os.LookupEnv("OIDC_URL"); exists {
					admin = strings.TrimSpace(admin)
					oidcUrl = strings.TrimSpace(oidcUrl)
					oidcUrl = fmt.Sprintf("%s#%s", oidcUrl, admin)
					if uid.Fragment == admin && uid.String() == oidcUrl {
						return true
					}
				}
			}
		}
		// If the user is in systems:master group (ClusterAdmin)
		groups := request.UserInfo.Groups
		for _, group := range groups {
			if group == "system:serviceaccounts:cert-manager" || group == "system:masters" {
				return true
			}
		}
		return false
	}
	return true
}
