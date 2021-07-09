package cluster

import (
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/raft"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/loadbalancer"
	"github.com/kube-vip/kube-vip/pkg/vip"
	log "github.com/sirupsen/logrus"
)

// StartRaftCluster - Begins a running instance of the Raft cluster
func (cluster *Cluster) StartRaftCluster(c *kubevip.Config) error {

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

	// Add Local Peer
	configuration.Servers = append(configuration.Servers, raft.Server{
		ID:      raft.ServerID(c.LocalPeer.ID),
		Address: raft.ServerAddress(fmt.Sprintf("%s:%d", c.LocalPeer.Address, c.LocalPeer.Port))})

	// If we want to start a node as leader then we will not add any remote peers, this will leave this as a cluster of one
	// The remotePeers will add themselves to the cluster as they're added
	if c.StartAsLeader != true {
		for x := range c.RemotePeers {
			// Make sure that we don't add in this server twice
			if c.LocalPeer.Address != c.RemotePeers[x].Address {

				// Build the address from the peer configuration
				peerAddress := fmt.Sprintf("%s:%d", c.RemotePeers[x].Address, c.RemotePeers[x].Port)

				// Set this peer into the raft configuration
				configuration.Servers = append(configuration.Servers, raft.Server{
					ID:      raft.ServerID(c.RemotePeers[x].ID),
					Address: raft.ServerAddress(peerAddress)})
			}
		}
		log.Info("This node will attempt to start as Follower")
	} else {
		log.Info("This node will attempt to start as Leader")
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
	isLeader := c.StartAsLeader

	// (attempt to) Remove the virtual IP, incase it already exists
	cluster.Network.DeleteIP()

	// leader log broadcast - this counter is used to stop flooding STDOUT with leader log entries
	var leaderbroadcast int
	// Managers for Vip load balancers and none-vip loadbalancers
	nonVipLB := loadbalancer.LBManager{}
	VipLB := loadbalancer.LBManager{}

	// Iterate through all Configurations
	for x := range c.LoadBalancers {
		// If the load balancer doesn't bind to the VIP
		if c.LoadBalancers[x].BindToVip == false {
			err = nonVipLB.Add("", &c.LoadBalancers[x])
			if err != nil {
				log.Warnf("Error creating loadbalancer [%s] type [%s] -> error [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type, err)
			}
		}
	}

	// On a cold start the node will sleep for 5 seconds to ensure that leader elections are complete
	log.Infoln("This instance will wait approximately 5 seconds, from cold start to ensure cluster elections are complete")
	time.Sleep(time.Second * 5)

	go func() {
		for {
			if c.AddPeersAsBackends == true {
				// Get addresses and change backends

				// c.LoadBalancers[0].Backends
				// for x := range raftServer.GetConfiguration().Configuration().Servers {
				// 	raftServer.GetConfiguration().Configuration().Servers[x].Address
				// }

			}
			// Broadcast the current leader on this node if it's the correct time (every leaderLogcount * time.Second)
			if leaderbroadcast == leaderLogcount {
				log.Infof("The Node [%s] is leading", raftServer.Leader())
				// Reset the timer
				leaderbroadcast = 0

				// ensure that if this node is the leader, it is set as the leader
				if localAddress == string(raftServer.Leader()) {
					// Re-broadcast arp to ensure network stays up to date
					if c.EnableARP == true {
						// Gratuitous ARP, will broadcast to new MAC <-> IP
						err = vip.ARPSendGratuitous(cluster.Network.IP(), c.Interface)
						if err != nil {
							log.Warnf("%v", err)
						}
					}
					if !isLeader {
						log.Infoln("This node is leading, but isnt the leader (correcting)")
						isLeader = true
					}
				} else {
					// (attempt to) Remove the virtual IP, incase it already exists to keep nodes clean
					cluster.Network.DeleteIP()
					isLeader = false
				}

			}
			leaderbroadcast++

			select {
			case leader := <-raftServer.LeaderCh():
				log.Infoln("New Election event")
				if leader {
					isLeader = true

					log.Info("This node is assuming leadership of the cluster")
					err = cluster.Network.AddIP()
					if err != nil {
						log.Warnf("%v", err)
					}

					// Once we have the VIP running, start the load balancer(s) that bind to the VIP

					for x := range c.LoadBalancers {

						if c.LoadBalancers[x].BindToVip == true {
							err = VipLB.Add(cluster.Network.IP(), &c.LoadBalancers[x])
							if err != nil {
								log.Warnf("Error creating loadbalancer [%s] type [%s] -> error [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type, err)
								log.Errorf("Dropping Leadership to another node in the cluster")
								raftServer.LeadershipTransfer()

								// Stop all load balancers associated with the VIP
								err = VipLB.StopAll()
								if err != nil {
									log.Warnf("%v", err)
								}

								err = cluster.Network.DeleteIP()
								if err != nil {
									log.Warnf("%v", err)
								}
							}
						}
					}

					if c.EnableARP == true {
						// Gratuitous ARP, will broadcast to new MAC <-> IP
						err = vip.ARPSendGratuitous(cluster.Network.IP(), c.Interface)
						if err != nil {
							log.Warnf("%v", err)
						}
					}
				} else {
					isLeader = false

					log.Info("This node is becoming a follower within the cluster")

					// Stop all load balancers associated with the VIP
					err = VipLB.StopAll()
					if err != nil {
						log.Warnf("%v", err)
					}

					err = cluster.Network.DeleteIP()
					if err != nil {
						log.Warnf("%v", err)
					}
				}

			case <-ticker.C:

				if isLeader {

					result, err := cluster.Network.IsSet()
					if err != nil {
						log.WithFields(log.Fields{"error": err, "ip": cluster.Network.IP(), "interface": cluster.Network.Interface()}).Error("Could not check ip")
					}

					if result == false {
						log.Error("This node is leader and is adopting the virtual IP")

						err = cluster.Network.AddIP()
						if err != nil {
							log.Warnf("%v", err)
						}
						// Once we have the VIP running, start the load balancer(s) that bind to the VIP

						for x := range c.LoadBalancers {

							if c.LoadBalancers[x].BindToVip == true {
								err = VipLB.Add(cluster.Network.IP(), &c.LoadBalancers[x])
								if err != nil {
									log.Warnf("Error creating loadbalancer [%s] type [%s] -> error [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type, err)
								}
							}
						}
						if c.EnableARP == true {
							// Gratuitous ARP, will broadcast to new MAC <-> IP
							err = vip.ARPSendGratuitous(cluster.Network.IP(), c.Interface)
							if err != nil {
								log.Warnf("%v", err)
							}
						}
					}
				}

			case <-cluster.stop:
				log.Info("[RAFT] Stopping this node")
				log.Info("[LOADBALANCER] Stopping load balancers")

				// Stop all load balancers associated with the VIP
				err = VipLB.StopAll()
				if err != nil {
					log.Warnf("%v", err)
				}

				// Stop all load balancers associated with the Host
				err = nonVipLB.StopAll()
				if err != nil {
					log.Warnf("%v", err)
				}

				if isLeader {
					log.Info("[VIP] Releasing the Virtual IP")
					err = cluster.Network.DeleteIP()
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
