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

package servermaster

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/pingcap/log"
	pb "github.com/pingcap/tiflow/engine/enginepb"
	"github.com/pingcap/tiflow/engine/model"
	"github.com/pingcap/tiflow/engine/pkg/notifier"
	"github.com/pingcap/tiflow/engine/servermaster/resource"
	schedModel "github.com/pingcap/tiflow/engine/servermaster/scheduler/model"
	"github.com/pingcap/tiflow/engine/test"
	"github.com/pingcap/tiflow/pkg/errors"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// ExecutorManager defines an interface to manager all executors
type ExecutorManager interface {
	HandleHeartbeat(req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error)
	AllocateNewExec(req *pb.RegisterExecutorRequest) (*model.NodeInfo, error)
	RegisterExec(info *model.NodeInfo)
	// ExecutorCount returns executor count with given status
	ExecutorCount(status model.ExecutorStatus) int
	HasExecutor(executorID string) bool
	ListExecutors() []*model.NodeInfo
	GetAddr(executorID model.ExecutorID) (string, bool)
	Start(ctx context.Context)
	Stop()

	// WatchExecutors returns a snapshot of all online executors plus
	// a stream of events describing changes that happen to the executors
	// after the snapshot is taken.
	WatchExecutors(ctx context.Context) (
		snap map[model.ExecutorID]string, stream *notifier.Receiver[model.ExecutorStatusChange], err error,
	)

	// GetExecutorInfos implements the interface scheduler.executorInfoProvider.
	// It is called by the scheduler as the source of truth for executors.
	GetExecutorInfos() map[model.ExecutorID]schedModel.ExecutorInfo
}

// ExecutorManagerImpl holds all the executors info, including liveness, status, resource usage.
type ExecutorManagerImpl struct {
	testContext *test.Context
	wg          sync.WaitGroup

	mu        sync.Mutex
	executors map[model.ExecutorID]*Executor

	initHeartbeatTTL  time.Duration
	keepAliveInterval time.Duration

	rescMgr resource.RescMgr
	logRL   *rate.Limiter

	notifier *notifier.Notifier[model.ExecutorStatusChange]
}

// NewExecutorManagerImpl creates a new ExecutorManagerImpl instance
func NewExecutorManagerImpl(initHeartbeatTTL, keepAliveInterval time.Duration, ctx *test.Context) *ExecutorManagerImpl {
	return &ExecutorManagerImpl{
		testContext:       ctx,
		executors:         make(map[model.ExecutorID]*Executor),
		initHeartbeatTTL:  initHeartbeatTTL,
		keepAliveInterval: keepAliveInterval,
		rescMgr:           resource.NewCapRescMgr(),
		logRL:             rate.NewLimiter(rate.Every(time.Second*5), 1 /*burst*/),
		notifier:          notifier.NewNotifier[model.ExecutorStatusChange](),
	}
}

func (e *ExecutorManagerImpl) removeExecutorImpl(id model.ExecutorID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	log.Info("begin to remove executor", zap.String("id", string(id)))
	exec, ok := e.executors[id]
	if !ok {
		// This executor has been removed
		return errors.ErrUnknownExecutorID.GenWithStackByArgs(id)
	}
	addr := exec.Addr
	delete(e.executors, id)
	e.rescMgr.Unregister(id)
	log.Info("notify to offline exec")
	if test.GetGlobalTestFlag() {
		e.testContext.NotifyExecutorChange(&test.ExecutorChangeEvent{
			Tp:   test.Delete,
			Time: time.Now(),
		})
	}

	e.notifier.Notify(model.ExecutorStatusChange{
		ID:   id,
		Tp:   model.EventExecutorOffline,
		Addr: addr,
	})
	return nil
}

// HandleHeartbeat implements pb interface,
func (e *ExecutorManagerImpl) HandleHeartbeat(req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	if e.logRL.Allow() {
		log.Info("handle heart beat", zap.Stringer("req", req))
	}
	e.mu.Lock()
	execID := model.ExecutorID(req.ExecutorId)
	exec, ok := e.executors[execID]

	// executor not exists
	if !ok {
		e.mu.Unlock()
		err := errors.ErrUnknownExecutorID.FastGenByArgs(req.ExecutorId)
		return &pb.HeartbeatResponse{Err: errors.ToPBError(err)}, nil
	}
	e.mu.Unlock()

	status := model.ExecutorStatus(req.Status)
	if err := exec.heartbeat(req.Ttl, status); err != nil {
		return &pb.HeartbeatResponse{Err: errors.ToPBError(err)}, nil
	}
	usage := model.RescUnit(req.GetResourceUsage())
	if err := e.rescMgr.Update(execID, usage, usage, status); err != nil {
		return nil, err
	}
	resp := &pb.HeartbeatResponse{}
	return resp, nil
}

// RegisterExec registers executor to both executor manager and resource manager
func (e *ExecutorManagerImpl) RegisterExec(info *model.NodeInfo) {
	log.Info("register executor", zap.Any("info", info))
	exec := &Executor{
		NodeInfo:       *info,
		lastUpdateTime: time.Now(),
		heartbeatTTL:   e.initHeartbeatTTL,
		status:         model.Initing,
		logRL:          rate.NewLimiter(rate.Every(time.Second*5), 1 /*burst*/),
	}
	e.mu.Lock()
	e.executors[info.ID] = exec
	e.notifier.Notify(model.ExecutorStatusChange{
		ID:   info.ID,
		Tp:   model.EventExecutorOnline,
		Addr: info.Addr,
	})
	e.mu.Unlock()
	e.rescMgr.Register(exec.ID, exec.Addr, model.RescUnit(exec.Capability))
}

