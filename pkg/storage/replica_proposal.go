// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval/result"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/spanset"
	"github.com/cockroachdb/cockroach/pkg/storage/stateloader"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/storage/storagepb"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/sysutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/kr/pretty"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"golang.org/x/time/rate"
)

// ProposalData is data about a command which allows it to be
// evaluated, proposed to raft, and for the result of the command to
// be returned to the caller.
type ProposalData struct {
	// The caller's context, used for logging proposals, reproposals, message
	// sends, and command application. In order to enable safely tracing events
	// beneath, modifying this ctx field in *ProposalData requires holding the
	// raftMu.
	ctx context.Context

	// An optional tracing span bound to the proposal. Will be cleaned
	// up when the proposal finishes.
	sp opentracing.Span

	// idKey uniquely identifies this proposal.
	// TODO(andreimatei): idKey is legacy at this point: We could easily key
	// commands by their MaxLeaseIndex, and doing so should be ok with a stop-
	// the-world migration. However, various test facilities depend on the
	// command ID for e.g. replay protection.
	idKey storagebase.CmdIDKey

	// proposedAtTicks is the (logical) time at which this command was
	// last (re-)proposed.
	proposedAtTicks int

	// command is serialized and proposed to raft. In the event of
	// reproposals its MaxLeaseIndex field is mutated.
	command *storagepb.RaftCommand

	// endCmds.finish is called after command execution to update the
	// timestamp cache & release latches.
	endCmds *endCmds

	// doneCh is used to signal the waiting RPC handler (the contents of
	// proposalResult come from LocalEvalResult).
	//
	// Attention: this channel is not to be signaled directly downstream of Raft.
	// Always use ProposalData.finishApplication().
	doneCh chan proposalResult

	// Local contains the results of evaluating the request tying the upstream
	// evaluation of the request to the downstream application of the command.
	// Nil when the proposal came from another node (i.e. the evaluation wasn't
	// done here).
	Local *result.LocalResult

	// Request is the client's original BatchRequest.
	// TODO(tschottdorf): tests which use TestingCommandFilter use this.
	// Decide how that will work in the future, presumably the
	// CommandFilter would run at proposal time or we allow an opaque
	// struct to be attached to a proposal which is then available as it
	// applies. Other than tests, we only need a few bits of the request
	// here; this could be replaced with isLease and isChangeReplicas
	// booleans.
	Request *roachpb.BatchRequest
}

// finishApplication is called when a command application has finished. The
// method will be called downstream of Raft if the command required consensus,
// but can be called upstream of Raft if the command did not and was never
// proposed.
//
// It first invokes the endCmds function and then sends the specified
// proposalResult on the proposal's done channel. endCmds is invoked here in
// order to allow the original client to be canceled. (When the original client
// is canceled, it won't be listening to this done channel, and so it can't be
// counted on to invoke endCmds itself.)
func (proposal *ProposalData) finishApplication(pr proposalResult) {
	if proposal.endCmds != nil {
		proposal.endCmds.done(pr.Reply, pr.Err)
		proposal.endCmds = nil
	}
	if proposal.sp != nil {
		tracing.FinishSpan(proposal.sp)
	}
	proposal.signalProposalResult(pr)
}

// returnProposalResult signals proposal.doneCh with the proposal result if it
// has not already been signaled. The method can be called even before the
// proposal has finished replication and command application, and does not
// release the request's latches.
func (proposal *ProposalData) signalProposalResult(pr proposalResult) {
	if proposal.doneCh != nil {
		proposal.doneCh <- pr
		proposal.doneCh = nil
	}
}

// TODO(tschottdorf): we should find new homes for the checksum, lease
// code, and various others below to leave here only the core logic.
// Not moving anything right now to avoid awkward diffs.

func (r *Replica) gcOldChecksumEntriesLocked(now time.Time) {
	for id, val := range r.mu.checksums {
		// The timestamp is valid only if set.
		if !val.gcTimestamp.IsZero() && now.After(val.gcTimestamp) {
			delete(r.mu.checksums, id)
		}
	}
}

