// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"encoding/json"
	"time"

	"github.com/pingcap/errors"
	"go.etcd.io/etcd/client/pkg/v3/logutil"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"

	"github.com/pingcap/log"
	pb "github.com/pingcap/tiflow/engine/enginepb"
	"github.com/pingcap/tiflow/engine/pkg/client"
	"github.com/pingcap/tiflow/engine/pkg/config"
	"github.com/pingcap/tiflow/engine/pkg/meta"
	metaModel "github.com/pingcap/tiflow/engine/pkg/meta/model"
	cerrors "github.com/pingcap/tiflow/pkg/errors"
)

// MetastoreManager maintains all metastore clients we need.
// Except for ServiceDiscoveryStore, FrameworkStore and BusinessClientConn,
// a MetastoreManager is not thread-safe.
//
// TODO refactor some code repetition together with servermaster.MetaStoreManager,
// and add integration tests between MetastoreManager in this file and servermaster.MetaStoreManager.
type MetastoreManager interface {
	// Init fetches metastore configurations from Servermaster and
	// creates the necessary client.
	// Init is made part of the interface because the interface is intended
	// to reflect the dependency between the objects during server initialization.
	// NOTE: Init must be called before other methods can be.
	Init(ctx context.Context, discoveryClient client.DiscoveryClient) error
	IsInitialized() bool
	Close()

	ServiceDiscoveryStore() *clientv3.Client
	FrameworkClientConn() metaModel.ClientConn
	BusinessClientConn() metaModel.ClientConn
}

// NewMetastoreManager returns a new MetastoreManager.
// Note that Init() should be called first before using it.
func NewMetastoreManager() MetastoreManager {
	return &metastoreManagerImpl{
		creator: metastoreCreatorImpl{},
	}
}

// metastoreManagerImpl implements MetastoreManager.
// We make the implementation private because it
// is the only one implementation used in production code.
type metastoreManagerImpl struct {
	initialized atomic.Bool

	serviceDiscoveryStore *clientv3.Client
	frameworkClientConn   metaModel.ClientConn
	businessClientConn    metaModel.ClientConn

	creator MetastoreCreator
}

// MetastoreCreator abstracts creation behavior of the various
// metastore clients.
type MetastoreCreator interface {
	CreateEtcdCliForServiceDiscovery(
		ctx context.Context, params metaModel.StoreConfig,
	) (*clientv3.Client, error)

	CreateClientConnForBusiness(
		ctx context.Context, params metaModel.StoreConfig,
	) (metaModel.ClientConn, error)

	CreateClientConnForFramework(
		ctx context.Context, params metaModel.StoreConfig,
	) (metaModel.ClientConn, error)
}

type metastoreCreatorImpl struct{}

func (c metastoreCreatorImpl) CreateEtcdCliForServiceDiscovery(
	ctx context.Context, params metaModel.StoreConfig,
) (*clientv3.Client, error) {
	logConfig := logutil.DefaultZapLoggerConfig
	logConfig.Level = zap.NewAtomicLevelAt(zapcore.ErrorLevel)
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:        params.Endpoints,
		Context:          ctx,
		LogConfig:        &logConfig,
		DialTimeout:      config.ServerMasterEtcdDialTimeout,
		AutoSyncInterval: config.ServerMasterEtcdSyncInterval,
		DialOptions: []grpc.DialOption{
			grpc.WithInsecure(),
			grpc.WithBlock(),
			grpc.WithConnectParams(grpc.ConnectParams{
				Backoff: backoff.Config{
					BaseDelay:  time.Second,
					Multiplier: 1.1,
					Jitter:     0.1,
					MaxDelay:   3 * time.Second,
				},
				MinConnectTimeout: 3 * time.Second,
			}),
		},
	})
	if err != nil {
		return nil, cerrors.ErrExecutorEtcdConnFail.Wrap(err)
	}
	return etcdCli, nil
}

func (c metastoreCreatorImpl) CreateClientConnForBusiness(
	_ context.Context, params metaModel.StoreConfig,
) (metaModel.ClientConn, error) {
	cc, err := meta.NewClientConn(&params)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return cc, nil
}

func (c metastoreCreatorImpl) CreateClientConnForFramework(
	_ context.Context, params metaModel.StoreConfig,
) (metaModel.ClientConn, error) {
	cc, err := meta.NewClientConn(&params)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return cc, err
}

