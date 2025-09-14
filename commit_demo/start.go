package commitctr

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	// "github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	containerd "github.com/containerd/containerd/v2/client"
)

func parseJSONSlice(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("invalid JSON array: %w", err)
	}
	return out, nil
}

func Start() {
	var (
		address      = flag.String("address", "/run/containerd/containerd.sock", "containerd address (e.g. /run/containerd/containerd.sock or unix:///run/containerd/containerd.sock)")
		namespace    = flag.String("namespace", "k8s.io", "containerd namespace")
		containerID  = flag.String("container", "1b048471fd6d20a18853701b61726046bd48f90ca999e029c4e63b56d5e329d5", "container ID or name")
		targetRef    = flag.String("ref", "192.168.1.100:5000/test/test:latest", "target image reference (e.g. repo:tag)")
		author       = flag.String("author", "", "image author")
		message      = flag.String("message", "", "commit message")
		pause        = flag.Bool("pause", true, "pause container during commit")
		format       = flag.String("format", "oci", "image format: docker or oci")
		changeCMD    = flag.String("cmd", "", "override CMD as JSON array, e.g. ['bash','-lc','echo hi']")
		changeEntryp = flag.String("entrypoint", "", "override ENTRYPOINT as JSON array")
	)

	flag.Parse()

	if *containerID == "" || *targetRef == "" {
		fmt.Fprintln(os.Stderr, "-container and -ref are required")
		flag.Usage()
		os.Exit(2)
	}

	addr := *address
	addr = strings.TrimPrefix(addr, "unix://")

	ctx := namespaces.WithNamespace(context.Background(), *namespace)
	client, err := containerd.New(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to containerd: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	cmdSlice, err := parseJSONSlice(*changeCMD)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid -cmd: %v\n", err)
		os.Exit(1)
	}
	entrySlice, err := parseJSONSlice(*changeEntryp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid -entrypoint: %v\n", err)
		os.Exit(1)
	}

	opts := Options{
		TargetRef:        *targetRef,
		Author:           *author,
		Message:          *message,
		Pause:            *pause,
		ChangeCMD:        cmdSlice,
		ChangeEntrypoint: entrySlice,
		Format:           *format,
	}

	dgst, err := Commit(ctx, client, *containerID, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "commit failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(dgst.String())
}
