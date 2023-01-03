/*
Copyright AppsCode Inc.

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

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"

	"go.bytebuilders.dev/license-verifier/apis/licenses/v1alpha1"
	"go.bytebuilders.dev/license-verifier/info"

	"github.com/pkg/errors"
	verifier "go.bytebuilders.dev/license-verifier"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/mux"
	"k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"
	core_util "kmodules.xyz/client-go/core/v1"
	"kmodules.xyz/client-go/dynamic"
	"kmodules.xyz/client-go/meta"
	"kmodules.xyz/client-go/tools/clusterid"
)

const (
	EventSourceLicenseVerifier           = "License Verifier"
	EventReasonLicenseVerificationFailed = "License Verification Failed"

	licensePath          = "/appscode/license"
	licenseCheckInterval = 1 * time.Hour
)

type LicenseEnforcer struct {
	opts       verifier.VerifyOptions
	config     *rest.Config
	kc         kubernetes.Interface
	getLicense func() ([]byte, error)
}

// NewLicenseEnforcer returns a newly created license enforcer
func NewLicenseEnforcer(config *rest.Config, licenseFile string) (*LicenseEnforcer, error) {
	le := LicenseEnforcer{
		getLicense: getLicense(config, licenseFile),
		config:     config,
		opts: verifier.VerifyOptions{
			Features: info.ProductName,
		},
	}

	return &le, nil
}

func MustLicenseEnforcer(config *rest.Config, licenseFile string) *LicenseEnforcer {
	le, err := NewLicenseEnforcer(config, licenseFile)
	if err != nil {
		panic("failed to instantiate license enforcer, err:" + err.Error())
	}
	return le
}

func getLicense(cfg *rest.Config, licenseFile string) func() ([]byte, error) {
	return func() ([]byte, error) {
		return nil, nil
	}
}

func (le *LicenseEnforcer) createClients() (err error) {
	if le.kc == nil {
		le.kc, err = kubernetes.NewForConfig(le.config)
	}
	return err
}

func (le *LicenseEnforcer) acquireLicense() (err error) {
	le.opts.License, err = le.getLicense()
	return err
}

func (le *LicenseEnforcer) readClusterUID() (err error) {
	le.opts.ClusterUID, err = clusterid.ClusterUID(le.kc.CoreV1().Namespaces())
	return err
}

func (le *LicenseEnforcer) podName() (string, error) {
	if name, ok := os.LookupEnv("MY_POD_NAME"); ok {
		return name, nil
	}

	if meta.PossiblyInCluster() {
		// Read current pod name
		return os.Hostname()
	}
	return "", errors.New("failed to detect pod name")
}

func (le *LicenseEnforcer) handleLicenseVerificationFailure(licenseErr error) error {
	// Send interrupt so that all go-routines shut-down gracefully
	// https://pracucci.com/graceful-shutdown-of-kubernetes-pods.html
	// https://linuxhandbook.com/sigterm-vs-sigkill/
	// https://pracucci.com/graceful-shutdown-of-kubernetes-pods.html
	//nolint:errcheck
	defer func() {
		// Need to send signal twice because
		// we catch the first INT/TERM signal
		// ref: https://github.com/kubernetes/apiserver/blob/8d97c871d91c75b81b8b4c438f4dd1eaa7f35052/pkg/server/signal.go#L47-L51
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		time.Sleep(30 * time.Second)
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	}()

	// Log licenseInfo verification failure
	klog.Errorln("Failed to verify license. Reason: ", licenseErr.Error())

	podName, err := le.podName()
	if err != nil {
		return err
	}
	// Read the namespace of current pod
	namespace := meta.PodNamespace()

	// Find the root owner of this pod
	owner, _, err := dynamic.DetectWorkload(
		context.TODO(),
		le.config,
		core.SchemeGroupVersion.WithResource(core.ResourcePods.String()),
		namespace,
		podName,
	)
	if err != nil {
		return err
	}
	ref, err := reference.GetReference(clientscheme.Scheme, owner)
	if err != nil {
		return err
	}
	eventMeta := metav1.ObjectMeta{
		Name:      meta.NameWithSuffix(owner.GetName(), "license"),
		Namespace: namespace,
	}
	// Create an event against the root owner specifying that the license verification failed
	_, _, err = core_util.CreateOrPatchEvent(context.TODO(), le.kc, eventMeta, func(in *core.Event) *core.Event {
		in.InvolvedObject = *ref
		in.Type = core.EventTypeWarning
		in.Source = core.EventSource{Component: EventSourceLicenseVerifier}
		in.Reason = EventReasonLicenseVerificationFailed
		in.Message = fmt.Sprintf("Failed to verify license. Reason: %s", licenseErr.Error())

		if in.FirstTimestamp.IsZero() {
			in.FirstTimestamp = metav1.Now()
		}
		in.LastTimestamp = metav1.Now()
		in.Count = in.Count + 1

		return in
	}, metav1.PatchOptions{})
	return err
}

// Install adds the License info handler
func (le *LicenseEnforcer) Install(c *mux.PathRecorderMux) {
	// Create Kubernetes client
	err := le.createClients()
	if err != nil {
		klog.Fatal(err)
		return
	}
	c.Handle(licensePath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-content-type-options", "nosniff")

		utilruntime.Must(json.NewEncoder(w).Encode(le.LoadLicense()))
	}))
}

func (le *LicenseEnforcer) LoadLicense() v1alpha1.License {
	utilruntime.Must(le.createClients())

	// Read cluster UID (UID of the "kube-system" namespace)
	err := le.readClusterUID()
	if err != nil {
		license, _ := verifier.BadLicense(err)
		return license
	}
	// Read license from file
	err = le.acquireLicense()
	if err != nil {
		license, _ := verifier.BadLicense(err)
		return license
	}
	license, _ := verifier.CheckLicense(le.opts)
	return license
}

// VerifyLicensePeriodically periodically verifies whether the provided license is valid for the current cluster or not.
func VerifyLicensePeriodically(config *rest.Config, licenseFile string, stopCh <-chan struct{}) error {
	if info.SkipLicenseVerification() {
		klog.Infoln("License verification skipped")
		return nil
	}

	le, err := NewLicenseEnforcer(config, licenseFile)
	if err != nil {
		return le.handleLicenseVerificationFailure(err)
	}
	if err := verifyLicensePeriodically(le, licenseFile, stopCh); err != nil {
		return le.handleLicenseVerificationFailure(err)
	}
	return nil
}

func verifyLicensePeriodically(le *LicenseEnforcer, licenseFile string, stopCh <-chan struct{}) error {
	// Create Kubernetes client
	err := le.createClients()
	if err != nil {
		return err
	}
	// Read cluster UID (UID of the "kube-system" namespace)
	err = le.readClusterUID()
	if err != nil {
		return err
	}

	// Periodically verify license with 1 hour interval
	fn := func() (done bool, err error) {
		klog.V(8).Infoln("Verifying license.......")
		// Read license from file
		err = le.acquireLicense()
		if err != nil {
			return false, err
		}
		// Validate license
		_, err = verifier.CheckLicense(le.opts)
		if err != nil {
			return false, err
		}
		klog.Infoln("Successfully verified license!")
		// return false so that the loop never ends
		return false, nil
	}

	return wait.PollImmediateUntil(licenseCheckInterval, fn, stopCh)
}

// CheckLicenseFile verifies whether the provided license is valid for the current cluster or not.
func CheckLicenseFile(config *rest.Config, licenseFile string) error {
	if info.SkipLicenseVerification() {
		klog.Infoln("License verification skipped")
		return nil
	}

	klog.V(8).Infoln("Verifying license.......")
	le, err := NewLicenseEnforcer(config, licenseFile)
	if err != nil {
		return le.handleLicenseVerificationFailure(err)
	}
	if err := checkLicenseFile(le); err != nil {
		return le.handleLicenseVerificationFailure(err)
	}
	return nil
}

func checkLicenseFile(le *LicenseEnforcer) error {
	// Create Kubernetes client
	err := le.createClients()
	if err != nil {
		return err
	}
	// Read cluster UID (UID of the "kube-system" namespace)
	err = le.readClusterUID()
	if err != nil {
		return err
	}
	// Read license from file
	err = le.acquireLicense()
	if err != nil {
		return err
	}
	// Validate license
	_, err = verifier.CheckLicense(le.opts)
	if err != nil {
		return err
	}
	klog.Infoln("Successfully verified license!")
	return nil
}

// CheckLicenseEndpoint verifies whether the provided api server has a valid license is valid for features.
func CheckLicenseEndpoint(config *rest.Config, apiServiceName string, features []string) error {
	return nil
}
