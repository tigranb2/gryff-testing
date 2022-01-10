package genericsmr

import (
	"bufio"
	"encoding/binary"
	"fastrpc"
	"fmt"
	"clientproto"
	"genericsmrproto"
	"io"
	"net"
	"os"
	"state"
	"time"
	"strings"
	"log"
	"dlog"
	"sync"
	"stats"
)

const CHAN_BUFFER_SIZE = 200000
const ADAPT_TIME_SEC = 10

type RPCPair struct {
	Obj  fastrpc.Serializable
	Chan chan fastrpc.Serializable
}

type Propose struct {
	*genericsmrproto.Propose
	Reply *bufio.Writer
}

type ClientRPC struct {
	Obj fastrpc.Serializable
	Reply *bufio.Writer
}

type ClientRPCPair struct {
	Obj fastrpc.Serializable
	Chan chan *ClientRPC
}

type Beacon struct {
	Rid       int32
	Timestamp uint64
}

type Replica struct {
	N            int        // total number of replicas
	Id           int32      // the ID of the current replica
	PeerAddrList []string   // array with the IP:port address of every replica
	Peers        []net.Conn // cache of connections to all other replicas
	PeerReaders  []*bufio.Reader
	PeerWriters  []*bufio.Writer
	Alive        []bool // connection status
	Listener     net.Listener

	State *state.State

	ProposeChan chan *Propose // channel for client proposals
	BeaconChan  chan *Beacon  // channel for beacons from peer replicas

	Shutdown bool

	Thrifty bool // send only as many messages as strictly required?
	Exec    bool // execute commands?
	Dreply  bool // reply to client after command has been executed?
	Beacon  bool // send beacons to detect how fast are the other replicas?

	Durable     bool     // log to a stable store?
	StableStore *os.File // file support for the persistent log

	PreferredPeerOrder []int32 // replicas in the preferred order of communication

	rpcTable map[uint8]*RPCPair
	rpcCode  uint8
	clientRpcTable map[uint8]*ClientRPCPair

	Ewma []float64

	OnClientConnect chan bool
	clientConnect bool
	pingChan chan *ClientRPC

	clientReaders map[int32]*bufio.Reader
	clientWriters map[int32]*bufio.Writer
	clientIdMapsLock *sync.Mutex
	statsFile string
	Stats *stats.StatsMap
	DoneAdaptingChan chan bool
	delayRPC []map[uint8]bool
	delayedRPC []map[uint8]chan fastrpc.Serializable
}

func NewReplica(id int, peerAddrList []string, thrifty bool, exec bool, dreply bool, clientConnect bool, statsFile string) *Replica {
	r := &Replica{
		len(peerAddrList),
		int32(id),
		peerAddrList,
		make([]net.Conn, len(peerAddrList)),
		make([]*bufio.Reader, len(peerAddrList)),
		make([]*bufio.Writer, len(peerAddrList)),
		make([]bool, len(peerAddrList)),
		nil,
		state.NewState(),
		make(chan *Propose, CHAN_BUFFER_SIZE),
		make(chan *Beacon, CHAN_BUFFER_SIZE),
		false,
		thrifty,
		exec,
		dreply,
		false,
		false,
		nil,
		make([]int32, len(peerAddrList)),
		make(map[uint8]*RPCPair),
		clientproto.GEN_GENERIC_SMR_BEACON_REPLY + 1,
		make(map[uint8]*ClientRPCPair),
		make([]float64, len(peerAddrList)),
		make(chan bool, 100),
		clientConnect,
		make(chan *ClientRPC, CHAN_BUFFER_SIZE),
		make(map[int32]*bufio.Reader),
		make(map[int32]*bufio.Writer),
		new(sync.Mutex),
		statsFile,
		stats.NewStatsMap(),
		make(chan bool, 1),
		make([]map[uint8]bool, 0),      // delayRPC
		make([]map[uint8]chan fastrpc.Serializable, 0), // delayedRPC
	}

	var err error

	if r.StableStore, err = os.Create(fmt.Sprintf("stable-store-replica%d", r.Id)); err != nil {
		log.Fatal(err)
	}

	for i := 0; i < r.N; i++ {
		r.PreferredPeerOrder[i] = int32((int(r.Id) + 1 + i) % r.N)
		r.Ewma[i] = 0.0
	}

	r.delayRPC = make([]map[uint8]bool, r.N)
	r.delayedRPC = make([]map[uint8]chan fastrpc.Serializable, r.N)
	for i := 0; i < r.N; i++ {
		r.delayRPC[i] = make(map[uint8]bool)
		r.delayedRPC[i] = make(map[uint8]chan fastrpc.Serializable)
	}

	r.RegisterClientRPC(new(clientproto.Ping), clientproto.GEN_PING, r.pingChan)

	go r.monitorPings()

	return r
}

