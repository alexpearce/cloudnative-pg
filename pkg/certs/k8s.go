/*
This file is part of Cloud Native PostgreSQL.

Copyright (C) 2019-2020 2ndQuadrant Italia SRL. Exclusively licensed to 2ndQuadrant Limited.
*/

package certs

import (
	"fmt"
	"io/ioutil"
	"path"

	"github.com/robfig/cron"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"gitlab.2ndquadrant.com/k8s/cloud-native-postgresql/pkg/fileutils"
)

var (
	log = ctrl.Log.WithName("pki")
)

// WebhookEnvironment represent the environment under which the WebHook server will work
type WebhookEnvironment struct {
	// Where to store the certificates
	CertDir string

	// The name of the secret where the CA certificate will be stored
	CaSecretName string

	// The name of the secret where the certificates will be stored
	SecretName string

	// The name of the service where the webhook server will be reachable
	ServiceName string

	// The name of the namespace where the operator is set up
	OperatorNamespace string

	// The name of the mutating webhook configuration in k8s, used to
	// inject the caBundle
	MutatingWebhookConfigurationName string

	// The name of the validating webhook configuration in k8s, used
	// to inject the caBundle
	ValidatingWebhookConfigurationName string
}

// EnsureRootCACertificate ensure that in the cluster there is a root CA Certificate
func EnsureRootCACertificate(client kubernetes.Interface, namespace string, name string) (*v1.Secret, error) {
	// Checking if the root CA already exist
	secret, err := client.CoreV1().Secrets(namespace).Get(name, metav1.GetOptions{})
	if err == nil {
		// Verify the temporal validity of this CA and renew the secret if needed
		_, err := renewCACertificate(client, secret)
		if err != nil {
			return nil, err
		}

		return secret, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	// Let's create the CA
	pair, err := CreateCA()
	if err != nil {
		return nil, err
	}

	secret = pair.GenerateCASecret(namespace, name)
	createdSecret, err := client.CoreV1().Secrets(namespace).Create(secret)
	if err != nil {
		return nil, err
	}
	return createdSecret, nil
}

// renewCACertificate renews a CA certificate if needed, returning the updated
// secret if the secret has been renewed
func renewCACertificate(client kubernetes.Interface, secret *v1.Secret) (*v1.Secret, error) {
	// Verify the temporal validity of this CA
	pair, err := ParseCASecret(secret)
	if err != nil {
		return nil, err
	}

	expiring, err := pair.IsExpiring()
	if err != nil {
		return nil, err
	}
	if !expiring {
		return secret, nil
	}

	privateKey, err := pair.ParseECPrivateKey()
	if err != nil {
		return nil, err
	}

	err = pair.RenewCertificate(privateKey)
	if err != nil {
		return nil, err
	}

	secret.Data["ca.crt"] = pair.Certificate
	updatedSecret, err := client.CoreV1().Secrets(secret.Namespace).Update(secret)
	if err != nil {
		return nil, err
	}

	return updatedSecret, nil
}

// Setup will setup the PKI infrastructure that is needed for the operator
// to correctly work, and copy the certificates which are required for the webhook
// server to run in the right folder
func (webhook WebhookEnvironment) Setup(client kubernetes.Interface) error {
	caSecret, err := EnsureRootCACertificate(
		client,
		webhook.OperatorNamespace,
		webhook.CaSecretName)
	if err != nil {
		return err
	}

	webhookSecret, err := webhook.EnsureCertificate(client, caSecret)
	if err != nil {
		return err
	}

	err = DumpSecretToDir(webhookSecret, webhook.CertDir)
	if err != nil {
		return err
	}

	err = webhook.InjectPublicKeyIntoMutatingWebhook(
		client,
		webhookSecret)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("mutating webhook configuration not found, cannot inject public key",
			"name", webhook.MutatingWebhookConfigurationName)
	} else if err != nil {
		return err
	}

	err = webhook.InjectPublicKeyIntoValidatingWebhook(
		client,
		webhookSecret)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("validating webhook configuration not found, cannot inject public key",
			"name", webhook.ValidatingWebhookConfigurationName)
	} else if err != nil {
		return err
	}

	return nil
}

// SchedulePeriodicMaintenance schedule a background periodic certificate maintenance,
// to automatically renew TLS certificates
func (webhook WebhookEnvironment) SchedulePeriodicMaintenance(client kubernetes.Interface) error {
	maintenance := func() {
		log.Info("Periodic TLS certificates maintenance")
		err := webhook.Setup(client)
		if err != nil {
			log.Error(err, "TLS maintenance failed")
		}
	}

	c := cron.New()
	err := c.AddFunc("@every 1h", maintenance)
	c.Start()

	if err != nil {
		return fmt.Errorf("error while scheduling CA maintenance: %w", err)
	}

	return nil
}