func (m *metastoreManagerImpl) Init(ctx context.Context, discoveryClient client.DiscoveryClient) (retErr error) {
	if m.initialized.Load() {
		log.Panic("MetastoreManager: double Init")
	}

	defer func() {
		// Close all store clients in case the final return value
		// is not nil.
		if retErr != nil {
			m.Close()
		}
	}()

	// TODO We will refactor similar code segments together with servermaster.MetaStoreManager.
	if err := m.initServerDiscoveryStore(ctx, discoveryClient); err != nil {
		return err
	}

	if err := m.initFrameworkStore(ctx, discoveryClient); err != nil {
		return err
	}

	if err := m.initBusinessStore(ctx, discoveryClient); err != nil {
		return err
	}

	m.initialized.Store(true)
	return nil
}

func (m *metastoreManagerImpl) IsInitialized() bool {
	return m.initialized.Load()
}

func (m *metastoreManagerImpl) initServerDiscoveryStore(ctx context.Context, discoveryClient client.DiscoveryClient) error {
	// Query service discovery metastore endpoints.
	resp, err := discoveryClient.QueryMetaStore(
		ctx,
		&pb.QueryMetaStoreRequest{Tp: pb.StoreType_ServiceDiscovery})
	if err != nil {
		return errors.Trace(err)
	}
	log.Info("Obtained discovery metastore endpoint", zap.String("addr", resp.Address))

	conf := parseStoreConfig([]byte(resp.Address))
	etcdCli, err := m.creator.CreateEtcdCliForServiceDiscovery(ctx, conf)
	if err != nil {
		return err
	}
	m.serviceDiscoveryStore = etcdCli
	return nil
}

func (m *metastoreManagerImpl) initFrameworkStore(ctx context.Context, discoveryClient client.DiscoveryClient) error {
	// Query framework metastore endpoints.
	resp, err := discoveryClient.QueryMetaStore(
		ctx,
		&pb.QueryMetaStoreRequest{Tp: pb.StoreType_SystemMetaStore})
	if err != nil {
		return errors.Trace(err)
	}
	log.Info("Obtained framework metastore endpoint", zap.String("addr", resp.Address))

	conf := parseStoreConfig([]byte(resp.Address))
	cc, err := m.creator.CreateClientConnForFramework(ctx, conf)
	if err != nil {
		return err
	}

	m.frameworkClientConn = cc
	return nil
}

func (m *metastoreManagerImpl) initBusinessStore(ctx context.Context, discoveryClient client.DiscoveryClient) error {
	// fetch business metastore connection endpoint
	resp, err := discoveryClient.QueryMetaStore(
		ctx,
		&pb.QueryMetaStoreRequest{Tp: pb.StoreType_AppMetaStore})
	if err != nil {
		return err
	}
	log.Info("Obtained business metastore endpoint", zap.String("addr", resp.Address))

	conf := parseStoreConfig([]byte(resp.Address))
	cc, err := m.creator.CreateClientConnForBusiness(ctx, conf)
	if err != nil {
		return err
	}

	m.businessClientConn = cc
	return nil
}

func (m *metastoreManagerImpl) ServiceDiscoveryStore() *clientv3.Client {
	if !m.initialized.Load() {
		log.Panic("ServiceDiscoveryStore is called before Init is successful")
	}
	return m.serviceDiscoveryStore
}

func (m *metastoreManagerImpl) FrameworkClientConn() metaModel.ClientConn {
	if !m.initialized.Load() {
		log.Panic("FrameworkStore is called before Init is successful")
	}
	return m.frameworkClientConn
}

func (m *metastoreManagerImpl) BusinessClientConn() metaModel.ClientConn {
	if !m.initialized.Load() {
		log.Panic("BusinessClientConn is called before Init is successful")
	}
	return m.businessClientConn
}

func (m *metastoreManagerImpl) Close() {
	if m.serviceDiscoveryStore != nil {
		_ = m.serviceDiscoveryStore.Close()
		m.serviceDiscoveryStore = nil
	}

	if m.frameworkClientConn != nil {
		_ = m.frameworkClientConn.Close()
		m.frameworkClientConn = nil
	}

	if m.businessClientConn != nil {
		_ = m.businessClientConn.Close()
		m.businessClientConn = nil
	}

	log.Info("MetastoreManager: Closed all metastores")
}

func parseStoreConfig(rawBytes []byte) metaModel.StoreConfig {
	var conf metaModel.StoreConfig

	// Try unmarshal as json first.
	err := json.Unmarshal(rawBytes, &conf)
	if err == nil {
		return conf
	}

	log.Info("Could not unmarshal metastore config, fallback to treating it as an endpoint list",
		zap.ByteString("raw-bytes", rawBytes))

	conf.SetEndpoints(string(rawBytes))
	return conf
}