func (r *Replica) computeChecksumPostApply(ctx context.Context, cc storagepb.ComputeChecksum) {
	stopper := r.store.Stopper()
	now := timeutil.Now()
	r.mu.Lock()
	var notify chan struct{}
	if c, ok := r.mu.checksums[cc.ChecksumID]; !ok {
		// There is no record of this ID. Make a new notification.
		notify = make(chan struct{})
	} else if !c.started {
		// A CollectChecksumRequest is waiting on the existing notification.
		notify = c.notify
	} else {
		log.Fatalf(ctx, "attempted to apply ComputeChecksum command with duplicated checksum ID %s",
			cc.ChecksumID)
	}

	r.gcOldChecksumEntriesLocked(now)

	// Create an entry with checksum == nil and gcTimestamp unset.
	r.mu.checksums[cc.ChecksumID] = ReplicaChecksum{started: true, notify: notify}
	desc := *r.mu.state.Desc
	r.mu.Unlock()

	if cc.Version != batcheval.ReplicaChecksumVersion {
		r.computeChecksumDone(ctx, cc.ChecksumID, nil, nil)
		log.Infof(ctx, "incompatible ComputeChecksum versions (requested: %d, have: %d)",
			cc.Version, batcheval.ReplicaChecksumVersion)
		return
	}

	// Caller is holding raftMu, so an engine snapshot is automatically
	// Raft-consistent (i.e. not in the middle of an AddSSTable).
	snap := r.store.engine.NewSnapshot()
	if cc.Checkpoint {
		checkpointBase := filepath.Join(r.store.engine.GetAuxiliaryDir(), "checkpoints")
		_ = os.MkdirAll(checkpointBase, 0700)
		sl := stateloader.Make(r.RangeID)
		rai, _, err := sl.LoadAppliedIndex(ctx, snap)
		if err != nil {
			log.Warningf(ctx, "unable to load applied index, continuing anyway")
		}
		// NB: the names here will match on all nodes, which is nice for debugging.
		checkpointDir := filepath.Join(checkpointBase, fmt.Sprintf("r%d_at_%d", r.RangeID, rai))
		if err := r.store.engine.CreateCheckpoint(checkpointDir); err != nil {
			log.Warningf(ctx, "unable to create checkpoint %s: %s", checkpointDir, err)
		} else {
			log.Infof(ctx, "created checkpoint %s", checkpointDir)
		}
	}

	// Compute SHA asynchronously and store it in a map by UUID.
	if err := stopper.RunAsyncTask(ctx, "storage.Replica: computing checksum", func(ctx context.Context) {
		defer snap.Close()
		var snapshot *roachpb.RaftSnapshotData
		if cc.SaveSnapshot {
			snapshot = &roachpb.RaftSnapshotData{}
		}
		result, err := r.sha512(ctx, desc, snap, snapshot, cc.Mode)
		if err != nil {
			log.Errorf(ctx, "%v", err)
			result = nil
		}
		r.computeChecksumDone(ctx, cc.ChecksumID, result, snapshot)
	}); err != nil {
		defer snap.Close()
		log.Error(ctx, errors.Wrapf(err, "could not run async checksum computation (ID = %s)", cc.ChecksumID))
		// Set checksum to nil.
		r.computeChecksumDone(ctx, cc.ChecksumID, nil, nil)
	}
}

