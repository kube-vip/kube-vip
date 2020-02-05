package cmd

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/ghodss/yaml"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/thebsdbox/kube-vip/pkg/cluster"
	"github.com/thebsdbox/kube-vip/pkg/kubevip"

	appv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var configPath string
var disableVIP bool
var logLevel uint32

// Release - this struct contains the release information populated when building kube-vip
var Release struct {
	Version string
	Build   string
}

var kubeVipCmd = &cobra.Command{
	Use:   "kube-vip",
	Short: "This is a server for providing a Virtual IP and load-balancer for the Kubernetes control-plane",
}

func init() {

	// Manage logging
	kubeVipCmd.PersistentFlags().Uint32VarP(&logLevel, "log", "l", 4, "Set the level of logging")

	// Get the configuration file
	kubeVipStart.Flags().StringVarP(&configPath, "config", "c", "", "Path to a kube-vip configuration")
	kubeVipStart.Flags().BoolVarP(&disableVIP, "disableVIP", "d", false, "Disable the VIP functionality")

	kubeVipCmd.AddCommand(kubeVipVersion)
	kubeVipCmd.AddCommand(kubeVipSample)
	kubeVipCmd.AddCommand(kubeVipStart)

	// Sample commands
	kubeVipSample.AddCommand(kubeVipSampleConfig)
	kubeVipSample.AddCommand(kubeVipSampleManifest)

}

// Execute - starts the command parsing process
func Execute() {
	if err := kubeVipCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

var kubeVipVersion = &cobra.Command{
	Use:   "version",
	Short: "Version and Release information about the Kubernetes Virtual IP Server",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("Kube-VIP Release Information\n")
		fmt.Printf("Version:  %s\n", Release.Version)
		fmt.Printf("Build:    %s\n", Release.Build)
	},
}

var kubeVipSample = &cobra.Command{
	Use:   "sample",
	Short: "Generate a Sample configuration",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

var kubeVipSampleConfig = &cobra.Command{
	Use:   "config",
	Short: "Generate a Sample configuration",
	Run: func(cmd *cobra.Command, args []string) {
		kubevip.SampleConfig()
	},
}

var kubeVipSampleManifest = &cobra.Command{
	Use:   "manifest",
	Short: "Generate a Sample kubernetes manifest",
	Run: func(cmd *cobra.Command, args []string) {
		// Generate the sample manifest specification
		p := &appv1.Pod{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Pod",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "kube-vip",
			},
			Spec: appv1.PodSpec{
				Containers: []appv1.Container{
					{
						Name:  "kube-vip",
						Image: "plndr/kube-vip:latest",
						SecurityContext: &appv1.SecurityContext{
							Capabilities: &appv1.Capabilities{
								Add: []appv1.Capability{
									"NET_ADMIN",
									"SYS_TIME",
								},
							},
						},
						Command: []string{
							"/kube/vip",
							"start",
							"-c",
							"/vip.yaml",
						},
						VolumeMounts: []appv1.VolumeMount{
							appv1.VolumeMount{
								Name:      "config",
								MountPath: "/vip.yaml",
							},
						},
					},
				},
				Volumes: []appv1.Volume{
					appv1.Volume{
						Name: "config",
						VolumeSource: appv1.VolumeSource{
							HostPath: &appv1.HostPathVolumeSource{
								Path: "/path/to/config",
							},
						},
					},
				},
				HostNetwork: true,
			},
		}

		b, _ := yaml.Marshal(p)
		fmt.Printf(string(b))
	},
}

var kubeVipStart = &cobra.Command{
	Use:   "start",
	Short: "Start the Virtual IP / Load balancer",
	Run: func(cmd *cobra.Command, args []string) {
		// Set the logging level for all subsequent functions
		log.SetLevel(log.Level(logLevel))

		if configPath == "" {
			cmd.Help()
			log.Fatalln("No Configuration has been specified")
		}

		c, err := kubevip.OpenConfig(configPath)
		if err != nil {
			log.Fatalf("%v", err)
		}
		var newCluster *cluster.Cluster

		if c.SingleNode {
			// If the Virtual IP isn't disabled then create the netlink configuration
			newCluster, err = cluster.InitCluster(c, disableVIP)
			if err != nil {
				log.Fatalf("%v", err)
			}
			// Start a single node cluster
			newCluster.StartSingleNode(c, disableVIP)
		} else {
			if disableVIP {
				log.Fatalln("Cluster mode requires the Virtual IP to be enabled, use single node with no VIP")
			}
			// If the Virtual IP isn't disabled then create the netlink configuration
			newCluster, err = cluster.InitCluster(c, disableVIP)
			if err != nil {
				log.Fatalf("%v", err)
			}
			// Start a multi-node (raft) cluster
			err = newCluster.StartCluster(c)
			if err != nil {
				log.Fatalf("%v", err)
			}
		}

		signalChan := make(chan os.Signal, 1)
		signal.Notify(signalChan, os.Interrupt)

		<-signalChan

		newCluster.Stop()

	},
}
