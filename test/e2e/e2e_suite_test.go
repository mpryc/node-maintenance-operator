// Copyright 2018 The Operator-SDK Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/reporters"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/medik8s/node-maintenance-operator/api/v1beta1"
)

const (
	junitDir = "/tmp/artifacts"
)

var (
	// The ns the operator is running in
	operatorNsName string
	// The ns for test deployments
	testNsName    string
	testNamespace *corev1.Namespace
	// namespace leases are created in
	leaseNs = "medik8s-leases"
)

var _ = BeforeSuite(func() {
	operatorNsName = os.Getenv("OPERATOR_NS")
	Expect(operatorNsName).ToNot(BeEmpty(), "OPERATOR_NS env var not set, can't start e2e test")

	testNsName = os.Getenv("TEST_NAMESPACE")
	Expect(testNsName).ToNot(BeEmpty(), "TEST_NAMESPACE env var not set, can't start e2e test")
	testNamespace = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNsName,
		},
	}

	// create test namespace
	err := Client.Create(context.TODO(), testNamespace)
	if errors.IsAlreadyExists(err) {
		logWarnln("test namespace already exists, that is unexpected")
	} else {
		Expect(err).ToNot(HaveOccurred())
	}

	// On non-OpenShift clusters (e.g. Kind), provision mock etcd guard DaemonSet & PDB
	// so the validating webhook can test etcd quorum protection logic without skipping.
	ns := &corev1.Namespace{}
	if err := Client.Get(context.TODO(), client.ObjectKey{Name: "openshift-etcd"}, ns); errors.IsNotFound(err) {
		logInfoln("Non-OpenShift cluster detected: provisioning mock etcd-guard DaemonSet & PDB for master quorum tests")
		ensureMockEtcdGuard(context.TODO())
	}

	// wait until webhooks are up and running by trying to create a CR and ignoring unexpected errors
	testCR := getNodeMaintenance("webhook-test", "some-not-existing-node-name")
	_ = createCRIgnoreUnrelatedErrors(testCR)
})

var _ = AfterSuite(func() {
	// Delete nodeMaintenances
	if err := Client.DeleteAllOf(context.TODO(), &v1beta1.NodeMaintenance{}); err != nil {
		logWarnf("failed to clean up node maintenances: %v", err)
	}

	// Delete test namespace
	if err := Client.Delete(context.TODO(), testNamespace); err != nil {
		logWarnf("failed to clean up test namespace: %v", err)
	}

	// Clean up mock etcd-guard namespace if present
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openshift-etcd"}}
	if err := Client.Delete(context.TODO(), ns); err != nil && !errors.IsNotFound(err) {
		logWarnf("failed to clean up openshift-etcd namespace: %v", err)
	}
})

func TestNodeMaintenance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Node Maintenance Operator e2e tests")
}

// NewJUnitReporter with the given name. testSuiteName must be a valid filename part
func NewJUnitReporter(testSuiteName string) *reporters.JUnitReporter {
	return reporters.NewJUnitReporter(fmt.Sprintf("%s/%s_%s.xml", junitDir, "unit_report", testSuiteName))
}

func ensureMockEtcdGuard(ctx context.Context) {
	// 1. Create openshift-etcd namespace
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "openshift-etcd",
		},
	}
	if err := Client.Create(ctx, ns); err != nil && !errors.IsAlreadyExists(err) {
		logWarnf("failed to create openshift-etcd namespace for etcd guard mock: %v", err)
	}

	// 2. Create etcd-guard DaemonSet targeting control-plane nodes
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etcd-guard",
			Namespace: "openshift-etcd",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "etcd-guard"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "etcd-guard"},
				},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"node-role.kubernetes.io/control-plane": "",
					},
					Containers: []corev1.Container{
						{
							Name:  "pause",
							Image: "registry.k8s.io/pause:3.9",
						},
					},
					Tolerations: []corev1.Toleration{
						{
							Key:      "node-role.kubernetes.io/control-plane",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "node-role.kubernetes.io/master",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
	}
	if err := Client.Create(ctx, ds); err != nil && !errors.IsAlreadyExists(err) {
		logWarnf("failed to create etcd-guard DaemonSet for mock: %v", err)
	}

	// 3. Create etcd-guard-pdb with maxUnavailable = 1
	maxUnavailable := intstr.FromInt32(1)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etcd-guard-pdb",
			Namespace: "openshift-etcd",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "etcd-guard"},
			},
		},
	}
	if err := Client.Create(ctx, pdb); err != nil && !errors.IsAlreadyExists(err) {
		logWarnf("failed to create etcd-guard-pdb for etcd guard mock: %v", err)
	}
}
