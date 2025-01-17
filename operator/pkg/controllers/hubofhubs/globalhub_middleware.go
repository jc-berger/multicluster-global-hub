/*
Copyright 2023.

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

package hubofhubs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stolostron/multicluster-global-hub/operator/apis/v1alpha4"
	globalhubv1alpha4 "github.com/stolostron/multicluster-global-hub/operator/apis/v1alpha4"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/config"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/deployer"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/postgres"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/renderer"
	transportprotocol "github.com/stolostron/multicluster-global-hub/operator/pkg/transporter"
	operatorutils "github.com/stolostron/multicluster-global-hub/operator/pkg/utils"
	"github.com/stolostron/multicluster-global-hub/pkg/constants"
	"github.com/stolostron/multicluster-global-hub/pkg/transport"
	"github.com/stolostron/multicluster-global-hub/pkg/utils"
)

// ReconcileMiddleware creates the kafka and postgres if needed.
// 1. create the kafka and postgres subscription at the same time
// 2. then create the kafka and postgres resources at the same time
// 3. wait for kafka and postgres ready
func (r *MulticlusterGlobalHubReconciler) ReconcileMiddleware(ctx context.Context, mgh *v1alpha4.MulticlusterGlobalHub,
) (ctrl.Result, error) {
	// initialize postgres and kafka at the same time
	var wg sync.WaitGroup

	errorChan := make(chan error, 2)
	// initialize transport
	wg.Add(1)
	go func() {
		defer wg.Done()
		transProtocol, err := detectTransportProtocol(ctx, r.Client)
		if err != nil {
			errorChan <- err
			return
		}

		if transProtocol == transport.SecretTransporter {
			conn, e := r.ReconcileTransport(ctx, mgh, transProtocol)
			if e != nil {
				errorChan <- e
			}
			r.MiddlewareConfig.TransportConn = conn
			return
		}

		// strimzi transporter -> use the kafka reconiler to get the connection
		// wait until the kafka crd is ready, then start the kafka reconciler
		if !r.KafkaInit {
			_, e := r.ReconcileTransport(ctx, mgh, transProtocol)
			if e != nil {
				errorChan <- e
				return
			}

			err = addKafkaCRDController(r.Manager, r)
			if err != nil {
				errorChan <- err
				return
			}

			r.KafkaInit = true
		}
		if r.KafkaController == nil || r.KafkaController.conn == nil {
			errorChan <- errors.New("the kafka controller is not ready")
			return
		}
		r.MiddlewareConfig.TransportConn = r.KafkaController.conn
	}()

	// initialize storage
	wg.Add(1)
	go func() {
		defer wg.Done()

		conn, e := r.ReconcileStorage(ctx, mgh)
		if e != nil {
			errorChan <- e
		}
		r.MiddlewareConfig.StorageConn = conn
	}()

	go func() {
		wg.Wait()
		close(errorChan)
	}()

	for err := range errorChan {
		if err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("middleware not ready, Error: %v", err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *MulticlusterGlobalHubReconciler) ReconcileTransport(ctx context.Context, mgh *v1alpha4.MulticlusterGlobalHub,
	transProtocol transport.TransportProtocol,
) (*transport.ConnCredential, error) {
	// apply kafka metric resources
	err := r.renderKafkaMetricsResources(mgh)
	if err != nil {
		return nil, err
	}

	// create the transport instance
	var trans transport.Transporter
	switch transProtocol {
	case transport.StrimziTransporter:
		trans, err = transportprotocol.NewStrimziTransporter(
			r.Client,
			mgh,
			transportprotocol.WithContext(ctx),
			transportprotocol.WithCommunity(operatorutils.IsCommunityMode()),
		)
		if err != nil {
			return nil, err
		}
	case transport.SecretTransporter:
		trans = transportprotocol.NewBYOTransporter(ctx, types.NamespacedName{
			Namespace: mgh.Namespace,
			Name:      constants.GHTransportSecretName,
		}, r.Client)
	}

	// create the user to connect the transport instance
	err = trans.CreateUser(transportprotocol.DefaultGlobalHubKafkaUser)
	if err != nil {
		return nil, err
	}
	// create global hub topics, create the status.global, spec and event topics
	// it's a placeholder for the manager to subscribe the `^status.*`
	topics := trans.GenerateClusterTopic(transportprotocol.GlobalHubClusterName)
	err = trans.CreateTopic(topics)
	if err != nil {
		return nil, err
	}
	// grant permission: read - status,event, write - spec;
	err = trans.GrantRead(transportprotocol.DefaultGlobalHubKafkaUser, topics.EventTopic)
	if err != nil {
		return nil, err
	}
	err = trans.GrantRead(transportprotocol.DefaultGlobalHubKafkaUser, topics.StatusTopic)
	if err != nil {
		return nil, err
	}
	err = trans.GrantWrite(transportprotocol.DefaultGlobalHubKafkaUser, topics.SpecTopic)
	if err != nil {
		return nil, err
	}

	var conn *transport.ConnCredential
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			conn, err = trans.GetConnCredential(transportprotocol.DefaultGlobalHubKafkaUser)
			if err != nil {
				r.Log.Info("waiting the kafka connection credential to be ready...", "message", err.Error())
				return false, err
			}
			return true, nil
		})
	if trans != nil {
		config.SetTransporter(trans)
	}
	return conn, err
}

func (r *MulticlusterGlobalHubReconciler) ReconcileStorage(ctx context.Context, mgh *v1alpha4.MulticlusterGlobalHub,
) (*postgres.PostgresConnection, error) {
	// support BYO postgres
	pgConnection, err := config.GetPGConnectionFromGHStorageSecret(ctx, r.Client)
	if err == nil {
		return pgConnection, nil
	} else if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}

	// then the storage secret is not found
	// if not-provided postgres secret, create crunchy postgres operator by subscription
	if config.GetInstallCrunchyOperator(mgh) {
		if err := r.EnsureCrunchyPostgresSubscription(ctx, mgh); err != nil {
			return nil, err
		}
	} else {
		// create the statefulset postgres and initialize the r.MiddlewareConfig.PgConnection
		pgConnection, err = r.InitPostgresByStatefulset(ctx, mgh)
		if err != nil {
			return nil, err
		}
	}

	if pgConnection == nil && config.GetInstallCrunchyOperator(mgh) {
		if err := r.EnsureCrunchyPostgres(ctx); err != nil {
			return nil, err
		}
	}

	if pgConnection == nil && config.GetInstallCrunchyOperator(mgh) {
		// store crunchy postgres connection
		err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 10*time.Minute, true,
			func(ctx context.Context) (bool, error) {
				if err := r.EnsureCrunchyPostgres(ctx); err != nil {
					r.Log.Info("waiting the postgres cluster to be ready...", "message", err.Error())
					return false, nil
				}

				pgConnection, err = config.GetPGConnectionFromBuildInPostgres(ctx, r.Client)
				if err != nil {
					r.Log.Info("waiting the postgres connection credential to be ready...", "message", err.Error())
					return false, nil
				}
				return true, nil
			})
	}
	return pgConnection, nil
}

func detectTransportProtocol(ctx context.Context, runtimeClient client.Client) (transport.TransportProtocol, error) {
	// get the transport secret
	kafkaSecret := &corev1.Secret{}
	err := runtimeClient.Get(ctx, types.NamespacedName{
		Name:      constants.GHTransportSecretName,
		Namespace: utils.GetDefaultNamespace(),
	}, kafkaSecret)
	if err == nil {
		return transport.SecretTransporter, nil
	}
	if !apierrors.IsNotFound(err) {
		return transport.SecretTransporter, err
	}

	// the transport secret is not found
	return transport.StrimziTransporter, nil
}

// renderKafkaMetricsResources renders the kafka podmonitor and metrics
func (r *MulticlusterGlobalHubReconciler) renderKafkaMetricsResources(
	mgh *globalhubv1alpha4.MulticlusterGlobalHub,
) error {
	if mgh.Spec.EnableMetrics {
		// render the kafka objects
		kafkaRenderer, kafkaDeployer := renderer.NewHoHRenderer(fs), deployer.NewHoHDeployer(r.Client)
		kafkaObjects, err := kafkaRenderer.Render("manifests/kafka", "",
			func(profile string) (interface{}, error) {
				return struct {
					Namespace string
				}{
					Namespace: utils.GetDefaultNamespace(),
				}, nil
			})
		if err != nil {
			return fmt.Errorf("failed to render kafka manifests: %w", err)
		}
		// create restmapper for deployer to find GVR
		dc, err := discovery.NewDiscoveryClientForConfig(r.Manager.GetConfig())
		if err != nil {
			return err
		}
		mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))

		if err = manipulateObj(kafkaObjects, mgh, kafkaDeployer, mapper, r.Manager.GetScheme()); err != nil {
			return fmt.Errorf("failed to create/update kafka objects: %w", err)
		}
	}
	return nil
}
