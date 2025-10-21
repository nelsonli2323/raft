package raft

// The file raftapi/raft.go defines the interface that raft must
// expose to servers (or the tester), but see comments below for each
// of these functions for more details.
//
// Make() creates a new raft peer that implements the raft interface.

import (
	//	"bytes"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	//	"cpsc416-2025w1/labgob"
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

	// Persistent state
	currentTerm int
	votedFor    int // -1 means none
	log         []LogEntry

	// Volatile state
	commitIndex int
	lastApplied int
	state       State

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
	// Your code here (3C).
	// Example:
	// w := new(bytes.Buffer)
	// e := labgob.NewEncoder(w)
	// e.Encode(rf.xxx)
	// e.Encode(rf.yyy)
	// raftstate := w.Bytes()
	// rf.persister.Save(raftstate, nil)
}

// restore previously persisted state.
func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) < 1 { // bootstrap without any state?
		return
	}
	// Your code here (3C).
	// Example:
	// r := bytes.NewBuffer(data)
	// d := labgob.NewDecoder(r)
	// var xxx
	// var yyy
	// if d.Decode(&xxx) != nil ||
	//    d.Decode(&yyy) != nil {
	//   error...
	// } else {
	//   rf.xxx = xxx
	//   rf.yyy = yyy
	// }
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
	// Your code here (3D).

}

// example RequestVote RPC arguments structure.
// field names must start with capital letters!
type RequestVoteArgs struct {
	Term         int
	CandidateId  int
	LastLogIndex int
	LastLogTerm  int
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

	if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.state = Follower
		rf.votedFor = -1
	}

	if args.Term == rf.currentTerm && (rf.votedFor == -1 || rf.votedFor == args.CandidateId) {
		lastLogIndex := len(rf.log) - 1
		lastLogTerm := rf.log[lastLogIndex].Term

		if args.LastLogTerm > lastLogTerm || (args.LastLogTerm == lastLogTerm && args.LastLogIndex >= lastLogIndex) {
			rf.votedFor = args.CandidateId
			reply.VoteGranted = true
			rf.lastHeartbeat = time.Now()
			//persist
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
	Term    int
	Success bool
}

// Heartbeats (empty entries)
// Log Replication: replicate if the leaders term not lower and log consistency checks pass
func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	reply.Success = false
	reply.Term = rf.currentTerm

	rf.lastHeartbeat = time.Now()
	if args.Term < rf.currentTerm {
		return
	} else if args.Term >= rf.currentTerm {
		rf.state = Follower
		rf.currentTerm = args.Term
		rf.votedFor = -1
		rf.lastHeartbeat = time.Now()
		reply.Success = true
	}

	if args.PrevLogIndex >= len(rf.log) || rf.log[args.PrevLogIndex].Term != args.PrevLogTerm {
		reply.Success = false
		return
	} else {
		rf.log = rf.log[:args.PrevLogIndex+1]
		rf.log = append(rf.log, args.Entries...)
		if args.LeaderCommit > rf.commitIndex {
			rf.commitIndex = min(args.LeaderCommit, len(rf.log)-1)
		}
		reply.Success = true
		// persist
	}
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)
	return ok
}

// applies committed logs to applyCh
func (rf *Raft) applier() {
	for !rf.killed() {
		rf.mu.Lock()
		for rf.lastApplied < rf.commitIndex {
			rf.lastApplied++
			entry := rf.log[rf.lastApplied]
			msg := raftapi.ApplyMsg{
				CommandValid: true,
				Command:      entry.Command,
				CommandIndex: rf.lastApplied,
			}
			rf.applyCh <- msg
		}
		rf.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
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
		rf.log = append(rf.log, LogEntry{Term: rf.currentTerm, Command: command})
		index = len(rf.log) - 1
		term = rf.currentTerm
		isLeader = true
		// persist
		for i := range rf.peers {
			if i != rf.me {
				go rf.replicateLog(i)
			}
		}
	}

	return index, term, isLeader
}

