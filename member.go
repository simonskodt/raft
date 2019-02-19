package raft

import (
	"sync"
	"sync/atomic"
	"time"
)

type leaderUpdate struct {
	lastIndex, commitIndex uint64
}

type member struct {
	storage          *storage
	dialFn           dialFn
	addr             string
	timeout          time.Duration
	heartbeatTimeout time.Duration

	connPoolMu sync.Mutex
	connPool   []*netConn
	maxConns   int

	nextIndex  uint64
	matchIndex uint64

	// owned exclusively by raft main goroutine
	// used to recalculateMatch
	matchedIndex uint64

	// leader notifies replicator with update
	leaderUpdateCh chan leaderUpdate

	// from what time the replicator unable to reach this member
	// zero value means it is reachable
	noContactMu sync.RWMutex
	noContact   time.Time

	ldr string // used for debug() calls from replicator
}

func (m *member) getConn() (*netConn, error) {
	m.connPoolMu.Lock()
	defer m.connPoolMu.Unlock()

	num := len(m.connPool)
	if num == 0 {
		return dial(m.dialFn, m.addr, m.timeout)
	}
	var conn *netConn
	conn, m.connPool[num-1] = m.connPool[num-1], nil
	m.connPool = m.connPool[:num-1]
	return conn, nil
}

func (m *member) returnConn(conn *netConn) {
	m.connPoolMu.Lock()
	defer m.connPoolMu.Unlock()

	if len(m.connPool) < m.maxConns {
		m.connPool = append(m.connPool, conn)
	} else {
		_ = conn.close()
	}
}

func (m *member) doRPC(typ rpcType, req, resp command) error {
	conn, err := m.getConn()
	if err != nil {
		return err
	}
	if err = conn.doRPC(typ, req, resp); err != nil {
		_ = conn.close()
		return err
	}
	m.returnConn(conn)
	return nil
}

func (m *member) requestVote(req *voteRequest) (*voteResponse, error) {
	resp := new(voteResponse)
	err := m.doRPC(rpcVote, req, resp)
	return resp, err
}

func (m *member) appendEntries(req *appendEntriesRequest) (*appendEntriesResponse, error) {
	resp := new(appendEntriesResponse)
	err := m.doRPC(rpcAppendEntries, req, resp)

	m.noContactMu.Lock()
	if err == nil {
		m.noContact = time.Time{} // zeroing
	} else if m.noContact.IsZero() {
		m.noContact = time.Now()
	}
	m.noContactMu.Unlock()

	return resp, err
}

// retries request until success or got stop signal
// last return value is true in case of stop signal
func (m *member) retryAppendEntries(req *appendEntriesRequest, stopCh <-chan struct{}, newTermCh chan<- uint64) (*appendEntriesResponse, bool) {
	var failures uint64
	for {
		resp, err := m.appendEntries(req)
		if err != nil {
			failures++
			select {
			case <-stopCh:
				return resp, true
			case <-time.After(backoff(failures)):
				debug(m.ldr, m.addr, "retry appendEntries")
				continue
			}
		}
		if resp.term > req.term {
			select {
			case <-stopCh:
			case newTermCh <- resp.term:
			}
			return resp, true
		}
		return resp, false
	}
}

const maxAppendEntries = 64 // todo: should be configurable

func (m *member) replicate(req *appendEntriesRequest, stopCh <-chan struct{}, matchUpdatedCh chan<- *member, newTermCh chan<- uint64) {
	lastIndex, matchIndex := req.prevLogIndex, m.getMatchIndex()

	// know which entries to replicate: fixes m.nextIndex and m.matchIndex
	// after loop: m.matchIndex + 1 == m.nextIndex
	for matchIndex+1 != m.nextIndex {
		m.storage.fillEntries(req, m.nextIndex, m.nextIndex-1) // zero entries
		resp, stop := m.retryAppendEntries(req, stopCh, newTermCh)
		if stop {
			return
		} else if resp.success {
			matchIndex = req.prevLogIndex
			m.setMatchIndex(stopCh, matchIndex, matchUpdatedCh)
			break
		} else {
			m.nextIndex = max(min(m.nextIndex-1, resp.lastLogIndex+1), 1)
		}
		select {
		case <-stopCh:
			return
		default:
		}
	}

	closedCh := func() <-chan time.Time {
		ch := make(chan time.Time)
		close(ch)
		return ch
	}()
	timerCh := closedCh

	for {
		select {
		case <-stopCh:
			return
		case update := <-m.leaderUpdateCh:
			lastIndex, req.leaderCommitIndex = update.lastIndex, update.commitIndex
			debug(m.ldr, m.addr, "{last:", lastIndex, "commit:", req.leaderCommitIndex, "} <-leaderUpdateCh")
			timerCh = closedCh
		case <-timerCh:
		}

		// setup request
		if matchIndex < lastIndex {
			// replication of entries [m.nextIndex, lastIndex] is pending
			maxIndex := min(lastIndex, m.nextIndex+uint64(maxAppendEntries)-1)
			m.storage.fillEntries(req, m.nextIndex, maxIndex)
			debug(m.ldr, m.addr, ">> appendEntriesRequest", len(req.entries))
		} else {
			// send heartbeat
			req.prevLogIndex, req.prevLogTerm, req.entries = lastIndex, req.term, nil // zero entries
			debug(m.ldr, m.addr, ">> heartbeat")
		}

		resp, stop := m.retryAppendEntries(req, stopCh, newTermCh)
		if stop {
			return
		} else if !resp.success {
			// follower have transitioned to candidate and started election
			assert(resp.term > req.term, "%s %s follower must have started election", m.ldr, m.addr)
			return
		}

		m.nextIndex = resp.lastLogIndex + 1
		matchIndex = resp.lastLogIndex
		m.setMatchIndex(stopCh, matchIndex, matchUpdatedCh)

		if matchIndex < lastIndex {
			// replication of entries [m.nextIndex, lastIndex] is still pending: no more sleeping!!!
			timerCh = closedCh
		} else {
			timerCh = afterRandomTimeout(m.heartbeatTimeout / 10)
		}
	}
}

func (m *member) getMatchIndex() uint64 {
	return atomic.LoadUint64(&m.matchIndex)
}

func (m *member) setMatchIndex(stopCh <-chan struct{}, v uint64, updatedCh chan<- *member) {
	atomic.StoreUint64(&m.matchIndex, v)
	select {
	case <-stopCh:
	case updatedCh <- m:
	}
}
