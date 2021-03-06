/*
 * Copyright 2017-2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package conn

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/dgraph-io/badger/y"
	"github.com/dgraph-io/dgo/protos/api"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/raftwal"
	"github.com/dgraph-io/dgraph/x"
	"github.com/golang/glog"
	"golang.org/x/net/context"
)

var (
	ErrNoNode = x.Errorf("No node has been set up yet")
)

type Node struct {
	x.SafeMutex

	joinLock sync.Mutex

	// Used to keep track of lin read requests.
	requestCh chan linReadReq

	// SafeMutex is for fields which can be changed after init.
	_confState *raftpb.ConfState
	_raft      raft.Node

	// Fields which are never changed after init.
	Cfg         *raft.Config
	MyAddr      string
	Id          uint64
	peers       map[uint64]string
	confChanges map[uint64]chan error
	messages    chan sendmsg
	RaftContext *pb.RaftContext
	Store       *raftwal.DiskStorage
	Rand        *rand.Rand

	Proposals proposals
	// applied is used to keep track of the applied RAFT proposals.
	// The stages are proposed -> committed (accepted by cluster) ->
	// applied (to PL) -> synced (to BadgerDB).
	Applied y.WaterMark
}

type ToGlog struct {
}

func (rl *ToGlog) Debug(v ...interface{})                   { glog.V(3).Info(v...) }
func (rl *ToGlog) Debugf(format string, v ...interface{})   { glog.V(3).Infof(format, v...) }
func (rl *ToGlog) Error(v ...interface{})                   { glog.Error(v...) }
func (rl *ToGlog) Errorf(format string, v ...interface{})   { glog.Errorf(format, v...) }
func (rl *ToGlog) Info(v ...interface{})                    { glog.Info(v...) }
func (rl *ToGlog) Infof(format string, v ...interface{})    { glog.Infof(format, v...) }
func (rl *ToGlog) Warning(v ...interface{})                 { glog.Warning(v...) }
func (rl *ToGlog) Warningf(format string, v ...interface{}) { glog.Warningf(format, v...) }
func (rl *ToGlog) Fatal(v ...interface{})                   { glog.Fatal(v...) }
func (rl *ToGlog) Fatalf(format string, v ...interface{})   { glog.Fatalf(format, v...) }
func (rl *ToGlog) Panic(v ...interface{})                   { log.Panic(v...) }
func (rl *ToGlog) Panicf(format string, v ...interface{})   { log.Panicf(format, v...) }

func NewNode(rc *pb.RaftContext, store *raftwal.DiskStorage) *Node {
	snap, err := store.Snapshot()
	x.Check(err)

	n := &Node{
		Id:     rc.Id,
		MyAddr: rc.Addr,
		Store:  store,
		Cfg: &raft.Config{
			ID:              rc.Id,
			ElectionTick:    100, // 2s if we call Tick() every 20 ms.
			HeartbeatTick:   1,   // 20ms if we call Tick() every 20 ms.
			Storage:         store,
			MaxSizePerMsg:   1 << 20, // 1MB should allow more batching.
			MaxInflightMsgs: 256,
			// We don't need lease based reads. They cause issues because they
			// require CheckQuorum to be true, and that causes a lot of issues
			// for us during cluster bootstrapping and later. A seemingly
			// healthy cluster would just cause leader to step down due to
			// "inactive" quorum, and then disallow anyone from becoming leader.
			// So, let's stick to default options.  Let's achieve correctness,
			// then we achieve performance. Plus, for the Dgraph alphas, we'll
			// be soon relying only on Timestamps for blocking reads and
			// achieving linearizability, than checking quorums (Zero would
			// still check quorums).
			ReadOnlyOption: raft.ReadOnlySafe,
			// When a disconnected node joins back, it forces a leader change,
			// as it starts with a higher term, as described in Raft thesis (not
			// the paper) in section 9.6. This setting can avoid that by only
			// increasing the term, if the node has a good chance of becoming
			// the leader.
			PreVote: true,

			// We can explicitly set Applied to the first index in the Raft log,
			// so it does not derive it separately, thus avoiding a crash when
			// the Applied is set to below snapshot index by Raft.
			// In case this is a new Raft log, first would be 1, and therefore
			// Applied would be zero, hence meeting the condition by the library
			// that Applied should only be set during a restart.
			//
			// Update: Set the Applied to the latest snapshot, because it seems
			// like somehow the first index can be out of sync with the latest
			// snapshot.
			Applied: snap.Metadata.Index,

			Logger: &ToGlog{},
		},
		// processConfChange etc are not throttled so some extra delta, so that we don't
		// block tick when applyCh is full
		Applied:     y.WaterMark{Name: fmt.Sprintf("Applied watermark")},
		RaftContext: rc,
		Rand:        rand.New(&lockedSource{src: rand.NewSource(time.Now().UnixNano())}),
		confChanges: make(map[uint64]chan error),
		messages:    make(chan sendmsg, 100),
		peers:       make(map[uint64]string),
		requestCh:   make(chan linReadReq),
	}
	n.Applied.Init()
	// This should match up to the Applied index set above.
	n.Applied.SetDoneUntil(n.Cfg.Applied)
	glog.Infof("Setting raft.Config to: %+v\n", n.Cfg)
	return n
}

// SetRaft would set the provided raft.Node to this node.
// It would check fail if the node is already set.
func (n *Node) SetRaft(r raft.Node) {
	n.Lock()
	defer n.Unlock()
	x.AssertTrue(n._raft == nil)
	n._raft = r
}

// Raft would return back the raft.Node stored in the node.
func (n *Node) Raft() raft.Node {
	n.RLock()
	defer n.RUnlock()
	return n._raft
}

// SetConfState would store the latest ConfState generated by ApplyConfChange.
func (n *Node) SetConfState(cs *raftpb.ConfState) {
	glog.Infof("Setting conf state to %+v\n", cs)
	n.Lock()
	defer n.Unlock()
	n._confState = cs
}

func (n *Node) DoneConfChange(id uint64, err error) {
	n.Lock()
	defer n.Unlock()
	ch, has := n.confChanges[id]
	if !has {
		return
	}
	delete(n.confChanges, id)
	ch <- err
}

func (n *Node) storeConfChange(che chan error) uint64 {
	n.Lock()
	defer n.Unlock()
	id := rand.Uint64()
	_, has := n.confChanges[id]
	for has {
		id = rand.Uint64()
		_, has = n.confChanges[id]
	}
	n.confChanges[id] = che
	return id
}

// ConfState would return the latest ConfState stored in node.
func (n *Node) ConfState() *raftpb.ConfState {
	n.RLock()
	defer n.RUnlock()
	return n._confState
}

func (n *Node) Peer(pid uint64) (string, bool) {
	n.RLock()
	defer n.RUnlock()
	addr, ok := n.peers[pid]
	return addr, ok
}

// addr must not be empty.
func (n *Node) SetPeer(pid uint64, addr string) {
	x.AssertTruef(addr != "", "SetPeer for peer %d has empty addr.", pid)
	n.Lock()
	defer n.Unlock()
	n.peers[pid] = addr
}

func (n *Node) Send(m raftpb.Message) {
	x.AssertTruef(n.Id != m.To, "Sending message to itself")
	data, err := m.Marshal()
	x.Check(err)

	// As long as leadership is stable, any attempted Propose() calls should be reflected in the
	// next raft.Ready.Messages. Leaders will send MsgApps to the followers; followers will send
	// MsgProp to the leader. It is up to the transport layer to get those messages to their
	// destination. If a MsgApp gets dropped by the transport layer, it will get retried by raft
	// (i.e. it will appear in a future Ready.Messages), but MsgProp will only be sent once. During
	// leadership transitions, proposals may get dropped even if the network is reliable.
	//
	// We can't do a select default here. The messages must be sent to the channel, otherwise we
	// should block until the channel can accept these messages. BatchAndSendMessages would take
	// care of dropping messages which can't be sent due to network issues to the corresponding
	// node. But, we shouldn't take the liberty to do that here. It would take us more time to
	// repropose these dropped messages anyway, than to block here a bit waiting for the messages
	// channel to clear out.
	n.messages <- sendmsg{to: m.To, data: data}
}

func (n *Node) Snapshot() (raftpb.Snapshot, error) {
	if n == nil || n.Store == nil {
		return raftpb.Snapshot{}, errors.New("Uninitialized node or raft store")
	}
	return n.Store.Snapshot()
}

func (n *Node) SaveToStorage(h raftpb.HardState, es []raftpb.Entry, s raftpb.Snapshot) {
	for {
		if err := n.Store.Save(h, es, s); err != nil {
			glog.Errorf("While trying to save Raft update: %v. Retrying...", err)
		} else {
			return
		}
	}
}

func (n *Node) PastLife() (uint64, bool, error) {
	var (
		sp      raftpb.Snapshot
		idx     uint64
		restart bool
		rerr    error
	)
	sp, rerr = n.Store.Snapshot()
	if rerr != nil {
		return 0, false, rerr
	}
	if !raft.IsEmptySnap(sp) {
		glog.Infof("Found Snapshot.Metadata: %+v\n", sp.Metadata)
		restart = true
		idx = sp.Metadata.Index
	}

	var hd raftpb.HardState
	hd, rerr = n.Store.HardState()
	if rerr != nil {
		return 0, false, rerr
	}
	if !raft.IsEmptyHardState(hd) {
		glog.Infof("Found hardstate: %+v\n", hd)
		restart = true
	}

	var num int
	num, rerr = n.Store.NumEntries()
	if rerr != nil {
		return 0, false, rerr
	}
	glog.Infof("Group %d found %d entries\n", n.RaftContext.Group, num)
	// We'll always have at least one entry.
	if num > 1 {
		restart = true
	}
	return idx, restart, nil
}

const (
	messageBatchSoftLimit = 10000000
)

func (n *Node) BatchAndSendMessages() {
	batches := make(map[uint64]*bytes.Buffer)
	failedConn := make(map[uint64]bool)
	for {
		totalSize := 0
		sm := <-n.messages
	slurp_loop:
		for {
			var buf *bytes.Buffer
			if b, ok := batches[sm.to]; !ok {
				buf = new(bytes.Buffer)
				batches[sm.to] = buf
			} else {
				buf = b
			}
			totalSize += 4 + len(sm.data)
			x.Check(binary.Write(buf, binary.LittleEndian, uint32(len(sm.data))))
			x.Check2(buf.Write(sm.data))

			if totalSize > messageBatchSoftLimit {
				// We limit the batch size, but we aren't pushing back on
				// n.messages, because the loop below spawns a goroutine
				// to do its dirty work.  This is good because right now
				// (*node).send fails(!) if the channel is full.
				break
			}

			select {
			case sm = <-n.messages:
			default:
				break slurp_loop
			}
		}

		for to, buf := range batches {
			if buf.Len() == 0 {
				continue
			}

			addr, has := n.Peer(to)
			pool, err := Get().Get(addr)
			if !has || err != nil {
				if exists := failedConn[to]; !exists {
					// So that we print error only the first time we are not able to connect.
					// Otherwise, the log is polluted with multiple errors.
					glog.Warningf("No healthy connection to node Id: %#x addr: [%s], err: %v\n",
						to, addr, err)
					failedConn[to] = true
				}
				continue
			}

			failedConn[to] = false
			data := make([]byte, buf.Len())
			copy(data, buf.Bytes())
			go n.doSendMessage(to, pool, data)
			buf.Reset()
		}
	}
}

func (n *Node) doSendMessage(to uint64, pool *Pool, data []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := pool.Get()

	c := pb.NewRaftClient(client)
	p := &api.Payload{Data: data}
	batch := &pb.RaftBatch{
		Context: n.RaftContext,
		Payload: p,
	}

	// We don't need to run this in a goroutine, because doSendMessage is
	// already being run in one.
	_, err := c.RaftMessage(ctx, batch)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "TransientFailure"):
			glog.Warningf("Reporting node: %d addr: %s as unreachable.", to, pool.Addr)
			n.Raft().ReportUnreachable(to)
			pool.SetUnhealthy()
		default:
			glog.V(3).Infof("Error while sending Raft message to node with addr: %s, err: %v\n",
				pool.Addr, err)
		}
	}
	// We don't need to do anything if we receive any error while sending message.
	// RAFT would automatically retry.
	return
}

// Connects the node and makes its peerPool refer to the constructed pool and address
// (possibly updating ourselves from the old address.)  (Unless pid is ourselves, in which
// case this does nothing.)
func (n *Node) Connect(pid uint64, addr string) {
	if pid == n.Id {
		return
	}
	if paddr, ok := n.Peer(pid); ok && paddr == addr {
		// Already connected.
		return
	}
	// Here's what we do.  Right now peerPool maps peer node id's to addr values.  If
	// a *pool can be created, good, but if not, we still create a peerPoolEntry with
	// a nil *pool.
	if addr == n.MyAddr {
		// TODO: Note this fact in more general peer health info somehow.
		glog.Infof("Peer %d claims same host as me\n", pid)
		n.SetPeer(pid, addr)
		return
	}
	Get().Connect(addr)
	n.SetPeer(pid, addr)
}

func (n *Node) DeletePeer(pid uint64) {
	if pid == n.Id {
		return
	}
	n.Lock()
	defer n.Unlock()
	delete(n.peers, pid)
}

var errInternalRetry = errors.New("Retry proposal again")

func (n *Node) proposeConfChange(ctx context.Context, pb raftpb.ConfChange) error {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	ch := make(chan error, 1)
	id := n.storeConfChange(ch)
	// TODO: Delete id from the map.
	pb.ID = id
	if err := n.Raft().ProposeConfChange(cctx, pb); err != nil {
		if cctx.Err() != nil {
			return errInternalRetry
		}
		glog.Warningf("Error while proposing conf change: %v", err)
		return err
	}
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-cctx.Done():
		return errInternalRetry
	}
}

func (n *Node) AddToCluster(ctx context.Context, pid uint64) error {
	addr, ok := n.Peer(pid)
	x.AssertTruef(ok, "Unable to find conn pool for peer: %#x", pid)
	rc := &pb.RaftContext{
		Addr:  addr,
		Group: n.RaftContext.Group,
		Id:    pid,
	}
	rcBytes, err := rc.Marshal()
	x.Check(err)

	cc := raftpb.ConfChange{
		Type:    raftpb.ConfChangeAddNode,
		NodeID:  pid,
		Context: rcBytes,
	}
	err = errInternalRetry
	for err == errInternalRetry {
		glog.Infof("Trying to add %#x to cluster. Addr: %v\n", pid, addr)
		glog.Infof("Current confstate at %#x: %+v\n", n.Id, n.ConfState())
		err = n.proposeConfChange(ctx, cc)
	}
	return err
}

func (n *Node) ProposePeerRemoval(ctx context.Context, id uint64) error {
	if n.Raft() == nil {
		return ErrNoNode
	}
	if _, ok := n.Peer(id); !ok && id != n.RaftContext.Id {
		return x.Errorf("Node %#x not part of group", id)
	}
	cc := raftpb.ConfChange{
		Type:   raftpb.ConfChangeRemoveNode,
		NodeID: id,
	}
	err := errInternalRetry
	for err == errInternalRetry {
		err = n.proposeConfChange(ctx, cc)
	}
	return err
}

type linReadReq struct {
	// A one-shot chan which we send a raft index upon.
	indexCh chan<- uint64
}

var errReadIndex = x.Errorf("Cannot get linearized read (time expired or no configured leader)")

func (n *Node) WaitLinearizableRead(ctx context.Context) error {
	indexCh := make(chan uint64, 1)

	select {
	case n.requestCh <- linReadReq{indexCh: indexCh}:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case index := <-indexCh:
		if index == 0 {
			return errReadIndex
		}
		return n.Applied.WaitForMark(ctx, index)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (n *Node) RunReadIndexLoop(closer *y.Closer, readStateCh <-chan raft.ReadState) {
	defer closer.Done()
	readIndex := func() (uint64, error) {
		// Read Request can get rejected then we would wait idefinitely on the channel
		// so have a timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		var activeRctx [8]byte
		x.Check2(n.Rand.Read(activeRctx[:]))
		if err := n.Raft().ReadIndex(ctx, activeRctx[:]); err != nil {
			glog.Errorf("Error while trying to call ReadIndex: %v\n", err)
			return 0, err
		}

	again:
		select {
		case <-closer.HasBeenClosed():
			return 0, errors.New("Closer has been called")
		case rs := <-readStateCh:
			if !bytes.Equal(activeRctx[:], rs.RequestCtx) {
				goto again
			}
			return rs.Index, nil
		case <-ctx.Done():
			glog.Warningf("[%#x] Read index context timed out\n", n.Id)
			return 0, errInternalRetry
		}
	} // end of readIndex func

	// We maintain one linearizable ReadIndex request at a time.  Others wait queued behind
	// requestCh.
	requests := []linReadReq{}
	for {
		select {
		case <-closer.HasBeenClosed():
			return
		case rs := <-readStateCh:
			// Do nothing, discard ReadState as we don't have any pending ReadIndex requests.
			glog.Warningf("Received a read state unexpectedly: %+v\n", rs)
		case req := <-n.requestCh:
		slurpLoop:
			for {
				requests = append(requests, req)
				select {
				case req = <-n.requestCh:
				default:
					break slurpLoop
				}
			}
			for {
				index, err := readIndex()
				if err == errInternalRetry {
					continue
				}
				if err != nil {
					index = 0
					glog.Errorf("[%#x] While trying to do lin read index: %v", n.Id, err)
				}
				for _, req := range requests {
					req.indexCh <- index
				}
				break
			}
			requests = requests[:0]
		}
	}
}
