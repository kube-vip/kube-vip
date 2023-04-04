//nolint:govet
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/kube-vip/kube-vip/pkg/k8s"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	watchtools "k8s.io/client-go/tools/watch"
)

// Methodology

// 1. Create a deployment
// 2. Expose the deployment

func main() {
	log.Info("🔬 beginning e2e tests")
	err := createKind()
	if err != nil {
		log.Fatal(err)
	}

	//return

	_, ignoreSimple := os.LookupEnv("IGNORE_SIMPLE")
	_, ignoreDeployments := os.LookupEnv("IGNORE_DEPLOY")
	_, ignoreLeaderFailover := os.LookupEnv("IGNORE_LEADER")
	_, ignoreLeaderActive := os.LookupEnv("IGNORE_ACTIVE")
	_, ignoreLocalDeploy := os.LookupEnv("IGNORE_LOCALDEPLOY")
	_, ignoreEgress := os.LookupEnv("IGNORE_EGRESS")
	_, retainCluster := os.LookupEnv("RETAIN_CLUSTER")

	if !retainCluster {
		defer func() {
			err := deleteKind()
			if err != nil {
				log.Fatal(err)
			}
		}()
	}

	nodeTolerate := os.Getenv("NODE_TOLERATE")

	d := "kube-vip-deploy"
	s := "kube-vip-service"
	l := "kube-vip-deploy-leader"

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	clientset, err := k8s.NewClientset(homeConfigPath, false, "")
	if err != nil {
		log.Fatalf("could not create k8s clientset from external file: %q: %v", homeConfigPath, err)
	}
	log.Debugf("Using external Kubernetes configuration from file [%s]", homeConfigPath)

	deploy := deployment{}
	err = deploy.createKVDs(ctx, clientset)
	if err != nil {
		log.Fatal(err)
	}

	if !ignoreSimple {
		// Simple Deployment test
		log.Infof("🧪 ---> simple deployment <---")
		deploy := deployment{
			name:         d,
			nodeAffinity: nodeTolerate,
			replicas:     2,
			server:       true,
		}
		err = deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		svc := service{
			name: s,
		}
		_, _, err = svc.createService(ctx, clientset)
		if err != nil {
			log.Error(err)
		}

		log.Warnf("🧹 deleting Service [%s]", s)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, s, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}

		log.Warnf("🧹 deleting Deployment [%s]", d)
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, d, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}
	if !ignoreDeployments {
		// Multiple deployment tests
		log.Infof("🧪 ---> multiple deployments <---")
		deploy := deployment{
			name:         l,
			nodeAffinity: nodeTolerate,
			replicas:     2,
			server:       true,
		}
		err = deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		if err != nil {
			log.Fatal(err)
		}
		for i := 1; i < 5; i++ {
			svc := service{
				name: fmt.Sprintf("%s-%d", s, i),
			}
			_, _, err = svc.createService(ctx, clientset)
			if err != nil {
				log.Fatal(err)
			}
		}
		for i := 1; i < 5; i++ {
			log.Warnf("🧹 deleting service [%s]", fmt.Sprintf("%s-%d", s, i))
			err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, fmt.Sprintf("%s-%d", s, i), metav1.DeleteOptions{})
			if err != nil {
				log.Fatal(err)
			}
		}
		log.Warnf("🧹 deleting deployment [%s]", d)
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, l, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}
	if !ignoreLeaderFailover {
		// Failover tests
		log.Infof("🧪 ---> leader failover deployment (local policy) <---")

		deploy := deployment{
			name:         d,
			nodeAffinity: nodeTolerate,
			replicas:     2,
			server:       true,
		}
		err = deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		svc := service{
			name:        s,
			egress:      false,
			policyLocal: true,
		}
		leader, _, err := svc.createService(ctx, clientset)
		if err != nil {
			log.Error(err)
		}

		err = leaderFailover(ctx, &s, &leader, clientset)
		if err != nil {
			log.Error(err)
		}

		log.Warnf("🧹 deleting Service [%s]", s)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, s, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}

		log.Warnf("🧹 deleting Deployment [%s]", d)
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, d, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}

	if !ignoreLeaderActive {
		// pod Failover tests
		log.Infof("🧪 ---> active pod failover deployment (local policy) <---")
		deploy := deployment{
			name:         d,
			nodeAffinity: nodeTolerate,
			replicas:     1,
			server:       true,
		}
		err = deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		svc := service{
			name:        s,
			policyLocal: true,
		}
		leader, _, err := svc.createService(ctx, clientset)
		if err != nil {
			log.Error(err)
		}

		err = podFailover(ctx, &s, &leader, clientset)
		if err != nil {
			log.Error(err)
		}

		log.Warnf("🧹 deleting Service [%s]", s)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, s, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}

		log.Warnf("🧹 deleting Deployment [%s]", d)
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, d, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}
	if !ignoreLocalDeploy {
		// Multiple deployment tests
		log.Infof("🧪 ---> multiple deployments (local policy) <---")
		deploy := deployment{
			name:         l,
			nodeAffinity: nodeTolerate,
			replicas:     2,
			server:       true,
		}
		err = deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		for i := 1; i < 5; i++ {
			svc := service{
				policyLocal: true,
				name:        fmt.Sprintf("%s-%d", s, i),
			}
			_, _, err = svc.createService(ctx, clientset)
			if err != nil {
				log.Fatal(err)
			}
		}
		for i := 1; i < 5; i++ {
			log.Warnf("🧹 deleting service [%s]", fmt.Sprintf("%s-%d", s, i))
			err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, fmt.Sprintf("%s-%d", s, i), metav1.DeleteOptions{})
			if err != nil {
				log.Fatal(err)
			}
		}
		log.Warnf("🧹 deleting deployment [%s]", d)
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, l, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}

	if !ignoreEgress {
		// pod Failover tests
		log.Infof("🧪 ---> egress IP re-write (local policy) <---")
		var egress string
		var found bool
		go func() {
			found = tcpServer(&egress)
		}()

		deploy := deployment{
			name:         d,
			nodeAffinity: nodeTolerate,
			replicas:     1,
			client:       true,
		}
		deploy.address = GetLocalIP()
		if deploy.address == "" {
			log.Fatalf("Unable to detect local IP address")
		} else {
			log.Infof("📠 found local address [%s]", deploy.address)
		}
		err = deploy.createDeployment(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}
		svc := service{
			policyLocal: true,
			name:        s,
			egress:      true,
		}

		ldr, egress, err := svc.createService(ctx, clientset)
		if err != nil {
			log.Fatal(err)
		}

		for i := 1; i < 5; i++ {
			if found {
				log.Infof("🕵️  egress has correct IP address")
				break
			}
			time.Sleep(time.Second * 1)
		}

		if !found {
			log.Errorf("%s %s", ldr, egress)
		}
		log.Warnf("🧹 deleting Service [%s]", s)
		err = clientset.CoreV1().Services(v1.NamespaceDefault).Delete(ctx, s, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}

		log.Warnf("🧹 deleting deployment [%s]", d)
		err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, d, metav1.DeleteOptions{})
		if err != nil {
			log.Fatal(err)
		}
	}
}

func httpTest(address string) error {
	Client := http.Client{
		Timeout: 1 * time.Second,
	}
	var err error
	for i := 0; i < 5; i++ {
		//nolint
		r, err := Client.Get(fmt.Sprintf("http://%s", address)) //nolint

		if err == nil {
			log.Infof("🕸️  successfully retrieved web data in [%ds]", i)
			r.Body.Close()

			return nil
		}
		time.Sleep(time.Second)
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

func tcpServer(egressAddress *string) bool {
	listen, err := net.Listen("tcp", ":12345") //nolint
	if err != nil {
		log.Error(err)
	}
	// close listener
	go func() {
		time.Sleep(time.Second * 10)
		listen.Close()
	}()
	for {
		conn, err := listen.Accept()
		if err != nil {
			return false
			//log.Fatal(err)
		}
		remoteAddress, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		log.Infof("📞 incoming from [%s]", remoteAddress)
		if remoteAddress == *egressAddress {
			return true
		}

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

func GetLocalIP() string {
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
