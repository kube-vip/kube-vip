package cluster

import (
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/raft"
	log "github.com/sirupsen/logrus"
	"github.com/thebsdbox/kube-vip/pkg/kubevip"
	"github.com/thebsdbox/kube-vip/pkg/loadbalancer"
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
func InitCluster(c *kubevip.Config) (*Cluster, error) {

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

func startNetworking(c *kubevip.Config) (*vip.Network, error) {
	network, err := vip.NewConfig(c.VIP, c.Interface)
	if err != nil {
		// log.WithFields(log.Fields{"error": err}).Error("Network failure")

		// os.Exit(-1)
		return nil, err
	}
	return &network, nil
}

// StartCluster - Begins a running instance of the Raft cluster
func (cluster *Cluster) StartCluster(c *kubevip.Config) error {

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

	// Start all NON-bindToVip Load Balancers

	// Iterate through all Configurations
	for x := range c.LoadBalancers {
		// If the load balancer doesn't bind to the VIP
		if c.LoadBalancers[x].BindToVip == false {
			if c.LoadBalancers[x].Type == "tcp" {
				// Bind to 0.0.0.0
				loadbalancer.StartTCP(&c.LoadBalancers[x], ":1")
			} else if c.LoadBalancers[x].Type == "http" {
				// Bind to 0.0.0.0
				loadbalancer.StartHTTP(&c.LoadBalancers[x], ":1")

			} else {
				// If the type isn't one of above then we don't understand it
				log.Warnf("Load Balancer [%s] uses unknown type [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type)
			}
		}
	}

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

					// Once we have the VIP running, start the load balancer(s) that bind to the VIP

					// Iterate through all Configurations
					for x := range c.LoadBalancers {
						// If the load balancer doesn't bind to the VIP
						if c.LoadBalancers[x].BindToVip == true {
							// DO IT
							if c.LoadBalancers[x].Type == "tcp" {
								loadbalancer.StartTCP(&c.LoadBalancers[x], c.VIP)
							} else if c.LoadBalancers[x].Type == "http" {
								loadbalancer.StartHTTP(&c.LoadBalancers[x], c.VIP)
							} else {
								// If the type isn't one of above then we don't understand it
								log.Warnf("Load Balancer [%s] uses unknown type [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type)
							}
						}
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
