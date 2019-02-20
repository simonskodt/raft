package raft

import (
	"container/list"
	"sort"
	"time"
)

func (r *Raft) runLeader() {
	ldr := &leaderState{
		Raft:         r,
		leaseTimeout: r.heartbeatTimeout, // todo: should it be same as heartbeatTimeout ?
		newEntries:   list.New(),
	}
	ldr.runLoop()
}

type leaderState struct {
	*Raft

	// if quorum of nodes are not reachable for this duration
	// leader steps down to follower
	leaseTimeout time.Duration

	// leader term starts from this index.
	// this index refers to noop entry
	startIndex uint64

	// queue in which user submitted entries are enqueued
	// committed entries are dequeued and handed over to fsm go-routine
	newEntries *list.List

	// holds running replications, key is addr
	repls map[string]*replication
}

func (ldr *leaderState) runLoop() {
	assert(ldr.leaderID == ldr.addr, "%s ldr.leaderID: got %s, want %s", ldr, ldr.leaderID, ldr.addr)

	ldr.startIndex = ldr.lastLogIndex + 1

	// add a blank no-op entry into log at the start of its term
	ldr.storeNewEntry(NewEntry{
		entry: &entry{
			typ: entryNoop,
		},
	})

	// to receive new term notifications from replicators
	newTermCh := make(chan uint64, len(ldr.members))

	// to receive matchIndex updates from replicators
	matchUpdatedCh := make(chan *replication, len(ldr.members))

	// to send stop signal to replicators
	stopReplsCh := make(chan struct{})

	defer func() {
		close(stopReplsCh)

		if ldr.leaderID == ldr.addr {
			ldr.leaderID = ""
		}

		// respond to any pending user entries
		for e := ldr.newEntries.Front(); e != nil; e = e.Next() {
			e.Value.(NewEntry).sendResponse(NotLeaderError{ldr.leaderID})
		}
	}()

	// start replication routine for each follower
	ldr.repls = make(map[string]*replication)
	for _, m := range ldr.members {
		if m.addr == ldr.addr {
			continue
		}

		// matchIndex initialized to zero
		m.matchIndex = 0 // todo: should we reset always to zero?
		repl := &replication{
			member:           m,
			heartbeatTimeout: ldr.heartbeatTimeout,
			storage:          ldr.storage,
			nextIndex:        ldr.lastLogIndex + 1, // nextIndex initialized to leader last log index + 1
			matchIndex:       m.matchIndex,
			stopCh:           stopReplsCh,
			matchUpdatedCh:   matchUpdatedCh,
			newTermCh:        newTermCh,
			leaderUpdateCh:   make(chan leaderUpdate, 1),
			str:              ldr.String() + " " + m.addr,
		}
		ldr.repls[m.addr] = repl

		// send initial empty AppendEntries RPCs (heartbeat) to each follower
		req := &appendEntriesRequest{
			term:              ldr.term,
			leaderID:          ldr.addr,
			leaderCommitIndex: ldr.commitIndex,
			prevLogIndex:      ldr.lastLogIndex,
			prevLogTerm:       ldr.lastLogTerm,
		}
		// don't retry on failure. so that we can respond to apply/inspect
		debug(repl, ">> firstHeartbeat")
		_, _ = repl.appendEntries(req)

		// todo: should runLeader wait for repls to stop ?
		ldr.wg.Add(1)
		go func() {
			defer ldr.wg.Done()
			repl.runLoop(req)
			debug(repl, "replication closed")
		}()
	}

	// todo: should count only voting repls here
	if len(ldr.repls) == 0 {
		ldr.commitAndApplyOnMajority() // for noop entry
	}

	leaseTimer := time.NewTicker(ldr.leaseTimeout)
	defer leaseTimer.Stop()

	for ldr.state == leader {
		select {
		case <-ldr.shutdownCh:
			return

		case newTerm := <-newTermCh:
			// if response contains term T > currentTerm:
			// set currentTerm = T, convert to follower
			debug(ldr, "leader -> follower")
			ldr.state = follower
			ldr.setTerm(newTerm)
			ldr.leaderID = ""
			stateChanged(ldr.Raft)
			return

		case rpc := <-ldr.rpcCh:
			ldr.replyRPC(rpc)

		case m := <-matchUpdatedCh:
		loop:
			// get latest matchIndex from all notified members
			for {
				m.member.matchIndex = m.getMatchIndex()
				select {
				case m = <-matchUpdatedCh:
					break
				default:
					break loop
				}
			}

			ldr.commitAndApplyOnMajority()

		case ne := <-ldr.ApplyCh:
			ldr.storeNewEntry(ne)

		case f := <-ldr.inspectCh:
			f(ldr.Raft)

		case <-leaseTimer.C:
			if !ldr.isQuorumReachable() {
				ldr.state = follower
				ldr.leaderID = ""
				stateChanged(ldr.Raft)
			}
		}
	}
}

