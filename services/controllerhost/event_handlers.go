// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package controllerhost

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/uber/cherami-client-go/common/backoff"
	"github.com/uber/cherami-server/.generated/go/admin"
	m "github.com/uber/cherami-server/.generated/go/metadata"
	"github.com/uber/cherami-server/.generated/go/shared"
	"github.com/uber/cherami-server/.generated/go/store"
	"github.com/uber/cherami-server/common"
	"github.com/uber/cherami-server/common/metrics"
	"github.com/pborman/uuid"
	"github.com/uber-common/bark"
	"github.com/uber/tchannel-go/thrift"
)

type (
	eventBase struct{}

	// ExtentCreatedEvent is generated by
	// the creation of a new extent. The
	// action will be to schedul notifications to
	// concerned input hosts.
	ExtentCreatedEvent struct {
		eventBase
		dstID    string
		inHostID string
		extentID string
		storeIDs []string
	}
	// ConsGroupUpdatedEvent is generated when
	// a new extent is available to the consumer
	// group for consumption. Action will be to
	// schedule notification to the concerned output hosts
	ConsGroupUpdatedEvent struct {
		eventBase
		dstID        string
		consGroupID  string
		extentID     string
		outputHostID string
	}
	// InputHostNotificationEvent is generated
	// to notify input hosts about a new extent
	InputHostNotificationEvent struct {
		eventBase
		dstID            string
		inputHostID      string
		extentID         string
		storeIDs         []string
		reason           string
		reasonContext    string
		notificationType admin.NotificationType
	}
	// OutputHostNotificationEvent is generated to
	// notify output hosts about a new extent
	OutputHostNotificationEvent struct {
		eventBase
		dstID            string
		consGroupID      string
		outputHostID     string
		reason           string
		reasonContext    string
		notificationType admin.NotificationType
	}
	// ExtentDownEvent is triggered whenever
	// an extent becomes unreachable and needs
	// to be Sealed
	ExtentDownEvent struct {
		eventBase
		state    int // event handler state, used so that we can retry this handler
		sealSeq  int64
		dstID    string
		extentID string
		storeIDs []string
	}

	// StoreExtentStatusOutOfSyncEvent is triggered
	// when one of the extent replicas (store)
	// is out of sync with others i.e. the
	// extent is SEALED but one of the stores
	// still reports it as OPEN
	StoreExtentStatusOutOfSyncEvent struct {
		eventBase
		dstID         string
		extentID      string
		storeID       string
		desiredStatus shared.ExtentStatus
	}

	// RemoteZoneExtentCreatedEvent is triggered
	// when a remote zone extent is created
	RemoteZoneExtentCreatedEvent struct {
		eventBase
		dstID    string
		extentID string
		storeIDs []string
	}

	// InputHostFailedEvent is triggered
	// when an input host fails
	InputHostFailedEvent struct {
		eventBase
		hostUUID string
	}
	// StoreHostFailedEvent is triggered
	// when a store host fails
	StoreHostFailedEvent struct {
		eventBase
		hostUUID string
	}
)

// ExtentDownEvent States
const (
	checkPreconditionState = iota
	sealExtentState
	updateMetadataState
	doneState
)

// how long from now are we willing to wait
// for the cache to refresh itself ?
const resultCacheRefreshMaxWaitTime = int64(500 * time.Millisecond)

var (
	sealExtentInitialCallTimeout = 2 * time.Second
	sealExtentRetryCallTimeout   = 20 * time.Second
	replicateExtentCallTimeout   = 20 * time.Second
)

// this is the list of "reasons" for notifications sent to outputhost/inputhost
const (
	notifyExtentCreated    = "ExtentCreated"
	notifyExtentRepaired   = "ExtentRepaired"
	notifyCGExtUpdated     = "CGExtUpdated"
	notifyDLQMergedExtents = "DLQMergedExtents"
	notifyCGDeleted        = "CGDeleted"
)

// Done provides default callback for all events
func (event *eventBase) Done(context *Context, err error) {}

// Handle provides default implementation for all events
func (event *eventBase) Handle(context *Context) error { return nil }

// NewExtentCreatedEvent creates and returns a ExtentCreatedEvent
func NewExtentCreatedEvent(dstID string, inHostID string, extentID string, storeIDs []string) Event {
	return &ExtentCreatedEvent{
		dstID:    dstID,
		inHostID: inHostID,
		extentID: extentID,
		storeIDs: storeIDs,
	}
}

// NewConsGroupUpdatedEvent creates and returns a ConsGroupUpdatedEvent
func NewConsGroupUpdatedEvent(dstID, consGroupID, extentID, outputHostID string) Event {
	return &ConsGroupUpdatedEvent{
		dstID:        dstID,
		consGroupID:  consGroupID,
		extentID:     extentID,
		outputHostID: outputHostID,
	}
}

