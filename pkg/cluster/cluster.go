package cluster

import (
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/raft"
	log "github.com/sirupsen/logrus"
	"github.com/thebsdbox/kube-vip/pkg/vip"
)

const leaderLogcount = 5

// Cluster - The Cluster object manages the state of the cluster for a particular node
type Cluster struct {
	stateMachine FSM
	stop         chan bool
	completed    chan bool
	network      *vip.Network
}

// InitCluster - Will attempt to initialise all of the required settings for the cluster
func InitCluster(c *Config) (*Cluster, error) {

	// TODO - Check for root (needed to netlink)

	// Start the Virtual IP Networking configuration
	network, err := startNetworking(c)
	if err != nil {
		return nil, err
	}

	// Initialise the Cluster structure
	newCluster := &Cluster{
		network: network,
	}

	return newCluster, nil
}

func startNetworking(c *Config) (*vip.Network, error) {
	network, err := vip.NewConfig(c.VIP, c.Interface)
	if err != nil {
		// log.WithFields(log.Fields{"error": err}).Error("Network failure")

		// os.Exit(-1)
		return nil, err
	}
	return &network, nil
}

// Start - Begins a running instance of the Raft cluster
func (cluster *Cluster) Start(c *Config) error {

	// Create local configuration address
	localAddress := fmt.Sprintf("%s:%d", c.LocalPeer.Address, c.LocalPeer.Port)

	// Begin the Raft configuration
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(c.LocalPeer.ID)
	logger := log.StandardLogger().Writer()
	config.LogOutput = logger

	// Initialize communication
	address, err := net.ResolveTCPAddr("tcp", localAddress)
	if err != nil {
		return err
	}

	// Create transport
	transport, err := raft.NewTCPTransport(localAddress, address, 3, 10*time.Second, logger)
	if err != nil {
		return err
	}

	// Create Raft structures
	snapshots := raft.NewInmemSnapshotStore()
	logStore := raft.NewInmemStore()
	stableStore := raft.NewInmemStore()

	// Cluster configuration
	configuration := raft.Configuration{}

	for x := range c.Peers {
		// Build the address from the peer configuration
		peerAddress := fmt.Sprintf("%s:%d", c.Peers[x].Address, c.Peers[x].Port)

		// Set this peer into the raft configuration
		configuration.Servers = append(configuration.Servers, raft.Server{
			ID:      raft.ServerID(c.Peers[x].ID),
			Address: raft.ServerAddress(peerAddress)})
	}

	// Bootstrap cluster
	if err := raft.BootstrapCluster(config, logStore, stableStore, snapshots, transport, configuration); err != nil {
		return err
	}

	// Create RAFT instance
	raftServer, err := raft.NewRaft(config, cluster.stateMachine, logStore, stableStore, snapshots, transport)
	if err != nil {
		return err
	}

	cluster.stop = make(chan bool, 1)
	cluster.completed = make(chan bool, 1)
	ticker := time.NewTicker(time.Second)
	isLeader := false

	// (attempt to) Remove the virtual IP, incase it already exists
	cluster.network.DeleteIP()

	// leader log broadcast - this counter is used to stop flooding STDOUT with leader log entries
	var leaderbroadcast int

	go func() {
		for {
			// Broadcast the current leader on this node if it's the correct time (every leaderLogcount * time.Second)
			if leaderbroadcast == leaderLogcount {
				log.Infof("The Node [%s] is leading", raftServer.Leader())
				// Reset the timer
				leaderbroadcast = 0
			}
			leaderbroadcast++

			select {
			case leader := <-raftServer.LeaderCh():
				if leader {
					isLeader = true

					log.Info("This node is assuming leadership of the cluster")

					err = cluster.network.AddIP()
					if err != nil {
						log.Warnf("%v", err)
					}

					if c.GratuitousARP == true {
						// Gratuitous ARP, will broadcast to new MAC <-> IP
						err = vip.ARPSendGratuitous(c.VIP, c.Interface)
						if err != nil {
							log.Warnf("%v", err)
						}
					}
				} else {
					isLeader = false

					log.Info("This node is becoming a follower within the cluster")

					err = cluster.network.DeleteIP()
					if err != nil {
						log.Warnf("%v", err)
					}
				}

			case <-ticker.C:
				if isLeader {
					result, err := cluster.network.IsSet()
					if err != nil {
						log.WithFields(log.Fields{"error": err, "ip": cluster.network.IP(), "interface": cluster.network.Interface()}).Error("Could not check ip")
					}

					if result == false {
						log.Error("This node is leader and is adopting the virtual IP")

						err = cluster.network.AddIP()
						if err != nil {
							log.Warnf("%v", err)
						}

						if c.GratuitousARP == true {
							// Gratuitous ARP, will broadcast to new MAC <-> IP
							err = vip.ARPSendGratuitous(c.VIP, c.Interface)
							if err != nil {
								log.Warnf("%v", err)
							}
						}
					}
				}

			case <-cluster.stop:
				log.Info("Stopping this node")

				if isLeader {
					log.Info("Releasing the Virtual IP")
					err = cluster.network.DeleteIP()
					if err != nil {
						log.Warnf("%v", err)
					}
				}

				close(cluster.completed)

				return
			}
		}
	}()

	log.Info("Started")

	return nil
}

// Stop - Will stop the Cluster and release VIP if needed
func (cluster *Cluster) Stop() {
	// Close the stop chanel, which will shut down the VIP (if needed)
	close(cluster.stop)

	// Wait until the completed channel is closed, signallign all shutdown tasks completed
	<-cluster.completed

	log.Info("Stopped")
}