// leasePostApply updates the Replica's internal state to reflect the
// application of a new Range lease. The method is idempotent, so it can be
// called repeatedly for the same lease safely. However, the method will panic
// if passed a lease with a lower sequence number than the current lease. By
// default, the method will also panic if passed a lease that indicates a
// forward sequence number jump (i.e. a skipped lease). This behavior can
// be disabled by passing permitJump as true.
func (r *Replica) leasePostApply(ctx context.Context, newLease roachpb.Lease, permitJump bool) {
	r.mu.Lock()
	replicaID := r.mu.replicaID
	// Pull out the last lease known to this Replica. It's possible that this is
	// not actually the last lease in the Range's lease sequence because the
	// Replica may have missed the application of a lease between prevLease and
	// newLease. However, this should only be possible if a snapshot includes a
	// lease update. All other forms of lease updates should be continuous
	// without jumps (see permitJump).
	prevLease := *r.mu.state.Lease
	r.mu.Unlock()

	iAmTheLeaseHolder := newLease.Replica.ReplicaID == replicaID
	// NB: in the case in which a node restarts, minLeaseProposedTS forces it to
	// get a new lease and we make sure it gets a new sequence number, thus
	// causing the right half of the disjunction to fire so that we update the
	// timestamp cache.
	leaseChangingHands := prevLease.Replica.StoreID != newLease.Replica.StoreID || prevLease.Sequence != newLease.Sequence

	if iAmTheLeaseHolder {
		// Log lease acquisition whenever an Epoch-based lease changes hands (or verbose
		// logging is enabled).
		if newLease.Type() == roachpb.LeaseEpoch && leaseChangingHands || log.V(1) {
			log.VEventf(ctx, 1, "new range lease %s following %s", newLease, prevLease)
		}
	}

	if leaseChangingHands && iAmTheLeaseHolder {
		// When taking over the lease, we need to check whether a merge is in
		// progress, as only the old leaseholder would have been explicitly notified
		// of the merge. If there is a merge in progress, maybeWatchForMerge will
		// arrange to block all traffic to this replica unless the merge aborts.
		if err := r.maybeWatchForMerge(ctx); err != nil {
			// We were unable to determine whether a merge was in progress. We cannot
			// safely proceed.
			log.Fatalf(ctx, "failed checking for in-progress merge while installing new lease %s: %s",
				newLease, err)
		}

		// If this replica is a new holder of the lease, update the low water
		// mark of the timestamp cache. Note that clock offset scenarios are
		// handled via a stasis period inherent in the lease which is documented
		// in the Lease struct.
		//
		// The introduction of lease transfers implies that the previous lease
		// may have been shortened and we are now applying a formally overlapping
		// lease (since the old lease holder has promised not to serve any more
		// requests, this is kosher). This means that we don't use the old
		// lease's expiration but instead use the new lease's start to initialize
		// the timestamp cache low water.
		setTimestampCacheLowWaterMark(r.store.tsCache, r.Desc(), newLease.Start)

		// Reset the request counts used to make lease placement decisions whenever
		// starting a new lease.
		if r.leaseholderStats != nil {
			r.leaseholderStats.resetRequestCounts()
		}
	}

	// Sanity check to make sure that the lease sequence is moving in the right
	// direction.
	if s1, s2 := prevLease.Sequence, newLease.Sequence; s1 != 0 {
		// We're at a version that supports lease sequence numbers.
		switch {
		case s2 < s1:
			log.Fatalf(ctx, "lease sequence inversion, prevLease=%s, newLease=%s",
				log.Safe(prevLease), log.Safe(newLease))
		case s2 == s1:
			// If the sequence numbers are the same, make sure they're actually
			// the same lease. This can happen when callers are using
			// leasePostApply for some of its side effects, like with
			// splitPostApply. It can also happen during lease extensions.
			if !prevLease.Equivalent(newLease) {
				log.Fatalf(ctx, "sequence identical for different leases, prevLease=%s, newLease=%s",
					log.Safe(prevLease), log.Safe(newLease))
			}
		case s2 == s1+1:
			// Lease sequence incremented by 1. Expected case.
		case s2 > s1+1 && !permitJump:
			log.Fatalf(ctx, "lease sequence jump, prevLease=%s, newLease=%s",
				log.Safe(prevLease), log.Safe(newLease))
		}
	}

	// Ordering is critical here. We only install the new lease after we've
	// checked for an in-progress merge and updated the timestamp cache. If the
	// ordering were reversed, it would be possible for requests to see the new
	// lease but not the updated merge or timestamp cache state, which can result
	// in serializability violations.
	r.mu.Lock()
	r.mu.state.Lease = &newLease
	expirationBasedLease := r.requiresExpiringLeaseRLocked()
	r.mu.Unlock()

	// Gossip the first range whenever its lease is acquired. We check to make
	// sure the lease is active so that a trailing replica won't process an old
	// lease request and attempt to gossip the first range.
	if leaseChangingHands && iAmTheLeaseHolder && r.IsFirstRange() && r.IsLeaseValid(newLease, r.store.Clock().Now()) {
		r.gossipFirstRange(ctx)
	}

	// Whenever we first acquire an expiration-based lease, notify the lease
	// renewer worker that we want it to keep proactively renewing the lease
	// before it expires.
	if leaseChangingHands && iAmTheLeaseHolder && expirationBasedLease && r.IsLeaseValid(newLease, r.store.Clock().Now()) {
		r.store.renewableLeases.Store(int64(r.RangeID), unsafe.Pointer(r))
		select {
		case r.store.renewableLeasesSignal <- struct{}{}:
		default:
		}
	}

	if leaseChangingHands && !iAmTheLeaseHolder {
		// Also clear and disable the push transaction queue. Any waiters
		// must be redirected to the new lease holder.
		r.txnWaitQueue.Clear(true /* disable */)
	}

	// If we're the current raft leader, may want to transfer the leadership to
	// the new leaseholder. Note that this condition is also checked periodically
	// when ticking the replica.
	r.maybeTransferRaftLeadership(ctx)

	// Notify the store that a lease change occurred and it may need to
	// gossip the updated store descriptor (with updated capacity).
	prevOwner := prevLease.OwnedBy(r.store.StoreID())
	currentOwner := newLease.OwnedBy(r.store.StoreID())
	if leaseChangingHands && (prevOwner || currentOwner) {
		if currentOwner {
			r.store.maybeGossipOnCapacityChange(ctx, leaseAddEvent)
		} else if prevOwner {
			r.store.maybeGossipOnCapacityChange(ctx, leaseRemoveEvent)
		}
		if r.leaseholderStats != nil {
			r.leaseholderStats.resetRequestCounts()
		}
	}

	// Potentially re-gossip if the range contains system data (e.g. system
	// config or node liveness). We need to perform this gossip at startup as
	// soon as possible. Trying to minimize how often we gossip is a fool's
	// errand. The node liveness info will be gossiped frequently (every few
	// seconds) in any case due to the liveness heartbeats. And the system config
	// will be gossiped rarely because it falls on a range with an epoch-based
	// range lease that is only reacquired extremely infrequently.
	if iAmTheLeaseHolder {
		if err := r.MaybeGossipSystemConfig(ctx); err != nil {
			log.Error(ctx, err)
		}
		if err := r.MaybeGossipNodeLiveness(ctx, keys.NodeLivenessSpan); err != nil {
			log.Error(ctx, err)
		}
		// Make sure the push transaction queue is enabled.
		r.txnWaitQueue.Enable()

		// Emit an MLAI on the leaseholder replica, as follower will be looking
		// for one and if we went on to quiesce, they wouldn't necessarily get
		// one otherwise (unless they ask for it, which adds latency).
		r.EmitMLAI()
	}

	// Mark the new lease in the replica's lease history.
	if r.leaseHistory != nil {
		r.leaseHistory.add(newLease)
	}
}

