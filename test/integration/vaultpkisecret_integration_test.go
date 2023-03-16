// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package integration

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/files"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	secretsv1alpha1 "github.com/hashicorp/vault-secrets-operator/api/v1alpha1"
)

func TestVaultPKISecret(t *testing.T) {
	if testWithHelm {
		t.Skipf("Test is not compatiable with Helm")
	}

	testID := strings.ToLower(random.UniqueId())
	testK8sNamespace := "k8s-tenant-" + testID
	testPKIMountPath := "pki-" + testID
	testVaultNamespace := ""
	testVaultConnectionName := "vaultconnection-test-tenant-1"
	testVaultAuthMethodName := "vaultauth-test-tenant-1"
	testVaultAuthMethodRole := "role1"

	operatorNS := os.Getenv("OPERATOR_NAMESPACE")
	require.NotEmpty(t, operatorNS, "OPERATOR_NAMESPACE is not set")

	clusterName := os.Getenv("KIND_CLUSTER_NAME")
	require.NotEmpty(t, clusterName, "KIND_CLUSTER_NAME is not set")
	k8sConfigContext := "kind-" + clusterName
	k8sOpts := &k8s.KubectlOptions{
		ContextName: k8sConfigContext,
		Namespace:   operatorNS,
	}
	kustomizeConfigPath := filepath.Join(kustomizeConfigRoot, "default")
	if !testWithHelm {
		deployOperatorWithKustomize(t, k8sOpts, kustomizeConfigPath)
	}

	tempDir, err := os.MkdirTemp(os.TempDir(), t.Name())
	require.Nil(t, err)

	tfDir, err := files.CopyTerraformFolderToDest(
		path.Join(testRoot, "vaultpkisecret/terraform"),
		tempDir,
		"terraform",
	)
	require.Nil(t, err)
	// Construct the terraform options with default retryable errors to handle the most common
	// retryable errors in terraform testing.
	terraformOptions := &terraform.Options{
		// Set the path to the Terraform code that will be tested.
		TerraformDir: tfDir,
		Vars: map[string]interface{}{
			"deploy_operator_via_helm":     testWithHelm,
			"k8s_vault_connection_address": testVaultAddress,
			"k8s_test_namespace":           testK8sNamespace,
			"k8s_config_context":           "kind-" + clusterName,
			"vault_pki_mount_path":         testPKIMountPath,
			"operator_helm_chart_path":     chartPath,
		},
	}
	if entTests {
		testVaultNamespace = "vault-tenant-" + testID
		terraformOptions.Vars["vault_enterprise"] = true
		terraformOptions.Vars["vault_test_namespace"] = testVaultNamespace
	}
	terraformOptions = setCommonTFOptions(t, terraformOptions)

	// Clean up resources with "terraform destroy" at the end of the test.
	t.Cleanup(func() {
		exportKindLogs(t)

		terraform.Destroy(t, terraformOptions)
		os.RemoveAll(tempDir)

		// Undeploy Kustomize
		if !testWithHelm {
			k8s.KubectlDeleteFromKustomize(t, k8sOpts, kustomizeConfigPath)
		}
	})

	// Run "terraform init" and "terraform apply". Fail the test if there are any errors.
	terraform.InitAndApply(t, terraformOptions)

	crdClient := getCRDClient(t)
	ctx := context.Background()

	// When we deploy the operator with Helm it will also deploy default VaultConnection/AuthMethod
	// resources, so these are not needed. In this case, we will also clear the VaultAuthRef field of
	// the target secret so that the controller uses the default AuthMethod.
	if !testWithHelm {
		// Create a VaultConnection CR
		testVaultConnection := &secretsv1alpha1.VaultConnection{
			ObjectMeta: v1.ObjectMeta{
				Name:      testVaultConnectionName,
				Namespace: testK8sNamespace,
			},
			Spec: secretsv1alpha1.VaultConnectionSpec{
				Address: testVaultAddress,
			},
		}

		defer crdClient.Delete(ctx, testVaultConnection)
		err := crdClient.Create(ctx, testVaultConnection)
		require.NoError(t, err)

		// Create a VaultAuth CR
		testVaultAuth := &secretsv1alpha1.VaultAuth{
			ObjectMeta: v1.ObjectMeta{
				Name:      testVaultAuthMethodName,
				Namespace: testK8sNamespace,
			},
			Spec: secretsv1alpha1.VaultAuthSpec{
				VaultConnectionRef: testVaultConnectionName,
				Namespace:          testVaultNamespace,
				Method:             "kubernetes",
				Mount:              "kubernetes",
				Kubernetes: &secretsv1alpha1.VaultAuthConfigKubernetes{
					Role:           testVaultAuthMethodRole,
					ServiceAccount: "default",
					TokenAudiences: []string{"vault"},
				},
			},
		}

		defer crdClient.Delete(ctx, testVaultAuth)
		err = crdClient.Create(ctx, testVaultAuth)
		require.NoError(t, err)
	}

	// Create a VaultPKI CR to trigger the sync
	getExisting := func() []*secretsv1alpha1.VaultPKISecret {
		return []*secretsv1alpha1.VaultPKISecret{
			{
				ObjectMeta: v1.ObjectMeta{
					Name:      "vaultpki-test-tenant-1",
					Namespace: testK8sNamespace,
				},
				Spec: secretsv1alpha1.VaultPKISecretSpec{
					VaultAuthRef: testVaultAuthMethodName,
					Namespace:    testVaultNamespace,
					Mount:        testPKIMountPath,
					Name:         "secret",
					CommonName:   "test1.example.com",
					Format:       "pem",
					Revoke:       true,
					Clear:        true,
					ExpiryOffset: "5s",
					TTL:          "15s",
					Destination: secretsv1alpha1.Destination{
						Name:   "pki1",
						Create: false,
					},
				},
			},
		}
	}
	// The Helm based integration test is expecting to use the default VaultAuthMethod+VaultConnection
	// so in order to get the controller to use the deployed default VaultAuthMethod we need set the VaultAuthRef to "".
	//if testWithHelm {
	//	testVaultPKI.Spec.VaultAuthRef = ""
	//}

	tests := []struct {
		name     string
		existing []*secretsv1alpha1.VaultPKISecret
		create   int
	}{
		{
			name:     "existing-only",
			existing: getExisting(),
		},
		{
			name:   "create-only",
			create: 10,
		},
		{
			name:     "mixed",
			existing: getExisting(),
			create:   5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var toTest []*secretsv1alpha1.VaultPKISecret
			for idx, obj := range tt.existing {
				name := fmt.Sprintf("%s-existing-%d", tt.name, idx)
				obj.Name = name
				toTest = append(toTest, obj)
			}

			for idx := 0; idx < tt.create; idx++ {
				dest := fmt.Sprintf("%s-create-%d", tt.name, idx)
				toTest = append(toTest, &secretsv1alpha1.VaultPKISecret{
					ObjectMeta: v1.ObjectMeta{
						Name:      dest,
						Namespace: testK8sNamespace,
					},
					Spec: secretsv1alpha1.VaultPKISecretSpec{
						Name:         "secret",
						Namespace:    testVaultNamespace,
						Mount:        testPKIMountPath,
						CommonName:   fmt.Sprintf("%s.example.com", dest),
						Format:       "pem",
						Revoke:       true,
						ExpiryOffset: "5s",
						TTL:          "15s",
						VaultAuthRef: testVaultAuthMethodName,
						Destination: secretsv1alpha1.Destination{
							Name:   dest,
							Create: true,
						},
					},
				})
			}

			require.Equal(t, len(toTest), tt.create+len(tt.existing))

			var count int
			for _, vpsObj := range toTest {
				count++
				name := fmt.Sprintf("%s", vpsObj.Name)
				t.Run(name, func(t *testing.T) {
					// capture vpsObj for parallel test
					vpsObj := vpsObj
					t.Parallel()

					t.Cleanup(func() {
						assert.NoError(t, crdClient.Delete(ctx, vpsObj))
					})
					require.NoError(t, crdClient.Create(ctx, vpsObj))

					// Wait for the operator to sync Vault PKI --> k8s Secret, and return the
					// serial number of the generated cert
					serialNumber, secret, err := waitForPKIData(t, 30, 1*time.Second,
						vpsObj.Spec.Destination.Name, vpsObj.ObjectMeta.Namespace,
						vpsObj.Spec.CommonName, "",
					)
					require.NoError(t, err)
					assert.NotEmpty(t, serialNumber)

					assertSyncableSecret(t, vpsObj,
						"secrets.hashicorp.com/v1alpha1",
						"VaultPKISecret", secret)

					// Use the serial number of the first generated cert to check that the cert
					// is updated
					newSerialNumber, secret, err := waitForPKIData(t, 30, 2*time.Second,
						vpsObj.Spec.Destination.Name, vpsObj.ObjectMeta.Namespace,
						vpsObj.Spec.CommonName, serialNumber,
					)
					require.NoError(t, err)
					assert.NotEmpty(t, newSerialNumber)

					assertSyncableSecret(t, vpsObj,
						"secrets.hashicorp.com/v1alpha1",
						"VaultPKISecret", secret)
				})
			}

			assert.Greater(t, count, 0, "no tests were run")
		})
	}
}
