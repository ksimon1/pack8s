package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/fromanirh/pack8s/iopodman"

	"github.com/fromanirh/pack8s/internal/pkg/images"
	"github.com/fromanirh/pack8s/internal/pkg/ledger"
	"github.com/fromanirh/pack8s/internal/pkg/mounts"
	"github.com/fromanirh/pack8s/internal/pkg/podman"
	"github.com/fromanirh/pack8s/internal/pkg/ports"

	"github.com/fromanirh/pack8s/cmd/okd"
)

type runOptions struct {
	privileged     bool
	nodes          uint
	memory         string
	cpu            uint
	secondaryNics  uint
	qemuArgs       string
	background     bool
	reverse        bool
	randomPorts    bool
	registryVolume string
	vncPort        uint
	registryPort   uint
	ocpPort        uint
	k8sPort        uint
	sshPort        uint
	nfsData        string
	logDir         string
	enableCeph     bool
}

var runOpts runOptions

// NewRunCommand returns command that runs given cluster
func NewRunCommand() *cobra.Command {

	run := &cobra.Command{
		Use:   "run",
		Short: "run a given cluster",
		RunE:  run,
		Args:  cobra.ExactArgs(1),
	}

	runOpts.privileged = true // always
	run.Flags().UintVarP(&runOpts.nodes, "nodes", "n", 1, "number of cluster nodes to start")
	run.Flags().StringVarP(&runOpts.memory, "memory", "m", "3096M", "amount of ram per node")
	run.Flags().UintVarP(&runOpts.cpu, "cpu", "c", 2, "number of cpu cores per node")
	run.Flags().UintVarP(&runOpts.secondaryNics, "secondary-nics", "", 0, "number of secondary nics to add")
	run.Flags().StringVar(&runOpts.qemuArgs, "qemu-args", "", "additional qemu args to pass through to the nodes")
	run.Flags().BoolVarP(&runOpts.background, "background", "b", false, "go to background after nodes are up")
	run.Flags().BoolVarP(&runOpts.reverse, "reverse", "r", false, "revert node startup order")
	run.Flags().BoolVar(&runOpts.randomPorts, "random-ports", true, "expose all ports on random localhost ports")
	run.Flags().StringVar(&runOpts.registryVolume, "registry-volume", "", "cache docker registry content in the specified volume")
	run.Flags().UintVar(&runOpts.vncPort, "vnc-port", 0, "port on localhost for vnc")
	run.Flags().UintVar(&runOpts.registryPort, "registry-port", 0, "port on localhost for the docker registry")
	run.Flags().UintVar(&runOpts.ocpPort, "ocp-port", 0, "port on localhost for the ocp cluster")
	run.Flags().UintVar(&runOpts.k8sPort, "k8s-port", 0, "port on localhost for the k8s cluster")
	run.Flags().UintVar(&runOpts.sshPort, "ssh-port", 0, "port on localhost for ssh server")
	run.Flags().StringVar(&runOpts.nfsData, "nfs-data", "", "path to data which should be exposed via nfs to the nodes")
	run.Flags().StringVar(&runOpts.logDir, "log-to-dir", "", "enables aggregated cluster logging to the folder")
	run.Flags().BoolVar(&runOpts.enableCeph, "enable-ceph", false, "enables dynamic storage provisioning using Ceph")

	run.AddCommand(
		okd.NewRunCommand(),
	)
	return run
}