func addSSTablePreApply(
	ctx context.Context,
	st *cluster.Settings,
	eng engine.Engine,
	sideloaded SideloadStorage,
	term, index uint64,
	sst storagepb.ReplicatedEvalResult_AddSSTable,
	limiter *rate.Limiter,
) bool {
	checksum := util.CRC32(sst.Data)

	if checksum != sst.CRC32 {
		log.Fatalf(
			ctx,
			"checksum for AddSSTable at index term %d, index %d does not match; at proposal time %x (%d), now %x (%d)",
			term, index, sst.CRC32, sst.CRC32, checksum, checksum,
		)
	}

	const modify, noModify = true, false

	path, err := sideloaded.Filename(ctx, index, term)
	if err != nil {
		log.Fatalf(ctx, "sideloaded SSTable at term %d, index %d is missing", term, index)
	}

	eng.PreIngestDelay(ctx)

	// as of VersionUnreplicatedRaftTruncatedState we were on rocksdb 5.17 so this
	// cluster version should indicate that we will never use rocksdb < 5.16 to
	// read these SSTs, so it is safe to use https://github.com/facebook/rocksdb/pull/4172
	// to avoid needing the global seq_no edits and the copies they required.
	canSkipSeqNo := st.Version.IsActive(cluster.VersionUnreplicatedRaftTruncatedState)

	copied := false
	if inmem, ok := eng.(engine.InMem); ok {
		path = fmt.Sprintf("%x", checksum)
		if err := inmem.WriteFile(path, sst.Data); err != nil {
			panic(err)
		}
	} else {
		ingestPath := path + ".ingested"

		canLinkToRaftFile := false
		// The SST may already be on disk, thanks to the sideloading mechanism.  If
		// so we can try to add that file directly, via a new hardlink if the file-
		// system support it, rather than writing a new copy of it. However, this is
		// only safe if we can do so without modifying the file since it is still
		// part of an immutable raft log message, but in some cases, described in
		// DBIngestExternalFile, RocksDB would modify the file. Fortunately we can
		// tell Rocks that it is not allowed to modify the file, in which case it
		// will return and error if it would have tried to do so, at which point we
		// can fall back to writing a new copy for Rocks to ingest.
		if _, links, err := sysutil.StatAndLinkCount(path); err == nil {
			// HACK: RocksDB does not like ingesting the same file (by inode) twice.
			// See facebook/rocksdb#5133. We can tell that we have tried to ingest
			// this file already if it has more than one link – one from the file raft
			// wrote and one from rocks. In that case, we should not try to give
			// rocks a link to the same file again.
			if links == 1 {
				canLinkToRaftFile = true
			} else {
				log.Warningf(ctx, "SSTable at index %d term %d may have already been ingested (link count %d) -- falling back to ingesting a copy",
					index, term, links)
			}
		}

		if canLinkToRaftFile {
			// If the fs supports it, make a hard-link for rocks to ingest. We cannot
			// pass it the path in the sideload store as it deletes the passed path on
			// success.
			if linkErr := eng.LinkFile(path, ingestPath); linkErr == nil {
				ingestErr := eng.IngestExternalFiles(ctx, []string{ingestPath}, canSkipSeqNo, noModify)
				if ingestErr == nil {
					// Adding without modification succeeded, no copy necessary.
					log.Eventf(ctx, "ingested SSTable at index %d, term %d: %s", index, term, ingestPath)
					return false
				}
				if rmErr := eng.DeleteFile(ingestPath); rmErr != nil {
					log.Fatalf(ctx, "failed to move ingest sst: %v", rmErr)
				}
				const seqNoMsg = "Global seqno is required, but disabled"
				const seqNoOnReIngest = "external file have non zero sequence number"
				// Repeated ingestion is still possible even with the link count checked
				// above, since rocks might have already compacted away the file.
				// However it does not flush compacted files from its cache, so it can
				// still react poorly to attempting to ingest again. If we get an error
				// that indicates we can't ingest, we'll make a copy and try again. That
				// attempt must succeed or we'll fatal, so any persistent error is still
				// going to be surfaced.
				ingestErrMsg := ingestErr.Error()
				isSeqNoErr := strings.Contains(ingestErrMsg, seqNoMsg) || strings.Contains(ingestErrMsg, seqNoOnReIngest)
				if _, ok := ingestErr.(*engine.RocksDBError); !ok || !isSeqNoErr {
					log.Fatalf(ctx, "while ingesting %s: %s", ingestPath, ingestErr)
				}
			}
		}

		path = ingestPath

		log.Eventf(ctx, "copying SSTable for ingestion at index %d, term %d: %s", index, term, path)

		// TODO(tschottdorf): remove this once sideloaded storage guarantees its
		// existence.
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			panic(err)
		}
		if _, err := os.Stat(path); err == nil {
			// The file we want to ingest exists. This can happen since the
			// ingestion may apply twice (we ingest before we mark the Raft
			// command as committed). Just unlink the file (RocksDB created a
			// hard link); after that we're free to write it again.
			if err := os.Remove(path); err != nil {
				log.Fatalf(ctx, "while removing existing file during ingestion of %s: %s", path, err)
			}
		}

		if err := writeFileSyncing(ctx, path, sst.Data, eng, 0600, st, limiter); err != nil {
			log.Fatalf(ctx, "while ingesting %s: %s", path, err)
		}
		copied = true
	}

	if err := eng.IngestExternalFiles(ctx, []string{path}, canSkipSeqNo, modify); err != nil {
		log.Fatalf(ctx, "while ingesting %s: %s", path, err)
	}
	log.Eventf(ctx, "ingested SSTable at index %d, term %d: %s", index, term, path)
	return copied
}

