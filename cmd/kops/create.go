/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/kops/cmd/kops/util"
	kopsapi "k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/kopscodecs"
	"k8s.io/kops/upup/pkg/fi/cloudup"
	"k8s.io/kops/util/pkg/text"
	"k8s.io/kops/util/pkg/vfs"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

type CreateOptions struct {
	Filenames []string
}

var (
	createLong = templates.LongDesc(i18n.T(`
		Create a resource:` + validResources +
		`
	Create a cluster, instancegroup, keypair, or secret using command line parameters,
	YAML configuration specification files, or stdin.
	(Note: keypairs and secrets cannot be created from YAML config files yet).
	`))

	createExample = templates.Examples(i18n.T(`

	# Create a cluster from the configuration specification in a YAML file.
	kops create -f my-cluster.yaml

	# Create secret from secret spec file.
	kops create -f secret.yaml

	# Create an instancegroup based on the YAML passed into stdin.
	cat instancegroup.yaml | kops create -f -
	`))

	createShort = i18n.T("Create a resource by command line, filename or stdin.")
)

func NewCmdCreate(f *util.Factory, out io.Writer) *cobra.Command {
	options := &CreateOptions{}

	cmd := &cobra.Command{
		Use:     "create {-f FILENAME}...",
		Short:   createShort,
		Long:    createLong,
		Example: createExample,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunCreate(context.TODO(), f, out, options)
		},
	}

	cmd.Flags().StringSliceVarP(&options.Filenames, "filename", "f", options.Filenames, "Filename to use to create the resource")
	cmd.MarkFlagRequired("filename")
	cmd.RegisterFlagCompletionFunc("filename", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"yaml", "json"}, cobra.ShellCompDirectiveFilterFileExt
	})

	// create subcommands
	cmd.AddCommand(NewCmdCreateCluster(f, out))
	cmd.AddCommand(NewCmdCreateInstanceGroup(f, out))
	cmd.AddCommand(NewCmdCreateKeypair(f, out))
	cmd.AddCommand(NewCmdCreateSecret(f, out))
	cmd.AddCommand(NewCmdCreateSSHPublicKey(f, out))

	return cmd
}

func RunCreate(ctx context.Context, f *util.Factory, out io.Writer, c *CreateOptions) error {
	clientset, err := f.Clientset()
	if err != nil {
		return err
	}

	clusterName := ""
	// var cSpec = false
	var sb bytes.Buffer
	fmt.Fprintf(&sb, "\n")
	for _, f := range c.Filenames {
		var contents []byte
		if f == "-" {
			contents, err = ConsumeStdin()
			if err != nil {
				return err
			}
		} else {
			contents, err = vfs.Context.ReadFile(f)
			if err != nil {
				return fmt.Errorf("error reading file %q: %v", f, err)
			}
		}
		// TODO: this does not support a JSON array
		sections := text.SplitContentToSections(contents)
		for _, section := range sections {
			o, gvk, err := kopscodecs.Decode(section, nil)
			if err != nil {
				return fmt.Errorf("error parsing file %q: %v", f, err)
			}

			switch v := o.(type) {
			case *kopsapi.Cluster:
				cloud, err := cloudup.BuildCloud(v)
				if err != nil {
					return err
				}

				// Adding a PerformAssignments() call here as the user might be trying to use
				// the new `-f` feature, with an old cluster definition.
				err = cloudup.PerformAssignments(v, cloud)
				if err != nil {
					return fmt.Errorf("error populating configuration: %v", err)
				}
				_, err = clientset.CreateCluster(ctx, v)
				if err != nil {
					if apierrors.IsAlreadyExists(err) {
						return fmt.Errorf("cluster %q already exists", v.ObjectMeta.Name)
					}
					return fmt.Errorf("error creating cluster: %v", err)
				}
				fmt.Fprintf(&sb, "Created cluster/%s\n", v.ObjectMeta.Name)
				// cSpec = true

			case *kopsapi.InstanceGroup:
				clusterName = v.ObjectMeta.Labels[kopsapi.LabelClusterName]
				if clusterName == "" {
					return fmt.Errorf("must specify %q label with cluster name to create instanceGroup", kopsapi.LabelClusterName)
				}
				cluster, err := clientset.GetCluster(ctx, clusterName)
				if err != nil {
					return fmt.Errorf("error querying cluster %q: %v", clusterName, err)
				}

				if cluster == nil {
					return fmt.Errorf("cluster %q not found", clusterName)
				}

				_, err = clientset.InstanceGroupsFor(cluster).Create(ctx, v, metav1.CreateOptions{})
				if err != nil {
					if apierrors.IsAlreadyExists(err) {
						return fmt.Errorf("instanceGroup %q already exists", v.ObjectMeta.Name)
					}
					return fmt.Errorf("error creating instanceGroup: %v", err)
				}
				fmt.Fprintf(&sb, "Created instancegroup/%s\n", v.ObjectMeta.Name)

			case *kopsapi.SSHCredential:
				clusterName = v.ObjectMeta.Labels[kopsapi.LabelClusterName]
				if clusterName == "" {
					return fmt.Errorf("must specify %q label with cluster name to create SSHCredential", kopsapi.LabelClusterName)
				}
				if v.Spec.PublicKey == "" {
					return fmt.Errorf("spec.PublicKey is required")
				}

				cluster, err := clientset.GetCluster(ctx, clusterName)
				if err != nil {
					return err
				}

				sshCredentialStore, err := clientset.SSHCredentialStore(cluster)
				if err != nil {
					return err
				}

				sshKeyArr := []byte(v.Spec.PublicKey)
				err = sshCredentialStore.AddSSHPublicKey(sshKeyArr)
				if err != nil {
					return err
				}
				fmt.Fprintf(&sb, "Added ssh credential\n")

			default:
				klog.V(2).Infof("Type of object was %T", v)
				return fmt.Errorf("Unhandled kind %q in %s", gvk, f)
			}
		}

	}
	{
		// If there is a value in this sb, this should mean that we have something to deploy
		// so let's advise the user how to engage the cloud provider and deploy
		if sb.String() != "" {
			fmt.Fprintf(&sb, "\n")
			fmt.Fprintf(&sb, "To deploy these resources, run: kops update cluster --name %s --yes\n", clusterName)
			fmt.Fprintf(&sb, "\n")
		}
		_, err := out.Write(sb.Bytes())
		if err != nil {
			return fmt.Errorf("error writing to output: %v", err)
		}
	}
	return nil
}
