package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	_, ignore_simple := os.LookupEnv("IGNORE_SIMPLE")
	_, ignore_deployments := os.LookupEnv("IGNORE_DEPLOY")
	_, ignore_leaderFailover := os.LookupEnv("IGNORE_LEADER")
	_, ignore_leaderActive := os.LookupEnv("IGNORE_ACTIVE")
	_, ignore_localDeploy := os.LookupEnv("IGNORE_LOCALDEPLOY")

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

	if !ignore_simple {
		// Simple Deployment test
		log.Infof("🧪 ---> simple deployment <---")
		err = createDeployment(ctx, d, 2, clientset)
		if err != nil {
			log.Fatal(err)
		}
		_, _, err = createService(ctx, s, clientset, false)
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
	if !ignore_deployments {
		// Multiple deployment tests
		log.Infof("🧪 ---> multiple deployments <---")

		err = createDeployment(ctx, l, 2, clientset)
		if err != nil {
			log.Fatal(err)
		}
		for i := 1; i < 5; i++ {
			_, _, err = createService(ctx, fmt.Sprintf("%s-%d", s, i), clientset, false)
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
	if !ignore_leaderFailover {
		// Failover tests
		log.Infof("🧪 ---> leader failover deployment (local policy) <---")

		err = createDeployment(ctx, d, 2, clientset)
		if err != nil {
			log.Fatal(err)
		}
		_, leader, err := createService(ctx, s, clientset, true)
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

	if !ignore_leaderActive {
		// pod Failover tests
		log.Infof("🧪 ---> active pod failover deployment (local policy) <---")

		err = createDeployment(ctx, d, 1, clientset)
		if err != nil {
			log.Fatal(err)
		}
		_, leader, err := createService(ctx, s, clientset, true)
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
	if !ignore_localDeploy {
		// Multiple deployment tests
		log.Infof("🧪 ---> multiple deployments (local policy) <---")

		err = createDeployment(ctx, l, 2, clientset)
		if err != nil {
			log.Fatal(err)
		}
		for i := 1; i < 5; i++ {
			_, _, err = createService(ctx, fmt.Sprintf("%s-%d", s, i), clientset, true)
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
}

func createDeployment(ctx context.Context, name string, replica int, clientset *kubernetes.Clientset) error {
	replicas := int32(replica)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "kube-vip",
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "kube-vip",
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "kube-vip-web",
							Image: "nginx:1.14.2",
							Ports: []v1.ContainerPort{
								{
									Name:          "http",
									Protocol:      v1.ProtocolTCP,
									ContainerPort: 80,
								},
							},
						},
					},
				},
			},
		},
	}
	result, err := clientset.AppsV1().Deployments(v1.NamespaceDefault).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	log.Infof("📝 created deployment [%s]", result.GetObjectMeta().GetName())
	return nil
}

func createService(ctx context.Context, name string, clientset *kubernetes.Clientset, localTraffic bool) (string, string, error) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"app": "kube-vip",
			},
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Port:     80,
					Protocol: v1.ProtocolTCP,
				},
			},
			Selector: map[string]string{
				"app": "kube-vip",
			},
			ClusterIP: "",
			Type:      v1.ServiceTypeLoadBalancer,
		},
	}
	if localTraffic {
		service.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	}
	log.Infof("🌍 creating service [%s]", service.Name)
	_, err := clientset.CoreV1().Services(v1.NamespaceDefault).Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		log.Fatal(err)
	}
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return clientset.CoreV1().Services(v1.NamespaceDefault).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	ch := rw.ResultChan()

	ready := false
	testAddress := ""
	currentLeader := ""
	// Used for tracking an active endpoint / pod
	for event := range ch {

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				log.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					log.Infof("🔎 found load balancer address [%s] on node [%s]", svc.Status.LoadBalancer.Ingress[0].IP, svc.Annotations["kube-vip.io/vipHost"])
					ready = true
					testAddress = svc.Status.LoadBalancer.Ingress[0].IP
					currentLeader = svc.Annotations["kube-vip.io/vipHost"]
				}
			}
		default:

		}
		if ready {
			break
		}
	}
	err = httpTest(testAddress)
	if err == nil {
		return testAddress, currentLeader, nil
	}
	return "", "", fmt.Errorf("web retrieval timeout ")
}

func httpTest(address string) error {
	Client := http.Client{
		Timeout: 1 * time.Second,
	}
	var err error
	for i := 0; i < 5; i++ {
		//nolint

		_, err = Client.Get(fmt.Sprintf("http://%s", address))
		if err == nil {
			log.Infof("🕸️  successfully retrieved web data in [%ds]", i)
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
		log.Fatal(err)
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
					httpTest(svc.Status.LoadBalancer.Ingress[0].IP)
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
		log.Fatal(err)
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
					httpTest(svc.Status.LoadBalancer.Ingress[0].IP)
					*leaderNode = svc.Annotations["kube-vip.io/vipHost"]
				}
			}
		default:

		}
	}
	return nil
}