func (r *Replica) handleReplicatedEvalResult(
	ctx context.Context,
	rResult storagepb.ReplicatedEvalResult,
	raftAppliedIndex, leaseAppliedIndex uint64,
) (shouldAssert bool) {
	// Fields for which no action is taken in this method are zeroed so that
	// they don't trigger an assertion at the end of the method (which checks
	// that all fields were handled).
	{
		rResult.IsLeaseRequest = false
		rResult.Timestamp = hlc.Timestamp{}
		rResult.PrevLeaseProposal = nil
	}

	if rResult.BlockReads {
		r.readOnlyCmdMu.Lock()
		defer r.readOnlyCmdMu.Unlock()
		rResult.BlockReads = false
	}

	// Update MVCC stats and Raft portion of ReplicaState.
	deltaStats := rResult.Delta.ToStats()
	r.mu.Lock()
	r.mu.state.Stats.Add(deltaStats)
	if raftAppliedIndex != 0 {
		r.mu.state.RaftAppliedIndex = raftAppliedIndex
	}
	if leaseAppliedIndex != 0 {
		r.mu.state.LeaseAppliedIndex = leaseAppliedIndex
	}
	needsSplitBySize := r.needsSplitBySizeRLocked()
	needsMergeBySize := r.needsMergeBySizeRLocked()
	r.mu.Unlock()

	r.store.metrics.addMVCCStats(deltaStats)
	rResult.Delta = enginepb.MVCCStatsDelta{}

	// NB: the bootstrap store has a nil split queue.
	// TODO(tbg): the above is probably a lie now.
	if r.store.splitQueue != nil && needsSplitBySize && r.splitQueueThrottle.ShouldProcess(timeutil.Now()) {
		r.store.splitQueue.MaybeAddAsync(ctx, r, r.store.Clock().Now())
	}

	// The bootstrap store has a nil merge queue.
	// TODO(tbg): the above is probably a lie now.
	if r.store.mergeQueue != nil && needsMergeBySize && r.mergeQueueThrottle.ShouldProcess(timeutil.Now()) {
		// TODO(tbg): for ranges which are small but protected from merges by
		// other means (zone configs etc), this is called on every command, and
		// fires off a goroutine each time. Make this trigger (and potentially
		// the split one above, though it hasn't been observed to be as
		// bothersome) less aggressive.
		r.store.mergeQueue.MaybeAddAsync(ctx, r, r.store.Clock().Now())
	}

	// The above are always present. The following are not always present but
	// should not trigger a ReplicaState assertion because they are either too
	// frequent to do so or because they do not change the ReplicaState.

	if rResult.State != nil {
		// Raft log truncation is too frequent to justify a replica state
		// assertion.
		if newTruncState := rResult.State.TruncatedState; newTruncState != nil {
			rResult.State.TruncatedState = nil // for assertion

			r.mu.Lock()
			r.mu.state.TruncatedState = newTruncState
			r.mu.Unlock()

			// Clear any entries in the Raft log entry cache for this range up
			// to and including the most recently truncated index.
			r.store.raftEntryCache.Clear(r.RangeID, newTruncState.Index+1)

			// Truncate the sideloaded storage. Note that this is safe only if the new truncated state
			// is durably on disk (i.e.) synced. This is true at the time of writing but unfortunately
			// could rot.
			{
				log.Eventf(ctx, "truncating sideloaded storage up to (and including) index %d", newTruncState.Index)
				if size, _, err := r.raftMu.sideloaded.TruncateTo(ctx, newTruncState.Index+1); err != nil {
					// We don't *have* to remove these entries for correctness. Log a
					// loud error, but keep humming along.
					log.Errorf(ctx, "while removing sideloaded files during log truncation: %s", err)
				} else {
					rResult.RaftLogDelta -= size
				}
			}
		}

		// ReplicaState.Stats was previously non-nullable which caused nodes to
		// send a zero-value MVCCStats structure. If the proposal was generated by
		// an old node, we'll have decoded that zero-value structure setting
		// ReplicaState.Stats to a non-nil value which would trigger the "unhandled
		// field in ReplicatedEvalResult" assertion to fire if we didn't clear it.
		if rResult.State.Stats != nil && (*rResult.State.Stats == enginepb.MVCCStats{}) {
			rResult.State.Stats = nil
		}

		if rResult.State.UsingAppliedStateKey {
			r.mu.Lock()
			// If we're already using the AppliedStateKey then there's nothing
			// to do. This flag is idempotent so it's ok that we see this flag
			// multiple times, but we want to make sure it doesn't cause us to
			// perform repeated state assertions, so clear it before the
			// shouldAssert determination.
			if r.mu.state.UsingAppliedStateKey {
				rResult.State.UsingAppliedStateKey = false
			}
			r.mu.Unlock()
		}

		if (*rResult.State == storagepb.ReplicaState{}) {
			rResult.State = nil
		}
	}

	if rResult.RaftLogDelta != 0 {
		r.mu.Lock()
		r.mu.raftLogSize += rResult.RaftLogDelta
		r.mu.raftLogLastCheckSize += rResult.RaftLogDelta
		// Ensure raftLog{,LastCheck}Size is not negative since it isn't persisted
		// between server restarts.
		if r.mu.raftLogSize < 0 {
			r.mu.raftLogSize = 0
		}
		if r.mu.raftLogLastCheckSize < 0 {
			r.mu.raftLogLastCheckSize = 0
		}
		r.mu.Unlock()
		rResult.RaftLogDelta = 0
	} else {
		// Check for whether to queue the range for Raft log truncation if this is
		// not a Raft log truncation command itself. We don't want to check the
		// Raft log for truncation on every write operation or even every operation
		// which occurs after the Raft log exceeds RaftLogQueueStaleSize. The logic
		// below queues the replica for possible Raft log truncation whenever an
		// additional RaftLogQueueStaleSize bytes have been written to the Raft
		// log.
		r.mu.Lock()
		checkRaftLog := r.mu.raftLogSize-r.mu.raftLogLastCheckSize >= RaftLogQueueStaleSize
		if checkRaftLog {
			r.mu.raftLogLastCheckSize = r.mu.raftLogSize
		}
		r.mu.Unlock()
		if checkRaftLog {
			r.store.raftLogQueue.MaybeAddAsync(ctx, r, r.store.Clock().Now())
		}
	}

	for _, sc := range rResult.SuggestedCompactions {
		r.store.compactor.Suggest(ctx, sc)
	}
	rResult.SuggestedCompactions = nil

	// The rest of the actions are "nontrivial" and may have large effects on the
	// in-memory and on-disk ReplicaStates. If any of these actions are present,
	// we want to assert that these two states do not diverge.
	shouldAssert = !rResult.Equal(storagepb.ReplicatedEvalResult{})

	// Process Split or Merge. This needs to happen after stats update because
	// of the ContainsEstimates hack.

	if rResult.Split != nil {
		splitPostApply(
			r.AnnotateCtx(ctx),
			rResult.Split.RHSDelta,
			&rResult.Split.SplitTrigger,
			r,
		)
		rResult.Split = nil
	}

	if rResult.Merge != nil {
		if err := r.store.MergeRange(
			ctx, r, rResult.Merge.LeftDesc, rResult.Merge.RightDesc, rResult.Merge.FreezeStart,
		); err != nil {
			// Our in-memory state has diverged from the on-disk state.
			log.Fatalf(ctx, "failed to update store after merging range: %s", err)
		}
		rResult.Merge = nil
	}

	// Update the remaining ReplicaState.

	if rResult.State != nil {
		if newDesc := rResult.State.Desc; newDesc != nil {
			r.setDesc(ctx, newDesc)
			rResult.State.Desc = nil
		}

		if newLease := rResult.State.Lease; newLease != nil {
			r.leasePostApply(ctx, *newLease, false /* permitJump */)
			rResult.State.Lease = nil
		}

		if newThresh := rResult.State.GCThreshold; newThresh != nil {
			if (*newThresh != hlc.Timestamp{}) {
				r.mu.Lock()
				r.mu.state.GCThreshold = newThresh
				r.mu.Unlock()
			}
			rResult.State.GCThreshold = nil
		}

		if newThresh := rResult.State.TxnSpanGCThreshold; newThresh != nil {
			if (*newThresh != hlc.Timestamp{}) {
				r.mu.Lock()
				r.mu.state.TxnSpanGCThreshold = newThresh
				r.mu.Unlock()
			}
			rResult.State.TxnSpanGCThreshold = nil
		}

		if rResult.State.UsingAppliedStateKey {
			r.mu.Lock()
			r.mu.state.UsingAppliedStateKey = true
			r.mu.Unlock()
			rResult.State.UsingAppliedStateKey = false
		}

		if (*rResult.State == storagepb.ReplicaState{}) {
			rResult.State = nil
		}
	}

	if change := rResult.ChangeReplicas; change != nil {
		if change.ChangeType == roachpb.REMOVE_REPLICA &&
			r.store.StoreID() == change.Replica.StoreID {
			// This wants to run as late as possible, maximizing the chances
			// that the other nodes have finished this command as well (since
			// processing the removal from the queue looks up the Range at the
			// lease holder, being too early here turns this into a no-op).
			// Lock ordering dictates that we don't hold any mutexes when adding,
			// so we fire it off in a task.
			r.store.replicaGCQueue.AddAsync(ctx, r, replicaGCPriorityRemoved)
		}
		rResult.ChangeReplicas = nil
	}

	if rResult.ComputeChecksum != nil {
		r.computeChecksumPostApply(ctx, *rResult.ComputeChecksum)
		rResult.ComputeChecksum = nil
	}

	if !rResult.Equal(storagepb.ReplicatedEvalResult{}) {
		log.Fatalf(ctx, "unhandled field in ReplicatedEvalResult: %s", pretty.Diff(rResult, storagepb.ReplicatedEvalResult{}))
	}
	return shouldAssert
}