// being replicating new log entries
func (rf *Raft) replicateLog(peer int) {
	for !rf.killed() {
		rf.mu.Lock()
		if rf.state != Leader {
			rf.mu.Unlock()
			return
		}

		prevIndex := rf.nextIndex[peer] - 1
		prevTerm := rf.log[prevIndex].Term
		entries := append([]LogEntry{}, rf.log[prevIndex+1:]...)

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
		ok := rf.sendAppendEntries(peer, args, reply)
		if !ok {
			time.Sleep(50 * time.Millisecond)
			continue
		}

		rf.mu.Lock()
		if reply.Term > rf.currentTerm {
			rf.state = Follower
			rf.currentTerm = reply.Term
			rf.votedFor = -1
			// persist
			rf.mu.Unlock()
			return
		}

		if reply.Success {
			rf.matchIndex[peer] = args.PrevLogIndex + len(args.Entries)
			rf.nextIndex[peer] = rf.matchIndex[peer] + 1

			// try to advance commit index
			for N := len(rf.log) - 1; N > rf.commitIndex; N-- {
				count := 1
				for j := range rf.peers {
					if j != rf.me && rf.matchIndex[j] >= N {
						count++
					}
				}
				if count > len(rf.peers)/2 && rf.log[N].Term == rf.currentTerm {
					rf.commitIndex = N
					break
				}
			}
			rf.mu.Unlock()
			return
		} else {
			rf.nextIndex[peer]--
			rf.mu.Unlock()
			time.Sleep(50 * time.Millisecond)
		}
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
	rf.currentTerm += 1
	rf.state = Candidate
	rf.votedFor = rf.me
	rf.voteCount = 1
	term := rf.currentTerm
	lastLogIndex := len(rf.log) - 1
	lastLogTerm := rf.log[lastLogIndex].Term
	rf.mu.Unlock()

	args := &RequestVoteArgs{
		Term:         term,
		CandidateId:  rf.me,
		LastLogIndex: lastLogIndex,
		LastLogTerm:  lastLogTerm,
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

	if rf.sendRequestVote(peer, args, reply) {
		rf.mu.Lock()
		defer rf.mu.Unlock()

		if rf.state != Candidate || args.Term != rf.currentTerm {
			return
		}

		if reply.VoteGranted {
			rf.voteCount += 1

			if rf.state == Candidate && rf.voteCount > len(rf.peers)/2 {
				rf.state = Leader
				for i := range rf.peers {
					rf.nextIndex[i] = len(rf.log)
					rf.matchIndex[i] = 0
				}
				go rf.sendHeartbeats()
			}
		} else if reply.Term > rf.currentTerm {
			rf.state = Follower
			rf.currentTerm = reply.Term
			rf.votedFor = -1
		}
	}
}

// Empty append entries acting as heartbeats
func (rf *Raft) sendHeartbeats() {
	for {
		rf.mu.Lock()
		if rf.state != Leader || rf.killed() {
			rf.mu.Unlock()
			return
		}

		for i := range rf.peers {
			if i == rf.me {
				continue
			}

			prevIndex := rf.nextIndex[i] - 1
			prevTerm := rf.log[prevIndex].Term

			args := &AppendEntriesArgs{
				Term:         rf.currentTerm,
				LeaderId:     rf.me,
				PrevLogIndex: prevIndex,
				PrevLogTerm:  prevTerm,
				Entries:      nil,
				LeaderCommit: rf.commitIndex,
			}

			reply := &AppendEntriesReply{}
			go rf.sendAppendEntries(i, args, reply)
		}
		rf.mu.Unlock()
		time.Sleep(rf.heartbeatInterval)
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
func Make(peers []*labrpc.ClientEnd, me int,
	persister *tester.Persister, applyCh chan raftapi.ApplyMsg) raftapi.Raft {
	rf := &Raft{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me
	rf.applyCh = applyCh

	rf.currentTerm = 0
	rf.votedFor = -1
	rf.log = []LogEntry{{Term: 0}}

	rf.commitIndex = 0
	rf.lastApplied = 0
	rf.state = Follower

	rf.voteCount = 0

	rf.nextIndex = make([]int, len(peers))
	rf.matchIndex = make([]int, len(peers))

	rf.heartbeatInterval = 150 * time.Millisecond
	rf.electionTimeout = time.Duration(300+rand.Int63n(200)) * time.Millisecond
	rf.lastHeartbeat = time.Now()
	// May need to add timeout for candidate

	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())

	// start ticker goroutine to start elections
	go rf.ticker()

	// start goroutine that applies commited log entries
	go rf.applier()

	return rf
}
