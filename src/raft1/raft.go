package raft

// The file raftapi/raft.go defines the interface that raft must
// expose to servers (or the tester), but see comments below for each
// of these functions for more details.
//
// Make() creates a new raft peer that implements the raft interface.

import (
	"bytes"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"cpsc416-2025w1/labgob"
	"cpsc416-2025w1/labrpc"
	"cpsc416-2025w1/raftapi"
	tester "cpsc416-2025w1/tester1"
)

type State int

const (
	Follower State = iota
	Candidate
	Leader
)

type LogEntry struct {
	Term    int
	Command interface{}
}

// A Go object implementing a single Raft peer.
type Raft struct {
	mu        sync.Mutex          // Lock to protect shared access to this peer's state
	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *tester.Persister   // Object to hold this peer's persisted state
	me        int                 // this peer's index into peers[]
	dead      int32               // set by Kill()

	appendCond *sync.Cond // condition variable to signal applier

	// Persistent state
	currentTerm int
	votedFor    int // -1 means none
	log         []LogEntry

	// Volatile state
	commitIndex int
	lastApplied int
	state       State

	// snapshotting
	snapshotIndex int
	snapshotTerm  int
	snapshotData  []byte

	// Candidate
	voteCount int

	// Leader
	nextIndex  []int
	matchIndex []int

	// Timers
	electionTimeout   time.Duration
	lastHeartbeat     time.Time
	heartbeatInterval time.Duration

	// Apply Channel
	applyCh chan raftapi.ApplyMsg
}

func (rf *Raft) localToGlobal(local int) int {
	return local + rf.snapshotIndex
}

func (rf *Raft) globalToLocal(global int) int {
	return global - rf.snapshotIndex
}

func (rf *Raft) getLog(globalIndex int) LogEntry {
	if globalIndex < rf.snapshotIndex || globalIndex >= rf.localToGlobal(len(rf.log)) {
		panic(fmt.Sprintf("server: %v getLog: global index out of bounds: got %d, snapshotIndex=%d, localLen=%d, log=%v",
			rf.me, globalIndex, rf.snapshotIndex, len(rf.log), rf.log))
	}

	if globalIndex == rf.snapshotIndex {
		return LogEntry{Term: rf.snapshotTerm, Command: nil}
	}

	return rf.log[rf.globalToLocal(globalIndex)]
}

// return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	return rf.currentTerm, rf.state == Leader
}

// save Raft's persistent state to stable storage,
// where it can later be retrieved after a crash and restart.
// see paper's Figure 2 for a description of what should be persistent.
// before you've implemented snapshots, you should pass nil as the
// second argument to persister.Save().
// after you've implemented snapshots, pass the current snapshot
// (or nil if there's not yet a snapshot).
func (rf *Raft) persist() {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	e.Encode(rf.votedFor)
	e.Encode(rf.currentTerm)
	e.Encode(rf.log)
	e.Encode(rf.snapshotIndex)
	e.Encode(rf.snapshotTerm)
	raftstate := w.Bytes()
	rf.persister.Save(raftstate, rf.snapshotData)
}

// restore previously persisted state.
func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) < 1 {
		return
	}
	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)
	var votedFor int
	var currentTerm int
	var log []LogEntry
	var snapshotIndex int
	var snapshotTerm int
	if d.Decode(&votedFor) != nil ||
		d.Decode(&currentTerm) != nil ||
		d.Decode(&log) != nil ||
		d.Decode(&snapshotIndex) != nil ||
		d.Decode(&snapshotTerm) != nil {
		panic("failed to read persistent state")
	} else {
		rf.currentTerm = currentTerm
		rf.votedFor = votedFor
		rf.log = log
		rf.snapshotIndex = snapshotIndex
		rf.snapshotTerm = snapshotTerm
	}

	rf.snapshotData = rf.persister.ReadSnapshot()
}

// how many bytes in Raft's persisted log?
func (rf *Raft) PersistBytes() int {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.persister.RaftStateSize()
}

