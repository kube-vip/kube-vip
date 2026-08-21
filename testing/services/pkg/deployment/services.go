//nolint:govet
package deployment

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gookit/slog"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/vishvananda/netlink"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	watchtools "k8s.io/client-go/tools/watch"
)

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

// failoverTest repeatedly kills the pod whose first container is named
// containerName on the node currently holding the VIP, then asserts that the
// service becomes reachable again afterwards.
//
// Only events observed after at least one pod was actually killed count as
// success, so a VIP that was reachable before the first kill cannot satisfy
// the check. Both Added and Modified events are accepted post-kill: the retry
// watcher restarts replay the current state as Added, which would otherwise
// swallow a Modified transition that happened during a restart gap.
func failoverTest(ctx context.Context, ns, action, containerName string, name, leaderNode *string, clientset *kubernetes.Clientset) error {
	killerCtx, cancelKiller := context.WithCancel(ctx)
	defer cancelKiller()

	// The killer always targets the node that held the VIP when the test
	// started; take a copy so the success path below can update *leaderNode
	// without racing the killer goroutine.
	targetNode := *leaderNode
	// kills counts the pods actually deleted; the reachability check only
	// accepts events observed after the first kill.
	var kills atomic.Int64
	go func() {
		slog.Infof("💀 killing [%s] pods on node [%s] five times", containerName, targetNode)
		for i := 0; i < 5; i++ {
			if killerCtx.Err() != nil {
				return
			}
			p, err := clientset.CoreV1().Pods(ns).List(killerCtx, metav1.ListOptions{})
			if err != nil {
				if killerCtx.Err() == nil {
					// Let the watch below fail the test; a Fatal here would
					// take down every parallel test in the process.
					slog.Errorf("💀 failed to list pods in namespace %q: %v", ns, err)
				}
				return
			}
			found := false
			for x := range p.Items {
				if p.Items[x].Spec.NodeName != targetNode || p.Items[x].Spec.Containers[0].Name != containerName {
					continue
				}
				found = true
				if err := clientset.CoreV1().Pods(ns).Delete(killerCtx, p.Items[x].Name, metav1.DeleteOptions{}); err != nil {
					if killerCtx.Err() == nil {
						slog.Errorf("💀 failed to delete pod %q: %v", p.Items[x].Name, err)
					}
					continue
				}
				kills.Add(1)
				slog.Infof("🔪 pod [%s] on [%s] has been deleted", p.Items[x].Name, p.Items[x].Spec.NodeName)
			}
			if !found {
				slog.Warnf("😱 no [%s] pod found on [%s] in namespace [%s]", containerName, targetNode, ns)
			}
			select {
			case <-killerCtx.Done():
				return
			case <-time.After(time.Second * 5):
			}
		}
	}()

	slog.Infof("👀 service [%s] for updates", *name)

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return clientset.CoreV1().Services(ns).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		return err
	}
	ch := rw.ResultChan()

	// The killer churns pods for ~25s (five kills, five seconds apart), so
	// give the VIP a comfortable margin beyond that to converge before the
	// strict verdict below, especially with other tests running in parallel.
	go func() {
		time.Sleep(time.Second * 60)
		rw.Stop()
	}()

	for event := range ch {
		switch event.Type {
		case watch.Added, watch.Modified:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				slog.Errorf("unable to parse Kubernetes services from API watcher")
				continue
			}
			if svc.Name != *name || len(svc.Status.LoadBalancer.Ingress) == 0 {
				continue
			}
			address := svc.Status.LoadBalancer.Ingress[0].IP
			node := svc.Annotations[kubevip.VipHost]
			if kills.Load() == 0 {
				// Nothing was killed yet, so this event cannot prove recovery.
				slog.Infof("🔎 found load balancer address [%s] on node [%s]", address, node)
				continue
			}
			slog.Infof("🔍 updated with address [%s] on node [%s]", address, node)
			if err := httpTest(address); err != nil {
				// Transient failure while the VIP converges; keep watching.
				slog.Warnf("🔍 service [%s] not yet reachable after %s: %v", *name, action, err)
				continue
			}
			*leaderNode = node
			rw.Stop()
			return nil
		default:

		}
	}
	// The watch drained without a single post-kill event passing httpTest.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("service [%s] watch aborted during %s: %w", *name, action, err)
	}
	if kills.Load() == 0 {
		return fmt.Errorf("no [%s] pod was killed on node [%s] in namespace [%s]; %s was never induced", containerName, targetNode, ns, action)
	}
	return fmt.Errorf("service [%s] never became reachable after %s", *name, action)
}

func leaderFailover(ctx context.Context, ns string, name, leaderNode *string, clientset *kubernetes.Clientset) error {
	// The kube-vip DaemonSet runs in the per-test namespace, not kube-system.
	return failoverTest(ctx, ns, "leader failover", "kube-vip", name, leaderNode, clientset)
}

func podFailover(ctx context.Context, ns string, name, leaderNode *string, clientset *kubernetes.Clientset) error {
	return failoverTest(ctx, ns, "pod failover", "kube-vip-web", name, leaderNode, clientset)
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
