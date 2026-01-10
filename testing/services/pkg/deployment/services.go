//nolint:govet
package deployment

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gookit/slog"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/testing/e2e"
	"github.com/vishvananda/netlink"

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
func (config *TestConfig) StartServiceTest(ctx context.Context, clientset *kubernetes.Clientset) []error {
	var err error
	var errs []error
	globalTempDirPath, err := os.MkdirTemp("/tmp", "kube-vip-service-tests")
	if err != nil {
		slog.Error(err)
		return []error{fmt.Errorf("failed to create temporary directory: %w", err)}
	}

	defer func() {
		if os.Getenv("E2E_KEEP_LOGS") != "true" {
			if err := os.RemoveAll(globalTempDirPath); err != nil {
				slog.Error(fmt.Errorf("failed to remove temporary directory %q: %w", globalTempDirPath, err))
			}
		}
	}()

	if config.Simple {
		err = config.SimpleDeployment(ctx, clientset)
		if err != nil {
			slog.Error(err)
			errs = append(errs, err)
		}
		tempDirPath, err := os.MkdirTemp(globalTempDirPath, "Simple")
		if err != nil {
			slog.Error(err)
			return []error{fmt.Errorf("failed to create temporary directory: %w", err)}
		}
		slog.Infof("saving logs to %q", tempDirPath)
		if err := e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Error(err)
		}
	}

	if config.Deployments {
		err = config.MultipleDeployments(ctx, clientset)
		if err != nil {
			slog.Error(err)
			errs = append(errs, err)
		}
		tempDirPath, err := os.MkdirTemp(globalTempDirPath, "Deployments")
		if err != nil {
			slog.Error(err)
			return []error{fmt.Errorf("failed to create temporary directory: %w", err)}
		}
		slog.Infof("saving logs to %q", tempDirPath)
		if err := e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Error(err)
		}
	}

	if config.LeaderFailover {
		// Failover tests
		err = config.Failover(ctx, clientset)
		if err != nil {
			slog.Error(err)
			errs = append(errs, err)
		}
		tempDirPath, err := os.MkdirTemp(globalTempDirPath, "LeaderFailover")
		if err != nil {
			slog.Error(err)
			return []error{fmt.Errorf("failed to create temporary directory: %w", err)}
		}
		slog.Infof("saving logs to %q", tempDirPath)
		if err := e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Error(err)
		}
	}

	if config.LeaderActive {
		// Failover tests
		err = config.Failover(ctx, clientset)
		if err != nil {
			slog.Error(err)
			errs = append(errs, err)
		}
		tempDirPath, err := os.MkdirTemp(globalTempDirPath, "LeaderActive")
		if err != nil {
			slog.Error(err)
			return []error{fmt.Errorf("failed to create temporary directory: %w", err)}
		}
		slog.Infof("saving logs to %q", tempDirPath)
		if err := e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Error(err)
		}
	}

	if config.LocalDeploy {
		// Failover tests
		err = config.LocalDeployment(ctx, clientset)
		if err != nil {
			slog.Error(err)
			errs = append(errs, err)
		}
		tempDirPath, err := os.MkdirTemp(globalTempDirPath, "LocalDeploy")
		if err != nil {
			slog.Error(err)
			return []error{fmt.Errorf("failed to create temporary directory: %w", err)}
		}
		slog.Infof("saving logs to %q", tempDirPath)
		if err := e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Error(err)
		}
	}

	if config.Egress {
		// Egress test
		err = config.EgressDeployment(ctx, clientset, false)
		if err != nil {
			slog.Error(err)
			errs = append(errs, err)
		}
		tempDirPath, err := os.MkdirTemp(globalTempDirPath, "EgressDeployment")
		if err != nil {
			slog.Error(err)
			return []error{fmt.Errorf("failed to create temporary directory: %w", err)}
		}
		slog.Infof("saving logs to %q", tempDirPath)
		if err := e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Error(err)
		}
	}

	if config.Egress && config.EgressInternal {
		// Egress test
		err = config.EgressDeployment(ctx, clientset, true)
		if err != nil {
			slog.Error(err)
			errs = append(errs, err)
		}
		tempDirPath, err := os.MkdirTemp(globalTempDirPath, "EgressDeploymentInternal")
		if err != nil {
			slog.Error(err)
			return []error{fmt.Errorf("failed to create temporary directory: %w", err)}
		}
		slog.Infof("saving logs to %q", tempDirPath)
		if err := e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Error(err)
		}
	}

	if config.EgressIPv6 {
		// Egress v6 tests
		err = config.Egressv6Deployment(ctx, clientset, false)
		if err != nil {
			slog.Error(err)
			errs = append(errs, err)
		}
		tempDirPath, err := os.MkdirTemp(globalTempDirPath, "EgressIPv6")
		if err != nil {
			slog.Error(err)
			return []error{fmt.Errorf("failed to create temporary directory: %w", err)}
		}
		slog.Infof("saving logs to %q", tempDirPath)
		if err := e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Error(err)
		}
	}

	if config.EgressIPv6 && config.EgressInternal {
		// Egress v6 tests
		err = config.Egressv6Deployment(ctx, clientset, true)
		if err != nil {
			slog.Error(err)
			errs = append(errs, err)
		}
		tempDirPath, err := os.MkdirTemp(globalTempDirPath, "EgressIPv6Internal")
		if err != nil {
			slog.Error(err)
			return []error{fmt.Errorf("failed to create temporary directory: %w", err)}
		}
		slog.Infof("saving logs to %q", tempDirPath)
		if err := e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Error(err)
		}
	}

	if config.DualStack {
		// Dualstack tests
		err = config.DualStackDeployment(ctx, clientset)
		if err != nil {
			slog.Error(err)
			errs = append(errs, err)
		}
		tempDirPath, err := os.MkdirTemp(globalTempDirPath, "DualStack")
		if err != nil {
			slog.Error(err)
			return []error{fmt.Errorf("failed to create temporary directory: %w", err)}
		}
		slog.Infof("saving logs to %q", tempDirPath)
		if err := e2e.GetLogs(ctx, clientset, tempDirPath); err != nil {
			slog.Error(err)
		}
	}

	const testComplete = "🏆 Testing Complete [%d] passed / [%d] failed"
	if len(errs) > 0 {
		slog.Errorf(testComplete, config.SuccessCounter, len(errs))
		return errs
	}
	slog.Infof(testComplete, config.SuccessCounter, len(errs))

	return errs
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
	rw, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
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
			// slog.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				slog.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					slog.Infof("🔎 found load balancer address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations[kubevip.VipHost])
				}
			}
		case watch.Modified:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				slog.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					slog.Infof("🔍 updated with address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations[kubevip.VipHost])
					err = httpTest(svc.Status.LoadBalancer.Ingress[0].IP)
					if err != nil {
						return err
					}
					*leaderNode = svc.Annotations[kubevip.VipHost]
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
	rw, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
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
			// slog.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				slog.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					slog.Infof("🔎 found load balancer address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations[kubevip.VipHost])
				}
			}
		case watch.Modified:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				slog.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == *name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					slog.Infof("🔍 updated with address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations[kubevip.VipHost])
					err = httpTest(svc.Status.LoadBalancer.Ingress[0].IP)
					if err != nil {
						slog.Fatal(err)
					}
					*leaderNode = svc.Annotations[kubevip.VipHost]
				}
			}
		default:

		}
	}
	return nil
}