// the service says it has created a snapshot that has
// all info up to and including index. this means the
// service no longer needs the log through (and including)
// that index. Raft should now trim its log as much as possible.
func (rf *Raft) Snapshot(index int, snapshot []byte) {
	DPrintf("SNAPSHOTTING for server: %v at index %v", rf.me, index)
	rf.mu.Lock()
	DPrintf("1!!! calling getlog for index %v", index)

	rf.snapshotTerm = rf.getLog(index).Term
	rf.snapshotData = snapshot

	rf.log = append([]LogEntry{{Term: 0}}, rf.log[rf.globalToLocal(index)+1:]...)
	rf.snapshotIndex = index

	DPrintf("server %v setting last applied to max of last applied %v and index %v", rf.me, rf.lastApplied, index)
	rf.lastApplied = max(rf.lastApplied, index)
	rf.commitIndex = max(rf.commitIndex, index)

	rf.persist()
	rf.mu.Unlock()
}

type InstallSnapshotArgs struct {
	Term              int
	LeaderId          int
	LastIncludedIndex int
	LastIncludedTerm  int
	Data              []byte
}
type InstallSnapshotReply struct {
	Term    int
	Success bool
}

func (rf *Raft) sendInstallSnapshot(server int, args *InstallSnapshotArgs, reply *InstallSnapshotReply) bool {
	ok := rf.peers[server].Call("Raft.InstallSnapshot", args, reply)
	return ok
}

func (rf *Raft) InstallSnapshot(args *InstallSnapshotArgs, reply *InstallSnapshotReply) {
	rf.mu.Lock()

	reply.Term = rf.currentTerm
	reply.Success = false

	// new term or snapshot is more up to date (ie. stale snapshot was sent)
	if args.Term < rf.currentTerm || args.LastIncludedIndex <= rf.snapshotIndex {
		rf.mu.Unlock()
		return
	}

	if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.state = Follower
		rf.votedFor = -1
		rf.persist()
	}

	rf.lastHeartbeat = time.Now()

	DPrintf("2!!! calling getLog for index %v", args.LastIncludedIndex)
	if args.LastIncludedIndex < rf.localToGlobal(len(rf.log)) && rf.getLog(args.LastIncludedIndex).Term == args.LastIncludedTerm {
		rf.log = append([]LogEntry{{Term: 0}}, rf.log[rf.globalToLocal(args.LastIncludedIndex)+1:]...)
	} else {
		rf.log = []LogEntry{{Term: 0}}
	}

	rf.snapshotData = args.Data
	rf.snapshotIndex = args.LastIncludedIndex
	rf.snapshotTerm = args.LastIncludedTerm

	DPrintf("server %v setting last applied to max of last applied %v and last included index %v", rf.me, rf.lastApplied, args.LastIncludedIndex)
	rf.lastApplied = max(rf.lastApplied, args.LastIncludedIndex)
	rf.commitIndex = max(rf.commitIndex, args.LastIncludedIndex)

	reply.Success = true

	rf.persist()
	rf.mu.Unlock()

	rf.applyCh <- raftapi.ApplyMsg{
		SnapshotValid: true,
		Snapshot:      args.Data,
		SnapshotTerm:  args.LastIncludedTerm,
		SnapshotIndex: args.LastIncludedIndex,
	}
}

type RequestVoteArgs struct {
	Term          int
	CandidateId   int
	LastLogIndex  int
	LastLogTerm   int
	SnapshotIndex int
}

// example RequestVote RPC reply structure.
// field names must start with capital letters!
type RequestVoteReply struct {
	Term        int
	VoteGranted bool
}

// example RequestVote RPC handler.
// Grants vote if candidate's term is greater, logs are at least as up to date
// and has not voted for anther peer already
func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	reply.VoteGranted = false

	// already leader, do not grant vote
	if args.Term == rf.currentTerm && rf.state == Leader {
		reply.Term = rf.currentTerm
		return
	}

	if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.state = Follower
		rf.votedFor = -1
		rf.persist()
	}

	if args.Term == rf.currentTerm && (rf.votedFor == -1 || rf.votedFor == args.CandidateId) {
		lastLogIndex := rf.localToGlobal(len(rf.log) - 1)
		DPrintf("3!!! calling getLog for index %v", lastLogIndex)
		lastLogTerm := rf.getLog(lastLogIndex).Term
		DPrintf("Candidate: %v, Term: %v, LastLogIndex: %v, LastLogTerm: %v SnapshotIndex: %v\n", args.CandidateId, args.Term, args.LastLogIndex, args.LastLogTerm, rf.snapshotIndex)
		DPrintf("Follower: %v, Term: %v, LastLogIndex: %v, LastLogTerm: %v SnapshotIndex: %v\n", rf.me, rf.currentTerm, lastLogIndex, lastLogTerm, rf.snapshotIndex)

		if args.LastLogTerm > lastLogTerm ||
			(args.LastLogTerm == lastLogTerm && args.LastLogIndex >= lastLogIndex) {
			DPrintf("granting vote")
			rf.votedFor = args.CandidateId
			rf.lastHeartbeat = time.Now()
			rf.persist()
			reply.VoteGranted = true
		}
	}

	reply.Term = rf.currentTerm
}

