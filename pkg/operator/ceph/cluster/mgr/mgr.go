/*
Copyright 2016 The Rook Authors. All rights reserved.

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

// Package mgr for the Ceph manager.
package mgr

import (
	"fmt"

	"github.com/coreos/pkg/capnslog"
	cephv1beta1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1beta1"
	rookalpha "github.com/rook/rook/pkg/apis/rook.io/v1alpha2"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/daemon/ceph/client"
	opspec "github.com/rook/rook/pkg/operator/ceph/spec"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var logger = capnslog.NewPackageLogger("github.com/rook/rook", "op-mgr")

const (
	appName              = "rook-ceph-mgr"
	keyringSecretKeyName = "keyring"
	prometheusModuleName = "prometheus"
	dashboardModuleName  = "dashboard"
	metricsPort          = 9283
	dashboardPort        = 7000
)

var mgrNames = []string{"a", "b"}

// Cluster represents the Rook and environment configuration settings needed to set up Ceph mgrs.
type Cluster struct {
	Namespace   string
	Replicas    int
	placement   rookalpha.Placement
	context     *clusterd.Context
	dataDir     string
	HostNetwork bool
	resources   v1.ResourceRequirements
	ownerRef    metav1.OwnerReference
	dashboard   cephv1beta1.DashboardSpec
	cephVersion cephv1beta1.CephVersionSpec
	rookVersion string
}

// mgrConfig for a single mgr
type mgrConfig struct {
	ResourceName string // the name rook gives to mgr resources in k8s metadata
	DaemonName   string // the name of the Ceph daemon ("a", "b", ...)
}

// New creates an instance of the mgr
func New(context *clusterd.Context, namespace, rookVersion string, cephVersion cephv1beta1.CephVersionSpec, placement rookalpha.Placement, hostNetwork bool, dashboard cephv1beta1.DashboardSpec,
	resources v1.ResourceRequirements, ownerRef metav1.OwnerReference) *Cluster {
	return &Cluster{
		context:     context,
		Namespace:   namespace,
		placement:   placement,
		rookVersion: rookVersion,
		cephVersion: cephVersion,
		Replicas:    1,
		dataDir:     k8sutil.DataDir,
		dashboard:   dashboard,
		HostNetwork: hostNetwork,
		resources:   resources,
		ownerRef:    ownerRef,
	}
}

// Start begins the process of running a cluster of Ceph mgrs.
func (c *Cluster) Start() error {
	logger.Infof("start running mgr")

	for i := 0; i < c.Replicas; i++ {
		if i >= len(mgrNames) {
			logger.Errorf("cannot have more than %d mgrs", len(mgrNames))
			break
		}

		daemonName := mgrNames[i]
		resourceName := fmt.Sprintf("%s-%s", appName, daemonName)
		if err := c.createKeyring(c.Namespace, resourceName, daemonName); err != nil {
			return fmt.Errorf("failed to create %s keyring. %+v", resourceName, err)
		}

		mgrConfig := &mgrConfig{
			DaemonName:   daemonName,
			ResourceName: resourceName,
		}

		// start the deployment
		deployment := c.makeDeployment(mgrConfig)
		if _, err := c.context.Clientset.ExtensionsV1beta1().Deployments(c.Namespace).Create(deployment); err != nil {
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create %s deployment. %+v", resourceName, err)
			}
			logger.Infof("%s deployment already exists", resourceName)
		} else {
			logger.Infof("%s deployment started", resourceName)
		}
	}

	if err := c.enablePrometheusModule(c.Namespace); err != nil {
		return fmt.Errorf("failed to enable mgr prometheus module. %+v", err)
	}

	// create the metrics service
	service := c.makeMetricsService(appName)
	if _, err := c.context.Clientset.CoreV1().Services(c.Namespace).Create(service); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create mgr service. %+v", err)
		}
		logger.Infof("mgr metrics service already exists")
	} else {
		logger.Infof("mgr metrics service started")
	}

	return c.configureDashboard()
}

func (c *Cluster) configureDashboard() error {
	// enable or disable the dashboard module
	if err := c.configureDashboardModule(c.Namespace, c.dashboard.Enabled); err != nil {
		return fmt.Errorf("failed to enable mgr dashboard module. %+v", err)
	}

	dashboardService := c.makeDashboardService(appName)
	if c.dashboard.Enabled {
		// expose the dashboard service
		if _, err := c.context.Clientset.CoreV1().Services(c.Namespace).Create(dashboardService); err != nil {
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create dashboard mgr service. %+v", err)
			}
			logger.Infof("dashboard service already exists")
		} else {
			logger.Infof("dashboard service started")
		}
	} else {
		// delete the dashboard service if it exists
		err := c.context.Clientset.CoreV1().Services(c.Namespace).Delete(dashboardService.Name, &metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete dashboard service. %+v", err)
		}
	}

	return nil
}

func (c *Cluster) makeMetricsService(name string) *v1.Service {
	labels := opspec.AppLabels(appName, c.Namespace)
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.Namespace,
			Labels:    labels,
		},
		Spec: v1.ServiceSpec{
			Selector: labels,
			Type:     v1.ServiceTypeClusterIP,
			Ports: []v1.ServicePort{
				{
					Name:     "http-metrics",
					Port:     int32(metricsPort),
					Protocol: v1.ProtocolTCP,
				},
			},
		},
	}

	k8sutil.SetOwnerRef(c.context.Clientset, c.Namespace, &svc.ObjectMeta, &c.ownerRef)
	return svc
}

func (c *Cluster) makeDashboardService(name string) *v1.Service {
	labels := opspec.AppLabels(appName, c.Namespace)
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-dashboard", name),
			Namespace: c.Namespace,
			Labels:    labels,
		},
		Spec: v1.ServiceSpec{
			Selector: labels,
			Type:     v1.ServiceTypeClusterIP,
			Ports: []v1.ServicePort{
				{
					Name:     "http-dashboard",
					Port:     int32(dashboardPort),
					Protocol: v1.ProtocolTCP,
				},
			},
		},
	}
	k8sutil.SetOwnerRef(c.context.Clientset, c.Namespace, &svc.ObjectMeta, &c.ownerRef)
	return svc
}

func (c *Cluster) createKeyring(clusterName, name, daemonName string) error {
	_, err := c.context.Clientset.CoreV1().Secrets(c.Namespace).Get(name, metav1.GetOptions{})
	if err == nil {
		logger.Infof("the mgr keyring was already generated")
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get mgr secrets. %+v", err)
	}

	// get-or-create-key for the user account
	keyring, err := createKeyring(c.context, clusterName, daemonName)
	if err != nil {
		return fmt.Errorf("failed to create mgr keyring. %+v", err)
	}

	// Store the keyring in a secret
	secrets := map[string]string{
		keyringSecretKeyName: keyring,
	}
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.Namespace,
		},
		StringData: secrets,
		Type:       k8sutil.RookType,
	}
	k8sutil.SetOwnerRef(c.context.Clientset, c.Namespace, &secret.ObjectMeta, &c.ownerRef)

	_, err = c.context.Clientset.CoreV1().Secrets(c.Namespace).Create(secret)
	if err != nil {
		return fmt.Errorf("failed to save mgr secrets. %+v", err)
	}

	return nil
}

// Ceph docs about the prometheus module: http://docs.ceph.com/docs/master/mgr/prometheus/
func (c *Cluster) enablePrometheusModule(clusterName string) error {
	if err := client.MgrEnableModule(c.context, clusterName, prometheusModuleName, true); err != nil {
		return fmt.Errorf("failed to enable mgr prometheus module. %+v", err)
	}
	return nil
}

// Ceph docs about the dashboard module: http://docs.ceph.com/docs/luminous/mgr/dashboard/
func (c *Cluster) configureDashboardModule(clusterName string, enable bool) error {
	if enable {
		if err := client.MgrEnableModule(c.context, clusterName, dashboardModuleName, true); err != nil {
			return fmt.Errorf("failed to enable mgr dashboard module. %+v", err)
		}
	} else {
		if err := client.MgrDisableModule(c.context, clusterName, dashboardModuleName); err != nil {
			return fmt.Errorf("failed to disable mgr dashboard module. %+v", err)
		}
	}
	return nil
}

func getKeyringProperties(name string) (string, []string) {
	username := fmt.Sprintf("mgr.%s", name)
	access := []string{"mon", "allow *"}
	return username, access
}

// create a keyring for the mds client with a limited set of privileges
func createKeyring(context *clusterd.Context, clusterName, name string) (string, error) {
	// get-or-create-key for the user account
	username, access := getKeyringProperties(name)
	keyring, err := client.AuthGetOrCreateKey(context, clusterName, username, access)
	if err != nil {
		return "", fmt.Errorf("failed to get or create auth key for %s. %+v", username, err)
	}

	return keyring, nil
}