// EnsureCertificate will ensure that a webhook certificate exists and is usable
func (webhook WebhookEnvironment) EnsureCertificate(
	client kubernetes.Interface, caSecret *v1.Secret) (*v1.Secret, error) {
	// Checking if the secret already exist
	secret, err := client.CoreV1().Secrets(
		webhook.OperatorNamespace).Get(webhook.SecretName, metav1.GetOptions{})
	if err == nil {
		// Verify the temporal validity of this certificate and
		// renew it if needed
		return renewServerCertificate(client, *caSecret, secret)
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	// Let's generate the webhook certificate
	caPair, err := ParseCASecret(caSecret)
	if err != nil {
		return nil, err
	}

	webhookHostname := fmt.Sprintf(
		"%v.%v.svc",
		webhook.ServiceName,
		webhook.OperatorNamespace)
	webhookPair, err := caPair.CreateAndSignPair(webhookHostname)
	if err != nil {
		return nil, err
	}

	secret = webhookPair.GenerateServerSecret(webhook.OperatorNamespace, webhook.SecretName)
	createdSecret, err := client.CoreV1().Secrets(
		webhook.OperatorNamespace).Create(secret)
	if err != nil {
		return nil, err
	}
	return createdSecret, nil
}

// renewServerCertificate renews a CA certificate if needed, the
// renewed secret or the original one
func renewServerCertificate(client kubernetes.Interface, caSecret v1.Secret, secret *v1.Secret) (*v1.Secret, error) {
	// Verify the temporal validity of this CA
	pair, err := ParseServerSecret(secret)
	if err != nil {
		return nil, err
	}

	expiring, err := pair.IsExpiring()
	if err != nil {
		return nil, err
	}
	if !expiring {
		return secret, nil
	}

	// Parse the CA secret to get the private key
	caPair, err := ParseCASecret(&caSecret)
	if err != nil {
		return nil, err
	}

	caPrivateKey, err := caPair.ParseECPrivateKey()
	if err != nil {
		return nil, err
	}

	err = pair.RenewCertificate(caPrivateKey)
	if err != nil {
		return nil, err
	}

	secret.Data["tls.crt"] = pair.Certificate
	updatedSecret, err := client.CoreV1().Secrets(secret.Namespace).Update(secret)
	if err != nil {
		return nil, err
	}

	return updatedSecret, nil
}

// DumpSecretToDir dumps the contents of a secret inside a directory, and this
// is useful for the webhook server to correctly run
func DumpSecretToDir(secret *v1.Secret, certDir string) error {
	resourceFileName := path.Join(certDir, "resource")

	oldVersionExist, err := fileutils.FileExists(resourceFileName)
	if err != nil {
		return err
	}
	if oldVersionExist {
		oldVersion, err := fileutils.ReadFile(resourceFileName)
		if err != nil {
			return err
		}

		if oldVersion == secret.ResourceVersion {
			// No need to rewrite certificates, the content
			// is just the same
			return nil
		}
	}

	for name, content := range secret.Data {
		fileName := path.Join(certDir, name)
		if err := ioutil.WriteFile(fileName, content, 0600); err != nil {
			return err
		}
	}

	err = ioutil.WriteFile(resourceFileName, []byte(secret.ResourceVersion), 0600)
	if err != nil {
		return err
	}

	return nil
}

// InjectPublicKeyIntoMutatingWebhook inject the TLS public key into the admitted
// ones for a certain mutating webhook configuration
func (webhook WebhookEnvironment) InjectPublicKeyIntoMutatingWebhook(
	client kubernetes.Interface, tlsSecret *v1.Secret) error {
	config, err := client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Get(
		webhook.MutatingWebhookConfigurationName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	for idx := range config.Webhooks {
		config.Webhooks[idx].ClientConfig.CABundle = tlsSecret.Data["tls.crt"]
	}

	_, err = client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Update(config)
	return err
}

// InjectPublicKeyIntoValidatingWebhook inject the TLS public key into the admitted
// ones for a certain validating webhook configuration
func (webhook WebhookEnvironment) InjectPublicKeyIntoValidatingWebhook(
	client kubernetes.Interface, tlsSecret *v1.Secret) error {
	config, err := client.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Get(
		webhook.ValidatingWebhookConfigurationName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	for idx := range config.Webhooks {
		config.Webhooks[idx].ClientConfig.CABundle = tlsSecret.Data["tls.crt"]
	}

	_, err = client.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Update(config)
	return err
}