// example code to send a RequestVote RPC to a server.
// server is the index of the target server in rf.peers[].
// expects RPC arguments in args.
// fills in *reply with RPC reply, so caller should
// pass &reply.
// the types of the args and reply passed to Call() must be
// the same as the types of the arguments declared in the
// handler function (including whether they are pointers).
//
// The labrpc package simulates a lossy network, in which servers
// may be unreachable, and in which requests and replies may be lost.
// Call() sends a request and waits for a reply. If a reply arrives
// within a timeout interval, Call() returns true; otherwise
// Call() returns false. Thus Call() may not return for a while.
// A false return can be caused by a dead server, a live server that
// can't be reached, a lost request, or a lost reply.
//
// Call() is guaranteed to return (perhaps after a delay) *except* if the
// handler function on the server side does not return.  Thus there
// is no need to implement your own timeouts around Call().
//
// look at the comments in ../labrpc/labrpc.go for more details.
//
// if you're having trouble getting RPC to work, check that you've
// capitalized all field names in structs passed over RPC, and
// that the caller passes the address of the reply struct with &, not
// the struct itself.
func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}

type AppendEntriesArgs struct {
	Term         int
	LeaderId     int
	PrevLogIndex int
	PrevLogTerm  int
	Entries      []LogEntry
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term          int
	Success       bool
	ConflictTerm  int
	ConflictIndex int
}

// Heartbeats (empty entries)
// Log Replication: replicate if the leaders term not lower and log consistency checks pass
func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	reply.Success = false
	reply.Term = rf.currentTerm
	reply.ConflictTerm = -1
	reply.ConflictIndex = -1

	if args.Term < rf.currentTerm {
		return
	}

	rf.state = Follower
	rf.currentTerm = args.Term
	rf.votedFor = -1
	rf.lastHeartbeat = time.Now()

	rf.persist()

	// the log index is beyond current log
	if args.PrevLogIndex >= rf.localToGlobal(len(rf.log)) {
		DPrintf("in here1")
		reply.ConflictIndex = rf.localToGlobal(len(rf.log))
		return
	}

	if args.PrevLogIndex < rf.snapshotIndex {
		DPrintf("in here2")
		reply.ConflictIndex = rf.snapshotIndex + 1
		return
	}

	DPrintf("4!!! calling getLog for index %v", args.PrevLogIndex)
	if rf.getLog(args.PrevLogIndex).Term != args.PrevLogTerm {
		DPrintf("5!!! calling getLog for index %v", args.PrevLogIndex)
		reply.ConflictTerm = rf.getLog(args.PrevLogIndex).Term

		reply.ConflictIndex = args.PrevLogIndex
		for i := args.PrevLogIndex - 1; i >= rf.snapshotIndex; i-- {
			DPrintf("6!!! calling getLog for index %v", i)
			if rf.getLog(i).Term != reply.ConflictTerm {
				break
			}
			reply.ConflictIndex = i
		}
		return
	}

	if len(args.Entries) > 0 {
		logIndex := args.PrevLogIndex + 1
		entryIndex := 0

		for logIndex < rf.localToGlobal(len(rf.log)) && entryIndex < len(args.Entries) {
			DPrintf("7!!! calling getLog for index %v", logIndex)
			if rf.getLog(logIndex).Term != args.Entries[entryIndex].Term {
				rf.log = rf.log[:rf.globalToLocal(logIndex)]
				break
			}
			logIndex++
			entryIndex++
		}
		if entryIndex < len(args.Entries) {
			rf.log = append(rf.log, args.Entries[entryIndex:]...)
		}

		rf.persist()
	}

	if args.LeaderCommit > rf.commitIndex {
		rf.commitIndex = min(args.LeaderCommit, rf.localToGlobal(len(rf.log)-1))
		DPrintf("3 --- server: %v, snapshotIndex: %v, log: %v, commitIndex: %v", rf.me, rf.snapshotIndex, rf.log, rf.commitIndex)
		rf.appendCond.Signal()
	}

	reply.Success = true
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)
	return ok
}