// NewInputHostNotificationEvent creates and returns a InputHostNotificationEvent
func NewInputHostNotificationEvent(dstID, inputHostID, extentID string, storeIDs []string, reason, reasonContext string, notificationType admin.NotificationType) Event {
	return &InputHostNotificationEvent{
		dstID:            dstID,
		inputHostID:      inputHostID,
		extentID:         extentID,
		storeIDs:         storeIDs,
		reason:           reason,
		reasonContext:    reasonContext,
		notificationType: notificationType,
	}
}

// NewOutputHostNotificationEvent creates and returns a OutputHostNotificationEvent
func NewOutputHostNotificationEvent(dstID, consGroupID, outputHostID, reason, reasonContext string, notificationType admin.NotificationType) Event {
	return &OutputHostNotificationEvent{
		dstID:            dstID,
		consGroupID:      consGroupID,
		outputHostID:     outputHostID,
		reason:           reason,
		reasonContext:    reasonContext,
		notificationType: notificationType,
	}
}

// NewStoreExtentStatusOutOfSyncEvent creates and returns a NewStoreExtentStatusOutOfSyncEvent
func NewStoreExtentStatusOutOfSyncEvent(dstID string, extentID string, storeID string, desiredStatus shared.ExtentStatus) Event {
	return &StoreExtentStatusOutOfSyncEvent{
		dstID:         dstID,
		extentID:      extentID,
		storeID:       storeID,
		desiredStatus: desiredStatus,
	}
}

// NewRemoteZoneExtentCreatedEvent creates and returns a RemoteZoneExtentCreatedEvent
func NewRemoteZoneExtentCreatedEvent(dstID string, extentID string, storeIDs []string) Event {
	return &RemoteZoneExtentCreatedEvent{
		dstID:    dstID,
		extentID: extentID,
		storeIDs: storeIDs,
	}
}

// NewExtentDownEvent creates and returns an ExtentDownEvent
func NewExtentDownEvent(sealSeq int64, dstID string, extentID string) Event {
	return &ExtentDownEvent{
		sealSeq:  sealSeq,
		dstID:    dstID,
		extentID: extentID,
	}
}

// NewInputHostFailedEvent creates and returns a InputHostFailedEvent
func NewInputHostFailedEvent(hostUUID string) Event {
	return &InputHostFailedEvent{hostUUID: hostUUID}
}

// NewStoreHostFailedEvent creates and returns a StoreHostFailedEvent
func NewStoreHostFailedEvent(hostUUID string) Event {
	return &StoreHostFailedEvent{hostUUID: hostUUID}
}

// Handle handles the creation of a new extent.
// Following are the async actions to be triggered on creation of an extent:
//    a. For every input host that serves a open extent for the DST
// 			1. Addd a InputHostNotificationEvent to reconfigure its clients
//	  b. For the input host that serves the newly created extent
//			1. Add a InputHostNotificationEvent to reconfigure ALL
func (event *ExtentCreatedEvent) Handle(context *Context) error {

	sw := context.m3Client.StartTimer(metrics.ExtentCreatedEventScope, metrics.ControllerLatencyTimer)
	defer sw.Stop()
	context.m3Client.IncCounter(metrics.ExtentCreatedEventScope, metrics.ControllerRequests)
	mm := context.mm
	// InputHost Notification
	inHostIDs := make(map[string]bool)
	inHostIDs[event.inHostID] = true
	// Notify all in hosts handling open extents for this destination
	filterBy := []shared.ExtentStatus{shared.ExtentStatus_OPEN}
	stats, err := mm.ListExtentsByDstIDStatus(event.dstID, filterBy)
	if err == nil {
		for _, stat := range stats {
			inHostIDs[stat.GetExtent().GetInputHostUUID()] = true
		}
	} else {
		context.m3Client.IncCounter(metrics.ExtentCreatedEventScope, metrics.ControllerErrMetadataReadCounter)
		context.log.WithField(common.TagErr, err).Error(`ListExtents failed, not all input hosts can be notified about new extent`)
	}

	notifyEvent := NewInputHostNotificationEvent(event.dstID, event.inHostID, event.extentID, event.storeIDs, notifyExtentCreated, event.extentID, admin.NotificationType_ALL)
	if !context.eventPipeline.Add(notifyEvent) {
		context.m3Client.IncCounter(metrics.ExtentCreatedEventScope, metrics.ControllerFailures)
		context.log.WithFields(bark.Fields{
			common.TagExt: common.FmtExt(event.extentID),
			common.TagIn:  common.FmtIn(event.inHostID),
		}).Error("ExtentCreatedEvent: Failed to enqueue InputHostNotificationEvent")
		return nil
	}

	delete(inHostIDs, event.inHostID)

	for k := range inHostIDs {
		notifyEvent = NewInputHostNotificationEvent(event.dstID, k, event.extentID, event.storeIDs, notifyExtentCreated, event.extentID, admin.NotificationType_CLIENT)
		succ := context.eventPipeline.Add(notifyEvent)
		if !succ {
			context.m3Client.IncCounter(metrics.ExtentCreatedEventScope, metrics.ControllerFailures)
			context.log.WithFields(bark.Fields{
				common.TagExt: common.FmtExt(event.extentID),
				common.TagIn:  common.FmtIn(k),
			}).Error("ExtentCreatedEvent: Failed to enqueue InputHostNotificationEvent")
		}
	}

	// Notify all output hosts serving this destination to force the
	// consumers to re-connect and consume from the new extent
	reconfigureAllConsumers(context, event.dstID, event.extentID, notifyExtentCreated, event.extentID, metrics.ExtentCreatedEventScope)

	return nil
}

