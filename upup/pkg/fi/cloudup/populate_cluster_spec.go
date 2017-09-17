/*
Copyright 2016 The Kubernetes Authors.

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

package cloudup

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"text/template"

	"github.com/golang/glog"

	api "k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/apis/kops/registry"
	"k8s.io/kops/pkg/apis/kops/util"
	"k8s.io/kops/pkg/apis/kops/validation"
	"k8s.io/kops/pkg/assets"
	"k8s.io/kops/pkg/dns"
	"k8s.io/kops/pkg/model"
	"k8s.io/kops/pkg/model/components"
	"k8s.io/kops/upup/models"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/loader"
	"k8s.io/kops/upup/pkg/fi/utils"
	"k8s.io/kops/util/pkg/vfs"
)

var EtcdClusters = []string{"main", "events"}

type populateClusterSpec struct {
	// InputCluster is the api object representing the whole cluster, as input by the user
	// We build it up into a complete config, but we write the values as input
	InputCluster *api.Cluster

	// ModelStore is the location where models are found
	ModelStore vfs.Path
	// Models is a list of cloudup models to apply
	Models []string

	// fullCluster holds the built completed cluster spec
	fullCluster *api.Cluster

	// assetBuilder holds the AssetBuilder, used to store assets we discover / remap
	assetBuilder *assets.AssetBuilder
}

func findModelStore() (vfs.Path, error) {
	p := models.NewAssetPath("")
	return p, nil
}

// PopulateClusterSpec takes a user-specified cluster spec, and computes the full specification that should be set on the cluster.
// We do this so that we don't need any real "brains" on the node side.
func PopulateClusterSpec(cluster *api.Cluster, assetBuilder *assets.AssetBuilder) (*api.Cluster, error) {
	modelStore, err := findModelStore()
	if err != nil {
		return nil, err
	}

	c := &populateClusterSpec{
		InputCluster: cluster,
		ModelStore:   modelStore,
		Models:       []string{"config"},
		assetBuilder: assetBuilder,
	}
	err = c.run()
	if err != nil {
		return nil, err
	}
	return c.fullCluster, nil
}

//
// Here be dragons
//
// This function has some `interesting` things going on.
// In an effort to let the cluster.Spec fall through I am
// hard coding topology in two places.. It seems and feels
// very wrong.. but at least now my new cluster.Spec.Topology
// struct is falling through..
// @kris-nova
//
func (c *populateClusterSpec) run() error {
	if err := validation.ValidateCluster(c.InputCluster, false); err != nil {
		return err
	}

	// Copy cluster & instance groups, so we can modify them freely
	cluster := &api.Cluster{}

	utils.JsonMergeStruct(cluster, c.InputCluster)

	err := c.assignSubnets(cluster)
	if err != nil {
		return err
	}

	err = cluster.FillDefaults()
	if err != nil {
		return err
	}

	err = PerformAssignments(cluster)
	if err != nil {
		return err
	}

	// TODO: Move to validate?
	// Check that instance groups are defined in valid zones
	{
		// TODO: Check that instance groups referenced here exist
		//clusterSubnets := make(map[string]*api.ClusterSubnetSpec)
		//for _, subnet := range cluster.Spec.Subnets {
		//	if clusterSubnets[subnet.Name] != nil {
		//		return fmt.Errorf("Subnets contained a duplicate value: %v", subnet.Name)
		//	}
		//	clusterSubnets[subnet.Name] = subnet
		//}

		// Check etcd configuration
		{
			for i, etcd := range cluster.Spec.EtcdClusters {
				if etcd.Name == "" {
					return fmt.Errorf("EtcdClusters #%d did not specify a Name", i)
				}

				for i, m := range etcd.Members {
					if m.Name == "" {
						return fmt.Errorf("EtcdMember #%d of etcd-cluster %s did not specify a Name", i, etcd.Name)
					}

					if fi.StringValue(m.InstanceGroup) == "" {
						return fmt.Errorf("EtcdMember %s:%s did not specify a InstanceGroup", etcd.Name, m.Name)
					}
				}

				etcdInstanceGroups := make(map[string]*api.EtcdMemberSpec)
				etcdNames := make(map[string]*api.EtcdMemberSpec)

				for _, m := range etcd.Members {
					if etcdNames[m.Name] != nil {
						return fmt.Errorf("EtcdMembers found with same name %q in etcd-cluster %q", m.Name, etcd.Name)
					}

					instanceGroupName := fi.StringValue(m.InstanceGroup)

					if etcdInstanceGroups[instanceGroupName] != nil {
						glog.Warningf("EtcdMembers are in the same InstanceGroup %q in etcd-cluster %q (fault-tolerance may be reduced)", instanceGroupName, etcd.Name)
					}

					//if clusterSubnets[zone] == nil {
					//	return fmt.Errorf("EtcdMembers for %q is configured in zone %q, but that is not configured at the k8s-cluster level", etcd.Name, m.Zone)
					//}
					etcdInstanceGroups[instanceGroupName] = m
				}

				if (len(etcdInstanceGroups) % 2) == 0 {
					// Not technically a requirement, but doesn't really make sense to allow
					return fmt.Errorf("There should be an odd number of master-zones, for etcd's quorum.  Hint: Use --zones and --master-zones to declare node zones and master zones separately.")
				}
			}
		}
	}

	keyStore, err := registry.KeyStore(cluster)
	if err != nil {
		return err
	}

	secretStore, err := registry.SecretStore(cluster)
	if err != nil {
		return err
	}

	if vfs.IsClusterReadable(secretStore.VFSPath()) {
		vfsPath := secretStore.VFSPath()
		cluster.Spec.SecretStore = vfsPath.Path()
	} else {
		// We could implement this approach, but it seems better to get all clouds using cluster-readable storage
		return fmt.Errorf("secrets path is not cluster readable: %v", secretStore.VFSPath())
	}

	if vfs.IsClusterReadable(keyStore.VFSPath()) {
		vfsPath := keyStore.VFSPath()
		cluster.Spec.KeyStore = vfsPath.Path()
	} else {
		// We could implement this approach, but it seems better to get all clouds using cluster-readable storage
		return fmt.Errorf("keyStore path is not cluster readable: %v", keyStore.VFSPath())
	}

	configBase, err := vfs.Context.BuildVfsPath(cluster.Spec.ConfigBase)
	if err != nil {
		return fmt.Errorf("error parsing ConfigBase %q: %v", cluster.Spec.ConfigBase, err)
	}
	if vfs.IsClusterReadable(configBase) {
		cluster.Spec.ConfigStore = configBase.Path()
	} else {
		// We could implement this approach, but it seems better to get all clouds using cluster-readable storage
		return fmt.Errorf("ConfigBase path is not cluster readable: %v", cluster.Spec.ConfigBase)
	}

	// Normalize k8s version
	versionWithoutV := strings.TrimSpace(cluster.Spec.KubernetesVersion)
	if strings.HasPrefix(versionWithoutV, "v") {
		versionWithoutV = versionWithoutV[1:]
	}
	if cluster.Spec.KubernetesVersion != versionWithoutV {
		glog.V(2).Infof("Normalizing kubernetes version: %q -> %q", cluster.Spec.KubernetesVersion, versionWithoutV)
		cluster.Spec.KubernetesVersion = versionWithoutV
	}
	cloud, err := BuildCloud(cluster)
	if err != nil {
		return err
	}

	if cluster.Spec.DNSZone == "" && !dns.IsGossipHostname(cluster.ObjectMeta.Name) {
		dns, err := cloud.DNS()
		if err != nil {
			return err
		}

		dnsType := api.DNSTypePublic
		if cluster.Spec.Topology != nil && cluster.Spec.Topology.DNS != nil && cluster.Spec.Topology.DNS.Type != "" {
			dnsType = cluster.Spec.Topology.DNS.Type
		}

		dnsZone, err := FindDNSHostedZone(dns, cluster.ObjectMeta.Name, dnsType)
		if err != nil {
			return fmt.Errorf("error determining default DNS zone: %v", err)
		}

		glog.V(2).Infof("Defaulting DNS zone to: %s", dnsZone)
		cluster.Spec.DNSZone = dnsZone
	}

	tags, err := buildCloudupTags(cluster)

	if err != nil {
		return err
	}

	modelContext := &model.KopsModelContext{
		Cluster: cluster,
	}
	tf := &TemplateFunctions{
		cluster:      cluster,
		tags:         tags,
		modelContext: modelContext,
	}

	templateFunctions := make(template.FuncMap)

	tf.AddTo(templateFunctions)

	if cluster.Spec.KubernetesVersion == "" {
		return fmt.Errorf("KubernetesVersion is required")
	}
	sv, err := util.ParseKubernetesVersion(cluster.Spec.KubernetesVersion)
	if err != nil {
		return fmt.Errorf("unable to determine kubernetes version from %q", cluster.Spec.KubernetesVersion)
	}

	optionsContext := &components.OptionsContext{
		ClusterName:       cluster.ObjectMeta.Name,
		KubernetesVersion: *sv,
		AssetBuilder:      c.assetBuilder,
	}

	var fileModels []string
	var codeModels []loader.OptionsBuilder
	for _, m := range c.Models {
		switch m {
		case "config":
			// Note: DefaultOptionsBuilder comes first
			codeModels = append(codeModels, &components.DefaultsOptionsBuilder{Context: optionsContext})
			codeModels = append(codeModels, &components.EtcdOptionsBuilder{Context: optionsContext})
			codeModels = append(codeModels, &components.KubeAPIServerOptionsBuilder{OptionsContext: optionsContext})
			codeModels = append(codeModels, &components.DockerOptionsBuilder{Context: optionsContext})
			codeModels = append(codeModels, &components.NetworkingOptionsBuilder{Context: optionsContext})
			codeModels = append(codeModels, &components.KubeDnsOptionsBuilder{Context: optionsContext})
			codeModels = append(codeModels, &components.KubeletOptionsBuilder{Context: optionsContext})
			codeModels = append(codeModels, &components.KubeControllerManagerOptionsBuilder{Context: optionsContext})
			codeModels = append(codeModels, &components.KubeSchedulerOptionsBuilder{OptionsContext: optionsContext})
			codeModels = append(codeModels, &components.KubeProxyOptionsBuilder{Context: optionsContext})
			fileModels = append(fileModels, m)

		default:
			fileModels = append(fileModels, m)
		}
	}

	specBuilder := &SpecBuilder{
		OptionsLoader: loader.NewOptionsLoader(templateFunctions, codeModels),
		Tags:          tags,
	}

	completed, err := specBuilder.BuildCompleteSpec(&cluster.Spec, c.ModelStore, fileModels)
	if err != nil {
		return fmt.Errorf("error building complete spec: %v", err)
	}

	// TODO: This should not be needed...
	completed.Topology = c.InputCluster.Spec.Topology
	//completed.Topology.Bastion = c.InputCluster.Spec.Topology.Bastion

	fullCluster := &api.Cluster{}
	*fullCluster = *cluster
	fullCluster.Spec = *completed
	tf.cluster = fullCluster

	if err := validation.ValidateCluster(fullCluster, true); err != nil {
		return fmt.Errorf("Completed cluster failed validation: %v", err)
	}

	c.fullCluster = fullCluster
	return nil
}

func (c *populateClusterSpec) assignSubnets(cluster *api.Cluster) error {
	if cluster.Spec.NonMasqueradeCIDR == "" {
		glog.Warningf("NonMasqueradeCIDR not set; can't auto-assign dependent subnets")
		return nil
	}

	_, nonMasqueradeCIDR, err := net.ParseCIDR(cluster.Spec.NonMasqueradeCIDR)
	if err != nil {
		return fmt.Errorf("error parsing NonMasqueradeCIDR %q: %v", cluster.Spec.NonMasqueradeCIDR, err)
	}
	nmOnes, nmBits := nonMasqueradeCIDR.Mask.Size()

	if cluster.Spec.KubeControllerManager == nil {
		cluster.Spec.KubeControllerManager = &api.KubeControllerManagerConfig{}
	}

	if cluster.Spec.KubeControllerManager.ClusterCIDR == "" {
		// Allocate as big a range as possible: the NonMasqueradeCIDR mask + 1, with a '1' in the extra bit
		ip := nonMasqueradeCIDR.IP.Mask(nonMasqueradeCIDR.Mask)

		ip4 := ip.To4()
		if ip4 != nil {
			n := binary.BigEndian.Uint32(ip4)
			n += uint32(1 << uint(nmBits-nmOnes-1))
			ip = make(net.IP, len(ip4))
			binary.BigEndian.PutUint32(ip, n)
		} else {
			return fmt.Errorf("IPV6 subnet computations not yet implements")
		}

		cidr := net.IPNet{IP: ip, Mask: net.CIDRMask(nmOnes+1, nmBits)}
		cluster.Spec.KubeControllerManager.ClusterCIDR = cidr.String()
		glog.V(2).Infof("Defaulted KubeControllerManager.ClusterCIDR to %v", cluster.Spec.KubeControllerManager.ClusterCIDR)
	}

	if cluster.Spec.ServiceClusterIPRange == "" {
		// Allocate from the '0' subnet; but only carve off 1/4 of that (i.e. add 1 + 2 bits to the netmask)
		cidr := net.IPNet{IP: nonMasqueradeCIDR.IP.Mask(nonMasqueradeCIDR.Mask), Mask: net.CIDRMask(nmOnes+3, nmBits)}
		cluster.Spec.ServiceClusterIPRange = cidr.String()
		glog.V(2).Infof("Defaulted ServiceClusterIPRange to %v", cluster.Spec.ServiceClusterIPRange)
	}

	return nil
}