func run(cmd *cobra.Command, args []string) (err error) {
	prefix, err := cmd.Flags().GetString("prefix")
	if err != nil {
		return err
	}

	portMap, err := ports.NewMappingFromFlags(cmd.Flags(), []ports.PortInfo{
		ports.PortInfo{
			ExposedPort: ports.PortSSH,
			Name:        "ssh-port",
		},
		ports.PortInfo{
			ExposedPort: ports.PortVNC,
			Name:        "vnc-port",
		},
		ports.PortInfo{
			ExposedPort: ports.PortAPI,
			Name:        "k8s-port",
		},
		ports.PortInfo{
			ExposedPort: ports.PortOCP,
			Name:        "ocp-port",
		},
		ports.PortInfo{
			ExposedPort: ports.PortRegistry,
			Name:        "registry-port",
		},
	})
	if err != nil {
		return err
	}

	cluster := args[0]

	b := context.Background()
	ctx, cancel := context.WithCancel(b)

	conn, err := podman.NewConnection(ctx)
	if err != nil {
		return err
	}

	exec := podman.NewExecutor(ctx, conn)

	ldgr := ledger.NewLedger(ctx, conn, cmd.OutOrStderr())

	defer func() {
		ldgr.Done <- err
	}()

	go func() {
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		<-interrupt
		cancel()
		ldgr.Done <- fmt.Errorf("Interrupt received, clean up")
	}()
	// Pull the cluster image
	err = images.PullImage(ctx, conn, "docker.io/"+cluster, os.Stdout)
	if err != nil {
		return err
	}

	dnsmasqName := fmt.Sprintf("%s-dnsmasq", prefix)
	dnsmasqExpose := ports.ToStrings(
		ports.PortSSH, ports.PortRegistry, ports.PortOCP,
		ports.PortAPI, ports.PortVNC,
	)
	dnsmasqPorts := portMap.ToStrings()
	dnsmasqID, err := ldgr.RunContainer(iopodman.Create{
		AddHost: &[]string{
			"nfs:192.168.66.2",
			"registry:192.168.66.2",
			"ceph:192.168.66.2",
		},
		Args: []string{cluster, "/bin/bash", "-c", "/dnsmasq.sh"},
		Env: &[]string{
			fmt.Sprintf("NUM_NODES=%d", runOpts.nodes),
			fmt.Sprintf("NUM_SECONDARY_NICS=%d", runOpts.secondaryNics),
		},
		Expose:     &dnsmasqExpose,
		Name:       &dnsmasqName,
		Privileged: &runOpts.privileged,
		Publish:    &dnsmasqPorts,
		PublishAll: &runOpts.randomPorts,
	})
	if err != nil {
		return err
	}

	dnsmasqNetwork := fmt.Sprintf("container:%s", dnsmasqID)

	// Pull the registry image
	err = images.PullImage(ctx, conn, images.DockerRegistryImage, os.Stdout)
	if err != nil {
		return err
	}

	// TODO: how to use the user-supplied name?
	var registryMounts mounts.MountMapping
	if runOpts.registryVolume != "" {
		registryMounts, err = mounts.NewVolumeMappings(ldgr, []mounts.MountInfo{
			mounts.MountInfo{
				Name: fmt.Sprintf("%s-registry", prefix),
				Path: "/var/lib/registry",
				Type: "volume",
			},
		})
		if err != nil {
			return err
		}
	}

	registryName := fmt.Sprintf("%s-registry", prefix)
	registryMountsStrings := registryMounts.ToStrings()
	_, err = ldgr.RunContainer(iopodman.Create{
		Args:       []string{images.DockerRegistryImage},
		Name:       &registryName,
		Mount:      &registryMountsStrings,
		Privileged: &runOpts.privileged,
		Network:    &dnsmasqNetwork,
	})
	if err != nil {
		return err
	}

	if runOpts.nfsData != "" {
		nfsData, err := filepath.Abs(runOpts.nfsData)
		if err != nil {
			return err
		}

		err = images.PullImage(ctx, conn, images.NFSGaneshaImage, os.Stdout)
		if err != nil {
			return err
		}

		nfsName := fmt.Sprintf("%s-nfs", prefix)
		nfsMounts := []string{fmt.Sprintf("type=bind,source=%s,destination=/data/nfs", nfsData)}
		_, err = ldgr.RunContainer(iopodman.Create{
			Args:       []string{images.NFSGaneshaImage},
			Name:       &nfsName,
			Mount:      &nfsMounts,
			Privileged: &runOpts.privileged,
			Network:    &dnsmasqNetwork,
		})
		if err != nil {
			return err
		}
	}

	if runOpts.enableCeph {
		err = images.PullImage(ctx, conn, images.CephImage, os.Stdout)
		if err != nil {
			return err
		}

		cephName := fmt.Sprintf("%s-ceph", prefix)
		_, err = ldgr.RunContainer(iopodman.Create{
			Args: []string{images.CephImage, "demo"},
			Name: &cephName,
			Env: &[]string{
				"MON_IP=192.168.66.2",
				"CEPH_PUBLIC_NETWORK=0.0.0.0/0",
				"DEMO_DAEMONS=osd,mds",
				"CEPH_DEMO_UID=demo",
			},
			Privileged: &runOpts.privileged,
			Network:    &dnsmasqNetwork,
		})
		if err != nil {
			return err
		}
	}

	if runOpts.logDir != "" {
		logDir, err := filepath.Abs(runOpts.logDir)
		if err != nil {
			return err
		}

		if _, err = os.Stat(logDir); os.IsNotExist(err) {
			os.Mkdir(logDir, 0755)
		}

		err = images.PullImage(ctx, conn, images.FluentdImage, os.Stdout)
		if err != nil {
			return err
		}

		fluentdMounts := []string{fmt.Sprintf("type=bind,source=%s,destination=/fluentd/log/collected", logDir)}
		fluentdName := fmt.Sprintf("%s-fluentd", prefix)
		_, err = ldgr.RunContainer(iopodman.Create{
			Args: []string{
				images.FluentdImage,
				"exec", "fluentd",
				"-i", "\"<system>\n log_level debug\n</system>\n<source>\n@type  forward\n@log_level error\nport  24224\n</source>\n<match **>\n@type file\npath /fluentd/log/collected\n</match>\"",
				"-p", "/fluentd/plugins", "$FLUENTD_OPT", "-v",
			},
			Name:       &fluentdName,
			Mount:      &fluentdMounts,
			Privileged: &runOpts.privileged,
			Network:    &dnsmasqNetwork,
		})
		if err != nil {
			return err
		}
	}

	wg := sync.WaitGroup{}
	wg.Add(int(runOpts.nodes))
	macCounter := 0

	for x := 0; x < int(runOpts.nodes); x++ {
		nodeQemuArgs := runOpts.qemuArgs

		for i := 0; i < int(runOpts.secondaryNics); i++ {
			netSuffix := fmt.Sprintf("%d-%d", x, i)
			macSuffix := fmt.Sprintf("%02x", macCounter)
			macCounter++
			nodeQemuArgs = fmt.Sprintf("%s -device virtio-net-pci,netdev=secondarynet%s,mac=52:55:00:d1:56:%s -netdev tap,id=secondarynet%s,ifname=stap%s,script=no,downscript=no", nodeQemuArgs, netSuffix, macSuffix, netSuffix, netSuffix)
		}

		if len(nodeQemuArgs) > 0 {
			nodeQemuArgs = "--qemu-args \"" + nodeQemuArgs + "\""
		}

		nodeName := nodeNameFromIndex(x + 1)
		nodeNum := fmt.Sprintf("%02d", x+1)
		if runOpts.reverse {
			nodeName = nodeNameFromIndex((int(runOpts.nodes) - x))
			nodeNum = fmt.Sprintf("%02d", (int(runOpts.nodes) - x))
		}

		nodeMounts, err := mounts.NewVolumeMappings(ldgr, []mounts.MountInfo{
			mounts.MountInfo{
				Name: fmt.Sprintf("%s-%s", prefix, nodeName),
				Path: "/var/run/disk",
				Type: "volume",
			},
		})
		if err != nil {
			return err
		}

		contNodeName := nodeContainer(prefix, nodeName)
		contNodeMountsStrings := nodeMounts.ToStrings()
		contNodeID, err := ldgr.RunContainer(iopodman.Create{
			Args: []string{cluster, "/bin/bash", "-c", fmt.Sprintf("/vm.sh -n /var/run/disk/disk.qcow2 --memory %s --cpu %s %s", runOpts.memory, strconv.Itoa(int(runOpts.cpu)), nodeQemuArgs)},
			Env: &[]string{
				fmt.Sprintf("NODE_NUM=%s", nodeNum),
			},
			Name:       &contNodeName,
			Mount:      &contNodeMountsStrings,
			Privileged: &runOpts.privileged,
			Network:    &dnsmasqNetwork,
		})
		if err != nil {
			return err
		}

		err = exec.Do(contNodeName, []string{"/bin/bash", "-c", "while [ ! -f /ssh_ready ] ; do sleep 1; done"}, os.Stdout)
		if err != nil {
			return fmt.Errorf("checking for ssh.sh script for node %s failed: %s", nodeName, err)
		}

		//check if we have a special provision script
		err = exec.Do(contNodeName, []string{"/bin/bash", "-c", fmt.Sprintf("test -f /scripts/%s.sh", nodeName)}, os.Stdout)
		if err == nil {
			err = exec.Do(contNodeName, []string{"/bin/bash", "-c", fmt.Sprintf("ssh.sh sudo /bin/bash < /scripts/%s.sh", nodeName)}, os.Stdout)
		} else {
			err = exec.Do(contNodeName, []string{"/bin/bash", "-c", "ssh.sh sudo /bin/bash < /scripts/nodes.sh"}, os.Stdout)
		}

		if err != nil {
			return fmt.Errorf("provisioning node %s failed: %s", nodeName, err)
		}

		go func(id string) {
			iopodman.WaitContainer().Call(ctx, conn, id, int64(1*time.Second))
			wg.Done()
		}(contNodeID)
	}

	if runOpts.enableCeph {
		// XXX begin
		keyRing := new(bytes.Buffer)
		err := exec.Do(nodeContainer(prefix, "ceph"), []string{
			"/bin/bash",
			"-c",
			"ceph auth print-key connent.admin | base64",
		}, keyRing)
		// XXX end
		if err != nil {
			return err
		}
		nodeName := nodeNameFromIndex(1)
		key := bytes.TrimSpace(keyRing.Bytes())
		err = exec.Do(nodeContainer(prefix, nodeName), []string{
			"/bin/bash",
			"-c",
			fmt.Sprintf("ssh.sh sudo sed -i \"s/replace-me/%s/g\" /tmp/ceph/ceph-secret.yaml", key),
		}, os.Stdout)
		if err != nil {
			return err
		}
		err = exec.Do(nodeContainer(prefix, nodeName), []string{
			"/bin/bash",
			"-c",
			"ssh.sh sudo /bin/bash < /scripts/ceph-csi.sh",
		}, os.Stdout)
		if err != nil {
			return fmt.Errorf("provisioning Ceph CSI failed: %s", err)
		}
	}

	// If logging is enabled, deploy the default fluent logging
	if runOpts.logDir != "" {
		nodeName := nodeNameFromIndex(1)
		err := exec.Do(nodeContainer(prefix, nodeName), []string{
			"/bin/bash",
			"-c",
			"ssh.sh sudo /bin/bash < /scripts/logging.sh",
		}, os.Stdout)
		if err != nil {
			return fmt.Errorf("provisioning logging failed: %s", err)
		}
	}

	// If background flag was specified, we don't want to clean up if we reach that state
	if !runOpts.background {
		wg.Wait()
		ldgr.Done <- fmt.Errorf("Done. please clean up")
	}
	return nil
}

func nodeNameFromIndex(x int) string {
	return fmt.Sprintf("node%02d", x)
}

func nodeContainer(prefix string, node string) string {
	return prefix + "-" + node
}
