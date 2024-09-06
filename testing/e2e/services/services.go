//nolint:govet
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	watchtools "k8s.io/client-go/tools/watch"
)

// Methodology

// 1. Create a deployment
// 2. Expose the deployment
func (config *testConfig) startServiceTest(ctx context.Context, clientset *kubernetes.Clientset) {
	nodeTolerate := os.Getenv("NODE_TOLERATE")

	d := "kube-vip-deploy"
	s := "kube-vip-service"
	l := "kube-vip-deploy-leader"

	if !config.ignoreSimple {
		// Simple Deployment test
		log.Infof("🧪 ---> simple deployment <---")
		deploy := deployment{
			name:         d,
			nodeAffinity: nodeTolerate,
			replicas:     2,
			server:       true,
		}
		err := deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		svc := service{
			name:     s,
			testHTTP: true,
			timeout:  10,
		}
		_, _, err = svc.createService(ctx, clientset)
		if err != nil {
			log.Error(err)
		} else {
			config.successCounter++
		}

		log.Infof("🧹 deleting Service [%s], deployment [%s]", s, d)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, s, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, d, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}
	if !config.ignoreDeployments {
		// Multiple deployment tests
		log.Infof("🧪 ---> multiple deployments <---")
		deploy := deployment{
			name:         l,
			nodeAffinity: nodeTolerate,
			replicas:     2,
			server:       true,
		}
		err := deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		if err != nil {
			log.Fatal(err)
		}
		for i := 1; i < 5; i++ {
			svc := service{
				name:     fmt.Sprintf("%s-%d", s, i),
				testHTTP: true,
				timeout:  30,
			}
			_, _, err = svc.createService(ctx, clientset)
			if err != nil {
				log.Fatal(err)
			}
			config.successCounter++
		}
		for i := 1; i < 5; i++ {
			log.Infof("🧹 deleting service [%s]", fmt.Sprintf("%s-%d", s, i))
			err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, fmt.Sprintf("%s-%d", s, i), metav1.DeleteOptions{})
			if err != nil {
				log.Fatal(err)
			}
		}
		log.Infof("🧹 deleting deployment [%s]", d)
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, l, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}
	if !config.ignoreLeaderFailover {
		// Failover tests
		log.Infof("🧪 ---> leader failover deployment (local policy) <---")

		deploy := deployment{
			name:         d,
			nodeAffinity: nodeTolerate,
			replicas:     2,
			server:       true,
		}
		err := deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		svc := service{
			name:        s,
			egress:      false,
			policyLocal: true,
			testHTTP:    true,
			timeout:     180,
		}
		leader, lbAddresses, err := svc.createService(ctx, clientset)
		if err != nil {
			log.Error(err)
		}
		lbAddress := lbAddresses[0]

		err = leaderFailover(ctx, &s, &leader, clientset)
		if err != nil {
			log.Error(err)
		} else {
			config.successCounter++
		}

		// Get all addresses on all nodes
		nodes, err := getAddressesOnNodes()
		if err != nil {
			log.Error(err)
		}
		// Make sure we don't exist in two places
		err = checkNodesForDuplicateAddresses(nodes, lbAddress)
		if err != nil {
			log.Fatal(err)
		}

		log.Infof("🧹 deleting Service [%s], deployment [%s]", s, d)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, s, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}

		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, d, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}

	if !config.ignoreLeaderActive {
		// pod Failover tests
		log.Infof("🧪 ---> active pod failover deployment (local policy) <---")
		deploy := deployment{
			name:         d,
			nodeAffinity: nodeTolerate,
			replicas:     1,
			server:       true,
		}
		err := deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		svc := service{
			name:        s,
			policyLocal: true,
			testHTTP:    true,
			timeout:     30,
		}
		leader, _, err := svc.createService(ctx, clientset)
		if err != nil {
			log.Error(err)
		}

		err = podFailover(ctx, &s, &leader, clientset)
		if err != nil {
			log.Error(err)
		} else {
			config.successCounter++
		}

		log.Infof("🧹 deleting Service [%s], deployment [%s]", s, d)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, s, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, d, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}
	if !config.ignoreLocalDeploy {
		// Multiple deployment tests
		log.Infof("🧪 ---> multiple deployments (local policy) <---")
		timeout := 30
		deploy := deployment{
			name:         l,
			nodeAffinity: nodeTolerate,
			replicas:     2,
			server:       true,
		}
		err := deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		for i := 1; i < 5; i++ {
			svc := service{
				policyLocal: true,
				name:        fmt.Sprintf("%s-%d", s, i),
				testHTTP:    true,
				timeout:     timeout,
			}
			_, lbAddresses, err := svc.createService(ctx, clientset)
			if err != nil {
				log.Fatal(err)
			}
			lbAddress := lbAddresses[0]

			config.successCounter++
			nodes, err := getAddressesOnNodes()
			if err != nil {
				log.Error(err)
			}
			err = checkNodesForDuplicateAddresses(nodes, lbAddress)
			if err != nil {
				log.Fatal(err)
			}
		}
		for i := 1; i < 5; i++ {
			log.Infof("🧹 deleting service [%s]", fmt.Sprintf("%s-%d", s, i))
			err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, fmt.Sprintf("%s-%d", s, i), metav1.DeleteOptions{})
			if err != nil {
				log.Fatal(err)
			}
		}
		log.Infof("🧹 deleting deployment [%s]", d)
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, l, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}

	if !config.ignoreEgress {
		// pod Failover tests
		log.Infof("🧪 ---> egress IP re-write (local policy) <---")
		var egress string
		var found bool
		timeout := 30
		// Set up a local listener
		go func() {
			found = tcpServer(false, &egress, timeout)
		}()

		deploy := deployment{
			name:         d,
			nodeAffinity: nodeTolerate,
			replicas:     1,
			client:       true,
		}

		// Find this machines IP address
		deploy.address = GetLocalIPv4()
		if deploy.address == "" {
			log.Fatalf("Unable to detect local IP address")
		}
		log.Infof("📠 found local address [%s]", deploy.address)
		// Create a deployment that connects back to this machines IP address
		err := deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}

		svc := service{
			policyLocal: true,
			name:        s,
			egress:      true,
			testHTTP:    false,
			timeout:     30,
		}

		_, lbAddresses, err := svc.createService(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		egress = lbAddresses[0]

		for i := 1; i < 10; i++ {
			if found {
				log.Infof("🕵️  egress has correct IP address")
				config.successCounter++
				break
			}
			time.Sleep(time.Second * 1)
		}

		if !found {
			log.Error("😱 No traffic found from loadbalancer address ")
		}
		log.Infof("🧹 deleting Service [%s], deployment [%s]", s, d)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, s, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, d, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}

	if !config.ignoreEgressIPv6 {
		// pod Failover tests
		log.Infof("🧪 ---> egress IP re-write IPv6 (local policy) <---")
		var egress string
		var found bool
		timeout := 60
		// Set up a local listener
		go func() {
			found = tcpServer(true, &egress, timeout)
		}()

		deploy := deployment{
			name:         d,
			nodeAffinity: nodeTolerate,
			replicas:     1,
			client:       true,
		}

		// Find this machines IP address
		deploy.address = GetLocalIPv6()
		if deploy.address == "" {
			log.Fatalf("Unable to detect local IP address")
		}
		log.Infof("📠 found local address [%s]", deploy.address)
		// Create a deployment that connects back to this machines IP address
		err := deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}

		svc := service{
			policyLocal:   true,
			name:          s,
			egress:        true,
			egressIPv6:    true,
			testHTTP:      false,
			timeout:       timeout,
			testDualstack: true,
		}

		_, lbAddresses, err := svc.createService(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		for x := range lbAddresses {
			ip := net.ParseIP(lbAddresses[x])
			// if ip == nil {
			// 	return errors.New("invalid address")
			// }
			if ip.To4() == nil {
				// use brackets for IPv6 address
				egress = lbAddresses[x]
				break
			}
		}

		for i := 1; i < timeout; i++ {
			if found {
				log.Infof("🕵️  egress has correct IP address")
				config.successCounter++
				break
			}
			time.Sleep(time.Second * 1)
		}

		if !found {
			log.Error("😱 No traffic found from loadbalancer address ")
		}
		log.Infof("🧹 deleting Service [%s], deployment [%s]", s, d)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, s, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, d, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}

	if !config.ignoreDualStack {
		// Dualstack loadbalancer test
		log.Infof("🧪 ---> testing dualstack loadbalancer service <---")
		deploy := deployment{
			name:         d,
			nodeAffinity: nodeTolerate,
			replicas:     2,
			server:       true,
		}
		err := deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		svc := service{
			name:          s,
			testHTTP:      true,
			testDualstack: true,
			timeout:       30,
		}
		_, _, err = svc.createService(ctx, clientset)
		if err != nil {
			log.Error(err)
		} else {
			config.successCounter++
		}

		log.Infof("🧹 deleting Service [%s], deployment [%s]", s, d)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, s, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, d, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}

	}

	log.Infof("🏆 Testing Complete [%d] passed", config.successCounter)
}

