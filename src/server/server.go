package main

import (
	"epaxos"
	"flag"
	"fmt"
	"gpaxos"
	"log"
	"mencius"
	"net/rpc"
	"os"
	"os/signal"
	"paxos"
	"runtime"
	"runtime/pprof"
	"abd"
	"gryff"
	"dlog"
	"serverlib"
)

var debug *bool = flag.Bool("debug", false, "Enable debug logging.")
var portnum *int = flag.Int("port", 7070, "Port # to listen on. Defaults to 7070")
var masterAddr *string = flag.String("maddr", "", "Master address. Defaults to localhost.")
var masterPort *int = flag.Int("mport", 7087, "Master port.  Defaults to 7087.")
var myAddr *string = flag.String("addr", "", "Server address (this machine). Defaults to localhost.")
var doGryff *bool = flag.Bool("t", false," Use Gryff as the replication protocol. Defaults to false.")
var doAbd *bool = flag.Bool("a", false, "Use ABD as the replication protocol. Defaults to false.")
var doMencius *bool = flag.Bool("m", false, "Use Mencius as the replication protocol. Defaults to false.")
var doGpaxos *bool = flag.Bool("g", false, "Use Generalized Paxos as the replication protocol. Defaults to false.")
var doEpaxos *bool = flag.Bool("e", false, "Use EPaxos as the replication protocol. Defaults to false.")
var procs *int = flag.Int("p", 2, "GOMAXPROCS. Defaults to 2")
var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")
var blockprofile = flag.String("blockprofile", "", "write block profile to file")
var thrifty = flag.Bool("thrifty", false, "Use only as many messages as strictly required for inter-replica communication.")
var exec = flag.Bool("exec", false, "Execute commands.")
var dreply = flag.Bool("dreply", false, "Reply to client only after command has been executed.")
var beacon = flag.Bool("beacon", false, "Send beacons to other replicas to compare their relative speeds.")
var durable = flag.Bool("durable", false, "Log to a stable store (i.e., a file in the current dir).")
var rpcPort = flag.Int("rpcport", 8070, "Port # for RPC requests. Defaults to 8070")
var proxy = flag.Bool("proxy", false, "Proxy client requests at nearest replica.")
var epaxosMode = flag.Bool("epaxosMode", false, "Run Gryff with same message pattern as EPaxos.")

var statsFile = flag.String("statsFile", "", "Name of file to which stats should be written.")
var memProfile = flag.String("memProfile", "", "Name of file to which a memory profile should be written.")
var noConflicts = flag.Bool("noConflicts", false, "True if all operations target different keys.")
var regular *bool = flag.Bool("regular", false, "Regular consistency.")
var rmwHandler = flag.String("rmwHandler", "epaxos", "Consensus protocol for rmws.")
var shortcircuitTime *int = flag.Int("shortcircuitTime", 100, "Timeout for shortcircuit writes in milliseconds")
var fastOverwriteTime *int = flag.Int("fastOverwriteTime", 100, "Timeout for fast overwrites writes in milliseconds")
var forceWritePeriod *int = flag.Int("forceWritePeriod", 100, "Maximum time between forced writes in milliseconds.")
var broadcastOptimizationEnabled *bool = flag.Bool("broadcastOptimizationEnabled", false, "Toggles the EPaxos broadcast optimization.")

func main() {
	flag.Parse()

	dlog.DLOG = *debug

	runtime.GOMAXPROCS(*procs)

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
	}
	if *blockprofile != "" {
		runtime.SetBlockProfileRate(1)
	}

	log.Printf("Server starting on port %d\n", *portnum)

	replicaId, nodeList := serverlib.RegisterWithMaster(*myAddr, *portnum, fmt.Sprintf("%s:%d", *masterAddr,
			*masterPort))

	log.Printf("Got node list from master: [")
	for i := 0; i < len(nodeList); i++ {
		log.Printf("%s", nodeList[i])
		if i != len(nodeList) - 1 {
			log.Printf(", ")
		}
	}
	log.Printf("]\n")

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt)

	if *doGryff {
		log.Println("Starting Gryff replica...")
		var rmwHandlerType gryff.RMWHandlerType
		switch *rmwHandler {
			case "sdp":
				rmwHandlerType = gryff.SDP
				break
			case "epaxos":
				rmwHandlerType = gryff.EPAXOS
				break
			default:
				log.Fatal("Unknown consensus protocol: ", *rmwHandler)
				break
		}
		rep := gryff.NewReplica(replicaId, nodeList, *thrifty, *exec, *dreply,
				*beacon, *durable, *statsFile, *regular, *proxy, *noConflicts,
				*epaxosMode, rmwHandlerType, *shortcircuitTime, *fastOverwriteTime,
				*forceWritePeriod, *broadcastOptimizationEnabled)
		rpc.Register(rep)
		//go catchKill(rep, interrupt)
	} else if *doAbd {
		log.Println("Starting ABD replica...")
		rep := abd.NewReplica(replicaId, nodeList, *thrifty, *exec, *dreply,
				*durable, *statsFile)
		rpc.Register(rep)
		go catchKill(rep, interrupt)
	} else if *doEpaxos {
		log.Println("Starting Egalitarian Paxos replica...")
		rep := epaxos.NewReplica(replicaId, nodeList, *thrifty, *exec, *dreply,
				*beacon, *durable, *statsFile, *noConflicts)
		rpc.Register(rep)
		go catchKill(rep, interrupt)
	} else if *doMencius {
		log.Println("Starting Mencius replica...")
		rep := mencius.NewReplica(replicaId, nodeList, *thrifty, *exec, *dreply,
				*durable, *statsFile)
		rpc.Register(rep)
		go catchKill(rep, interrupt)
	} else if *doGpaxos {
		log.Println("Starting Generalized Paxos replica...")
		rep := gpaxos.NewReplica(replicaId, nodeList, *thrifty, *exec, *dreply,
				*statsFile)
		rpc.Register(rep)
		go catchKill(rep, interrupt)
	} else {
		log.Println("Starting classic Paxos replica...")
		rep := paxos.NewReplica(replicaId, nodeList, *thrifty, *exec, *dreply,
				*beacon, *durable, *statsFile)
		rpc.Register(rep)
		go catchKill(rep, interrupt)
	}

	serverlib.Serve(*rpcPort)
}

type Finishable interface {
	Finish()
}

func catchKill(f Finishable, interrupt chan os.Signal) {
	sig := <-interrupt
	log.Printf("Caught signal %d.\n", sig)
	if *cpuprofile != "" {
		pprof.StopCPUProfile()
	}
	if *blockprofile != "" {
		f, err := os.Create(*blockprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.Lookup("block").WriteTo(f, 0)
		f.Close()
	}
	if *statsFile != "" {
		log.Printf("Writing stats to file %s.\n", *statsFile)
		f.Finish() 
	}
	if *memProfile != "" {
		f, err := os.Create(*memProfile)
		if err != nil {
			log.Fatal("could not create memory profile: ", err)
		}
		runtime.GC() // get up-to-date statistics
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Fatal("could not write memory profile: ", err)
		}
		f.Close()
	}
	os.Exit(0)
}