// Handle schedules output host notifications
func (event *ConsGroupUpdatedEvent) Handle(context *Context) error {

	sw := context.m3Client.StartTimer(metrics.ConsGroupUpdatedEventScope, metrics.ControllerLatencyTimer)
	defer sw.Stop()
	context.m3Client.IncCounter(metrics.ConsGroupUpdatedEventScope, metrics.ControllerRequests)

	mm := context.mm

	outHostIDs := make(map[string]bool)
	outHostIDs[event.outputHostID] = true

	filterBy := []m.ConsumerGroupExtentStatus{m.ConsumerGroupExtentStatus_OPEN}
	cgExtents, err := mm.ListExtentsByConsumerGroup(event.dstID, event.consGroupID, filterBy)
	if err == nil {
		for _, cge := range cgExtents {
			outHostIDs[cge.GetOutputHostUUID()] = true
		}
	} else {
		context.m3Client.IncCounter(metrics.ConsGroupUpdatedEventScope, metrics.ControllerErrMetadataReadCounter)
	}

	notifyEvent := NewOutputHostNotificationEvent(event.dstID, event.consGroupID, event.outputHostID, notifyCGExtUpdated, event.extentID, admin.NotificationType_ALL)
	if !context.eventPipeline.Add(notifyEvent) {
		context.log.WithFields(bark.Fields{
			common.TagCnsm: common.FmtCnsm(event.consGroupID),
			common.TagOut:  common.FmtOut(event.outputHostID),
			common.TagExt:  common.FmtExt(event.extentID),
		}).Error("ConsGroupUpdatedEvent: Failed to enqueue OutputHostNotificationEvent")
	}

	delete(outHostIDs, event.outputHostID)

	for k := range outHostIDs {
		notifyEvent = NewOutputHostNotificationEvent(event.dstID, event.consGroupID, k, notifyCGExtUpdated, event.extentID, admin.NotificationType_CLIENT)
		if !context.eventPipeline.Add(notifyEvent) {
			context.log.WithFields(bark.Fields{
				common.TagCnsm: common.FmtCnsm(event.consGroupID),
				common.TagOut:  common.FmtOut(k),
				common.TagExt:  common.FmtExt(event.extentID),
			}).Error("ConsGroupUpdatedEvent: Failed to enqueue OutputHostNotificationEvent")
		}
	}

	return nil
}

const (
	retryInitialInterval = 500 * time.Millisecond
	retryMaxInterval     = 2 * time.Second
	retryExpiryInterval  = 1 * time.Minute
	thriftCallTimeout    = 10 * time.Second
	retryMaxAttempts     = 3
)

// Handle sends notification to an input host
func (event *InputHostNotificationEvent) Handle(context *Context) error {

	sw := context.m3Client.StartTimer(metrics.InputNotifyEventScope, metrics.ControllerLatencyTimer)
	defer sw.Stop()
	context.m3Client.IncCounter(metrics.InputNotifyEventScope, metrics.ControllerRequests)

	addr, err := context.rpm.ResolveUUID(common.InputServiceName, event.inputHostID)
	if err != nil {
		context.m3Client.IncCounter(metrics.InputNotifyEventScope, metrics.ControllerFailures)
		context.m3Client.IncCounter(metrics.InputNotifyEventScope, metrics.ControllerErrResolveUUIDCounter)
		context.log.WithField(common.TagIn, event.inputHostID).Debug(`Cannot send notification, failed to resolve inputhost uuid`)
		return nil
	}

	adminClient, err := common.CreateInputHostAdminClient(context.channel, addr)
	if err != nil {
		context.m3Client.IncCounter(metrics.InputNotifyEventScope, metrics.ControllerFailures)
		context.m3Client.IncCounter(metrics.InputNotifyEventScope, metrics.ControllerErrCreateTChanClientCounter)
		context.log.WithField(common.TagErr, err).Error(`Failed to create input host client`)
		return nil
	}

	update := &admin.DestinationUpdatedNotification{
		DestinationUUID: common.StringPtr(event.dstID),
		Type:            common.AdminNotificationTypePtr(event.notificationType),
		ExtentUUID:      common.StringPtr(event.extentID),
		StoreIds:        event.storeIDs,
	}

	req := &admin.DestinationsUpdatedRequest{
		UpdateUUID: common.StringPtr(uuid.New()),
		Updates:    []*admin.DestinationUpdatedNotification{update},
	}

	updateOp := func() error {
		ctx, cancel := thrift.NewContext(thriftCallTimeout)
		defer cancel()
		return adminClient.DestinationsUpdated(ctx, req)
	}

	context.log.WithFields(bark.Fields{
		common.TagDst:        common.FmtDst(event.dstID),
		common.TagExt:        common.FmtExt(event.extentID),
		`notifyType`:         update.GetType(),
		`reason`:             event.reason,
		`context`:            event.reasonContext,
		common.TagIn:         common.FmtIn(event.inputHostID),
		common.TagUpdateUUID: req.GetUpdateUUID(),
	}).Info("InputHostNotificationEvent: Sending notification to inputhost")

	err = backoff.Retry(updateOp, notificationRetryPolicy(), common.IsRetryableTChanErr)
	if err != nil {
		context.m3Client.IncCounter(metrics.InputNotifyEventScope, metrics.ControllerFailures)
		context.log.WithFields(bark.Fields{
			common.TagDst:        common.FmtDst(event.dstID),
			common.TagExt:        common.FmtExt(event.extentID),
			`notifyType`:         update.GetType(),
			`reason`:             event.reason,
			`context`:            event.reasonContext,
			common.TagIn:         common.FmtIn(event.inputHostID),
			common.TagUpdateUUID: req.GetUpdateUUID(),
			`hostaddr`:           addr,
			`error`:              err,
		}).Error("InputHostNotificationEvent: Failed to send notification to inputhost")
	}

	return nil
}