// AllocateNewExec allocates new executor info to a give RegisterExecutorRequest
// and then registers the executor.
func (e *ExecutorManagerImpl) AllocateNewExec(req *pb.RegisterExecutorRequest) (*model.NodeInfo, error) {
	executor := req.Executor
	log.Info("allocate new executor", zap.Stringer("executor", executor))

	e.mu.Lock()
	var executorID model.ExecutorID
	for {
		executorID = generateExecutorID(executor.GetName())
		if _, ok := e.executors[executorID]; !ok {
			break
		}
	}
	info := &model.NodeInfo{
		ID:         executorID,
		Addr:       executor.GetAddress(),
		Name:       executor.GetName(),
		Capability: int(executor.GetCapability()),
	}
	e.mu.Unlock()

	e.RegisterExec(info)
	return info, nil
}

func generateExecutorID(name string) model.ExecutorID {
	val := rand.Uint32()
	id := fmt.Sprintf("%s-%08x", name, val)
	return model.ExecutorID(id)
}

// HasExecutor implements ExecutorManager.HasExecutor
func (e *ExecutorManagerImpl) HasExecutor(executorID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.executors[model.ExecutorID(executorID)]
	return ok
}

// ListExecutors implements ExecutorManager.ListExecutors
func (e *ExecutorManagerImpl) ListExecutors() []*model.NodeInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	ret := make([]*model.NodeInfo, 0, len(e.executors))
	for _, exec := range e.executors {
		info := exec.NodeInfo
		ret = append(ret, &info)
	}
	return ret
}

// Executor records the status of an executor instance.
type Executor struct {
	model.NodeInfo
	status model.ExecutorStatus

	mu sync.RWMutex
	// Last heartbeat
	lastUpdateTime time.Time
	heartbeatTTL   time.Duration
	logRL          *rate.Limiter
}

func (e *Executor) checkAlive() bool {
	if e.logRL.Allow() {
		log.Info("check alive", zap.String("exec", string(e.NodeInfo.ID)))
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.status == model.Tombstone {
		return false
	}
	if e.lastUpdateTime.Add(e.heartbeatTTL).Before(time.Now()) {
		e.status = model.Tombstone
		return false
	}
	return true
}

func (e *Executor) heartbeat(ttl uint64, status model.ExecutorStatus) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.status == model.Tombstone {
		return errors.ErrTombstoneExecutor.FastGenByArgs(e.ID)
	}
	e.lastUpdateTime = time.Now()
	e.heartbeatTTL = time.Duration(ttl) * time.Millisecond
	e.status = status
	return nil
}

func (e *Executor) statusEqual(status model.ExecutorStatus) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.status == status
}

// Start implements ExecutorManager.Start. It starts a background goroutine to
// check whether all executors are alive periodically.
func (e *ExecutorManagerImpl) Start(ctx context.Context) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(e.keepAliveInterval)
		defer func() {
			ticker.Stop()
			log.Info("check executor alive finished")
		}()
		for {
			select {
			case <-ticker.C:
				err := e.checkAliveImpl()
				if err != nil {
					log.Info("check alive meet error", zap.Error(err))
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop implements ExecutorManager.Stop
func (e *ExecutorManagerImpl) Stop() {
	e.wg.Wait()
	e.notifier.Close()
}

func (e *ExecutorManagerImpl) checkAliveImpl() error {
	e.mu.Lock()
	for id, exec := range e.executors {
		if !exec.checkAlive() {
			e.mu.Unlock()
			err := e.removeExecutorImpl(id)
			return err
		}
	}
	e.mu.Unlock()
	return nil
}

// ExecutorCount implements ExecutorManager.ExecutorCount
func (e *ExecutorManagerImpl) ExecutorCount(status model.ExecutorStatus) (count int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, executor := range e.executors {
		if executor.statusEqual(status) {
			count++
		}
	}
	return
}

// GetAddr implements ExecutorManager.GetAddr
func (e *ExecutorManagerImpl) GetAddr(executorID model.ExecutorID) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	executor, exists := e.executors[executorID]
	if !exists {
		return "", false
	}

	return executor.Addr, true
}

// WatchExecutors implements the ExecutorManager interface.
func (e *ExecutorManagerImpl) WatchExecutors(
	ctx context.Context,
) (snap map[model.ExecutorID]string, receiver *notifier.Receiver[model.ExecutorStatusChange], err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	snap = make(map[model.ExecutorID]string, len(e.executors))
	for executorID, exec := range e.executors {
		snap[executorID] = exec.Addr
	}

	if err := e.notifier.Flush(ctx); err != nil {
		return nil, nil, err
	}

	receiver = e.notifier.NewReceiver()
	return
}

// GetExecutorInfos returns necessary information on the executor that
// is needed for scheduling.
func (e *ExecutorManagerImpl) GetExecutorInfos() map[model.ExecutorID]schedModel.ExecutorInfo {
	e.mu.Lock()
	defer e.mu.Unlock()

	ret := make(map[model.ExecutorID]schedModel.ExecutorInfo, len(e.executors))
	for id, exec := range e.executors {
		resStatus, ok := e.rescMgr.CapacityForExecutor(id)
		if !ok {
			continue
		}
		schedInfo := schedModel.ExecutorInfo{
			ID:             id,
			ResourceStatus: *resStatus,
			Labels:         exec.Labels,
		}
		ret[id] = schedInfo
	}
	return ret
}
