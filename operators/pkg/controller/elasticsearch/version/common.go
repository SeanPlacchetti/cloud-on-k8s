// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package version

import (
	"crypto/sha256"
	"fmt"

	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/common/certificates"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	commonv1alpha1 "github.com/elastic/cloud-on-k8s/operators/pkg/apis/common/v1alpha1"
	"github.com/elastic/cloud-on-k8s/operators/pkg/apis/elasticsearch/v1alpha1"
	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/common/defaults"
	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/common/hash"
	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/common/version"
	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/common/volume"
	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/elasticsearch/initcontainer"
	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/elasticsearch/label"
	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/elasticsearch/name"
	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/elasticsearch/pod"
	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/elasticsearch/settings"
	"github.com/elastic/cloud-on-k8s/operators/pkg/controller/elasticsearch/user"
	esvolume "github.com/elastic/cloud-on-k8s/operators/pkg/controller/elasticsearch/volume"
	"github.com/elastic/cloud-on-k8s/operators/pkg/utils/stringsutil"
)

var (
	// DefaultResources for the Elasticsearch container. The JVM default heap size is 1Gi, so we
	// request at least 2Gi for the container to make sure ES can work properly.
	// Not applying this minimum default would make ES randomly crash (OOM) on small machines.
	DefaultResources = corev1.ResourceRequirements{
		Requests: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
)

// NewExpectedPodSpecs creates PodSpecContexts for all Elasticsearch nodes in the given Elasticsearch cluster
func NewExpectedPodSpecs(
	es v1alpha1.Elasticsearch,
	paramsTmpl pod.NewPodSpecParams,
	newEnvironmentVarsFn func(p pod.NewPodSpecParams) []corev1.EnvVar,
	newESConfigFn func(clusterName string, config commonv1alpha1.Config) (settings.CanonicalConfig, error),
	newInitContainersFn func(imageName string, setVMMaxMapCount *bool, transportCerts volume.SecretVolume, clusterName string) ([]corev1.Container, error),
) ([]pod.PodSpecContext, error) {
	podSpecs := make([]pod.PodSpecContext, 0, es.Spec.NodeCount())

	for _, node := range es.Spec.Nodes {
		// add default PVCs to the node spec
		node.VolumeClaimTemplates = defaults.AppendDefaultPVCs(
			node.VolumeClaimTemplates, node.PodTemplate.Spec, esvolume.DefaultVolumeClaimTemplates...,
		)

		for i := int32(0); i < node.NodeCount; i++ {
			params := pod.NewPodSpecParams{
				// cluster-wide params
				Elasticsearch: es,
				// volumes
				UsersSecretVolume:  paramsTmpl.UsersSecretVolume,
				ProbeUser:          paramsTmpl.ProbeUser,
				UnicastHostsVolume: paramsTmpl.UnicastHostsVolume,
				// volume and init container for the keystore
				KeystoreResources: paramsTmpl.KeystoreResources,
				// pod params
				NodeSpec: node,
			}
			podSpecCtx, err := podSpecContext(
				params,
				newEnvironmentVarsFn,
				newESConfigFn,
				newInitContainersFn,
			)
			if err != nil {
				return nil, err
			}

			podSpecs = append(podSpecs, podSpecCtx)
		}
	}

	return podSpecs, nil
}

// podSpecContext creates a new PodSpecContext for an Elasticsearch node
func podSpecContext(
	p pod.NewPodSpecParams,
	newEnvironmentVarsFn func(p pod.NewPodSpecParams) []corev1.EnvVar,
	newESConfigFn func(clusterName string, config commonv1alpha1.Config) (settings.CanonicalConfig, error),
	newInitContainersFn func(elasticsearchImage string, setVMMaxMapCount *bool, transportCerts volume.SecretVolume, clusterName string) ([]corev1.Container, error),
) (pod.PodSpecContext, error) {
	// setup volumes
	probeSecret := volume.NewSelectiveSecretVolumeWithMountPath(
		user.ElasticInternalUsersSecretName(p.Elasticsearch.Name), esvolume.ProbeUserVolumeName,
		esvolume.ProbeUserSecretMountPath, []string{p.ProbeUser.Name},
	)
	httpCertificatesVolume := volume.NewSecretVolumeWithMountPath(
		certificates.HTTPCertsInternalSecretName(name.ESNamer, p.Elasticsearch.Name),
		esvolume.HTTPCertificatesSecretVolumeName,
		esvolume.HTTPCertificatesSecretVolumeMountPath,
	)

	// A few secret volumes will be generated based on the pod name.
	// At this point the (maybe future) pod does not have a name yet: we still want to
	// create corresponding volumes and volume mounts for pod spec comparisons.
	// Let's create them with a placeholder for the pod name. Volume mounts will be correct,
	// and secret refs in Volumes Mounts will be fixed right before pod creation,
	// if this spec ends up leading to a new pod creation.
	podNamePlaceholder := "pod-name-placeholder"
	transportCertificatesVolume := volume.NewSecretVolumeWithMountPath(
		name.TransportCertsSecret(podNamePlaceholder),
		esvolume.TransportCertificatesSecretVolumeName,
		esvolume.TransportCertificatesSecretVolumeMountPath,
	)
	configVolume := settings.ConfigSecretVolume(podNamePlaceholder)

	// append future volumes from PVCs (not resolved to a claim yet)
	persistentVolumes := make([]corev1.Volume, 0, len(p.NodeSpec.VolumeClaimTemplates))
	for _, claimTemplate := range p.NodeSpec.VolumeClaimTemplates {
		persistentVolumes = append(persistentVolumes, corev1.Volume{
			Name: claimTemplate.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					// actual claim name will be resolved and fixed right before pod creation
					ClaimName: "claim-name-placeholder",
				},
			},
		})
	}

	// build on top of the user-provided pod template spec
	builder := defaults.NewPodTemplateBuilder(p.NodeSpec.PodTemplate, v1alpha1.ElasticsearchContainerName).
		WithDockerImage(p.Elasticsearch.Spec.Image, stringsutil.Concat(pod.DefaultImageRepository, ":", p.Elasticsearch.Spec.Version)).
		WithResources(DefaultResources).
		WithTerminationGracePeriod(pod.DefaultTerminationGracePeriodSeconds).
		WithPorts(pod.DefaultContainerPorts).
		WithReadinessProbe(*pod.NewReadinessProbe()).
		WithAffinity(pod.DefaultAffinity(p.Elasticsearch.Name)).
		WithEnv(newEnvironmentVarsFn(p)...)

	// setup init containers
	initContainers, err := newInitContainersFn(
		builder.Container.Image,
		p.Elasticsearch.Spec.SetVMMaxMapCount,
		transportCertificatesVolume,
		p.Elasticsearch.Name)

	if err != nil {
		return pod.PodSpecContext{}, err
	}

	scriptsVolume := volume.NewConfigMapVolumeWithMode(
		name.ScriptsConfigMap(p.Elasticsearch.Name),
		esvolume.ScriptsVolumeName,
		esvolume.ScriptsVolumeMountPath,
		0755)

	builder = builder.
		WithVolumes(
			append(
				persistentVolumes, // includes the data volume, unless specified differently in the pod template
				append(
					initcontainer.PluginVolumes.Volumes(),
					esvolume.DefaultLogsVolume,
					p.UsersSecretVolume.Volume(),
					p.UnicastHostsVolume.Volume(),
					probeSecret.Volume(),
					transportCertificatesVolume.Volume(),
					httpCertificatesVolume.Volume(),
					scriptsVolume.Volume(),
					configVolume.Volume(),
				)...)...).
		WithVolumeMounts(
			append(
				initcontainer.PluginVolumes.EsContainerVolumeMounts(),
				esvolume.DefaultDataVolumeMount,
				esvolume.DefaultLogsVolumeMount,
				p.UsersSecretVolume.VolumeMount(),
				p.UnicastHostsVolume.VolumeMount(),
				probeSecret.VolumeMount(),
				transportCertificatesVolume.VolumeMount(),
				httpCertificatesVolume.VolumeMount(),
				scriptsVolume.VolumeMount(),
				configVolume.VolumeMount(),
			)...)

	if p.KeystoreResources != nil {
		builder = builder.
			WithVolumes(p.KeystoreResources.Volume).
			WithInitContainers(p.KeystoreResources.InitContainer)
	}

	builder = builder.
		WithInitContainers(initContainers...).
		WithInitContainerDefaults()

	// generate the configuration
	// actual volumes to propagate it will be created later on
	config := p.NodeSpec.Config
	if config == nil {
		config = &commonv1alpha1.Config{}
	}
	esConfig, err := newESConfigFn(p.Elasticsearch.Name, *config)
	if err != nil {
		return pod.PodSpecContext{}, err
	}
	unpackedCfg, err := esConfig.Unpack()
	if err != nil {
		return pod.PodSpecContext{}, err
	}

	// set labels
	esVersion, err := version.Parse(p.Elasticsearch.Spec.Version)
	if err != nil {
		return pod.PodSpecContext{}, err
	}
	labels := label.NewPodLabels(p.Elasticsearch, *esVersion, unpackedCfg)
	if p.KeystoreResources != nil {
		configChecksum := sha256.New224()
		_, _ = configChecksum.Write([]byte(p.KeystoreResources.Version))
		labels[label.ConfigChecksumLabelName] = fmt.Sprintf("%x", configChecksum.Sum(nil))
	}
	builder = builder.WithLabels(labels)

	return pod.PodSpecContext{
		NodeSpec:    p.NodeSpec,
		PodTemplate: builder.PodTemplate,
		Config:      esConfig,
	}, nil
}

// NewPod constructs a pod from the given parameters.
func NewPod(
	es v1alpha1.Elasticsearch,
	podSpecCtx pod.PodSpecContext,
) corev1.Pod {
	// build a pod based on the podSpecCtx template
	template := *podSpecCtx.PodTemplate.DeepCopy()
	esPod := corev1.Pod{
		ObjectMeta: template.ObjectMeta,
		Spec:       template.Spec,
	}

	// label the pod with a hash of its template, for comparison purpose,
	// before it gets assigned a name
	esPod.Labels = hash.SetTemplateHashLabel(esPod.Labels, template)

	// set name & namespace
	esPod.Name = name.NewPodName(es.Name, podSpecCtx.NodeSpec)
	esPod.Namespace = es.Namespace

	// set hostname and subdomain based on pod and cluster names
	if esPod.Spec.Hostname == "" {
		esPod.Spec.Hostname = esPod.Name
	}
	if esPod.Spec.Subdomain == "" {
		esPod.Spec.Subdomain = es.Name
	}

	return esPod
}

// quantityToMegabytes returns the megabyte value of the provided resource.Quantity
func quantityToMegabytes(q resource.Quantity) int {
	return int(q.Value()) / 1024 / 1024
}