// Handle sends notification to an output host
func (event *OutputHostNotificationEvent) Handle(context *Context) error {
	sw := context.m3Client.StartTimer(metrics.OutputNotifyEventScope, metrics.ControllerLatencyTimer)
	defer sw.Stop()

	context.m3Client.IncCounter(metrics.OutputNotifyEventScope, metrics.ControllerRequests)

	addr, err := context.rpm.ResolveUUID(common.OutputServiceName, event.outputHostID)
	if err != nil {
		context.m3Client.IncCounter(metrics.OutputNotifyEventScope, metrics.ControllerFailures)
		context.m3Client.IncCounter(metrics.OutputNotifyEventScope, metrics.ControllerErrResolveUUIDCounter)
		context.log.WithFields(bark.Fields{
			common.TagOut: event.outputHostID,
			common.TagErr: err,
		}).Debug(`Cannot send notification, failed to resolve outputhost uuid`)
		return nil
	}

	adminClient, err := common.CreateOutputHostAdminClient(context.channel, addr)
	if err != nil {
		context.m3Client.IncCounter(metrics.OutputNotifyEventScope, metrics.ControllerFailures)
		context.m3Client.IncCounter(metrics.OutputNotifyEventScope, metrics.ControllerErrCreateTChanClientCounter)
		context.log.WithField(common.TagErr, err).Error(`Failed to create output host client`)
		return nil
	}

	update := &admin.ConsumerGroupUpdatedNotification{
		ConsumerGroupUUID: common.StringPtr(event.consGroupID),
		Type:              common.AdminNotificationTypePtr(event.notificationType),
	}

	req := &admin.ConsumerGroupsUpdatedRequest{
		UpdateUUID: common.StringPtr(uuid.New()),
		Updates:    []*admin.ConsumerGroupUpdatedNotification{update},
	}

	updateOp := func() error {
		ctx, cancel := thrift.NewContext(thriftCallTimeout)
		defer cancel()
		return adminClient.ConsumerGroupsUpdated(ctx, req)
	}

	context.log.WithFields(bark.Fields{
		common.TagCnsm:       common.FmtCnsm(event.consGroupID),
		common.TagDst:        common.FmtDst(event.dstID),
		`notifyType`:         update.GetType(),
		`reason`:             event.reason,
		`context`:            event.reasonContext,
		common.TagOut:        common.FmtIn(event.outputHostID),
		common.TagUpdateUUID: req.GetUpdateUUID(),
	}).Info("OutputHostNotificationEvent: Sending notification to outputhost")

	err = backoff.Retry(updateOp, notificationRetryPolicy(), common.IsRetryableTChanErr)
	if err != nil {
		context.m3Client.IncCounter(metrics.OutputNotifyEventScope, metrics.ControllerFailures)
		context.log.WithFields(bark.Fields{
			common.TagCnsm:       common.FmtCnsm(event.consGroupID),
			common.TagDst:        common.FmtDst(event.dstID),
			`notifyType`:         update.GetType(),
			`reason`:             event.reason,
			`context`:            event.reasonContext,
			common.TagOut:        common.FmtIn(event.outputHostID),
			common.TagUpdateUUID: req.GetUpdateUUID(),
			`hostaddr`:           addr,
			`error`:              err,
		}).Error("OutputHostNotificationEvent: Failed to send notification to outputhost")
	}

	return nil
}

