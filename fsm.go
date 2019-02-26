package raft

import (
	"container/list"
)

// todo: add availability methods
type FSM interface {
	Apply(cmd []byte) interface{}
}

func (r *Raft) fsmLoop() {
	defer r.wg.Done()
	for ne := range r.fsmApplyCh {
		debug(r.addr, "fsm.apply", ne.typ, ne.index)
		var resp interface{}
		if ne.typ == entryUpdate || ne.typ == entryQuery {
			resp = r.fsm.Apply(ne.entry.data)
		}
		ne.task.reply(resp)
	}
	debug(r, "fsmLoop shutdown")
}

// if commitIndex > lastApplied: increment lastApplied, apply
// log[lastApplied] to state machine
//
// in case of leader:
// 		- newEntries is not nil
//      - reply end user with response
func (r *Raft) applyCommitted(newEntries *list.List) {
	for {
		// send query entries to fsm
		if newEntries != nil {
			for newEntries.Len() > 0 {
				elem := newEntries.Front()
				ne := elem.Value.(NewEntry)
				if ne.index == r.lastApplied+1 && ne.typ == entryQuery {
					newEntries.Remove(elem)
					debug(r, "fms <- {", ne.typ, ne.index, "}")
					select {
					case <-r.shutdownCh:
						return
					case r.fsmApplyCh <- ne:
					}
				} else {
					break
				}
			}
		}

		if r.commitIndex > r.lastApplied {
			// get lastApplied+1 entry
			var ne NewEntry
			if newEntries != nil && newEntries.Len() > 0 {
				elem := newEntries.Front()
				if elem.Value.(NewEntry).index == r.lastApplied+1 {
					ne = newEntries.Remove(elem).(NewEntry)
				}
			}
			if ne.entry == nil {
				ne.entry = &entry{}
				r.storage.getEntry(r.lastApplied+1, ne.entry)
			}

			switch ne.typ {
			case entryConfig:
				r.configs.Committed = r.configs.Latest
				r.storage.setConfigs(r.configs)
				ne.reply(nil)
				if !r.configs.Latest.isVoter(r.id) && r.state == Leader {
					debug(r, "leader -> follower notVoter")
					r.state = Follower
					r.leader = ""
					r.stateChanged()
				}
				// todo: we can provide option to shutdown
				//       if it is no longer part of new config
			case entryUpdate, entryQuery, entryBarrier:
				debug(r, "fms <- {", ne.typ, ne.index, "}")
				select {
				case <-r.shutdownCh:
					return
				case r.fsmApplyCh <- ne:
				}
			default:
				assert(ne.typ == entryNop, "got %d, want %d", ne.typ, entryNop)
			}
			r.lastApplied++
		} else {
			return
		}
	}
}
