// Copyright (c) 2017 Tigera, Inc. All rights reserved.

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

package watchersyncer

import (
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/libcalico-go/lib/backend/model"
	cerrors "github.com/projectcalico/libcalico-go/lib/errors"
)

const (
	maxUpdatesToConsolidate = 1000
)

// ResourceType groups together the watch and conversion information for a
// specific resource type.
type ResourceType struct {
	// The ListInterface used to perform a watch on this resource type.
	ListInterface model.ListInterface

	// An update processor used to convert the update event prior to it being sent in
	// the Syncer.
	UpdateProcessor SyncerUpdateProcessor
}

// SyncerUpdateProcessor is used to convert a Watch update into one or more additional
// Syncer updates.
type SyncerUpdateProcessor interface {
	// Process is called to process a watch update.  The processor may convert this
	// to zero or more updates.  The processor may use these calls to maintain a local cache
	// if required.  It is safe for the processor to send multiple duplicate adds or deletes
	// since the WatcherSyncer maintains it's own cache and will swallow duplicates.
	// A KVPair with a nil value indicates a delete.  A non nil value indicates an add/modified.
	// The processor may respond with any number of adds or deletes.
	Process(*model.KVPair) ([]*model.KVPair, error)

	// OnSyncerStarting is called when syncer is starting a full sync for the associated resource
	// type.  That means it is first going to list current resources and then watch for any updates.
	// If the processor maintains a private internal cache, then the cache should be cleared at
	// this point since the cache will be re-populated from the sync.
	OnSyncerStarting()
}

// New creates a new multiple Watcher-backed api.Syncer.
func New(client api.Client, resourceTypes []ResourceType, callbacks api.SyncerCallbacks) api.Syncer {
	rs := &watcherSyncer{
		watcherCaches: make([]*watcherCache, len(resourceTypes)),
		results:       make(chan interface{}, 2000),
		callbacks:     callbacks,
	}
	for i, r := range resourceTypes {
		rs.watcherCaches[i] = newWatcherCache(client, r, rs.results)
	}
	return rs
}

// watcherSyncer implements the api.Syncer interface.
type watcherSyncer struct {
	status        api.SyncStatus
	watcherCaches []*watcherCache
	results       chan interface{}
	numSynced     int
	callbacks     api.SyncerCallbacks
}

func (rs *watcherSyncer) Start() {
	log.Info("Start called")
	go rs.run()
}

// Send a status update and store the status.
func (rs *watcherSyncer) sendStatusUpdate(status api.SyncStatus) {
	log.WithField("Status", status).Info("Sending status update")
	rs.callbacks.OnStatusUpdated(status)
	rs.status = status
}

// run implements the main syncer loop that loops forever receiving watch events and translating
// to syncer updates.
func (rs *watcherSyncer) run() {
	log.Debug("Sending initial status event and starting watchers")
	rs.sendStatusUpdate(api.WaitForDatastore)
	for _, wc := range rs.watcherCaches {
		go wc.run()
	}

	log.Info("Starting main event processing loop")
	var updates []api.Update
	for {
		// Block until there is data.
		result := <-rs.results

		// Process the data - this will append the data in subsequent calls, and action
		// it if we hit a non-update event.
		updates := rs.processResult(updates, result)

		// Append results into the one update until we either flush the channel or we
		// hit our fixed limit per update.
	consolidatationloop:
		for ii := 0; ii < maxUpdatesToConsolidate; ii++ {
			select {
			case next := <-rs.results:
				updates = rs.processResult(updates, next)
			default:
				break consolidatationloop
			}
		}

		// Perform final processing (pass in a nil result) before we loop and hit the blocking
		// call again.
		updates = rs.sendUpdates(updates)
	}
}

// Process a result from the result channel.  We don't immediately action updates, but
// instead start grouping them together so that we can send a larger single update to
// Felix.
func (rs *watcherSyncer) processResult(updates []api.Update, result interface{}) []api.Update {

	// Switch on the result type.
	switch r := result.(type) {
	case []api.Update:
		// This is an update.  If we don't have previous updates then also check to see
		// if we need to shift the status into Resync.
		// We append these updates to the previous if there were any.
		if len(updates) == 0 && rs.status == api.WaitForDatastore {
			rs.sendStatusUpdate(api.ResyncInProgress)
		}
		updates = append(updates, r...)

	case error:
		// Received an error.  Firstly, send any updates that we have grouped.
		updates = rs.sendUpdates(updates)

		// If this is a parsing error, and if the callbacks support
		// it, then send the error update.
		log.WithError(r).Info("Error received in main syncer event processing loop")
		if ec, ok := rs.callbacks.(api.SyncerParseFailCallbacks); ok {
			log.Debug("SyncerParseFailCallbacks interface is supported")
			if pe, ok := r.(cerrors.ErrorParsingDatastoreEntry); ok {
				ec.ParseFailed(pe.RawKey, pe.RawValue)
			}
		}

	case api.SyncStatus:
		// Received a synced event.  If we are still waiting for datastore, send a
		// ResyncInProgress since at least one watcher has connected.
		log.WithField("SyncUpdate", r).Debug("Received sync status event from watcher")
		if r == api.InSync {
			log.Info("Received InSync event from one of the watcher caches")

			if rs.status == api.WaitForDatastore {
				rs.sendStatusUpdate(api.ResyncInProgress)
			}

			// Increment the count of synced events.
			rs.numSynced++

			// If we have now received synced events from all of our watchers then we are in
			// sync.  If we have any updates, send them first and then send the status update.
			if rs.numSynced == len(rs.watcherCaches) {
				log.Info("All watchers have sync'd data - sending data and final sync")
				updates = rs.sendUpdates(updates)
				rs.sendStatusUpdate(api.InSync)
			}
		}
	}

	// Return the accumulated or processed updated.
	return updates
}

// sendUpdates is used to send the consoidated set of updates.  Returns nil.
func (rs *watcherSyncer) sendUpdates(updates []api.Update) []api.Update {
	log.WithField("NumUpdates", len(updates)).Debug("Sending syncer updates (if any to send)")
	if len(updates) > 0 {
		rs.callbacks.OnUpdates(updates)
	}
	return nil
}
