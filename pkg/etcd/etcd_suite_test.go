//go:build integration
// +build integration

package etcd_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	etcdVersion     = "v3.5.9"
	etcdBinDir      = "bin"
	etcdBinPath     = etcdBinDir + "/etcd"
	downloadURL     = "https://storage.googleapis.com/etcd"
	tmpDownloadFile = "etcd.tar.gz"
	pidFile         = "etcd.pid"
)

func TestMain(m *testing.M) {
	logrus.SetLevel(logrus.DebugLevel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expectSuccess(startEtcd(ctx), "starting etcd")

	os.Exit(runTestsWithCleanup(m, func() {
		expectSuccess(stopEtcd(), "stopping etcd")
	}))
}

func runTestsWithCleanup(m *testing.M, cleanup func()) int {
	defer cleanup()
	return m.Run()
}

func expectSuccess(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s\n", msg, err)
	}
}

func startEtcd(ctx context.Context) error {
	if _, err := os.Stat(pidFile); err == nil {
		log.Println("Etcd already running, reusing")
		return nil
	}

	etcdPath, err := installEtcd(ctx)
	if err != nil {
		errors.Wrap(err, "installing etcd for tests")
	}

	etcdCmd := exec.Command(etcdPath, "--data-dir", "./etcd-data")
	if os.Getenv("ETCD_SERVER_LOGS") == "true" {
		log.Println("Enabling etcd server logs")
		etcdCmd.Stdout = os.Stdout
		etcdCmd.Stderr = os.Stderr
	}
	log.Println("Starting etcd")
	if err := etcdCmd.Start(); err != nil {
		errors.Wrap(err, "starting etcd for tests")
	}

	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(etcdCmd.Process.Pid)), 0o600); err != nil {
		return err
	}

	log.Println("Waiting for etcd to be up")
	time.Sleep(time.Second)

	return nil
}

func installEtcd(ctx context.Context) (string, error) {
	projectRoot, err := filepath.Abs("../../")
	if err != nil {
		return "", err
	}
	binDir := filepath.Join(projectRoot, etcdBinDir)
	etcdPath := filepath.Join(projectRoot, etcdBinPath)

	if _, err := os.Stat(etcdPath); err == nil {
		log.Println("Etcd already installed, skipping")
		return etcdPath, nil
	}

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}

	download := fmt.Sprintf("%s/%s/etcd-%s-linux-amd64.tar.gz", downloadURL, etcdVersion, etcdVersion)

	// Hacky to run bash, but simplifies this code a lot
	cmd := fmt.Sprintf("curl -sL %s | tar -xzvf - -C %s --strip-components=1", download, binDir)
	out, err := exec.CommandContext(ctx, "bash", "-c", cmd).CombinedOutput()
	if err != nil {
		return "", errors.Wrapf(err, "downloading etcd: %s", string(out))
	}

	return etcdPath, nil
}

func stopEtcd() error {
	if os.Getenv("REUSE_ETCD") == "true" {
		log.Println("REUSE_ETCD=true, leaving etcd running")
		return nil
	}

	if _, err := os.Stat(pidFile); os.IsNotExist(err) {
		log.Println("Etcd pid file doesn't exit, skipping cleanup")
		return nil
	}

	dat, err := os.ReadFile(pidFile)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(string(dat))
	if err != nil {
		return err
	}

	etcdProcess, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	log.Println("Stopping etcd")
	if err := etcdProcess.Kill(); err != nil {
		return errors.Wrap(err, "Failed stopping etcd")
	}

	log.Println("Deleting etcd data")
	if err := os.RemoveAll("./etcd-data"); err != nil {
		return errors.Wrap(err, "deleting etcd data")
	}

	log.Println("Deleting etcd pid file")
	if err := os.RemoveAll(pidFile); err != nil {
		return errors.Wrap(err, "deleting pid file")
	}

	return nil
}
