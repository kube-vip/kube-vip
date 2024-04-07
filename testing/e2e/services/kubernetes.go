package main

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	watchtools "k8s.io/client-go/tools/watch"
)

// service defines the settings for a new service
type service struct {
	name          string
	egress        bool // enable egress
	policyLocal   bool // set the policy to local pods
	testHTTP      bool
	testDualstack bool // test dualstack loadbalancer services
}

type deployment struct {
	replicas     int
	server       bool
	client       bool
	address      string
	nodeAffinity string
	name         string
}

func (d *deployment) createKVDs(ctx context.Context, clientset *kubernetes.Clientset, imagepath string) error {
	ds := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-vip-ds",
			Namespace: "kube-system",
			Labels: map[string]string{
				"app.kubernetes.io/name": "kube-vip-ds",
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": "kube-vip-ds",
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": "kube-vip-ds",
					},
				},
				Spec: v1.PodSpec{
					ServiceAccountName: "kube-vip",
					HostNetwork:        true,
					Containers: []v1.Container{
						{
							Args: []string{
								"manager",
							},
							Env: []v1.EnvVar{
								{
									Name:  "vip_arp",
									Value: "true",
								},
								{
									Name:  "vip_cidr",
									Value: "32",
								},
								{
									Name:  "svc_enable",
									Value: "true",
								},
								{
									Name:  "svc_election",
									Value: "true",
								},
								{
									Name:  "EGRESS_CLEAN",
									Value: "true",
								},
								{
									Name:  "vip_loglevel",
									Value: "5",
								},
								{
									Name:  "egress_withnftables",
									Value: "true",
								},
							},
							Image: imagepath,
							Name:  "kube-vip",
							SecurityContext: &v1.SecurityContext{
								Capabilities: &v1.Capabilities{
									Add: []v1.Capability{
										"NET_ADMIN",
										"NET_RAW",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := clientset.AppsV1().DaemonSets("kube-system").Create(ctx, &ds, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	return nil

}
func (d *deployment) createDeployment(ctx context.Context, clientset *kubernetes.Clientset) error {
	replicas := int32(d.replicas)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: d.name,
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
							Image: "plndr/e2e:0.0.1",
							Ports: []v1.ContainerPort{
								{
									Name:          "http",
									Protocol:      v1.ProtocolTCP,
									ContainerPort: 80,
								},
							},
							ImagePullPolicy: v1.PullIfNotPresent,
						},
					},
				},
			},
		},
	}

	if d.server {
		deployment.Spec.Template.Spec.Containers[0].Env =
			[]v1.EnvVar{
				{
					Name:  "E2EMODE",
					Value: "SERVER",
				},
			}
	}

	if d.client && d.address != "" {
		deployment.Spec.Template.Spec.Containers[0].Env =
			[]v1.EnvVar{
				{
					Name:  "E2EMODE",
					Value: "CLIENT",
				},
				{
					Name:  "E2EADDRESS",
					Value: d.address,
				},
			}
	}

	if d.nodeAffinity != "" {
		deployment.Spec.Template.Spec.NodeName = d.nodeAffinity
	}

	result, err := clientset.AppsV1().Deployments(v1.NamespaceDefault).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	log.Infof("📝 created deployment [%s]", result.GetObjectMeta().GetName())
	return nil
}

func (s *service) createService(ctx context.Context, clientset *kubernetes.Clientset) (currentLeader string, loadBalancerAddresses []string, err error) {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
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

	if s.egress {
		svc.Annotations = map[string]string{ //kube-vip.io/egress: "true"
			"kube-vip.io/egress": "true",
		}
	}
	if s.policyLocal {
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	}

	if s.testDualstack {
		if svc.Annotations == nil {
			svc.Annotations = make(map[string]string)
		}
		ipv4VIP, err := generateIPv4VIP()
		if err != nil {
			log.Fatal(err)
		}
		ipv6VIP, err := generateIPv6VIP()
		if err != nil {
			log.Fatal(err)
		}
		svc.Annotations["kube-vip.io/loadbalancerIPs"] = fmt.Sprintf("%s,%s", ipv4VIP, ipv6VIP)
		svc.Labels["implementation"] = "kube-vip"
		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}
		ipfPolicy := v1.IPFamilyPolicyRequireDualStack
		svc.Spec.IPFamilyPolicy = &ipfPolicy
	}

	log.Infof("🌍 creating service [%s]", svc.Name)
	_, err = clientset.CoreV1().Services(v1.NamespaceDefault).Create(ctx, svc, metav1.CreateOptions{})
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
	go func() {
		time.Sleep(time.Second * 10)
		rw.Stop()
	}()
	ready := false

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
			if svc.Name == s.name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					for _, ingress := range svc.Status.LoadBalancer.Ingress {
						loadBalancerAddresses = append(loadBalancerAddresses, ingress.IP)
					}
					log.Infof("🔎 found load balancer addresses [%s] on node [%s]", loadBalancerAddresses, svc.Annotations["kube-vip.io/vipHost"])
					ready = true
					currentLeader = svc.Annotations["kube-vip.io/vipHost"]
				}
			}
		default:

		}
		if ready {
			break
		}
	}
	if s.testHTTP {
		for _, lbAddress := range loadBalancerAddresses {
			err = httpTest(lbAddress)
			if err != nil {
				return "", nil, fmt.Errorf("web retrieval timeout ")

			}
		}
	}
	return currentLeader, loadBalancerAddresses, nil
}