func tcpServer(egressAddress *string, timeout int, network string) bool {
	var listen net.Listener
	var err error

	port := ":12345"
	if network == "tcp6" {
		port = ":12346"
	}

	listen, err = net.Listen(network, port) //nolint
	if err != nil {
		slog.Error(err)
	}

	srvChan := make(chan any)
	finishChan := make(chan any)

	go func() {
		defer func() {
			err = listen.Close()
			if err != nil {
				slog.Error(err)
			}
			close(finishChan)
		}()
		select {
		case <-srvChan:
			return
		case <-time.After(time.Second * time.Duration(timeout)):
			return
		}
	}()

	result := false
	go func() {
		defer close(srvChan)
		for {
			conn, err := listen.Accept()
			if err != nil {
				result = false
				return
			}

			remoteAddress, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
			if remoteAddress == *egressAddress {
				slog.Infof("📞 👍 incoming from egress Address [%s]", remoteAddress)
				result = true
				return
			}
			slog.Infof("📞 👎 incoming from pod address [%s]", remoteAddress)
			go handleRequest(conn)
		}
	}()

	<-finishChan
	return result
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

func GetLocalIP(ifName string, family int) (*net.IP, *net.IPNet, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, nil, fmt.Errorf("netlink: failed to list links: %w", err)
	}

	famStr := utils.IPv4Family

	if family == netlink.FAMILY_V6 {
		famStr = utils.IPv6Family
	}

	for _, link := range links {
		if strings.Contains(link.Attrs().Name, ifName) {
			ip, ipnet, err := getNetwork(link, family)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get %s address: %w", famStr, err)
			}
			if ip == nil {
				return nil, nil, fmt.Errorf("failed to find %s address on the interface %q", famStr, ifName)
			}
			return ip, ipnet, nil
		}
	}

	return nil, nil, nil
}

func GetLocalIPv4(ifName string) (*net.IP, *net.IPNet, error) {
	return GetLocalIP(ifName, netlink.FAMILY_V4)
}

func GetLocalIPv6(ifName string) (*net.IP, *net.IPNet, error) {
	return GetLocalIP(ifName, netlink.FAMILY_V6)
}
