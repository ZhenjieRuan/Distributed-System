package kvpaxos

import "net"
import "fmt"
import "net/rpc"
import "log"
import "paxos"
import "sync"
import "sync/atomic"
import "os"
import "time"
import "syscall"
import "encoding/gob"
import "math/rand"

const Debug = 0

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug > 0 {
		fmt.Printf(format, a...)
	}
	return
}

type Op struct {
	Key   string // value of this op instance
	Value string // key this op instance affects
	JID   int64
	Me    int
	Op    string // operation type (get,put,append)
}

type KVPaxos struct {
	mu         sync.Mutex
	l          net.Listener
	me         int
	dead       int32 // for testing
	unreliable int32 // for testing
	px         *paxos.Paxos
	replied    map[int64]string
	applied    map[int64]bool
	log        map[int64]bool
	db         map[string]string
	processed  int
}

// interface for Paxos

//px = paxos.Make(peers []string, me int)

//px.Start(seq int, v interface{}) --> start agreement on new instance

//px.Status(seq int) (fate Fate, v interface{}) --> get info about an instance

//px.Done(seq int) --> ok to forget all instances <= seq

//px.Max() int --> highest instance seq known, or -1

//px.Min() int --> instances before this have been forgotten

func (kv *KVPaxos) Get(args *GetArgs, reply *GetReply) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	DPrintf("[JID %d] [**Get**] [KVPaxos %d] [Key %s]\n", args.JID, kv.me, args.Key)
	if _, exist := kv.log[args.JID]; !exist {
		kv.log[args.JID] = true
		op := Op{
			Key:   args.Key,
			Value: "",
			JID:   args.JID,
			Me:    kv.me,
			Op:    "Get",
		}
		seq := kv.px.Max() + 1
		ok := kv.makeAgreement(seq, op)
		if ok == false {
			// current server not in majority, can't serve client request
			reply.Err = Timeout
		} else {
			value, err := kv.updateDbAndGetValue(args.Key)
			if err == OK {
				kv.replied[args.CID] = value
				DPrintf("[Processed] [JID %d] [**Get**] [KVPaxos %d] [Key %s] [Value %s]\n", args.JID, kv.me, args.Key, value)
				reply.Value = value
				reply.Err = OK
			} else {
				reply.Err = ErrNoKey
			}
		}
	} else if _, exist = kv.replied[args.CID]; exist {
		reply.Err = OK
		reply.Value = kv.replied[args.CID]
		DPrintf("[Already Processed] [JID %d] [**Get**] [KVPaxos %d] [Key %s] [Value %s]\n", args.JID, kv.me, args.Key, kv.replied[args.CID])
	}
	return nil
}

func (kv *KVPaxos) PutAppend(args *PutAppendArgs, reply *PutAppendReply) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	DPrintf("[JID %d] [KVPaxos %d] [**%s**] [Key %s] [Value %s]\n", args.JID, kv.me, args.Op, args.Key, args.Value)
	if _, exist := kv.log[args.JID]; !exist {
		kv.log[args.JID] = true
		op := Op{
			Key:   args.Key,
			Value: args.Value,
			JID:   args.JID,
			Me:    kv.me,
			Op:    args.Op,
		}
		seq := kv.px.Max() + 1
		ok := kv.makeAgreement(seq, op)
		if ok == false {
			reply.Err = Timeout
		} else {
			reply.Err = OK
		}
	} else {
		reply.Err = OK
	}
	return nil
}

func (kv *KVPaxos) makeAgreement(seq int, op Op) bool {
	// try increasing seq number until current JID have been logged
	kv.px.Start(seq, op)
	for status, v := kv.checkStatus(seq); v.JID != op.JID; {
		if status != paxos.Decided {
			return false
		}
		seq++
		kv.px.Start(seq, op)
		status, v = kv.checkStatus(seq)
	}
	DPrintf("[Agreement] [JID %d] [KVPaxos %d] [Seq %d] [**%s**] [Key %s] [Value %s]\n", op.JID, kv.me, seq, op.Op, op.Key, op.Value)
	return true
}

func (kv *KVPaxos) updateDbAndGetValue(key string) (string, Err) {
	// applying all existing seq's change to db
	start := kv.processed
	end := kv.px.Max()
	for seq := start; seq < end; seq++ {
		status, value := kv.px.Status(seq)
		var v Op
		if status == paxos.Decided {
			v = value.(Op)
			DPrintf("[Updating] [KVPaxos %d] [Seq %d] [**%s**] [Key %s] [Value %s]\n", kv.me, seq, v.Op, v.Key, v.Value)
			kv.apply(v)
		} else if status != paxos.Forgotten {
			DPrintf("[Updating] [KVPaxos %d] [Seq %d] [Hole] [Status %v] [Value %v]\n", kv.me, seq, status, value)
			status, v = kv.learn(seq)
			if v.Value != "" {
				kv.apply(v)
			}
		}
	}

	kv.processed = end
	// call Done() to release memory
	kv.px.Done(end)
	if value, exist := kv.db[key]; !exist {
		return "", ErrNoKey
	} else {
		return value, OK
	}
}