func (ldr *leaderState) storeNewEntry(ne NewEntry) {
	if ne.entry == nil {
		ne.entry = &entry{}
	}
	ne.data, ne.Data = ne.Data, nil
	ne.index, ne.term = ldr.lastLogIndex+1, ldr.term

	// append entry to local log
	if ne.typ == entryNoop {
		debug(ldr, "log.append noop", ne.index)
	} else {
		debug(ldr, "log.append cmd", ne.index)
	}
	ldr.storage.append(ne.entry)
	ldr.lastLogIndex, ldr.lastLogTerm = ne.index, ne.term
	ldr.newEntries.PushBack(ne)

	if ne.typ != entryNoop {
		// we updated lastLogIndex, so notify replicators
		ldr.notifyReplicators()
	}
}

// computes N such that, a majority of matchIndex[i] ≥ N
func (ldr *leaderState) majorityMatchIndex() uint64 {
	majorityMatchIndex := ldr.lastLogIndex
	if len(ldr.members) > 1 {
		matched := make(decrUint64Slice, len(ldr.members))
		for i, m := range ldr.members {
			if m.addr == ldr.addr {
				matched[i] = ldr.lastLogIndex
			} else {
				matched[i] = m.matchIndex
			}
		}
		// sort in decrease order
		sort.Sort(matched)
		majorityMatchIndex = matched[ldr.quorumSize()-1]
	}
	return majorityMatchIndex
}

// If majorityMatchIndex(N) > commitIndex,
// and log[N].term == currentTerm: set commitIndex = N
func (ldr *leaderState) commitAndApplyOnMajority() {
	majorityMatchIndex := ldr.majorityMatchIndex()

	// note: if majorityMatchIndex >= ldr.startIndex, it also mean
	// majorityMatchIndex.term == currentTerm
	if majorityMatchIndex > ldr.commitIndex && majorityMatchIndex >= ldr.startIndex {
		ldr.commitIndex = majorityMatchIndex
		debug(ldr, "commitIndex", ldr.commitIndex)
		ldr.fsmApply(ldr.newEntries)
		ldr.notifyReplicators() // we updated commit index
	}
}

func (ldr *leaderState) isQuorumReachable() bool {
	reachable := 0
	now := time.Now()
	for _, m := range ldr.members {
		if m.addr == ldr.addr {
			reachable++
		} else {
			repl := ldr.repls[m.addr]
			repl.noContactMu.RLock()
			noContact := repl.noContact
			repl.noContactMu.RUnlock()
			if noContact.IsZero() || now.Sub(noContact) < ldr.leaseTimeout {
				reachable++
			}
		}
	}
	if reachable < ldr.quorumSize() {
		debug(ldr, "quorumUnreachable: ", reachable, "<", ldr.quorumSize())
	}
	return reachable >= ldr.quorumSize()
}

func (ldr *leaderState) notifyReplicators() {
	// todo: should count only voting repls here
	if len(ldr.repls) == 0 {
		ldr.commitAndApplyOnMajority()
		return
	}

	leaderUpdate := leaderUpdate{
		lastIndex:   ldr.lastLogIndex,
		commitIndex: ldr.commitIndex,
	}
	for _, repl := range ldr.repls {
		select {
		case repl.leaderUpdateCh <- leaderUpdate:
		case <-repl.leaderUpdateCh:
			repl.leaderUpdateCh <- leaderUpdate
		}
	}
}

type decrUint64Slice []uint64

func (s decrUint64Slice) Len() int           { return len(s) }
func (s decrUint64Slice) Less(i, j int) bool { return s[i] > s[j] }
func (s decrUint64Slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