func httpTest(address string) error {
	log.Infof("🕷️  testing HTTP request against [%s]", address)
	Client := http.Client{
		Timeout: 2 * time.Second,
	}
	ip := net.ParseIP(address)
	if ip == nil {
		return errors.New("invalid address")
	}
	if ip.To4() == nil {
		// use brackets for IPv6 address
		address = fmt.Sprintf("[%s]", address)
	}
	var err error
	for i := 0; i < 5; i++ {
		var r *http.Response
		//nolint
		r, err = Client.Get(fmt.Sprintf("http://%s", address)) //nolint

		if err == nil {
			log.Infof("🕸️  successfully retrieved web data in [%ds]", i)
			r.Body.Close()

			return nil
		}
		time.Sleep(time.Second * 2)
	}
	return err
}

func leaderFailover(ctx context.Context, name, leaderNode *string, clientset *kubernetes.Clientset) error {
	go func() {
		log.Infof("💀 killing leader five times")
		for i := 0; i < 5; i++ {
			p, err := clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{})
			if err != nil {
				log.Fatal(err)
			}

			for x := range p.Items {
				if p.Items[x].Spec.NodeName == *leaderNode {
					if p.Items[x].Spec.Containers[0].Name == "kube-vip" {
						err = clientset.CoreV1().Pods("kube-system").Delete(ctx, p.Items[x].Name, metav1.DeleteOptions{})
						if err != nil {
							log.Fatal(err)
						}
						log.Infof("🔪 leader pod [%s] has been deleted", p.Items[x].Name)
					}
				}
			}
			time.Sleep(time.Second * 5)
		}
	}()

	log.Infof("👀 service [%s] for updates", *name)

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return clientset.CoreV1().Services(v1.NamespaceDefault).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		return err
	}
	ch := rw.ResultChan()

	go func() {
		time.Sleep(time.Second * 30)
		rw.Stop()
	}()

	// Used for tracking an active endpoint / pod
	for event := range ch {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added:
			// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				log.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					log.Infof("🔎 found load balancer address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations["kube-vip.io/vipHost"])
				}
			}
		case watch.Modified:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				log.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					log.Infof("🔍 updated with address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations["kube-vip.io/vipHost"])
					err = httpTest(svc.Status.LoadBalancer.Ingress[0].IP)
					if err != nil {
						return err
					}
					*leaderNode = svc.Annotations["kube-vip.io/vipHost"]
				}
			}
		default:

		}
	}
	return nil
}

