// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package shoot

import (
	"time"

	gardenv1beta1 "github.com/gardener/gardener/pkg/apis/garden/v1beta1"
	"github.com/gardener/gardener/pkg/apis/garden/v1beta1/helper"
	"github.com/gardener/gardener/pkg/operation"
	botanistpkg "github.com/gardener/gardener/pkg/operation/botanist"
	cloudbotanistpkg "github.com/gardener/gardener/pkg/operation/cloudbotanist"
	"github.com/gardener/gardener/pkg/operation/common"
	hybridbotanistpkg "github.com/gardener/gardener/pkg/operation/hybridbotanist"
	"github.com/gardener/gardener/pkg/utils"
	"github.com/gardener/gardener/pkg/utils/flow"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// reconcileShoot reconciles the Shoot cluster's state.
// It receives a Garden object <garden> which stores the Shoot object and the operation type.
func (c *defaultControl) reconcileShoot(o *operation.Operation, operationType gardenv1beta1.ShootLastOperationType) *gardenv1beta1.LastError {
	// We create the botanists (which will do the actual work).
	botanist, err := botanistpkg.New(o)
	if err != nil {
		return formatError("Failed to create a Botanist", err)
	}
	seedCloudBotanist, err := cloudbotanistpkg.New(o, common.CloudPurposeSeed)
	if err != nil {
		return formatError("Failed to create a Seed CloudBotanist", err)
	}
	shootCloudBotanist, err := cloudbotanistpkg.New(o, common.CloudPurposeShoot)
	if err != nil {
		return formatError("Failed to create a Shoot CloudBotanist", err)
	}
	hybridBotanist, err := hybridbotanistpkg.New(o, botanist, seedCloudBotanist, shootCloudBotanist)
	if err != nil {
		return formatError("Failed to create a HybridBotanist", err)
	}

	var (
		defaultTimeout  = 30 * time.Second
		defaultInterval = 5 * time.Second
		managedDNS      = o.Shoot.Info.Spec.DNS.Provider != gardenv1beta1.DNSUnmanaged
		isCloud         = o.Shoot.Info.Spec.Cloud.Local == nil

		g               = flow.NewGraph("Shoot cluster reconciliation")
		deployNamespace = g.Add(flow.Task{
			Name: "Deploying Shoot namespace in Seed",
			Fn:   flow.TaskFn(botanist.DeployNamespace).RetryUntilTimeout(defaultInterval, defaultTimeout),
		})
		_ = g.Add(flow.Task{
			Name:         "Deploying cloud metadata service network policy",
			Fn:           flow.TaskFn(botanist.DeployCloudMetadataServiceNetworkPolicy).DoIf(isCloud).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(deployNamespace),
		})
		deployCloudProviderSecret = g.Add(flow.Task{
			Name:         "Deploying cloud provider account secret",
			Fn:           flow.TaskFn(botanist.DeployCloudProviderSecret).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(deployNamespace),
		})
		deployKubeAPIServerService = g.Add(flow.Task{
			Name:         "Deploying Kubernetes API server service",
			Fn:           flow.TaskFn(hybridBotanist.DeployKubeAPIServerService).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(deployNamespace),
		})
		waitUntilKubeAPIServerServiceIsReady = g.Add(flow.Task{
			Name:         "Waiting until Kubernetes API server service has reported readiness",
			Fn:           flow.TaskFn(botanist.WaitUntilKubeAPIServerServiceIsReady).DoIf(isCloud),
			Dependencies: flow.NewTaskIDs(deployKubeAPIServerService),
		})
		deploySecrets = g.Add(flow.Task{
			Name:         "Deploying Shoot certificates / keys",
			Fn:           botanist.DeploySecrets,
			Dependencies: flow.NewTaskIDs(waitUntilKubeAPIServerServiceIsReady),
		})
		_ = g.Add(flow.Task{
			Name:         "Deploying internal domain DNS record",
			Fn:           flow.TaskFn(botanist.DeployInternalDomainDNSRecord),
			Dependencies: flow.NewTaskIDs(waitUntilKubeAPIServerServiceIsReady),
		})
		_ = g.Add(flow.Task{
			Name:         "Deploying external domain DNS record",
			Fn:           flow.TaskFn(botanist.DeployExternalDomainDNSRecord).DoIf(managedDNS),
			Dependencies: flow.NewTaskIDs(deployNamespace),
		})
		deployInfrastructure = g.Add(flow.Task{
			Name:         "Deploying Shoot infrastructure",
			Fn:           shootCloudBotanist.DeployInfrastructure,
			Dependencies: flow.NewTaskIDs(deploySecrets, deployCloudProviderSecret),
		})
		deployBackupInfrastructure = g.Add(flow.Task{
			Name: "Deploying backup infrastructure",
			Fn:   flow.TaskFn(botanist.DeployBackupInfrastructure).DoIf(isCloud),
		})
		waitUntilBackupInfrastructureReconciled = g.Add(flow.Task{
			Name:         "Waiting until the backup infrastructure has been reconciled",
			Fn:           flow.TaskFn(botanist.WaitUntilBackupInfrastructureReconciled).DoIf(isCloud),
			Dependencies: flow.NewTaskIDs(deployBackupInfrastructure),
		})
		deployETCD = g.Add(flow.Task{
			Name:         "Deploying main and events etcd",
			Fn:           flow.TaskFn(hybridBotanist.DeployETCD).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(deploySecrets, deployCloudProviderSecret, waitUntilBackupInfrastructureReconciled),
		})
		waitUntilEtcdReady = g.Add(flow.Task{
			Name:         "Waiting until main and event etcd report readiness",
			Fn:           botanist.WaitUntilEtcdReady,
			Dependencies: flow.NewTaskIDs(deployETCD),
		})
		deployKubeAPIServer = g.Add(flow.Task{
			Name:         "Deploying Kubernetes API server",
			Fn:           flow.TaskFn(hybridBotanist.DeployKubeAPIServer).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(deploySecrets, deployETCD, waitUntilEtcdReady, waitUntilKubeAPIServerServiceIsReady),
		})
		deployCloudProviderConfig = g.Add(flow.Task{
			Name:         "Deploying cloud provider configuration",
			Fn:           flow.TaskFn(hybridBotanist.DeployCloudProviderConfig).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(deployInfrastructure, deployCloudProviderSecret),
		})
		_ = g.Add(flow.Task{
			Name:         "Deploying cloud controller manager",
			Fn:           flow.TaskFn(hybridBotanist.DeployCloudControllerManager).DoIf(isCloud).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(deploySecrets, deployCloudProviderSecret, deployKubeAPIServer, deployCloudProviderConfig),
		})
		_ = g.Add(flow.Task{
			Name:         "Deploying Kubernetes controller manager",
			Fn:           flow.TaskFn(hybridBotanist.DeployKubeControllerManager).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(deploySecrets, deployCloudProviderSecret, deployKubeAPIServer, deployCloudProviderConfig),
		})
		_ = g.Add(flow.Task{
			Name:         "Deploying Kubernetes scheduler",
			Fn:           flow.TaskFn(hybridBotanist.DeployKubeScheduler).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(deploySecrets, deployKubeAPIServer),
		})
		waitUntilKubeAPIServerIsReady = g.Add(flow.Task{
			Name:         "Waiting until Kubernetes API server reports readiness",
			Fn:           botanist.WaitUntilKubeAPIServerReady,
			Dependencies: flow.NewTaskIDs(deployKubeAPIServer),
		})
		initializeShootClients = g.Add(flow.Task{
			Name:         "Initializing connection to Shoot",
			Fn:           flow.TaskFn(botanist.InitializeShootClients).RetryUntilTimeout(defaultInterval, 2*time.Minute),
			Dependencies: flow.NewTaskIDs(waitUntilKubeAPIServerIsReady),
		})
		deployKubeAddonManager = g.Add(flow.Task{
			Name:         "Deploying Kubernetes addon manager",
			Fn:           flow.TaskFn(hybridBotanist.DeployKubeAddonManager).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(initializeShootClients, deployInfrastructure),
		})
		deployMachineControllerManager = g.Add(flow.Task{
			Name:         "Deploying machine controller manager",
			Fn:           flow.TaskFn(botanist.DeployMachineControllerManager).DoIf(isCloud).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(initializeShootClients, deployKubeAddonManager),
		})
		reconcileMachines = g.Add(flow.Task{
			Name:         "Reconciling Shoot workers",
			Fn:           flow.TaskFn(hybridBotanist.ReconcileMachines).DoIf(isCloud).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(deployMachineControllerManager, deployInfrastructure, initializeShootClients),
		})
		_ = g.Add(flow.Task{
			Name:         "Deploying Kube2IAM resources",
			Fn:           flow.TaskFn(shootCloudBotanist.DeployKube2IAMResources).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(deployInfrastructure),
		})
		_ = g.Add(flow.Task{
			Name:         "Ensuring ingress DNS record",
			Fn:           flow.TaskFn(botanist.EnsureIngressDNSRecord).DoIf(managedDNS).RetryUntilTimeout(defaultInterval, 10*time.Minute),
			Dependencies: flow.NewTaskIDs(deployKubeAddonManager),
		})
		waitUntilVPNConnectionExists = g.Add(flow.Task{
			Name:         "Waiting until the Kubernetes API server can connect to the Shoot workers",
			Fn:           flow.TaskFn(botanist.WaitUntilVPNConnectionExists).SkipIf(o.Shoot.Hibernated),
			Dependencies: flow.NewTaskIDs(deployKubeAddonManager, reconcileMachines),
		})
		applyCreateHook = g.Add(flow.Task{
			Name:         "Applying create hooks",
			Fn:           flow.TaskFn(seedCloudBotanist.ApplyCreateHook).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(waitUntilVPNConnectionExists),
		})
		deploySeedMonitoring = g.Add(flow.Task{
			Name:         "Deploying Shoot monitoring stack in Seed",
			Fn:           flow.TaskFn(botanist.DeploySeedMonitoring).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(waitUntilKubeAPIServerIsReady, initializeShootClients, waitUntilVPNConnectionExists, reconcileMachines, applyCreateHook),
		})
		_ = g.Add(flow.Task{
			Name:         "Deploy cluster autoscaler",
			Fn:           flow.TaskFn(botanist.DeployClusterAutoscaler).RetryUntilTimeout(defaultInterval, defaultTimeout),
			Dependencies: flow.NewTaskIDs(reconcileMachines, deployKubeAddonManager, deploySeedMonitoring),
		})
		f = g.Compile()
	)

	err = f.Run(flow.Opts{Logger: o.Logger, ProgressReporter: o.ReportShootProgress})
	if err != nil {
		o.Logger.Errorf("Failed to reconcile Shoot %q: %+v", o.Shoot.Info.Name, err)

		return &gardenv1beta1.LastError{
			Codes:       helper.ExtractErrorCodes(flow.Causes(err)),
			Description: helper.FormatLastErrDescription(err),
		}
	}

	// Register the Shoot as Seed cluster if it was annotated properly and in the garden namespace
	if o.Shoot.Info.Namespace == common.GardenNamespace {
		if o.ShootedSeed != nil {
			if err := botanist.RegisterAsSeed(o.ShootedSeed.Protected, o.ShootedSeed.Visible); err != nil {
				o.Logger.Errorf("Could not register Shoot %q as Seed: %+v", o.Shoot.Info.Name, err)
			}
		} else {
			if err := botanist.UnregisterAsSeed(); err != nil {
				o.Logger.Errorf("Could not unregister Shoot %q as Seed: %+v", o.Shoot.Info.Name, err)
			}
		}
	}

	o.Logger.Infof("Successfully reconciled Shoot %q", o.Shoot.Info.Name)
	return nil
}

func (c *defaultControl) updateShootStatusReconcile(o *operation.Operation, operationType gardenv1beta1.ShootLastOperationType, state gardenv1beta1.ShootLastOperationState) error {
	var (
		status = o.Shoot.Info.Status
		now    = metav1.Now()
	)

	if len(status.UID) == 0 {
		o.Shoot.Info.Status.UID = o.Shoot.Info.UID
	}
	if len(status.TechnicalID) == 0 {
		o.Shoot.Info.Status.TechnicalID = o.Shoot.SeedNamespace
	}

	o.Shoot.Info.Status.Conditions = nil
	o.Shoot.Info.Status.Gardener = *(o.GardenerInfo)
	o.Shoot.Info.Status.ObservedGeneration = o.Shoot.Info.Generation
	o.Shoot.Info.Status.LastOperation = &gardenv1beta1.LastOperation{
		Type:           operationType,
		State:          state,
		Progress:       1,
		Description:    "Reconciliation of Shoot cluster state in progress.",
		LastUpdateTime: now,
	}

	newShoot, err := c.updater.UpdateShootStatus(o.Shoot.Info)
	if err == nil {
		o.Shoot.Info = newShoot
	}
	return err
}

func (c *defaultControl) updateShootStatusResetRetry(o *operation.Operation, operationType gardenv1beta1.ShootLastOperationType) error {
	now := metav1.Now()
	o.Shoot.Info.Status.RetryCycleStartTime = &now
	return c.updateShootStatusReconcile(o, operationType, gardenv1beta1.ShootLastOperationStateError)
}

func (c *defaultControl) updateShootStatusReconcileStart(o *operation.Operation, operationType gardenv1beta1.ShootLastOperationType) error {
	var (
		status = o.Shoot.Info.Status
		now    = metav1.Now()
	)

	if status.RetryCycleStartTime == nil || o.Shoot.Info.Generation != o.Shoot.Info.Status.ObservedGeneration {
		o.Shoot.Info.Status.RetryCycleStartTime = &now
	}
	return c.updateShootStatusReconcile(o, operationType, gardenv1beta1.ShootLastOperationStateProcessing)
}

func (c *defaultControl) updateShootStatusReconcileSuccess(o *operation.Operation, operationType gardenv1beta1.ShootLastOperationType) error {
	o.Shoot.Info.Status.RetryCycleStartTime = nil
	o.Shoot.Info.Status.Seed = o.Seed.Info.Name
	o.Shoot.Info.Status.LastError = nil
	o.Shoot.Info.Status.LastOperation = &gardenv1beta1.LastOperation{
		Type:           operationType,
		State:          gardenv1beta1.ShootLastOperationStateSucceeded,
		Progress:       100,
		Description:    "Shoot cluster state has been successfully reconciled.",
		LastUpdateTime: metav1.Now(),
	}

	newShoot, err := c.updater.UpdateShootStatus(o.Shoot.Info)
	if err == nil {
		o.Shoot.Info = newShoot
	}
	return err
}

func (c *defaultControl) updateShootStatusReconcileError(o *operation.Operation, operationType gardenv1beta1.ShootLastOperationType, lastError *gardenv1beta1.LastError) (gardenv1beta1.ShootLastOperationState, error) {
	var (
		state         = gardenv1beta1.ShootLastOperationStateFailed
		description   = lastError.Description
		lastOperation = o.Shoot.Info.Status.LastOperation
		progress      = 1
	)

	if !utils.TimeElapsed(o.Shoot.Info.Status.RetryCycleStartTime, c.config.Controllers.Shoot.RetryDuration.Duration) {
		description += " Operation will be retried."
		state = gardenv1beta1.ShootLastOperationStateError
	} else {
		o.Shoot.Info.Status.RetryCycleStartTime = nil
	}

	if lastOperation != nil {
		progress = lastOperation.Progress
	}

	o.Shoot.Info.Status.LastError = lastError
	o.Shoot.Info.Status.LastOperation = &gardenv1beta1.LastOperation{
		Type:           operationType,
		State:          state,
		Progress:       progress,
		Description:    description,
		LastUpdateTime: metav1.Now(),
	}
	o.Shoot.Info.Status.Gardener = *(o.GardenerInfo)

	if newShoot, err := c.updater.UpdateShootStatus(o.Shoot.Info); err == nil {
		o.Shoot.Info = newShoot
	}

	newShootAfterLabel, err := c.updater.UpdateShootLabels(o.Shoot.Info, computeLabelsWithShootHealthiness(false))
	if err == nil {
		o.Shoot.Info = newShootAfterLabel
	}
	return state, err
}
