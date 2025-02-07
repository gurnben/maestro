package dispatcher

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/buraksezer/consistent"
	"github.com/cespare/xxhash"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/openshift-online/maestro/pkg/api"
	"github.com/openshift-online/maestro/pkg/client/cloudevents"
	"github.com/openshift-online/maestro/pkg/dao"
	"github.com/openshift-online/maestro/pkg/logger"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
)

var _ Dispatcher = &HashDispatcher{}

// HashDispatcher is an implementation of Dispatcher. It uses consistent hashing to map consumers to maestro instances.
// Only the maestro instance that is mapped to a consumer will process the resource status update from that consumer.
// Need to trigger status resync for the consumer when an instance is up or down.
type HashDispatcher struct {
	instanceID   string
	instanceDao  dao.InstanceDao
	consumerDao  dao.ConsumerDao
	sourceClient cloudevents.SourceClient
	consumerSet  mapset.Set[string]
	workQueue    workqueue.RateLimitingInterface
	consistent   *consistent.Consistent
}

func NewHashDispatcher(instanceID string, instanceDao dao.InstanceDao, consumerDao dao.ConsumerDao, sourceClient cloudevents.SourceClient) *HashDispatcher {
	return &HashDispatcher{
		instanceID:   instanceID,
		instanceDao:  instanceDao,
		consumerDao:  consumerDao,
		sourceClient: sourceClient,
		consumerSet:  mapset.NewSet[string](),
		workQueue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "hash-dispatcher"),
		consistent: consistent.New(nil, consistent.Config{
			PartitionCount:    7,    // consumer IDs are distributed among partitions, select a big PartitionCount for more consumers.
			ReplicationFactor: 20,   // the numbers for maestro instances to be replicated on consistent hash ring.
			Load:              1.25, // Load is used to calculate average load, 1.25 is reasonable for most cases.
			Hasher:            hasher{},
		}),
	}
}

// Start initializes and runs the dispatcher, updating the hashing ring and consumer set for the current instance.
func (d *HashDispatcher) Start(ctx context.Context) {
	// start a goroutine to handle status resync requests
	go d.startStatusResyncWorkers(ctx)

	// start a goroutine to periodically check the instances and consumers.
	go wait.Until(d.check, 5*time.Second, ctx.Done())

	// wait until context is canceled
	<-ctx.Done()
	d.workQueue.ShutDown()
}

// Dispatch checks if the provided consumer ID is owned by the current maestro instance.
// It returns true if the consumer is part of the current instance's consumer set;
// otherwise, it returns false.
func (d *HashDispatcher) Dispatch(consumerID string) bool {
	return d.consumerSet.Contains(consumerID)
}

// OnInstanceUp adds the new instance to the hashing ring and updates the consumer set for the current instance.
func (d *HashDispatcher) OnInstanceUp(instanceID string) error {
	members := d.consistent.GetMembers()
	for _, member := range members {
		if member.String() == instanceID {
			// instance already exists, hashing ring won't be changed
			return nil
		}
	}

	// add the new instance to the hashing ring
	d.consistent.Add(&api.ServerInstance{
		Meta: api.Meta{
			ID: instanceID,
		},
	})

	return d.updateConsumerSet()
}

// OnInstanceDown removes the instance from the hashing ring and updates the consumer set for the current instance.
func (d *HashDispatcher) OnInstanceDown(instanceID string) error {
	members := d.consistent.GetMembers()
	deletedMember := true
	for _, member := range members {
		if member.String() == instanceID {
			// the instance is still in the hashing ring
			deletedMember = false
			break
		}
	}

	// if the instance is already deleted, the hash ring won't be changed
	if deletedMember {
		return nil
	}

	// remove the instance from the hashing ring
	d.consistent.Remove(instanceID)

	return d.updateConsumerSet()
}

