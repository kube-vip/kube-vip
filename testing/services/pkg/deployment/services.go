//nolint:govet
package deployment

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/gookit/slog"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	watchtools "k8s.io/client-go/tools/watch"
)

// Methodology

// 1. Create a deployment
// 2. Expose the deployment
func (config *TestConfig) StartServiceTest(ctx context.Context, clientset *kubernetes.Clientset) {
	var err error
	if config.Simple {
		err = config.SimpleDeployment(ctx, clientset)
		if err != nil {
			slog.Error(err)
		}
	}

	if config.Deployments {
		err = config.MultipleDeployments(ctx, clientset)
		if err != nil {
			slog.Error(err)
		}
	}

	if config.LeaderFailover {
		// Failover tests
		err = config.Failover(ctx, clientset)
		if err != nil {
			slog.Error(err)
		}
	}

	if config.LeaderActive {
		// Failover tests
		err = config.Failover(ctx, clientset)
		if err != nil {
			slog.Error(err)
		}
	}
	if config.LocalDeploy {
		// Failover tests
		err = config.LocalDeployment(ctx, clientset)
		if err != nil {
			slog.Error(err)
		}
	}

	if config.Egress {
		// Failover tests
		err = config.EgressDeployment(ctx, clientset)
		if err != nil {
			slog.Error(err)
		}
	}

	if config.EgressIPv6 {
		// Failover tests
		err = config.Egressv6Deployment(ctx, clientset)
		if err != nil {
			slog.Error(err)
		}
	}

	if config.DualStack {
		// Failover tests
		err = config.DualStackDeployment(ctx, clientset)
		if err != nil {
			slog.Error(err)
		}
	}

	slog.Infof("🏆 Testing Complete [%d] passed", config.SuccessCounter)
}

func httpTest(address string) error {
	slog.Infof("🕷️  testing HTTP request against [%s]", address)
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
			slog.Infof("🕸️  successfully retrieved web data in [%ds]", i)
			r.Body.Close()

			return nil
		}
		time.Sleep(time.Second * 2)
	}
	return err
}

func leaderFailover(ctx context.Context, name, leaderNode *string, clientset *kubernetes.Clientset) error {
	go func() {
		slog.Infof("💀 killing leader five times")
		for i := 0; i < 5; i++ {
			p, err := clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{})
			if err != nil {
				slog.Fatal(err)
			}

			for x := range p.Items {
				if p.Items[x].Spec.NodeName == *leaderNode {
					if p.Items[x].Spec.Containers[0].Name == "kube-vip" {
						err = clientset.CoreV1().Pods("kube-system").Delete(ctx, p.Items[x].Name, metav1.DeleteOptions{})
						if err != nil {
							slog.Fatal(err)
						}
						slog.Infof("🔪 leader pod [%s] has been deleted", p.Items[x].Name)
					}
				}
			}
			time.Sleep(time.Second * 5)
		}
	}()

	slog.Infof("👀 service [%s] for updates", *name)

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return clientset.CoreV1().Services(v1.NamespaceDefault).Watch(ctx, options)
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
			// slog.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				slog.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					slog.Infof("🔎 found load balancer address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations["kube-vip.io/vipHost"])
				}
			}
		case watch.Modified:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				slog.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					slog.Infof("🔍 updated with address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations["kube-vip.io/vipHost"])
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
		slog.Infof("💀 killing active pod five times")
		for i := 0; i < 5; i++ {
			p, err := clientset.CoreV1().Pods(v1.NamespaceDefault).List(ctx, metav1.ListOptions{})
			if err != nil {
				slog.Fatal(err)
			}
			found := false
			for x := range p.Items {
				if p.Items[x].Spec.NodeName == *leaderNode {
					if p.Items[x].Spec.Containers[0].Name == "kube-vip-web" {
						found = true
						err = clientset.CoreV1().Pods(v1.NamespaceDefault).Delete(ctx, p.Items[x].Name, metav1.DeleteOptions{})
						if err != nil {
							slog.Fatal(err)
						}
						slog.Infof("🔪 active pod [%s] on [%s] has been deleted", p.Items[x].Name, p.Items[x].Spec.NodeName)
					}
				}
			}
			if !found {
				slog.Warnf("😱 No Pod found on [%s]", *leaderNode)
			}
			time.Sleep(time.Second * 5)
		}
	}()

	slog.Infof("👀 service [%s] for updates", *name)

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return clientset.CoreV1().Services(v1.NamespaceDefault).Watch(ctx, options)
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
			// slog.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				slog.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					slog.Infof("🔎 found load balancer address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations["kube-vip.io/vipHost"])
				}
			}
		case watch.Modified:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				slog.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					slog.Infof("🔍 updated with address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations["kube-vip.io/vipHost"])
					err = httpTest(svc.Status.LoadBalancer.Ingress[0].IP)
					if err != nil {
						slog.Fatal(err)
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
		slog.Error(err)
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
			// slog.Fatal(err)
		}
		remoteAddress, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		// If it is IPv6 expand the address
		// if IPv6 {
		// 	addr, _ := netip.ParseAddr(remoteAddress)
		// 	remoteAddress = addr.StringExpanded()
		// }

		if remoteAddress == *egressAddress {
			slog.Infof("📞 👍 incoming from egress Address [%s]", remoteAddress)
			return true
		}
		slog.Infof("📞 👎 incoming from pod address [%s]", remoteAddress)
		go handleRequest(conn)
	}
}

func handleRequest(conn net.Conn) {
	// incoming request
	buffer := make([]byte, 1024)
	_, err := conn.Read(buffer)
	if err != nil {
		slog.Error(err)
	}
	// write data to response
	time := time.Now().Format(time.ANSIC)
	responseStr := fmt.Sprintf("Your message is: %v. Received time: %v", string(buffer[:]), time)
	_, err = conn.Write([]byte(responseStr))
	if err != nil {
		slog.Error(err)
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