func (r *Replica) handleLocalEvalResult(ctx context.Context, lResult result.LocalResult) {
	// Fields for which no action is taken in this method are zeroed so that
	// they don't trigger an assertion at the end of the method (which checks
	// that all fields were handled).
	{
		lResult.Reply = nil
	}

	// ======================
	// Non-state updates and actions.
	// ======================

	// The caller is required to detach and handle intents.
	if lResult.Intents != nil {
		log.Fatalf(ctx, "LocalEvalResult.Intents should be nil: %+v", lResult.Intents)
	}
	if lResult.EndTxns != nil {
		log.Fatalf(ctx, "LocalEvalResult.EndTxns should be nil: %+v", lResult.EndTxns)
	}

	if lResult.GossipFirstRange {
		// We need to run the gossip in an async task because gossiping requires
		// the range lease and we'll deadlock if we try to acquire it while
		// holding processRaftMu. Specifically, Replica.redirectOnOrAcquireLease
		// blocks waiting for the lease acquisition to finish but it can't finish
		// because we're not processing raft messages due to holding
		// processRaftMu (and running on the processRaft goroutine).
		if err := r.store.Stopper().RunAsyncTask(
			ctx, "storage.Replica: gossipping first range",
			func(ctx context.Context) {
				hasLease, pErr := r.getLeaseForGossip(ctx)

				if pErr != nil {
					log.Infof(ctx, "unable to gossip first range; hasLease=%t, err=%s", hasLease, pErr)
				} else if !hasLease {
					return
				}
				r.gossipFirstRange(ctx)
			}); err != nil {
			log.Infof(ctx, "unable to gossip first range: %s", err)
		}
		lResult.GossipFirstRange = false
	}

	if lResult.MaybeAddToSplitQueue {
		r.store.splitQueue.MaybeAddAsync(ctx, r, r.store.Clock().Now())
		lResult.MaybeAddToSplitQueue = false
	}

	if lResult.MaybeGossipSystemConfig {
		if err := r.MaybeGossipSystemConfig(ctx); err != nil {
			log.Error(ctx, err)
		}
		lResult.MaybeGossipSystemConfig = false
	}
	if lResult.MaybeGossipNodeLiveness != nil {
		if err := r.MaybeGossipNodeLiveness(ctx, *lResult.MaybeGossipNodeLiveness); err != nil {
			log.Error(ctx, err)
		}
		lResult.MaybeGossipNodeLiveness = nil
	}

	if lResult.Metrics != nil {
		r.store.metrics.handleMetricsResult(ctx, *lResult.Metrics)
		lResult.Metrics = nil
	}

	if lResult.UpdatedTxns != nil {
		for _, txn := range *lResult.UpdatedTxns {
			r.txnWaitQueue.UpdateTxn(ctx, txn)
			lResult.UpdatedTxns = nil
		}
	}

	if (lResult != result.LocalResult{}) {
		log.Fatalf(ctx, "unhandled field in LocalEvalResult: %s", pretty.Diff(lResult, result.LocalResult{}))
	}
}