func (r *Replica) GetClientWriter(clientId int32) *bufio.Writer {
	r.clientIdMapsLock.Lock()
	client, ok := r.clientWriters[clientId]
	r.clientIdMapsLock.Unlock()
	if ok {
		return client
	} else {
		return nil
	}
}

func (r *Replica) UndelayRPCs(replica int, opCode uint8) {
	for len(r.delayedRPC[replica][opCode]) > 0 {
		msg := <-r.delayedRPC[replica][opCode]
		w := r.PeerWriters[replica]
		w.WriteByte(opCode)
		msg.Marshal(w)
		w.Flush()
	}
}

func (r *Replica) DelayRPC(replica int, opCode uint8) {
	_, ok := r.delayedRPC[replica][opCode]
	if !ok {
		r.delayedRPC[replica][opCode] = make(chan fastrpc.Serializable, CHAN_BUFFER_SIZE)
	}
	r.delayRPC[replica][opCode] = true
}

func (r *Replica) ShouldDelayNextRPC(replica int, opCode uint8) bool {
	delay, ok := r.delayRPC[replica][opCode]
	r.delayRPC[replica][opCode] = false
	return ok && delay
}

func (r *Replica) SlowClock(c chan bool) {
	for !r.Shutdown {
		time.Sleep(150 * 1e6) // 150 ms
		c <- true
	}
}

func (r *Replica) StopAdapting() {
	time.Sleep(1000 * 1000 * 1000 * ADAPT_TIME_SEC)
	r.Beacon = false
	time.Sleep(1000 * 1000 * 1000)

	for i := 0; i < r.N-1; i++ {
		min := i
		for j := i + 1; j < r.N-1; j++ {
			if r.Ewma[r.PreferredPeerOrder[j]] < r.Ewma[r.PreferredPeerOrder[min]] {
				min = j
			}
		}
		aux := r.PreferredPeerOrder[i]
		r.PreferredPeerOrder[i] = r.PreferredPeerOrder[min]
		r.PreferredPeerOrder[min] = aux
	}

	log.Printf("Ewma=%v.\n", r.Ewma)
	log.Printf("PreferredPeerOrder=%v.\n", r.PreferredPeerOrder)
	r.DoneAdaptingChan <- true
}

/* Client API */

func (r *Replica) Ping(args *genericsmrproto.PingArgs, reply *genericsmrproto.PingReply) error {
	return nil
}

func (r *Replica) BeTheLeader(args *genericsmrproto.BeTheLeaderArgs, reply *genericsmrproto.BeTheLeaderReply) error {
	return nil
}

func (r *Replica) monitorPings() {
	for !r.Shutdown {
		select {
			case pingS := <-r.pingChan:
				ping := pingS.Obj.(*clientproto.Ping)
				r.handlePing(ping, pingS.Reply)
		}
	}
}

func (r *Replica) handlePing(ping *clientproto.Ping, w *bufio.Writer) {
	log.Printf("Received Ping from client %d\n", ping.ClientId)
	w.WriteByte(clientproto.GEN_PING_REPLY)
	pingReply := &clientproto.PingReply{r.Id, ping.Ts}
	pingReply.Marshal(w)
	w.Flush()
}

/* ============= */