// Handle handles an InputHostFailedEvent. All it does is to list all
// OPEN extents for the input host and enqueue an ExtentDownEvent for
// each one of them.
func (event *InputHostFailedEvent) Handle(context *Context) error {
	sw := context.m3Client.StartTimer(metrics.InputFailedEventScope, metrics.ControllerLatencyTimer)
	defer sw.Stop()
	context.m3Client.IncCounter(metrics.InputFailedEventScope, metrics.ControllerRequests)
	stats, err := context.mm.ListExtentsByInputIDStatus(event.hostUUID, common.MetadataExtentStatusPtr(shared.ExtentStatus_OPEN))
	if err != nil {
		// metadata store is temporarily unavailable. The extents held
		// by this input host will be sealed eventually when the background
		// reconciler task kicks in
		context.m3Client.IncCounter(metrics.InputFailedEventScope, metrics.ControllerFailures)
		context.m3Client.IncCounter(metrics.InputFailedEventScope, metrics.ControllerErrMetadataReadCounter)
		context.log.WithFields(bark.Fields{
			common.TagErr: err,
			common.TagIn:  event.hostUUID,
		}).Error(`InputHostFailedEvent: Cannot list extents`)
		return nil
	}
	createExtentDownEvents(context, stats)
	return nil
}

// Handle handles an StoreHostFailedEvent. All it does is to list all
// OPEN extents for the store host and enqueue an ExtentDownEvent for
// each one of them.
func (event *StoreHostFailedEvent) Handle(context *Context) error {
	sw := context.m3Client.StartTimer(metrics.StoreFailedEventScope, metrics.ControllerLatencyTimer)
	defer sw.Stop()
	context.m3Client.IncCounter(metrics.StoreFailedEventScope, metrics.ControllerRequests)
	stats, err := context.mm.ListExtentsByStoreIDStatus(event.hostUUID, common.MetadataExtentStatusPtr(shared.ExtentStatus_OPEN))
	if err != nil {
		// metadata intermittent failure, we will wait for the background
		// reconciler task to catch up and seal this extent
		context.m3Client.IncCounter(metrics.StoreFailedEventScope, metrics.ControllerFailures)
		context.m3Client.IncCounter(metrics.InputFailedEventScope, metrics.ControllerErrMetadataReadCounter)
		context.log.WithFields(bark.Fields{
			common.TagErr:  err,
			common.TagStor: event.hostUUID,
		}).Error(`StoreHostFailedEvent: Cannot list extents`)
		return nil
	}
	createExtentDownEvents(context, stats)
	return nil
}

// Handle handles an StoreExtentStatusOutOfSyncEvent.
// This handler reissues SealExtent call to an out
// of sync store host without updating metadata state
// This handler assumes that the extent was previously
// sealed successfully in atleast one store.
func (event *StoreExtentStatusOutOfSyncEvent) Handle(context *Context) error {

	sw := context.m3Client.StartTimer(metrics.StoreExtentStatusOutOfSyncEventScope, metrics.ControllerLatencyTimer)
	defer sw.Stop()

	context.m3Client.IncCounter(metrics.StoreExtentStatusOutOfSyncEventScope, metrics.ControllerRequests)

	addr, err := context.rpm.ResolveUUID(common.StoreServiceName, event.storeID)
	if err != nil {
		return errRetryable
	}

	err = sealExtentOnStore(context, event.storeID, addr, event.extentID, 0, false, metrics.StoreExtentStatusOutOfSyncEventScope)
	if err != nil {
		context.m3Client.IncCounter(metrics.StoreExtentStatusOutOfSyncEventScope, metrics.ControllerFailures)
		context.log.WithFields(bark.Fields{
			common.TagDst:  common.FmtDst(event.dstID),
			common.TagExt:  common.FmtExt(event.extentID),
			common.TagStor: common.FmtStor(event.storeID),
			common.TagErr:  err.Error(),
		}).Error("StoreExtentStatusOutOfSyncEvent: SealExtent failed on out of sync store host")
	}

	// invalidate the store extent cache regardless of the outcome of sealExtent
	// this will make sure we don't get into a tight retry loop, say, when a host
	// is down.  As long as the store is out of sync, this event will be
	// re-generated by extent monitor once every 2 minutes
	context.extentMonitor.invalidateStoreExtentCache(event.storeID, event.extentID)
	context.extentSeals.inProgress.Remove(event.extentID)

	return nil
}

