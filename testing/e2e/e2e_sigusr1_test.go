//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("SIGUSR1 Signal Handler", func() {
	var (
		session *gexec.Session
		ctx     context.Context
		cancel  context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.TODO(), 30*time.Second)
	})

	AfterEach(func() {
		if session != nil {
			session.Terminate()
			Eventually(session).Should(gexec.Exit())
		}
		cancel()
	})

	When("kube-vip receives SIGUSR1", func() {
		It("should dump configuration without terminating", func() {
			Skip("Skipping until signal handler is implemented")

			kvPath := os.Getenv("KUBE_VIP_PATH")
			if kvPath == "" {
				kvPath = "../../kube-vip"
			}

			cmd := exec.CommandContext(ctx, kvPath, "manager",
				"--address", "192.168.1.100",
				"--interface", "lo",
				"--port", "6443",
			)

			var err error
			session, err = gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())

			time.Sleep(2 * time.Second)

			err = session.Command.Process.Signal(syscall.SIGUSR1)
			Expect(err).NotTo(HaveOccurred())

			Eventually(session.Out, "5s").Should(gbytes.Say("KUBE-VIP CONFIGURATION DUMP"))
			Eventually(session.Out).Should(gbytes.Say("VIP: 192.168.1.100"))

			Consistently(session, "2s").ShouldNot(gexec.Exit())
		})

		It("should handle multiple SIGUSR1 signals", func() {
			Skip("Skipping until signal handler is implemented")

			kvPath := os.Getenv("KUBE_VIP_PATH")
			if kvPath == "" {
				kvPath = "../../kube-vip"
			}

			cmd := exec.CommandContext(ctx, kvPath, "manager",
				"--address", "192.168.1.100",
				"--interface", "lo",
			)

			var err error
			session, err = gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())

			time.Sleep(2 * time.Second)

			for i := 0; i < 3; i++ {
				err = session.Command.Process.Signal(syscall.SIGUSR1)
				Expect(err).NotTo(HaveOccurred())
				time.Sleep(1 * time.Second)
			}

			Eventually(session.Out).Should(gbytes.Say("KUBE-VIP CONFIGURATION DUMP"))
			Consistently(session, "2s").ShouldNot(gexec.Exit())
		})
	})
})