// applies committed logs to applyCh
// could use condition variables if desired
func (rf *Raft) applier() {
	for !rf.killed() {
		rf.mu.Lock()

		for rf.commitIndex <= rf.lastApplied {
			rf.appendCond.Wait()
		}

		if rf.lastApplied < rf.snapshotIndex {
			rf.lastApplied = rf.snapshotIndex
			rf.mu.Unlock()
			continue
		}

		messages := []raftapi.ApplyMsg{}
		for rf.commitIndex > rf.lastApplied {
			rf.lastApplied++
			DPrintf("8!!! calling getLog for index %v", rf.lastApplied)
			entry := rf.getLog(rf.lastApplied)
			messages = append(messages, raftapi.ApplyMsg{
				CommandValid: true,
				Command:      entry.Command,
				CommandIndex: rf.lastApplied,
			})
		}

		rf.mu.Unlock()

		for _, msg := range messages {
			rf.applyCh <- msg
		}
	}
}

// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election. even if the Raft instance has been killed,
// this function should return gracefully.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	index := -1
	term := -1
	isLeader := false

	if rf.state == Leader {
		DPrintf("server %v received command %v", rf.me, command)
		rf.log = append(rf.log, LogEntry{Term: rf.currentTerm, Command: command})
		index = rf.localToGlobal(len(rf.log) - 1)
		term = rf.currentTerm
		isLeader = true
		rf.persist()
		// logs get replicated in replicationHeartbeatLoop
	}

	return index, term, isLeader
}

