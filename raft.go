package raft

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"
)

type State byte

const (
	Follower  State = 'F'
	Candidate       = 'C'
	Leader          = 'L'
)

func (s State) String() string {
	switch s {
	case Follower:
		return "follower"
	case Candidate:
		return "candidate"
	case Leader:
		return "leader"
	}
	return string(s)
}

type Options struct {
	HeartbeatTimeout   time.Duration
	LeaderLeaseTimeout time.Duration
}

func DefaultOptions() Options {
	return Options{
		HeartbeatTimeout:   1000 * time.Millisecond,
		LeaderLeaseTimeout: 1000 * time.Millisecond,
	}
}

type Raft struct {
	*server
	dialFn dialFn

	id      NodeID
	addr    string //todo: always get it from latest config
	configs Configs
	wg      sync.WaitGroup

	fsmApplyCh chan NewEntry
	fsm        FSM

	storage *Storage
	term    uint64
	state   State
	leader  string //todo: use id instead of addr

	votedFor  string //todo: use id instead of addr
	hbTimeout time.Duration

	lastLogIndex uint64
	lastLogTerm  uint64
	commitIndex  uint64
	lastApplied  uint64

	connPools map[string]*connPool

	ldrLeaseTimeout time.Duration
	ldr             *leadership
	taskCh          chan Task
	newEntryCh      chan NewEntry
	trace           Trace
	shutdownMu      sync.Mutex
	shutdownCh      chan struct{}
}

func New(id NodeID, opt Options, fsm FSM, storage *Storage, trace Trace) (*Raft, error) {
	if err := storage.init(); err != nil {
		return nil, err
	}

	term, votedFor, err := storage.vars.GetVote()
	if err != nil {
		return nil, err
	}

	var lastLogIndex, lastLogTerm uint64
	last, err := storage.lastEntry()
	if err != nil {
		return nil, err
	}
	if last != nil {
		lastLogIndex, lastLogTerm = last.index, last.term
	}

	configs, err := storage.getConfigs()
	if err != nil {
		return nil, err
	}

	addr := ""
	if self, ok := configs.Latest.Nodes[id]; ok {
		addr = self.Addr
	}

	server := newServer(2 * opt.HeartbeatTimeout)
	r := &Raft{
		id:              id,
		addr:            addr,
		storage:         storage,
		fsm:             fsm,
		term:            term,
		votedFor:        votedFor,
		lastLogIndex:    lastLogIndex,
		lastLogTerm:     lastLogTerm,
		configs:         configs,
		state:           Follower,
		hbTimeout:       opt.HeartbeatTimeout,
		ldrLeaseTimeout: opt.LeaderLeaseTimeout,
		dialFn:          net.DialTimeout,
		server:          server,
		connPools:       make(map[string]*connPool),
		fsmApplyCh:      make(chan NewEntry, 128), // todo configurable capacity
		newEntryCh:      make(chan NewEntry, 100), // todo configurable capacity
		taskCh:          make(chan Task, 100),     // todo configurable capacity
		trace:           trace,
		shutdownCh:      make(chan struct{}),
	}
	return r, nil
}

func (r *Raft) ID() NodeID {
	return r.id
}

func (r *Raft) FSM() FSM {
	return r.fsm
}

// tells whether shutdown was called
func (r *Raft) shutdownCalled() bool {
	select {
	case <-r.shutdownCh:
		return true
	default:
		return false
	}
}

// todo: note that we dont support multiple listeners

func (r *Raft) Serve(l net.Listener) error {
	r.shutdownMu.Lock()
	shutdownCalled := r.shutdownCalled()
	if !shutdownCalled {
		r.wg.Add(2)
	}
	r.shutdownMu.Unlock()
	if shutdownCalled {
		return ErrServerClosed
	}

	if r.trace.Starting != nil {
		r.trace.Starting(r.liveInfo())
	}
	go r.loop()
	go r.fsmLoop()
	return r.server.serve(l)
}

func (r *Raft) Shutdown() *sync.WaitGroup {
	r.shutdownMu.Lock()
	defer r.shutdownMu.Unlock()
	if !r.shutdownCalled() {
		debug(r.id, ">> shutdown()")
		if r.trace.ShuttingDown != nil {
			r.trace.ShuttingDown(r.liveInfo())
		}
		close(r.shutdownCh)
	}
	return &r.wg
}

func (r *Raft) loop() {
	defer r.wg.Done()
	for {
		select {
		case <-r.shutdownCh:
			debug(r, "loop shutdown")
			r.server.shutdown()
			debug(r, "server shutdown")
			close(r.fsmApplyCh)
			return
		default:
		}

		switch r.state {
		case Follower:
			r.runFollower()
		case Candidate:
			r.runCandidate()
		case Leader:
			r.runLeader()
		}
	}
}

func (r *Raft) setTerm(term uint64) {
	if err := r.storage.vars.SetVote(term, ""); err != nil {
		panic(fmt.Sprintf("stable.Set failed: %v", err))
	}
	r.term, r.votedFor = term, ""
}

func (r *Raft) setVotedFor(v string) {
	if err := r.storage.vars.SetVote(r.term, v); err != nil {
		panic(fmt.Sprintf("save votedFor failed: %v", err))
	}
	r.votedFor = v
}

func (r *Raft) getConnPool(addr string) *connPool {
	pool, ok := r.connPools[addr]
	if !ok {
		pool = &connPool{
			addr:    addr,
			dialFn:  r.dialFn,
			timeout: 10 * time.Second, // todo
			max:     3,                //todo
		}
		r.connPools[addr] = pool
	}
	return pool
}

type NotLeaderError struct {
	Leader string
}

func (e NotLeaderError) Error() string {
	return "node is not the leader"
}

func afterRandomTimeout(min time.Duration) <-chan time.Time {
	return time.After(min + time.Duration(rand.Int63())%min)
}
