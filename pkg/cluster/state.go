package cluster

import (
	"io"

	"github.com/hashicorp/raft"
)

// FSM - Finite State Machine for Raft
type FSM struct {
}

// Apply - TODO
func (fsm FSM) Apply(log *raft.Log) interface{} {
	return nil
}

// Restore - TODO
func (fsm FSM) Restore(snap io.ReadCloser) error {
	return nil
}

// Snapshot - TODO, returns an empty snapshot
func (fsm FSM) Snapshot() (raft.FSMSnapshot, error) {
	return Snapshot{}, nil
}

// Snapshot -
type Snapshot struct {
}

// Persist -
func (snapshot Snapshot) Persist(sink raft.SnapshotSink) error {
	return nil
}

// Release -
func (snapshot Snapshot) Release() {
}