func (r *Replica) handleEvalResultRaftMuLocked(
	ctx context.Context,
	lResult *result.LocalResult,
	rResult storagepb.ReplicatedEvalResult,
	raftAppliedIndex, leaseAppliedIndex uint64,
) {
	shouldAssert := r.handleReplicatedEvalResult(ctx, rResult, raftAppliedIndex, leaseAppliedIndex)
	if lResult != nil {
		r.handleLocalEvalResult(ctx, *lResult)
	}
	if shouldAssert {
		// Assert that the on-disk state doesn't diverge from the in-memory
		// state as a result of the side effects.
		r.mu.Lock()
		r.assertStateLocked(ctx, r.store.Engine())
		r.mu.Unlock()
	}
}

// proposalResult indicates the result of a proposal. Exactly one of
// Reply and Err is set, and it represents the result of the proposal.
type proposalResult struct {
	Reply   *roachpb.BatchResponse
	Err     *roachpb.Error
	Intents []result.IntentsWithArg
	EndTxns []result.EndTxnIntents
}

// evaluateProposal generates a Result from the given request by
// evaluating it, returning both state which is held only on the
// proposer and that which is to be replicated through Raft. The
// return value is ready to be inserted into Replica's proposal map
// and subsequently passed to submitProposalLocked.
//
// The method also returns a flag indicating if the request needs to
// be proposed through Raft and replicated. This flag will be false
// either if the request was a no-op or if it hit an error. In this
// case, the result can be sent directly back to the client without
// going through Raft, but while still handling LocalEvalResult.
//
// Replica.mu must not be held.
func (r *Replica) evaluateProposal(
	ctx context.Context, idKey storagebase.CmdIDKey, ba roachpb.BatchRequest, spans *spanset.SpanSet,
) (*result.Result, bool, *roachpb.Error) {
	if ba.Timestamp == (hlc.Timestamp{}) {
		return nil, false, roachpb.NewErrorf("can't propose Raft command with zero timestamp")
	}

	// Evaluate the commands. If this returns without an error, the batch should
	// be committed. Note that we don't hold any locks at this point. This is
	// important since evaluating a proposal is expensive.
	// TODO(tschottdorf): absorb all returned values in `res` below this point
	// in the call stack as well.
	batch, ms, br, res, pErr := r.evaluateWriteBatch(ctx, idKey, ba, spans)

	// Note: reusing the proposer's batch when applying the command on the
	// proposer was explored as an optimization but resulted in no performance
	// benefit.
	defer batch.Close()

	if pErr != nil {
		pErr = r.maybeSetCorrupt(ctx, pErr)

		txn := pErr.GetTxn()
		if txn != nil && ba.Txn == nil {
			log.Fatalf(ctx, "error had a txn but batch is non-transactional. Err txn: %s", txn)
		}

		// Failed proposals can't have any Result except for what's
		// whitelisted here.
		intents := res.Local.DetachIntents()
		endTxns := res.Local.DetachEndTxns(true /* alwaysOnly */)
		res.Local = result.LocalResult{
			Intents: &intents,
			EndTxns: &endTxns,
			Metrics: res.Local.Metrics,
		}
		res.Replicated.Reset()
		return &res, false /* needConsensus */, pErr
	}

	// Set the local reply, which is held only on the proposing replica and is
	// returned to the client after the proposal completes, or immediately if
	// replication is not necessary.
	res.Local.Reply = br

	// needConsensus determines if the result needs to be replicated and
	// proposed through Raft. This is necessary if at least one of the
	// following conditions is true:
	// 1. the request created a non-empty write batch.
	// 2. the request had an impact on the MVCCStats. NB: this is possible
	//    even with an empty write batch when stats are recomputed.
	// 3. the request has replicated side-effects.
	// 4. the cluster is in "clockless" mode, in which case consensus is
	//    used to enforce a linearization of all reads and writes.
	needConsensus := !batch.Empty() ||
		ms != (enginepb.MVCCStats{}) ||
		!res.Replicated.Equal(storagepb.ReplicatedEvalResult{}) ||
		r.store.Clock().MaxOffset() == timeutil.ClocklessMaxOffset

	if needConsensus {
		// Set the proposal's WriteBatch, which is the serialized representation of
		// the proposals effect on RocksDB.
		res.WriteBatch = &storagepb.WriteBatch{
			Data: batch.Repr(),
		}

		// Set the proposal's replicated result, which contains metadata and
		// side-effects that are to be replicated to all replicas.
		res.Replicated.IsLeaseRequest = ba.IsLeaseRequest()
		res.Replicated.Timestamp = ba.Timestamp
		if r.store.cfg.Settings.Version.IsActive(cluster.VersionMVCCNetworkStats) {
			res.Replicated.Delta = ms.ToStatsDelta()
		} else {
			res.Replicated.DeprecatedDelta = &ms
		}
		// If the RangeAppliedState key is not being used and the cluster version is
		// high enough to guarantee that all current and future binaries will
		// understand the key, we send the migration flag through Raft. Because
		// there is a delay between command proposal and application, we may end up
		// setting this migration flag multiple times. This is ok, because the
		// migration is idempotent.
		// TODO(nvanbenschoten): This will be baked in to 2.1, so it can be removed
		// in the 2.2 release.
		r.mu.RLock()
		usingAppliedStateKey := r.mu.state.UsingAppliedStateKey
		r.mu.RUnlock()
		if !usingAppliedStateKey &&
			r.ClusterSettings().Version.IsActive(cluster.VersionRangeAppliedStateKey) {
			if res.Replicated.State == nil {
				res.Replicated.State = &storagepb.ReplicaState{}
			}
			res.Replicated.State.UsingAppliedStateKey = true
		}
	}

	return &res, needConsensus, nil
}

// requestToProposal converts a BatchRequest into a ProposalData, by
// evaluating it. The returned ProposalData is partially valid even
// on a non-nil *roachpb.Error and should be proposed through Raft
// if ProposalData.command is non-nil.
func (r *Replica) requestToProposal(
	ctx context.Context,
	idKey storagebase.CmdIDKey,
	ba roachpb.BatchRequest,
	endCmds *endCmds,
	spans *spanset.SpanSet,
) (*ProposalData, *roachpb.Error) {
	res, needConsensus, pErr := r.evaluateProposal(ctx, idKey, ba, spans)

	// Fill out the results even if pErr != nil; we'll return the error below.
	proposal := &ProposalData{
		ctx:     ctx,
		idKey:   idKey,
		endCmds: endCmds,
		doneCh:  make(chan proposalResult, 1),
		Local:   &res.Local,
		Request: &ba,
	}

	if needConsensus {
		proposal.command = &storagepb.RaftCommand{
			ReplicatedEvalResult: res.Replicated,
			WriteBatch:           res.WriteBatch,
			LogicalOpLog:         res.LogicalOpLog,
		}
	}

	return proposal, pErr
}
