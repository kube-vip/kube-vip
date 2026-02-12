//go:build e2e
// +build e2e

package e2e

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
	"sigs.k8s.io/kind/pkg/cmd"
	load "sigs.k8s.io/kind/pkg/cmd/kind/load/docker-image"
	"sigs.k8s.io/kind/pkg/exec"
	kindlog "sigs.k8s.io/kind/pkg/log"
)

func DeleteNodes(n ...nodes.Node) {
	Expect(deleteNodes(n...)).To(Succeed())
}

func deleteNodes(n ...nodes.Node) error {
	if len(n) == 0 {
		return nil
	}
	const command = "docker"
	args := make([]string, 0, len(n)+3) // allocate once
	args = append(args,
		"rm",
		"-f", // force the container to be delete now
		"-v", // delete volumes
	)
	for _, node := range n {
		args = append(args, node.String())
	}
	if err := exec.Command(command, args...).Run(); err != nil {
		return errors.Wrap(err, "failed to delete nodes")
	}
	return nil
}

func NodeIPv4(node nodes.Node) string {
	ip, _, err := node.IP()
	Expect(err).NotTo(HaveOccurred())
	return ip
}

func LoadDockerImageToKind(logger kindlog.Logger, imagePath, clusterName string) error {
	loadImageCmd := load.NewCommand(logger, cmd.StandardIOStreams())
	loadImageCmd.SetArgs([]string{"--name", clusterName, imagePath})
	return loadImageCmd.Execute()
}

func RunInNode(node nodes.Node, command string, args ...string) {
	Expect(PrintCommandOutputIfErr(
		node.Command(command, args...).Run(),
	)).To(Succeed())
}

func StopPodInNode(node nodes.Node, containerName string) {
	RunInNode(node,
		"bash", "-c",
		fmt.Sprintf(
			"crictl pods  --output json --name %s-%s | jq -r \".items[0].id\" | xargs crictl stopp",
			containerName,
			node.String(),
		),
	)
}

func CopyFromNodeToDisk(node nodes.Node, org, dst string) {
	dstFile, err := os.Create(dst)
	Expect(err).NotTo(HaveOccurred())
	defer dstFile.Close()

	Expect(node.Command("cat", org).SetStdout(dstFile).Run()).To(Succeed())
}

func CopyFolderFromNodeToDisk(node nodes.Node, org, dst string) {
	Expect(os.MkdirAll(dst, 0o755)).To(Succeed())

	for _, file := range filesInNodeFolder(node, org) {
		CopyFromNodeToDisk(node, file, filepath.Join(dst, filepath.Base(file)))
	}
}

func CopyFolderFromNodeToNode(org, dst nodes.Node, folder string) {
	for _, folder := range foldersInNodeFolder(org, folder) {
		CopyFolderFromNodeToNode(org, dst, folder)
	}

	for _, file := range filesInNodeFolder(org, folder) {
		Expect(nodeutils.CopyNodeToNode(org, dst, file)).To(Succeed())
	}
}

func filesInNodeFolder(node nodes.Node, folder string) []string {
	return commandOutputInLines(
		node,
		"find", folder, "-maxdepth", "1", "-mindepth", "1", "-type", "f",
	)
}

func foldersInNodeFolder(node nodes.Node, folder string) []string {
	return commandOutputInLines(
		node,
		"find", folder, "-maxdepth", "1", "-mindepth", "1", "-type", "d",
	)
}

func commandOutputInLines(node nodes.Node, command string, args ...string) []string {
	var linesB bytes.Buffer
	Expect(node.Command(
		command, args...,
	).SetStdout(&linesB).Run()).To(Succeed())

	var lines []string
	scanner := bufio.NewScanner(&linesB)
	for scanner.Scan() {
		if l := scanner.Text(); l != "" {
			lines = append(lines, l)
		}
	}
	Expect(scanner.Err()).To(Succeed())

	return lines
}

func PrintCommandOutputIfErr(err error) error {
	tErr := err
	for tErr != nil {
		runErrP := &exec.RunError{}
		runErr := &runErrP
		if errors.As(tErr, runErr) {
			klog.Errorf("Command failed %s:\n%s", (*runErr).Command, string((*runErr).Output))
			break
		}
	}

	return tErr
}