// updateConsumerSet updates the consumer set for the current instance based on the hashing ring.
func (d *HashDispatcher) updateConsumerSet() error {
	// return if the hashing ring is not ready
	if d.consistent == nil || len(d.consistent.GetMembers()) == 0 {
		return nil
	}

	ctx := context.TODO()
	log := logger.NewOCMLogger(ctx)

	// get all consumers and update the consumer set for the current instance
	consumers, err := d.consumerDao.All(ctx)
	if err != nil {
		return fmt.Errorf("unable to list consumers: %s", err.Error())
	}

	toAddConsumers, toRemoveConsumers := []string{}, []string{}
	for _, consumer := range consumers {
		instanceID := d.consistent.LocateKey([]byte(consumer.ID)).String()
		if instanceID == d.instanceID {
			if !d.consumerSet.Contains(consumer.ID) {
				// new consumer added to the current instance, need to resync resource status updates for this consumer
				// log.V(4).Infof("Adding new consumer %s to consumer set", consumer.ID)
				toAddConsumers = append(toAddConsumers, consumer.ID)
				d.workQueue.Add(consumer.ID)
			}
		} else {
			// remove the consumer from the set if it is not in the current instance
			if d.consumerSet.Contains(consumer.ID) {
				// log.V(4).Infof("Removing consumer %s from consumer set", consumer.ID)
				toRemoveConsumers = append(toRemoveConsumers, consumer.ID)
			}
		}
	}

	_ = d.consumerSet.Append(toAddConsumers...)
	d.consumerSet.RemoveAll(toRemoveConsumers...)
	log.V(4).Infof("Consumers set for current instance: %s", d.consumerSet.String())

	return nil
}

// startStatusResyncWorkers starts the status resync workers to process status resync requests.
func (d *HashDispatcher) startStatusResyncWorkers(ctx context.Context) {
	wg := &sync.WaitGroup{}
	maxConcurrentResyncHandlers := 10
	wg.Add(maxConcurrentResyncHandlers)
	for i := 0; i < maxConcurrentResyncHandlers; i++ {
		go func() {
			defer wg.Done()
			for d.processNextResync(ctx) {
			}
		}()
	}
	wg.Wait()
}

// check checks the instances & consumers and updates the hashing ring and consumer set for the current instance.
func (d *HashDispatcher) check() {
	ctx := context.TODO()
	log := logger.NewOCMLogger(ctx)

	instances, err := d.instanceDao.All(ctx)
	if err != nil {
		log.Error(fmt.Sprintf("Unable to get all maestro instances: %s", err.Error()))
		return
	}

	// ensure the hashing ring members are up-to-date
	members := d.consistent.GetMembers()
	for _, member := range members {
		isMemberActive := false
		for _, instance := range instances {
			if member.String() == instance.ID {
				isMemberActive = true
				break
			}
		}
		if !isMemberActive {
			d.consistent.Remove(member.String())
		}
	}

	if err := d.updateConsumerSet(); err != nil {
		log.Error(fmt.Sprintf("Unable to update consumer set: %s", err.Error()))
	}
}

// processNextResync attempts to resync resource status updates for new consumers added to the current maestro instance
// using the cloudevents source client. It returns true if the resync is successful.
func (d *HashDispatcher) processNextResync(ctx context.Context) bool {
	consumerID, shutdown := d.workQueue.Get()
	if shutdown {
		// workqueue has been shutdown, return false
		return false
	}

	// We call Done here so the workqueue knows we have finished
	// processing this item. We also must remember to call Forget if we
	// do not want this work item being re-queued. For example, we do
	// not call Forget if a transient error occurs, instead the item is
	// put back on the workqueue and attempted again after a back-off
	// period.
	defer d.workQueue.Done(consumerID)

	consumerIDStr, ok := consumerID.(string)
	if !ok {
		d.workQueue.Forget(consumerID)
		// return true to indicate that we should continue processing the next item
		return true
	}

	log := logger.NewOCMLogger(ctx)
	log.V(4).Infof("processing status resync request for consumer %s", consumerIDStr)
	if err := d.sourceClient.Resync(ctx, []string{consumerIDStr}); err != nil {
		log.Error(fmt.Sprintf("failed to resync resourcs status for consumer %s: %s", consumerIDStr, err))
		// Put the item back on the workqueue to handle any transient errors.
		d.workQueue.AddRateLimited(consumerID)
	}

	return true
}

// hasher is an implementation of consistent.Hasher (github.com/buraksezer/consistent) interface
type hasher struct{}

// Sum64 returns the 64-bit xxHash of data
func (h hasher) Sum64(data []byte) uint64 {
	return xxhash.Sum64(data)
}
