package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/fromanirh/pack8s/internal/pkg/podman"
)

type showOptions struct {
	containerIdsOnly bool
}

var showOpts showOptions

func NewShowCommand() *cobra.Command {
	show := &cobra.Command{
		Use:   "show",
		Short: "show lists containers belonging to the cluster",
		RunE:  showContainers,
		Args:  cobra.NoArgs,
	}

	show.Flags().BoolVarP(&showOpts.containerIdsOnly, "ids", "i", false, "show only container ids")

	return show
}

func showContainers(cmd *cobra.Command, args []string) error {

	prefix, err := cmd.Flags().GetString("prefix")
	if err != nil {
		return err
	}

	podmanSocket, err := cmd.Flags().GetString("podman-socket")
	if err != nil {
		return err
	}

	ctx := context.Background()
	hnd, err := podman.NewHandle(ctx, podmanSocket)
	if err != nil {
		return err
	}

	containers, err := hnd.GetPrefixedContainers(prefix)
	if err != nil {
		return err
	}

	if showOpts.containerIdsOnly {
		for _, cont := range containers {
			fmt.Printf("%s\n", cont.Id)
		}
		return nil
	} else {
		if len(containers) >= 1 {
			fmt.Printf("# Container:\n")
			for _, cont := range containers {
				fmt.Printf("%-32s\t%s\n", cont.Names, cont.Id)
			}
		} else {
			fmt.Printf("no containers found for cluster %s\n", prefix)
		}
	}

	volumes, err := hnd.GetPrefixedVolumes(prefix)
	if err != nil {
		return err
	}

	if len(volumes) >= 1 {
		fmt.Printf("# Volumes:\n")
		for _, vol := range volumes {
			fmt.Printf("%-32s\t@%s\n", vol.Name, vol.MountPoint)
		}
	} else {
		fmt.Printf("no volumes found for cluster %s\n", prefix)
	}

	return nil
}