func (r *Replica) ConnectToPeers() {
	var b [4]byte
	bs := b[:4]
	done := make(chan bool)

	go r.waitForPeerConnections(done)

	log.Printf("Beginning to connect to peers...\n")
	//connect to peers
	for i := 0; i < int(r.Id); i++ {
		for done := false; !done; {
			log.Printf("Dialing peer %d with addr %s\n", i,
				r.PeerAddrList[i])
			if conn, err := net.Dial("tcp", r.PeerAddrList[i]); err == nil {
				r.Peers[i] = conn
				done = true
			} else {
				log.Printf("Error dialing peer %d: %v\n", i, err)
				time.Sleep(1e9)
			}
		}
		log.Printf("Sending my id %d to peer %d\n", r.Id, i)
		binary.LittleEndian.PutUint32(bs, uint32(r.Id))
		if _, err := r.Peers[i].Write(bs); err != nil {
			log.Println("Write id error:", err)
			continue
		}
		r.Alive[i] = true
		r.PeerReaders[i] = bufio.NewReader(r.Peers[i])
		r.PeerWriters[i] = bufio.NewWriter(r.Peers[i])
	}
	<-done
	log.Printf("Replica id: %d. Done connecting to peers\n", r.Id)

	for rid, reader := range r.PeerReaders {
		if int32(rid) == r.Id {
			continue
		}
		go r.replicaListener(rid, reader)
	}
}

func (r *Replica) ConnectToPeersNoListeners() {
	var b [4]byte
	bs := b[:4]
	done := make(chan bool)

	go r.waitForPeerConnections(done)

	//connect to peers
	for i := 0; i < int(r.Id); i++ {
		for done := false; !done; {
			if conn, err := net.Dial("tcp", r.PeerAddrList[i]); err == nil {
				r.Peers[i] = conn
				done = true
			} else {
				time.Sleep(1e9)
			}
		}
		dlog.Printf("Writing my id %d to peer %d.\n", uint32(r.Id), i)
		binary.LittleEndian.PutUint32(bs, uint32(r.Id))
		if _, err := r.Peers[i].Write(bs); err != nil {
			log.Println("Write id error:", err)
			continue
		}
		r.Alive[i] = true
		r.PeerReaders[i] = bufio.NewReader(r.Peers[i])
		r.PeerWriters[i] = bufio.NewWriter(r.Peers[i])
	}
	<-done
	log.Printf("Replica id: %d. Done connecting to peers\n", r.Id)
}

/* Peer (replica) connections dispatcher */
func (r *Replica) waitForPeerConnections(done chan bool) {
	var b [4]byte
	bs := b[:4]

	var err error
	lAddr := r.PeerAddrList[r.Id][strings.Index(r.PeerAddrList[r.Id], ":"):]
	log.Printf("Listening for peers on %s\n", lAddr)
	r.Listener, err = net.Listen("tcp", lAddr)
	if err != nil {
		log.Printf("Error listening to peer %d: %v\n", r.Id, err)
		os.Exit(1)
	}

	for i := r.Id + 1; i < int32(r.N); i++ {
		conn, err := r.Listener.Accept()
		if err != nil {
			log.Println("Error accepting connection from peer: %v\n",
				err)
			continue
		}
		dlog.Printf("Accepted connection from peer %s.\n", conn.RemoteAddr().String())
		if n, err := io.ReadFull(conn, bs); err != nil || n != len(bs) {
			log.Printf("Error reading peer id: %v (%d bytes read)\n", err, n)
			continue
		}
		id := int32(binary.LittleEndian.Uint32(bs))
		if id <= r.Id || id >= int32(r.N) {
			log.Fatalf("Read incorrect id %d from connecting peer\n", id)
		}
		log.Printf("Peer sent %d as id (%d total peers).\n", id, len(r.Peers))
		r.Peers[id] = conn
		r.PeerReaders[id] = bufio.NewReader(conn)
		r.PeerWriters[id] = bufio.NewWriter(conn)
		r.Alive[id] = true
		log.Printf("Successfully established connection with peer %d\n", id)
	}

	done <- true
}

