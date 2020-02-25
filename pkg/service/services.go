package service

import (
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/thebsdbox/kube-vip/pkg/cluster"
	"github.com/thebsdbox/kube-vip/pkg/kubevip"
)

func (sm *Manager) stopService(uid string) error {
	found := false
	for x := range sm.serviceInstances {
		if sm.serviceInstances[x].service.UID == uid {
			found = true
			sm.serviceInstances[x].cluster.Stop()
		}
	}
	if found == false {
		return fmt.Errorf("Unable to find/stop service [%s]", uid)
	}
	return nil
}

func (sm *Manager) deleteService(uid string) error {
	var updatedInstances []serviceInstance
	found := false
	for x := range sm.serviceInstances {
		// Add the running services to the new array
		if sm.serviceInstances[x].service.UID != uid {
			updatedInstances = append(updatedInstances, sm.serviceInstances[x])
		} else {
			// Flip the found when we match
			found = true
		}
	}
	if found == false {
		return fmt.Errorf("Unable to find/stop service [%s]", uid)
	}

	// Update the service array
	sm.serviceInstances = updatedInstances

	log.Debugf("Removed [%s] from manager, [%d] services remain", uid, len(sm.serviceInstances))

	return nil
}

func (sm *Manager) syncServices(s *plndrServices) error {
	log.Debugf("Service Sync starting")
	// Iterate through the synchronising services
	for x := range s.Services {
		foundInstance := false
		for y := range sm.serviceInstances {
			if s.Services[x].UID == sm.serviceInstances[y].service.UID {
				// We have found this instance in the manager and we can update it
				foundInstance = true
			}
		}
		// This instance wasn't found, we need to add it to the manager
		if foundInstance == false {
			log.Infof("New Service being added [%s] / [%s]", s.Services[x].ServiceName, s.Services[x].UID)
			// Generate Load Balancer configu
			newLB := kubevip.LoadBalancer{
				Name:      fmt.Sprintf("kube-vip generated for [%s]", s.Services[x].ServiceName),
				Port:      s.Services[x].Port,
				Type:      "tcp",
				BindToVip: true,
			}
			// Generate new Virtual IP configuration
			newVip := kubevip.Config{
				VIP:           s.Services[x].Vip,
				Interface:     Interface,
				SingleNode:    true,
				GratuitousARP: EnableArp,
			}
			// Add Load Balancer Configuration
			newVip.LoadBalancers = append(newVip.LoadBalancers, newLB)
			// Create new Virtual IP service for Manager
			newService := &serviceInstance{
				vipConfig: newVip,
				service:   s.Services[x],
			}

			// TODO - start VIP
			c, err := cluster.InitCluster(&newService.vipConfig, false)
			if err != nil {
				log.Errorf("Failed to add Service [%s] / [%s]", s.Services[x].ServiceName, s.Services[x].UID)
				return err
			}
			err = c.StartSingleNode(&newService.vipConfig, false)
			if err != nil {
				log.Errorf("Failed to add Service [%s] / [%s]", s.Services[x].ServiceName, s.Services[x].UID)
				return err
			}
			newService.cluster = *c
			// Begin watching this service
			go sm.newWatcher(newService)
			// Add new service to manager configuration
			sm.serviceInstances = append(sm.serviceInstances, *newService)
		}
	}

	return nil
}