// Handle handles an RemoteExtentCreatedEvent.
// This handler calls store to start replication.
// The first store will be issued with a remote replication request
// The rest of stores will be issued with a re-replication request
func (event *RemoteZoneExtentCreatedEvent) Handle(context *Context) error {
	sw := context.m3Client.StartTimer(metrics.RemoteZoneExtentCreatedEventScope, metrics.ControllerLatencyTimer)
	defer sw.Stop()

	context.m3Client.IncCounter(metrics.RemoteZoneExtentCreatedEventScope, metrics.ControllerRequests)

	var err error
	primaryStoreID := event.storeIDs[0]
	primaryStoreAddr, err := context.rpm.ResolveUUID(common.StoreServiceName, primaryStoreID)
	if err != nil {
		return errRetryable
	}

	primaryStoreClient, err := context.clientFactory.GetThriftStoreClient(primaryStoreAddr, primaryStoreID)
	if err != nil {
		context.log.WithFields(bark.Fields{
			common.TagExt:  common.FmtExt(event.extentID),
			common.TagStor: common.FmtStor(primaryStoreID),
			common.TagErr:  err,
		}).Error(`Client factory failed to get store client`)
		return err
	}

	ctx, cancel := thrift.NewContext(replicateExtentCallTimeout)
	defer cancel()

	req := store.NewRemoteReplicateExtentRequest()
	req.DestinationUUID = common.StringPtr(event.dstID)
	req.ExtentUUID = common.StringPtr(event.extentID)
	err = primaryStoreClient.RemoteReplicateExtent(ctx, req)
	if err != nil {
		context.log.WithFields(bark.Fields{
			common.TagExt:  common.FmtExt(event.extentID),
			common.TagStor: common.FmtStor(primaryStoreID),
			common.TagErr:  err,
		}).Error("Attempt to call RemoteReplicateExtent on storehost failed")
		return err
	}

	for i := 1; i < len(event.storeIDs); i++ {
		secondaryStoreID := event.storeIDs[i]
		secondaryStoreAddr, err := context.rpm.ResolveUUID(common.StoreServiceName, secondaryStoreID)
		if err != nil {
			return errRetryable
		}

		secondaryStoreClient, err := context.clientFactory.GetThriftStoreClient(secondaryStoreAddr, secondaryStoreID)
		if err != nil {
			context.log.WithFields(bark.Fields{
				common.TagExt:  common.FmtExt(event.extentID),
				common.TagStor: common.FmtStor(secondaryStoreID),
				common.TagErr:  err,
			}).Error(`Client factory failed to get store client`)
			return err
		}

		req := store.NewReplicateExtentRequest()
		req.DestinationUUID = common.StringPtr(event.dstID)
		req.ExtentUUID = common.StringPtr(event.extentID)
		req.StoreUUID = common.StringPtr(primaryStoreID)
		err = secondaryStoreClient.ReplicateExtent(ctx, req)
		if err != nil {
			context.log.WithFields(bark.Fields{
				common.TagExt:  common.FmtExt(event.extentID),
				common.TagStor: common.FmtStor(secondaryStoreID),
				`error`:        err,
			}).Error("Attempt to call ReplicateExtent on storehost failed")
			return err
		}
	}

	return nil
}