func podFailover(ctx context.Context, name, leaderNode *string, clientset *kubernetes.Clientset) error {
	go func() {
		log.Infof("💀 killing active pod five times")
		for i := 0; i < 5; i++ {
			p, err := clientset.CoreV1().Pods(v1.NamespaceDefault).List(ctx, metav1.ListOptions{})
			if err != nil {
				log.Fatal(err)
			}
			found := false
			for x := range p.Items {
				if p.Items[x].Spec.NodeName == *leaderNode {
					if p.Items[x].Spec.Containers[0].Name == "kube-vip-web" {
						found = true
						err = clientset.CoreV1().Pods(v1.NamespaceDefault).Delete(ctx, p.Items[x].Name, metav1.DeleteOptions{})
						if err != nil {
							log.Fatal(err)
						}
						log.Infof("🔪 active pod [%s] on [%s] has been deleted", p.Items[x].Name, p.Items[x].Spec.NodeName)
					}
				}
			}
			if !found {
				log.Warnf("😱 No Pod found on [%s]", *leaderNode)
			}
			time.Sleep(time.Second * 5)
		}
	}()

	log.Infof("👀 service [%s] for updates", *name)

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return clientset.CoreV1().Services(v1.NamespaceDefault).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		return err
	}
	ch := rw.ResultChan()

	go func() {
		time.Sleep(time.Second * 30)
		rw.Stop()
	}()

	// Used for tracking an active endpoint / pod
	for event := range ch {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added:
			// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				log.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					log.Infof("🔎 found load balancer address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations["kube-vip.io/vipHost"])
				}
			}
		case watch.Modified:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				log.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					log.Infof("🔍 updated with address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations["kube-vip.io/vipHost"])
					err = httpTest(svc.Status.LoadBalancer.Ingress[0].IP)
					if err != nil {
						log.Fatal(err)
					}
					*leaderNode = svc.Annotations["kube-vip.io/vipHost"]
				}
			}
		default:

		}
	}
	return nil
}