/* Client connections dispatcher */
func (r *Replica) WaitForClientConnections() {
	// NOTE: all peers must be connected before clients begin attempting to connect
	//   otherwise we might accept a client connection as a peer connection due to
	//   listening for RPCs on the same port
	for !r.Shutdown {
		conn, err := r.Listener.Accept()
		if err != nil {
			log.Printf("Error accepting client connection: %v\n", err)
			continue
		}
		log.Printf("Accepted client connection %s\n", conn.RemoteAddr().String())
		go r.clientListener(conn)

		if r.clientConnect {
			r.OnClientConnect <- true
		}
	}
}

func (r *Replica) replicaListener(rid int, reader *bufio.Reader) {
	var msgType uint8
	var err error = nil
	var gbeacon genericsmrproto.Beacon
	var gbeaconReply genericsmrproto.BeaconReply

	for err == nil && !r.Shutdown {

		if msgType, err = reader.ReadByte(); err != nil {
			break
		}

		switch uint8(msgType) {

		case clientproto.GEN_GENERIC_SMR_BEACON:
			if err = gbeacon.Unmarshal(reader); err != nil {
				break
			}
			beacon := &Beacon{int32(rid), gbeacon.Timestamp}
			r.BeaconChan <- beacon
			break

		case clientproto.GEN_GENERIC_SMR_BEACON_REPLY:
			if err = gbeaconReply.Unmarshal(reader); err != nil {
				break
			}
			//TODO: UPDATE STUFF
			diff := float64(uint64(time.Now().UnixNano())-gbeaconReply.Timestamp)
			if r.Ewma[rid] == 0 {
				r.Ewma[rid] = diff
			} else {
				r.Ewma[rid] = 0.99*r.Ewma[rid] + 0.01*diff
			}
			log.Println(r.Ewma)
			break

		default:
			if rpair, present := r.rpcTable[msgType]; present {
				obj := rpair.Obj.New()
				if err = obj.Unmarshal(reader); err != nil {
					break
				}
				dlog.Printf("[%d] Done unmarshaling message with op %d from replica %d.\n", r.Id, msgType, rid)
				rpair.Chan <- obj
				r.Stats.Max(fmt.Sprintf("server_rpc_%d_chan_length", msgType), len(rpair.Chan))
			} else {
				log.Printf("Error: received unknown message type %d.\n", msgType)
			}
		}
	}
}

func (r *Replica) NeedsWaitForExecute(cmd *state.Command) bool {
	return r.Dreply && !cmd.CanReplyWithoutExecute()
}

func (r *Replica) clientListener(conn net.Conn) {
	var err error
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	var idBytes [4]byte
	idBytesS := idBytes[:4]
	n, err := io.ReadFull(reader, idBytesS)
	if err != nil || n != 4 {
		log.Printf("Error reading connecting client id: %v, %d.\n", err, n)
		return
	}
	clientId := int32(binary.LittleEndian.Uint32(idBytesS))

	r.clientIdMapsLock.Lock()
	r.clientReaders[clientId] = reader
	r.clientWriters[clientId] = writer
	r.clientIdMapsLock.Unlock()

	var msgType byte //:= make([]byte, 1)
	var errS string
	for !r.Shutdown && err == nil {
		//dlog.Printf("[%d] Waiting for message from client...\n", r.Id)
		if msgType, err = reader.ReadByte(); err != nil {
			errS = "reading opcode"
			break
		}

		//dlog.Printf("[%d] Read opcode %d from client %s.\n", r.Id, msgType, conn.RemoteAddr().String())

		switch uint8(msgType) {

		case clientproto.GEN_PROPOSE:
			prop := new(genericsmrproto.Propose)
			if err = prop.Unmarshal(reader); err != nil {
				errS = "reading GEN_PROPOSE"
				break
			}
			r.ProposeChan <- &Propose{prop, writer}
			break

		case clientproto.GEN_READ:
			read := new(genericsmrproto.Read)
			if err = read.Unmarshal(reader); err != nil {
				errS = "reading GEN_READ"
				break
			}
			//r.ReadChan <- read
			break

		case clientproto.GEN_PROPOSE_AND_READ:
			pr := new(genericsmrproto.ProposeAndRead)
			if err = pr.Unmarshal(reader); err != nil {
				errS = "reading GEN_PROPOSE_AND_READ"
				break
			}
			//r.ProposeAndReadChan <- pr
			break
		default:
			if rpair, present := r.clientRpcTable[msgType]; present {
				obj := rpair.Obj.New()
				if err = obj.Unmarshal(reader); err != nil {
					errS = "unmarshaling message"
					break
				}
				dlog.Printf("[%d] Done unmarshaling message with op %d from client %s.\n", r.Id,
						msgType, conn.RemoteAddr().String())
				rpair.Chan <- &ClientRPC{obj, writer}
				r.Stats.Max(fmt.Sprintf("client_rpc_%d_chan_length", msgType), len(rpair.Chan))
			} else {
				log.Printf("Error: received unknown message type: %d\n", msgType)
			}
		}
	}
	if err != nil && err != io.EOF {
		log.Printf("Error %s from client: %v\n", errS, err) 
	}
}