// Handle seals an extent and updates metadata
func (event *ExtentDownEvent) Handle(context *Context) error {

	sw := context.m3Client.StartTimer(metrics.ExtentDownEventScope, metrics.ControllerLatencyTimer)
	defer sw.Stop()
	var err error
	var stats *shared.ExtentStats
	var addr string
	var isRetry = !(event.state == checkPreconditionState)

	context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerRequests)

	for {
		switch event.state {

		case checkPreconditionState:
			stats, err = context.mm.ReadExtentStats(event.dstID, event.extentID)
			if err != nil {
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerErrMetadataReadCounter)
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerFailures)
				return errRetryable
			}

			if err == nil && stats.GetStatus() != shared.ExtentStatus_OPEN {
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerFailures)
				context.log.WithFields(bark.Fields{
					common.TagDst: common.FmtDst(event.dstID),
					common.TagExt: common.FmtExt(event.extentID),
				}).Error("ExtentDownEvent: Extent is not in OPEN state, dropping event")
				return nil // non-retryable
			}

			// if we cannot read the stats, we should fail immediately
			if err != nil {
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerErrMetadataReadCounter)
				context.log.WithFields(bark.Fields{
					common.TagDst: common.FmtDst(event.dstID),
					common.TagExt: common.FmtExt(event.extentID),
					`error`:       err,
				}).Error("Cannot read extent stats")
				return errRetryable
			}
			event.storeIDs = stats.GetExtent().GetStoreUUIDs()
			event.state = sealExtentState

		case sealExtentState:
			// Filter the store hosts that are healthy
			// and issue a seal operation on each one of them
			stores := make(map[string]string, len(event.storeIDs))
			for _, s := range event.storeIDs {
				addr, err = context.rpm.ResolveUUID(common.StoreServiceName, s)
				if err != nil {
					continue
				}
				stores[s] = addr
			}

			if len(stores) < 1 {
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerFailures)
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerErrNoHealthyStoreCounter)
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerErrSealFailed)
				context.log.WithFields(bark.Fields{
					common.TagDst: common.FmtDst(event.dstID),
					common.TagExt: common.FmtExt(event.extentID),
				}).Error("Can't seal extent, none of the store hosts are healthy")
				return errRetryable
			}

			// Extent seals are rate limited, block until we
			// can acquire a token from the rate limited bucket
			var rateLimited bool
			if !isRetry {
				// On the first attempt, just try once and backoff,
				// the event will get thrown into the retry executor
				consumed, _ := context.extentSeals.tokenBucket.TryConsume(1)
				rateLimited = !consumed
			} else {
				rateLimited = !context.extentSeals.tokenBucket.Consume(1, 10*time.Second)
			}

			if rateLimited {
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerRateLimited)
				return errRetryable
			}

			// TODO: Store API doesn't currently return
			// the sealed sequence number in response.
			// Fix this code to pick the min_seq(all_stores)
			// and update metadata accordingly
			var nSuccess int32
			wg := sync.WaitGroup{}

			for k, v := range stores {
				wg.Add(1)
				go func(uuid string, addr string) {
					defer wg.Done()
					e := sealExtentOnStore(context, uuid, addr, event.extentID, event.sealSeq, isRetry, metrics.ExtentDownEventScope)
					if e == nil {
						atomic.AddInt32(&nSuccess, 1)
						context.extentMonitor.invalidateStoreExtentCache(uuid, event.extentID)
					}
				}(k, v)
			}

			wg.Wait()

			if atomic.LoadInt32(&nSuccess) < 1 {
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerFailures)
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerErrSealFailed)
				context.log.WithFields(bark.Fields{
					common.TagDst: common.FmtDst(event.dstID),
					common.TagExt: common.FmtExt(event.extentID),
				}).Error("Sealing extent timed out on all stores")
				return errRetryable
			}

			event.state = updateMetadataState
			context.log.WithFields(bark.Fields{
				common.TagDst: common.FmtDst(event.dstID),
				common.TagExt: common.FmtExt(event.extentID),
			}).Info("Extent SEALED")

		case updateMetadataState:
			// Atleast one store was successful in sealing
			// update metadata state for the extent
			err := context.mm.SealExtent(event.dstID, event.extentID)
			if err != nil {
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerFailures)
				context.m3Client.IncCounter(metrics.ExtentDownEventScope, metrics.ControllerErrMetadataUpdateCounter)
				context.log.WithFields(bark.Fields{
					common.TagDst: common.FmtDst(event.dstID),
					common.TagExt: common.FmtExt(event.extentID),
					`error`:       err,
				}).Error("Extent sealed, but failed to update metadata")

				// If SealExtent throws an IllegalStateError, it means that the extent
				// state already moved forward beyond SEALED. This can happen either
				// because of cassandra's loose consistency scenarios or under controller
				// failover. If the state is moved forward, let's log this and just move ahead.
				_, stateErr := err.(*m.IllegalStateError)
				if !stateErr {
					return errRetryable
				}
				context.log.WithFields(bark.Fields{
					common.TagDst: common.FmtDst(event.dstID),
					common.TagExt: common.FmtExt(event.extentID),
				}).Error("Moving forward without updating metadata after SEALing extent, state has already moved forward")
			}
			context.extentSeals.failed.Remove(event.extentID)
			event.state = doneState
		case doneState:
			return nil
		default:
			context.log.WithField(common.TagState, event.state).Error(`ExtentDownEvent encountered illegal state`)
			return nil
		}
	}
}

// Done does cleanup for ExtentDownEvent
func (event *ExtentDownEvent) Done(context *Context, err error) {
	if err != nil {
		// extent not sealed after all retries, add it
		// to the failed set. Extents can remain in this
		// set for a long time, until the next trigger
		// for sealing happens. So, this is a best effort
		// at keeping track of failed exents so we can
		// filter them out from our GetInputHosts results.
		if context.extentSeals.failed.Size() > maxFailedExtentSealSetSize {
			context.log.WithFields(bark.Fields{
				common.TagDst: common.FmtDst(event.dstID),
				common.TagExt: common.FmtExt(event.extentID),
			}).Error("All retries exceeded for SEALing, cannot keep track of another failed extent in memory, too many failed extents")
		} else {
			context.extentSeals.failed.Put(event.extentID, Boolean(true))
		}
	}
	// We are done with our attempts to seal this extent
	// Remove it from the inProgress set. This would mean
	// we could potentially give this extent as an answer
	// in the GetInputHosts API. Consider creating an
	// PENDING_SEAL metadata extent state to avoid this.
	context.extentSeals.inProgress.Remove(event.extentID)
}

// triggerCacheRefreshForCG forces a result cache
// refresh for the given consumer group
func triggerCacheRefreshForCG(context *Context, cgID string) {

	now := time.Now().UnixNano()
	result := context.resultCache.readOutputHosts(cgID, now)
	if !result.cacheHit || result.refreshCache {
		return // already about to be refreshed
	}

	cacheEntry := result.resultCacheEntry
	deadline := now + resultCacheRefreshMaxWaitTime
	if result.expiry < deadline {
		return // about to be refreshed soon
	}

	// overwrite the next refresh time to now
	context.resultCache.write(cgID,
		resultCacheParams{
			dstType:  cacheEntry.dstType,
			nExtents: cacheEntry.nExtents,
			hostIDs:  cacheEntry.hostIDs,
			expiry:   now,
		})
}

