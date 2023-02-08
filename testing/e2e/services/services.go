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
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	clientset, err := k8s.NewClientset(homeConfigPath, false, "")
	if err != nil {
		log.Fatalf("could not create k8s clientset from external file: %q: %v", homeConfigPath, err)
	}
	log.Debugf("Using external Kubernetes configuration from file [%s]", homeConfigPath)

	d := "kube-vip-deploy"
	s := "kube-vip-service"
	err = createDeployment(ctx, d, clientset)
	if err != nil {
		log.Fatal(err)
	}
	err = createService(ctx, s, clientset)
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

	d = "kube-vip-deploy-leader"
	err = createDeployment(ctx, d, clientset)
	if err != nil {
		log.Fatal(err)
	}
	for i := 1; i < 5; i++ {
		err = createService(ctx, fmt.Sprintf("%s-%d", s, i), clientset)
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
	err = clientset.AppsV1().Deployments(v1.NamespaceDefault).Delete(ctx, d, metav1.DeleteOptions{})
	if err != nil {
		log.Fatal(err)
	}
}

func createDeployment(ctx context.Context, name string, clientset *kubernetes.Clientset) error {
	replicas := int32(2)
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
							Name:  "web",
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

func createService(ctx context.Context, name string, clientset *kubernetes.Clientset) error {
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
				}
			}
		default:

		}
		if ready {
			break
		}
	}
	var Client http.Client
	var backoffSchedule = []time.Duration{
		1 * time.Second,
		3 * time.Second,
		10 * time.Second,
	}

	for _, backoff := range backoffSchedule {
		//nolint
		_, err = Client.Get(fmt.Sprintf("http://%s", testAddress))
		if err == nil {
			log.Info("🕸️  successfully retrieved web data")
			return nil
		}
		log.Errorf("⏰ retry in [%v] : [%+v]", backoff, err)
		time.Sleep(backoff)
	}

	return fmt.Errorf(" web retrieval timeout ")
}