func (r *Replica) RegisterRPC(msgObj fastrpc.Serializable, notify chan fastrpc.Serializable) uint8 {
	code := r.rpcCode
	r.rpcCode++
	r.rpcTable[code] = &RPCPair{msgObj, notify}
	return code
}

func (r *Replica) RegisterClientRPC(msgObj fastrpc.Serializable, opCode uint8, notify chan *ClientRPC) {
	r.clientRpcTable[opCode] = &ClientRPCPair{msgObj, notify}
}

func (r *Replica) SendMsg(peerId int32, code uint8, msg fastrpc.Serializable) {
	if r.ShouldDelayNextRPC(int(peerId), code) {
		r.delayedRPC[peerId][code] <- msg
	} else {
		dlog.Printf("[%d] Sending message with op %d to replica %d.\n", r.Id, code, peerId)
		w := r.PeerWriters[peerId]
		w.WriteByte(code)
		msg.Marshal(w)
		w.Flush()
	}
}

func (r *Replica) SendMsgNoFlush(peerId int32, code uint8, msg fastrpc.Serializable) {
	w := r.PeerWriters[peerId]
	w.WriteByte(code)
	msg.Marshal(w)
}

func (r *Replica) ReplyPropose(reply *genericsmrproto.ProposeReply, w *bufio.Writer) {
	//r.clientMutex.Lock()
	//defer r.clientMutex.Unlock()
	w.WriteByte(clientproto.GEN_PROPOSE_REPLY)
	reply.Marshal(w)
	w.Flush()
}

func (r *Replica) ReplyProposeTS(reply *genericsmrproto.ProposeReplyTS, w *bufio.Writer) {
	//r.clientMutex.Lock()
	//defer r.clientMutex.Unlock()
	w.WriteByte(clientproto.GEN_PROPOSE_REPLY)
	reply.Marshal(w)
	w.Flush()
}

func (r *Replica) SendBeacon(peerId int32) {
	w := r.PeerWriters[peerId]
	w.WriteByte(clientproto.GEN_GENERIC_SMR_BEACON)
	beacon := &genericsmrproto.Beacon{uint64(time.Now().UnixNano())}
	beacon.Marshal(w)
	w.Flush()
}

func (r *Replica) ReplyBeacon(beacon *Beacon) {
	w := r.PeerWriters[beacon.Rid]
	w.WriteByte(clientproto.GEN_GENERIC_SMR_BEACON_REPLY)
	rb := &genericsmrproto.BeaconReply{beacon.Timestamp}
	rb.Marshal(w)
	w.Flush()
}

// updates the preferred order in which to communicate with peers according to a preferred quorum
func (r *Replica) UpdatePreferredPeerOrder(quorum []int32) {
	aux := make([]int32, r.N)
	i := 0
	for _, p := range quorum {
		if p == r.Id {
			continue
		}
		aux[i] = p
		i++
	}

	for _, p := range r.PreferredPeerOrder {
		found := false
		for j := 0; j < i; j++ {
			if aux[j] == p {
				found = true
				break
			}
		}
		if !found {
			aux[i] = p
			i++
		}
	}

	r.PreferredPeerOrder = aux
}

func (r *Replica) Finish() {
	if len(r.statsFile) > 0 {
		r.Stats.Export(r.statsFile)
	}
}