// sends heartbeats (empty entries) if peer is caught up
// replicates logs if peer is not caught up
func (rf *Raft) replicationHeartbeatLoop(peer int) {
	for !rf.killed() {
		rf.mu.Lock()
		if rf.state != Leader {
			rf.mu.Unlock()
			return
		}

		prevIndex := rf.nextIndex[peer] - 1
		DPrintf("5.1 --- server: %v, prevIndex: %v, snapshotIndex: %v, isSmaller: %v", rf.me, prevIndex, rf.snapshotIndex, prevIndex < rf.snapshotIndex)

		if rf.nextIndex[peer] <= rf.snapshotIndex {
			DPrintf("5 --- server: %v, prevIndex: %v, snapshotIndex: %v, log: %v", rf.me, prevIndex, rf.snapshotIndex, rf.log)
			args := &InstallSnapshotArgs{
				Term:              rf.currentTerm,
				LeaderId:          rf.me,
				LastIncludedIndex: rf.snapshotIndex,
				LastIncludedTerm:  rf.snapshotTerm,
				Data:              rf.snapshotData,
			}
			rf.mu.Unlock()

			reply := &InstallSnapshotReply{}
			rf.sendInstallSnapshot(peer, args, reply)

			rf.mu.Lock()

			if rf.state != Leader || rf.currentTerm != args.Term {
				rf.mu.Unlock()
				return
			}

			if reply.Term > rf.currentTerm {
				rf.state = Follower
				rf.currentTerm = reply.Term
				rf.votedFor = -1
				rf.persist()
				rf.mu.Unlock()
				return
			}

			rf.nextIndex[peer] = rf.snapshotIndex + 1
			rf.matchIndex[peer] = rf.snapshotIndex

			rf.mu.Unlock()
			continue
		}

		DPrintf("9!!! calling getLog for index %v", prevIndex)
		prevTerm := rf.getLog(prevIndex).Term

		var entries []LogEntry
		if rf.nextIndex[peer] < rf.localToGlobal(len(rf.log)) {
			entries = append([]LogEntry{}, rf.log[rf.globalToLocal(rf.nextIndex[peer]):]...)
		} else {
			entries = nil // heartbeat
		}

		args := &AppendEntriesArgs{
			Term:         rf.currentTerm,
			LeaderId:     rf.me,
			PrevLogIndex: prevIndex,
			PrevLogTerm:  prevTerm,
			Entries:      entries,
			LeaderCommit: rf.commitIndex,
		}
		rf.mu.Unlock()

		reply := &AppendEntriesReply{}
		DPrintf("server %v sending append entries to peer %v, prevLogIndex %v, prevLogTerm %v, entries %v, leaderCommit %v snapshotIndex: %v\n leaderLog %v", rf.me, peer, prevIndex, prevTerm, entries, rf.commitIndex, rf.snapshotIndex, rf.log)
		ok := rf.sendAppendEntries(peer, args, reply)
		if !ok {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		rf.mu.Lock()
		if rf.state != Leader || rf.currentTerm != args.Term {
			rf.mu.Unlock()
			return
		}

		if reply.Term > rf.currentTerm {
			rf.state = Follower
			rf.currentTerm = reply.Term
			rf.votedFor = -1
			rf.persist()
			rf.mu.Unlock()
			return
		}

		// the next index wasn't updated when snapshot index was updated
		if reply.Success {
			rf.matchIndex[peer] = args.PrevLogIndex + len(args.Entries)
			rf.nextIndex[peer] = rf.matchIndex[peer] + 1

			DPrintf("setting matchIndex and nextIndex for peer %v to %v and %v, len of entries: %v\n", peer, rf.matchIndex[peer], rf.nextIndex[peer], len(entries))

			for N := len(rf.log) - 1; N > rf.globalToLocal(rf.commitIndex); N-- {
				count := 1
				globalN := rf.localToGlobal(N)
				for j := range rf.peers {
					if j != rf.me && rf.matchIndex[j] >= globalN {
						count++
					}
				}
				DPrintf("10!!! calling getLog for index %v with commitIndex %v", globalN, rf.commitIndex)
				if count > len(rf.peers)/2 && globalN > rf.snapshotIndex && rf.getLog(globalN).Term == rf.currentTerm {
					rf.commitIndex = globalN
					DPrintf("1 --- server: %v, snapshotIndex: %v, log: %v, commitIndex: %v", rf.me, rf.snapshotIndex, rf.log, rf.commitIndex)
					rf.appendCond.Signal()
					break
				}
			}
		} else {
			if reply.ConflictTerm == -1 {
				rf.nextIndex[peer] = reply.ConflictIndex
			} else if reply.ConflictIndex <= rf.snapshotIndex {
				var ssArgs InstallSnapshotArgs
				var ssReply InstallSnapshotReply

				ssArgs.LeaderId = rf.me
				ssArgs.Term = rf.currentTerm
				ssArgs.LastIncludedIndex = rf.snapshotIndex
				ssArgs.LastIncludedTerm = rf.snapshotTerm
				ssArgs.Data = rf.snapshotData

				DPrintf("4 --- server: %v sending snapshot to peer %v, snapshotIndex: %v, conflictIndex: %v", rf.me, peer, rf.snapshotIndex, reply.ConflictIndex)
				rf.mu.Unlock()
				rf.sendInstallSnapshot(peer, &ssArgs, &ssReply)
				rf.mu.Lock()

				if rf.state != Leader || rf.currentTerm != ssArgs.Term {
					rf.mu.Unlock()
					return
				}

				if reply.Term > rf.currentTerm {
					rf.state = Follower
					rf.currentTerm = reply.Term
					rf.votedFor = -1
					rf.persist()
					rf.mu.Unlock()
					return
				}

				rf.nextIndex[peer] = rf.snapshotIndex + 1
				rf.matchIndex[peer] = rf.snapshotIndex
			} else {
				foundTerm := false
				for i := len(rf.log) - 1; i >= 0; i-- {
					globalIndex := rf.localToGlobal(i)
					DPrintf("11!!! calling getLog for index %v", globalIndex)
					if rf.getLog(globalIndex).Term == reply.ConflictTerm {
						rf.nextIndex[peer] = rf.localToGlobal(i + 1)
						foundTerm = true
						break
					}
				}
				if !foundTerm {
					rf.nextIndex[peer] = reply.ConflictIndex
				}
			}
		}

		rf.mu.Unlock()
		time.Sleep(rf.heartbeatInterval)
	}
}

// the tester doesn't halt goroutines created by Raft after each test,
// but it does call the Kill() method. your code can use killed() to
// check whether Kill() has been called. the use of atomic avoids the
// need for a lock.
//
// the issue is that long-running goroutines use memory and may chew
// up CPU time, perhaps causing later tests to fail and generating
// confusing debug output. any goroutine with a long-running loop
// should call killed() to check whether it should stop.
func (rf *Raft) Kill() {
	atomic.StoreInt32(&rf.dead, 1)
	// Your code here, if desired.
}

func (rf *Raft) killed() bool {
	z := atomic.LoadInt32(&rf.dead)
	return z == 1
}

// Monitor election timeouts
// Used by leaders to send periodic heartbeats
func (rf *Raft) ticker() {
	for !rf.killed() {
		rf.mu.Lock()
		state := rf.state
		lastHeartbeat := rf.lastHeartbeat
		electionTimeout := rf.electionTimeout
		rf.mu.Unlock()

		if state != Leader && time.Since(lastHeartbeat) > electionTimeout {
			rf.startElection()
		}

		// pause for a random amount of time between 50 and 350
		// milliseconds.
		ms := 50 + (rand.Int63() % 300)
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
}

// Becomes candidate, increments term, votes for itself and tries to get votes
// from peers
func (rf *Raft) startElection() {
	rf.mu.Lock()
	rf.currentTerm++
	rf.state = Candidate
	rf.votedFor = rf.me
	rf.voteCount = 1
	term := rf.currentTerm
	lastLogIndex := rf.localToGlobal(len(rf.log) - 1)
	DPrintf("server %v: 12!!! calling getLog for index %v", rf.me, lastLogIndex)
	lastLogTerm := rf.getLog(lastLogIndex).Term
	rf.persist()
	rf.mu.Unlock()

	args := &RequestVoteArgs{
		Term:          term,
		CandidateId:   rf.me,
		LastLogIndex:  lastLogIndex,
		LastLogTerm:   lastLogTerm,
		SnapshotIndex: rf.snapshotIndex,
	}

	for i := range rf.peers {
		if i == rf.me {
			continue
		}

		go rf.requestVoteFromPeer(i, args)
	}
}

// Gather vote from peer and update election state
// If it wins majority of votes, becomes leader and start sending heartbeats
func (rf *Raft) requestVoteFromPeer(peer int, args *RequestVoteArgs) {
	reply := &RequestVoteReply{}
	backoff := 10 * time.Millisecond

	for !rf.sendRequestVote(peer, args, reply) && !rf.killed() {
		rf.mu.Lock()
		// no longer candidate (another leader sent hearbeats)
		if rf.state != Candidate || args.Term != rf.currentTerm {
			rf.mu.Unlock()
			return
		}
		rf.mu.Unlock()
		time.Sleep(backoff)
		backoff *= 2
	}

	rf.mu.Lock()
	defer rf.mu.Unlock()

	// no longer candidate (another leader sent hearbeats)
	if rf.state != Candidate || args.Term != rf.currentTerm {
		return
	}

	if reply.VoteGranted {
		rf.voteCount += 1

		if rf.state == Candidate && rf.voteCount > len(rf.peers)/2 {
			rf.state = Leader

			rf.nextIndex[rf.me] = rf.localToGlobal(len(rf.log))
			rf.matchIndex[rf.me] = rf.localToGlobal(len(rf.log) - 1)
			for i := range rf.peers {
				if i == rf.me {
					continue
				}

				rf.nextIndex[i] = rf.localToGlobal(len(rf.log))
				rf.matchIndex[i] = rf.localToGlobal(0)
				go rf.replicationHeartbeatLoop(i)
			}

		}
	} else if reply.Term > rf.currentTerm {
		rf.state = Follower
		rf.currentTerm = reply.Term
		rf.votedFor = -1
		rf.persist()
	}
}

// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. persister is a place for this server to
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
func Make(peers []*labrpc.ClientEnd, me int, persister *tester.Persister, applyCh chan raftapi.ApplyMsg) raftapi.Raft {
	rf := &Raft{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me
	rf.applyCh = applyCh
	rf.appendCond = sync.NewCond(&rf.mu)

	rf.currentTerm = 0
	rf.votedFor = -1
	rf.log = []LogEntry{{Term: 0}}

	rf.commitIndex = 0
	rf.lastApplied = 0
	rf.state = Follower

	rf.snapshotIndex = 0
	rf.snapshotTerm = 0
	rf.snapshotData = nil

	rf.voteCount = 0

	rf.heartbeatInterval = 50 * time.Millisecond
	rf.electionTimeout = time.Duration(300+rand.Int63n(200)) * time.Millisecond
	rf.lastHeartbeat = time.Now()

	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())

	rf.nextIndex = make([]int, len(peers))
	rf.matchIndex = make([]int, len(peers))

	for i := range len(peers) {
		rf.nextIndex[i] = rf.localToGlobal(len(rf.log))
		rf.matchIndex[i] = rf.localToGlobal(0)
	}

	// start ticker goroutine to start elections
	go rf.ticker()

	// start goroutine that applies commited log entries
	go rf.applier()

	return rf
}
