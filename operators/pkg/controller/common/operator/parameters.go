// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package operator

import (
	"github.com/elastic/cloud-on-k8s/operators/pkg/about"
	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/common/certificates"
	"github.com/elastic/cloud-on-k8s/operators/pkg/utils/net"
)

// Parameters contain parameters to create new operators.
type Parameters struct {
	// OperatorNamespace is the control plane namespace of the operator.
	OperatorNamespace string
	// OperatorInfo is information about the operator
	OperatorInfo about.OperatorInfo
	// Dialer is used to create the Elasticsearch HTTP client.
	Dialer net.Dialer
	// CACertRotation defines the rotation params for CA certificates.
	CACertRotation certificates.RotationParams
	// CertRotation defines the rotation params for non-CA certificates.
	CertRotation certificates.RotationParams
}