func reconfigureAllConsumers(context *Context, dstID, extentID, reason, reasonContext string, m3Scope int) {
	// Notify every output host serving this destination
	// to force the consumers to reconfigure and consume
	// from the new extents
	var err error

	consGroups, err := context.mm.ListConsumerGroupsByDstID(dstID)
	if err != nil {
		context.m3Client.IncCounter(metrics.ExtentCreatedEventScope, metrics.ControllerErrMetadataReadCounter)
		context.log.WithField(common.TagErr, err).Error(`ListConsumerGroups failed, cannot notify output hosts about new extent`)
	}

	for _, cgd := range consGroups {

		if cgd.GetStatus() != shared.ConsumerGroupStatus_ENABLED {
			continue
		}

		filterBy := []m.ConsumerGroupExtentStatus{m.ConsumerGroupExtentStatus_OPEN}
		extents, err := context.mm.ListExtentsByConsumerGroup(dstID, cgd.GetConsumerGroupUUID(), filterBy)
		if err != nil {
			continue
		}

		outhosts := make(map[string]struct{})

		for _, ext := range extents {
			outhosts[ext.GetOutputHostUUID()] = struct{}{}
		}

		for k := range outhosts {
			notifyEvent := NewOutputHostNotificationEvent(dstID, cgd.GetConsumerGroupUUID(), k, reason, reasonContext, admin.NotificationType_CLIENT)
			if !context.eventPipeline.Add(notifyEvent) {
				context.log.WithFields(bark.Fields{
					common.TagDst:  common.FmtDst(dstID),
					common.TagCnsm: common.FmtCnsm(cgd.GetConsumerGroupUUID()),
					common.TagExt:  common.FmtExt(extentID),
					common.TagOut:  common.FmtOut(k),
					"reason":       reason,
					"context":      context,
				}).Error("reconfigureAllConsumers: Failed to enqueue OutputHostNotificationEvent, event queue full")
			}
		}

		triggerCacheRefreshForCG(context, cgd.GetConsumerGroupUUID())
	}
}

func createExtentDownEvents(context *Context, stats []*shared.ExtentStats) {
	for _, stat := range stats {
		if !common.IsRemoteZoneExtent(stat.GetExtent().GetOriginZone(), context.localZone) {
			addExtentDownEvent(context, 0, stat.GetExtent().GetDestinationUUID(), stat.GetExtent().GetExtentUUID())
		}
	}
}

func sealExtentOnStore(context *Context, storeUUID string, storeAddr string, extentID string, seq int64, isRetry bool, m3Scope int) error {
	client, err := context.clientFactory.GetThriftStoreClient(storeAddr, storeUUID)
	if err != nil {
		context.log.WithField(common.TagErr, err).Error(`Client factory failed to vend store client`)
		return err
	}

	defer context.clientFactory.ReleaseThriftStoreClient(storeUUID)

	req := store.NewSealExtentRequest()
	req.ExtentUUID = common.StringPtr(extentID)
	if seq > 0 {
		req.SequenceNumber = common.Int64Ptr(seq)
	}

	var timeout = sealExtentInitialCallTimeout
	var retryPolicy = sealExtentInitialRetryPolicy()

	if isRetry {
		timeout = sealExtentRetryCallTimeout
		retryPolicy = sealExtentRetryPolicy()
	}

	sealOp := func() error {
		ctx, cancel := thrift.NewContext(timeout)
		defer cancel()
		err := client.SealExtent(ctx, req)
		if err != nil {
			context.log.WithFields(bark.Fields{
				common.TagExt:  common.FmtExt(extentID),
				common.TagStor: common.FmtStor(storeUUID),
				`storeaddr`:    storeAddr,
				`error`:        err,
			}).Error("Attempt to seal extent on storehost failed")
		}
		return err
	}

	err = backoff.Retry(sealOp, retryPolicy, common.IsRetryableTChanErr)
	if err != nil {
		context.log.WithFields(bark.Fields{
			common.TagExt:  common.FmtExt(extentID),
			common.TagStor: common.FmtStor(storeUUID),
			`storeaddr`:    storeAddr,
			`error`:        err,
		}).Error("Sealing extent failed on store, retries exceeded")
	}
	return err
}

func createRetryPolicy(initial time.Duration, max time.Duration, expiry time.Duration, maxAttempts int) backoff.RetryPolicy {
	retryPolicy := backoff.NewExponentialRetryPolicy(initial)
	retryPolicy.SetMaximumInterval(max)
	retryPolicy.SetExpirationInterval(expiry)
	retryPolicy.SetMaximumAttempts(maxAttempts)
	return retryPolicy
}

// Use short timeout and retries on first attempt, if that fails
// throw the seal to a retryWorker, where we can afford to use a
// larger timeout
func sealExtentInitialRetryPolicy() backoff.RetryPolicy {
	return createRetryPolicy(500*time.Millisecond, 10*time.Second, time.Minute, 2)
}

func sealExtentRetryPolicy() backoff.RetryPolicy {
	return createRetryPolicy(3*time.Second, 30*time.Second, time.Minute, 3)
}

func notificationRetryPolicy() backoff.RetryPolicy {
	return createRetryPolicy(500*time.Millisecond, 10*time.Second, time.Minute, 3)
}
