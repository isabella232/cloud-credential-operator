/*
Copyright 2021 The OpenShift Authors.

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

package openstack

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	operatorv1 "github.com/openshift/api/operator/v1"

	"github.com/openshift/cloud-credential-operator/pkg/openstack"
	"github.com/openshift/cloud-credential-operator/pkg/operator/constants"
	"github.com/openshift/cloud-credential-operator/pkg/operator/metrics"
	"github.com/openshift/cloud-credential-operator/pkg/operator/secretannotator/status"
	secretutils "github.com/openshift/cloud-credential-operator/pkg/operator/secretannotator/utils"
	statuscontroller "github.com/openshift/cloud-credential-operator/pkg/operator/status"
	"github.com/openshift/cloud-credential-operator/pkg/operator/utils"
)

func NewReconciler(mgr manager.Manager) reconcile.Reconciler {
	c := mgr.GetClient()
	r := &ReconcileCloudCredSecret{
		Client: c,
		Logger: log.WithField("controller", constants.SecretAnnotatorControllerName),
	}

	s := status.NewSecretStatusHandler(c)
	statuscontroller.AddHandler(constants.SecretAnnotatorControllerName, s)

	return r
}

func cloudCredSecretObjectCheck(secret metav1.Object) bool {
	return secret.GetNamespace() == constants.CloudCredSecretNamespace && secret.GetName() == constants.OpenStackCloudCredsSecretName
}

func Add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(constants.SecretAnnotatorControllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to cluster cloud secret
	p := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return cloudCredSecretObjectCheck(e.MetaNew)
		},
		CreateFunc: func(e event.CreateEvent) bool {
			return cloudCredSecretObjectCheck(e.Meta)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return cloudCredSecretObjectCheck(e.Meta)
		},
	}
	err = c.Watch(&source.Kind{Type: &corev1.Secret{}}, &handler.EnqueueRequestForObject{}, p)
	if err != nil {
		return err
	}

	err = secretutils.WatchCCOConfig(c, types.NamespacedName{
		Namespace: constants.CloudCredSecretNamespace,
		Name:      constants.OpenStackCloudCredsSecretName,
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileCloudCredSecret{}

type ReconcileCloudCredSecret struct {
	client.Client
	Logger log.FieldLogger
}

// Reconcile will typically annotate the cloud cred secret to indicate the capabilities of the cloud credentials:
// 1) 'mint' for indicating that the creds can be used to create new sub-creds
// 2) 'passthrough' for indicating that the creds are capable enough to potentially be used as-is
// 3) 'insufficient' for indicating that the creds are not usable for the cluster
// In the event that the operator config resource has specified a mode to operate under (mint/passthrough)
// then skip trying to determine the capabilities, and just annotate the secret.
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;update
func (r *ReconcileCloudCredSecret) Reconcile(request reconcile.Request) (returnResult reconcile.Result, returnErr error) {
	start := time.Now()

	defer func() {
		dur := time.Since(start)
		metrics.MetricControllerReconcileTime.WithLabelValues(constants.SecretAnnotatorControllerName).Observe(dur.Seconds())
	}()

	mode, conflict, err := utils.GetOperatorConfiguration(r.Client, r.Logger)
	if err != nil {
		r.Logger.WithError(err).Error("error checking operator configuration")
		return reconcile.Result{}, err
	}
	if !utils.IsValidMode(mode) {
		r.Logger.Errorf("invalid mode of %s set", mode)
		return reconcile.Result{}, fmt.Errorf("invalide mode of %s set", mode)
	}
	if conflict {
		r.Logger.Error("configuration conflict between legacy configmap and operator config")
		return reconcile.Result{}, fmt.Errorf("configuration conflict")
	}
	if mode == operatorv1.CloudCredentialsModeManual {
		r.Logger.Info("operator in disabled / manual mode")
		return reconcile.Result{}, err
	}
	switch mode {
	case operatorv1.CloudCredentialsModeDefault,
		operatorv1.CloudCredentialsModePassthrough:
	default:
		const msg = "OpenStack only supports Passthrough mode"
		r.Logger.Error(msg)
		return reconcile.Result{}, fmt.Errorf(msg)
	}

	secret := &corev1.Secret{}
	err = r.Get(context.Background(), request.NamespacedName, secret)
	if err != nil {
		r.Logger.Debugf("secret not found: %v", err)
		return reconcile.Result{}, err
	}

	clouds, err := openstack.GetRootCloudCredentialsSecretData(secret, r.Logger)
	if err != nil {
		r.Logger.WithError(err).Error("errored getting clouds.yaml from secret")
		return reconcile.Result{}, err
	}

	clouds, cloudsUpdated, err := fixInvalidCACertFile(clouds)
	if err != nil {
		r.Logger.WithError(err).Error("errored checking clouds.yaml")
		return reconcile.Result{}, err
	}

	if cloudsUpdated {
		openstack.SetRootCloudCredentialsSecretData(secret, clouds)
		err := r.Update(context.TODO(), secret)
		if err != nil {
			r.Logger.WithError(err).Error("error writing updated root secret")
		}
		return reconcile.Result{}, err
	}

	if mode != operatorv1.CloudCredentialsModeDefault {
		annotation, err := utils.ModeToAnnotation(mode)
		if err != nil {
			r.Logger.WithError(err).Error("failed to convert operator mode to annotation")
			return reconcile.Result{}, err
		}
		err = r.updateSecretAnnotations(secret, annotation)
		if err != nil {
			r.Logger.WithError(err).Error("errored while annotating secret")
		}
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// fixInvalidCACertFile ensures that clouds.yaml has the right CACertFile value
// For more information: https://bugzilla.redhat.com/show_bug.cgi?id=1940142
//
// The installer no longer generates an invalid cacert as of 4.7, and this
// method will fix any invalid secret present during 4.8. We can therefore
// remove this code in 4.9.
func fixInvalidCACertFile(content string) (string, bool, error) {
	clouds := make(map[string]interface{})

	err := yaml.Unmarshal([]byte(content), &clouds)
	if err != nil {
		return "", false, err
	}

	var updatePath func(y map[string]interface{}, path ...string) bool
	updatePath = func(y map[string]interface{}, path ...string) bool {
		field, ok := y[path[0]]
		if !ok {
			// clouds.yaml doesn't contain this path. Nothing to update
			return false
		}

		if len(path) == 1 {
			y[path[0]] = openstack.CACertFile
			return true
		}

		fieldMap, ok := field.(map[string]interface{})
		if !ok {
			// clouds.yaml with this non-final path doesn't contain more children. Nothing to update.
			return false
		}

		// Descend a level of the struct
		return updatePath(fieldMap, path[1:]...)
	}

	// If clouds/openstack/cacert exists in clouds.yaml, set it to caCertFile
	updated := updatePath(clouds, "clouds", openstack.OpenStackCloudName, "cacert")
	if !updated {
		return content, false, nil
	}

	res, err := yaml.Marshal(clouds)
	if err != nil {
		return "", false, err
	}

	return string(res), true, nil
}

func (r *ReconcileCloudCredSecret) updateSecretAnnotations(secret *corev1.Secret, value string) error {
	secretAnnotations := secret.GetAnnotations()
	if secretAnnotations == nil {
		secretAnnotations = map[string]string{}
	}

	secretAnnotations[constants.AnnotationKey] = value
	secret.SetAnnotations(secretAnnotations)

	return r.Update(context.TODO(), secret)
}