func tcpServer(IPv6 bool, egressAddress *string, timeout int) bool {
	var listen net.Listener
	var err error
	if !IPv6 {
		listen, err = net.Listen("tcp", ":12345") //nolint
	} else {
		listen, err = net.Listen("tcp6", ":12346") //nolint

	}
	if err != nil {
		log.Error(err)
	}
	// close listener
	go func() {
		time.Sleep(time.Second * time.Duration(timeout))
		listen.Close()
	}()
	for {
		conn, err := listen.Accept()
		if err != nil {
			return false
			// log.Fatal(err)
		}
		remoteAddress, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		// If it is IPv6 expand the address
		// if IPv6 {
		// 	addr, _ := netip.ParseAddr(remoteAddress)
		// 	remoteAddress = addr.StringExpanded()
		// }

		if remoteAddress == *egressAddress {
			log.Infof("📞 👍 incoming from egress Address [%s]", remoteAddress)
			return true
		}
		log.Infof("📞 👎 incoming from pod address [%s]", remoteAddress)
		go handleRequest(conn)
	}
}

func handleRequest(conn net.Conn) {
	// incoming request
	buffer := make([]byte, 1024)
	_, err := conn.Read(buffer)
	if err != nil {
		log.Error(err)
	}
	// write data to response
	time := time.Now().Format(time.ANSIC)
	responseStr := fmt.Sprintf("Your message is: %v. Received time: %v", string(buffer[:]), time)
	_, err = conn.Write([]byte(responseStr))
	if err != nil {
		log.Error(err)
	}
	// close conn
	conn.Close()
}

func GetLocalIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, address := range addrs {
		// check the address type and if it is not a loopback the display it
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

func GetLocalIPv6() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, address := range addrs {
		// check the address type and if it is not a loopback the display it
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() == nil && !ipnet.IP.IsLinkLocalUnicast() {
			if ipnet.IP.To16() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}
