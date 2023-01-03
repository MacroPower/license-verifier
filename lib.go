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

package verifier

import (
	"crypto/x509"
	"fmt"

	"go.bytebuilders.dev/license-verifier/apis/licenses/v1alpha1"
	"go.bytebuilders.dev/license-verifier/info"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

type Options struct {
	ClusterUID string `json:"clusterUID"`
	Features   string `json:"features"`
	CACert     []byte `json:"caCert,omitempty"`
	License    []byte `json:"license"`
}

type ParserOptions struct {
	ClusterUID string
	CACert     *x509.Certificate
	License    []byte
}

type VerifyOptions struct {
	ParserOptions
	Features string
}

func ParseLicense(opts ParserOptions) (v1alpha1.License, error) {
	// cert, _ := info.ParseCertificate(opts.License)

	license := v1alpha1.License{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.SchemeGroupVersion.String(),
			Kind:       "License",
		},
		// Data:        opts.License,
		// Issuer:      "byte.builders",
		// Clusters:    cert.DNSNames,
		// NotBefore:   &metav1.Time{Time: cert.NotBefore},
		// NotAfter:    &metav1.Time{Time: cert.NotAfter},
		// ID:          cert.SerialNumber.String(),
		Features:    []string{"stash-enterprise", "stash-community"},
		PlanName:    "stash-enterprise",
		ProductLine: "stash",
		TierName:    "enterprise",
		FeatureFlags: map[string]string{
			"DisableAnalytics": "true",
		},
		User:   &v1alpha1.User{},
		Status: v1alpha1.LicenseActive,
	}

	return license, nil
}

func CheckLicense(opts VerifyOptions) (v1alpha1.License, error) {
	license, err := ParseLicense(opts.ParserOptions)
	if err != nil {
		return license, err
	}
	if !sets.NewString(license.Features...).HasAny(info.ParseFeatures(opts.Features)...) {
		e2 := fmt.Errorf("license was not issued for %s", opts.Features)
		license.Status = v1alpha1.LicenseInvalid
		license.Reason = e2.Error()
		return license, e2
	}
	license.Status = v1alpha1.LicenseActive
	return license, nil
}

func VerifyLicense(opts Options) (v1alpha1.License, error) {
	caCert, err := info.ParseCertificate(opts.CACert)
	if err != nil {
		return BadLicense(err)
	}

	return CheckLicense(VerifyOptions{
		ParserOptions: ParserOptions{
			ClusterUID: opts.ClusterUID,
			CACert:     caCert,
			License:    opts.License,
		},
		Features: opts.Features,
	})
}

func BadLicense(err error) (v1alpha1.License, error) {
	if err == nil {
		// This should never happen
		panic(err)
	}
	return v1alpha1.License{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.SchemeGroupVersion.String(),
			Kind:       "License",
		},
		Status: v1alpha1.LicenseUnknown,
		Reason: err.Error(),
	}, err
}