func (kv *KVPaxos) apply(v Op) {
	if _, exist := kv.applied[v.JID]; !exist {
		switch v.Op {
		case "Put":
			kv.db[v.Key] = v.Value
		case "Append":
			kv.db[v.Key] += v.Value
		}
		kv.applied[v.JID] = true
	}
}

func (kv *KVPaxos) learn(seq int) (paxos.Fate, Op) {
	kv.px.Start(seq, Op{})
	status, v := kv.checkStatus(seq)
	if status != paxos.Decided {
		DPrintf("[Updating] [KVPaxos %d] [Seq %d] [Learn Failed]\n", kv.me, seq)
		return status, Op{}
	}
	DPrintf("[Updating] [KVPaxos %d] [Seq %d] [Learned] [**%s**] [Key %s] [Value %s]\n", kv.me, seq, v.Op, v.Key, v.Value)
	return status, v
}

func (kv *KVPaxos) checkStatus(seq int) (paxos.Fate, Op) {
	to := 10 * time.Millisecond
	for {
		status, op := kv.px.Status(seq)
		kv.printStatus(seq, status)
		if status == paxos.Decided {
			return status, op.(Op)
		}
		time.Sleep(to)
		if to < 10*time.Second {
			to *= 2
		} else {
			return status, Op{}
		}
	}
}

func (kv *KVPaxos) printStatus(seq int, status paxos.Fate) {
	DPrintf("[KVPaxos %d] [Seq %d] Status is ", kv.me, seq)
	switch status {
	case paxos.Forgotten:
		DPrintf("Forgotten\n")
	case paxos.Decided:
		DPrintf("Decided\n")
	case paxos.Pending:
		DPrintf("Pending\n")
	default:
		DPrintf("New\n")
	}
}

// tell the server to shut itself down.
// please do not change these two functions.
func (kv *KVPaxos) kill() {
	DPrintf("Kill(%d): die\n", kv.me)
	atomic.StoreInt32(&kv.dead, 1)
	kv.l.Close()
	kv.px.Kill()
}

// call this to find out if the server is dead.
func (kv *KVPaxos) isdead() bool {
	return atomic.LoadInt32(&kv.dead) != 0
}

// please do not change these two functions.
func (kv *KVPaxos) setunreliable(what bool) {
	if what {
		atomic.StoreInt32(&kv.unreliable, 1)
	} else {
		atomic.StoreInt32(&kv.unreliable, 0)
	}
}

func (kv *KVPaxos) isunreliable() bool {
	return atomic.LoadInt32(&kv.unreliable) != 0
}

//
// servers[] contains the ports of the set of
// servers that will cooperate via Paxos to
// form the fault-tolerant key/value service.
// me is the index of the current server in servers[].
//
func StartServer(servers []string, me int) *KVPaxos {
	// call gob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	gob.Register(Op{})

	kv := new(KVPaxos)
	kv.me = me
	kv.replied = make(map[int64]string)
	kv.applied = make(map[int64]bool)
	kv.log = make(map[int64]bool)
	kv.db = make(map[string]string)
	kv.processed = 0

	// Your initialization code here.

	rpcs := rpc.NewServer()
	rpcs.Register(kv)

	kv.px = paxos.Make(servers, me, rpcs)

	os.Remove(servers[me])
	l, e := net.Listen("unix", servers[me])
	if e != nil {
		log.Fatal("listen error: ", e)
	}
	kv.l = l

	// please do not change any of the following code,
	// or do anything to subvert it.

	go func() {
		for kv.isdead() == false {
			conn, err := kv.l.Accept()
			if err == nil && kv.isdead() == false {
				if kv.isunreliable() && (rand.Int63()%1000) < 100 {
					// discard the request.
					conn.Close()
				} else if kv.isunreliable() && (rand.Int63()%1000) < 200 {
					// process the request but force discard of reply.
					c1 := conn.(*net.UnixConn)
					f, _ := c1.File()
					err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
					if err != nil {
						fmt.Printf("shutdown: %v\n", err)
					}
					go rpcs.ServeConn(conn)
				} else {
					go rpcs.ServeConn(conn)
				}
			} else if err == nil {
				conn.Close()
			}
			if err != nil && kv.isdead() == false {
				fmt.Printf("KVPaxos(%v) accept: %v\n", me, err.Error())
				kv.kill()
			}
		}
	}()

	return kv
}
