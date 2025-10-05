package deployment

import (
	"context"
	"fmt"
	"time"

	"github.com/gookit/slog"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	watchtools "k8s.io/client-go/tools/watch"
)

// service defines the settings for a new service
type Service struct {
	name           string
	egress         bool // enable egress
	egressInternal bool // enable Internal egress
	egressIPv6     bool // egress should be IPv6
	policyLocal    bool // set the policy to local pods
	testHTTP       bool
	testDualstack  bool // test dualstack loadbalancer services
	timeout        int  // how long to wait for the service to be created
}

type Deployment struct {
	replicas     int32
	server       bool
	client       bool
	address      string
	nodeAffinity string
	name         string
}

func (d *Deployment) CreateKVDs(ctx context.Context, clientset *kubernetes.Clientset, imagepath string) error {
	ds := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-vip-ds",
			Namespace: "kube-system",
			Labels: map[string]string{
				"app.kubernetes.io/name": "kube-vip-ds",
				"app":                    "kube-vip",
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": "kube-vip-ds",
					"app":                    "kube-vip",
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": "kube-vip-ds",
						"app":                    "kube-vip",
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
									Name:  "vip_subnet",
									Value: "auto,auto",
								},
								{
									Name:  "svc_enable",
									Value: "true",
								},
								{
									Name:  "enable_endpointslices",
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
									Value: "-4",
								},
								{
									Name:  "egress_withnftables",
									Value: "true",
								},
								{
									Name: "vip_nodename",
									ValueFrom: &v1.EnvVarSource{
										FieldRef: &v1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
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
func (d *Deployment) CreateDeployment(ctx context.Context, clientset *kubernetes.Clientset) error {
	replicas := d.replicas
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
							Image: "docker.io/plndr/e2e:0.0.1",
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

	slog.Infof("📝 created deployment [%s]", result.GetObjectMeta().GetName())
	return nil
}

func (s *Service) CreateService(ctx context.Context, clientset *kubernetes.Clientset) (currentLeader string, loadBalancerAddresses []string, err error) {
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
			kubevip.Egress: "true",
		}
	}

	if s.egressInternal {
		svc.Annotations[kubevip.EgressInternal] = "true"
	}

	if s.egressIPv6 {
		svc.Annotations[kubevip.EgressIPv6] = "true"
	}

	//svc.Annotations["kube-vip.io/egress-denied-networks"] = "172.18.0.0/24"
	//svc.Annotations["kube-vip.io/egress-allowed-networks"] = "172.18.0.0/24"

	//svc.Annotations["kube-vip.io/egress-allowed-networks"] = "192.168.0.0/24, 172.18.0.0/24"

	if s.policyLocal {
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	}

	if s.testDualstack {
		if svc.Annotations == nil {
			svc.Annotations = make(map[string]string)
		}
		ipv4VIP, err := generateIPv4VIP()
		if err != nil {
			slog.Fatal(err)
		}
		ipv6VIP, err := generateIPv6VIP()
		if err != nil {
			slog.Fatal(err)
		}
		svc.Annotations[kubevip.LoadbalancerIPAnnotation] = fmt.Sprintf("%s,%s", ipv4VIP, ipv6VIP)
		svc.Labels["implementation"] = "kube-vip"
		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}
		ipfPolicy := v1.IPFamilyPolicyRequireDualStack
		svc.Spec.IPFamilyPolicy = &ipfPolicy
	}

	slog.Infof("🌍 creating service [%s]", svc.Name)
	_, err = clientset.CoreV1().Services(v1.NamespaceDefault).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil {
		slog.Fatal(err)
	}
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return clientset.CoreV1().Services(v1.NamespaceDefault).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		slog.Fatal(err)
	}
	ch := rw.ResultChan()
	go func() {
		time.Sleep(time.Second * time.Duration(s.timeout))
		rw.Stop()
	}()
	ready := false

	// Used for tracking an active endpoint / pod
	for event := range ch {

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				slog.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == s.name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					for _, ingress := range svc.Status.LoadBalancer.Ingress {
						loadBalancerAddresses = append(loadBalancerAddresses, ingress.IP)
					}
					slog.Infof("🔎 found load balancer addresses [%s] on node [%s]", loadBalancerAddresses, svc.Annotations[kubevip.VipHost])
					ready = true
					currentLeader = svc.Annotations[kubevip.VipHost]
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
				slog.Infof("web retrieval err: %s", err.Error())
				return "", nil, fmt.Errorf("web retrieval timeout ")

			}
		}
	}
	return currentLeader, loadBalancerAddresses, nil
}
