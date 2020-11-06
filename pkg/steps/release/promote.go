package release

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	coreapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreclientset "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	imagev1 "github.com/openshift/api/image/v1"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/results"
	"github.com/openshift/ci-tools/pkg/steps"
	"github.com/openshift/ci-tools/pkg/steps/utils"
)

// promotionStep will tag a full release suite
// of images out to the configured namespace.
type promotionStep struct {
	config             api.PromotionConfiguration
	images             []api.ProjectDirectoryImageBuildStepConfiguration
	requiredImages     sets.String
	srcClient          ctrlruntimeclient.Client
	dstClient          ctrlruntimeclient.Client
	jobSpec            *api.JobSpec
	podClient          steps.PodClient
	eventClient        coreclientset.EventsGetter
	pushSecret         *coreapi.Secret
	imageCreatorClient ctrlruntimeclient.Client
}

func targetName(config api.PromotionConfiguration) string {
	if len(config.Name) > 0 {
		return fmt.Sprintf("%s/%s:${component}", config.Namespace, config.Name)
	}
	return fmt.Sprintf("%s/${component}:%s", config.Namespace, config.Tag)
}

func (s *promotionStep) Inputs() (api.InputDefinition, error) {
	return nil, nil
}

func (*promotionStep) Validate() error { return nil }

var promotionRetry = wait.Backoff{
	Steps:    20,
	Duration: 10 * time.Millisecond,
	Factor:   1.2,
	Jitter:   0.1,
}

func (s *promotionStep) Run(ctx context.Context) error {
	return results.ForReason("promoting_images").ForError(s.run(ctx))
}

func (s *promotionStep) run(ctx context.Context) error {
	tags, names := toPromote(s.config, s.images, s.requiredImages)
	if len(names) == 0 {
		log.Println("Nothing to promote, skipping...")
		return nil
	}

	log.Printf("Promoting tags to %s: %s", targetName(s.config), strings.Join(names.List(), ", "))
	pipeline := &imagev1.ImageStream{}
	if err := s.srcClient.Get(ctx, ctrlruntimeclient.ObjectKey{
		Namespace: s.jobSpec.Namespace(),
		Name:      api.PipelineImageStream,
	}, pipeline); err != nil {
		return fmt.Errorf("could not resolve pipeline imagestream: %w", err)
	}

	if s.pushSecret != nil {
		if s.imageCreatorClient != nil {
			// This should never happen
			return fmt.Errorf("image-creator client is nil")
		}

		if err := s.imageCreatorClient.Get(ctx, types.NamespacedName{Name: s.config.Namespace}, &coreapi.Namespace{}); err != nil {
			if !errors.IsNotFound(err) {
				return fmt.Errorf("failed to check if namespace %s exists: %w", s.config.Namespace, err)
			}
			if err := s.imageCreatorClient.Create(ctx, &coreapi.Namespace{ObjectMeta: meta.ObjectMeta{Name: s.config.Namespace}}); err != nil && !errors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create namespace %s: %w", s.config.Namespace, err)
			}
		}

		imageMirrorTarget := getImageMirrorTarget(ctx, s.imageCreatorClient, s.config, tags, pipeline)
		if len(imageMirrorTarget) == 0 {
			log.Println("Nothing to promote, skipping...")
			return nil
		}

		if _, err := steps.RunPod(ctx, s.podClient, s.eventClient, getPromotionPod(imageMirrorTarget, s.jobSpec.Namespace())); err != nil {
			return fmt.Errorf("unable to run promotion pod: %w", err)
		}
		return nil
	}

	if len(s.config.Name) > 0 {
		return retry.RetryOnConflict(promotionRetry, func() error {
			is := &imagev1.ImageStream{}
			err := s.dstClient.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: s.config.Namespace, Name: s.config.Name}, is)
			if errors.IsNotFound(err) {
				is.Namespace = s.config.Namespace
				is.Name = s.config.Name
				if err := s.dstClient.Create(ctx, is); err != nil {
					return fmt.Errorf("could not retrieve target imagestream: %w", err)
				}
			}

			for dst, src := range tags {
				if valid, _ := utils.FindStatusTag(pipeline, src); valid != nil {
					is.Spec.Tags = append(is.Spec.Tags, imagev1.TagReference{
						Name: dst,
						From: valid,
					})
				}
			}

			if err := s.dstClient.Update(ctx, is); err != nil {
				if errors.IsConflict(err) {
					return err
				}
				return fmt.Errorf("could not promote image streams: %w", err)
			}
			return nil
		})
	}

	for dst, src := range tags {
		valid, _ := utils.FindStatusTag(pipeline, src)
		if valid == nil {
			continue
		}

		err := retry.RetryOnConflict(promotionRetry, func() error {
			err := s.dstClient.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: s.config.Namespace, Name: dst}, &imagev1.ImageStream{})
			if errors.IsNotFound(err) {
				err = s.dstClient.Create(ctx, &imagev1.ImageStream{
					ObjectMeta: meta.ObjectMeta{
						Name:      dst,
						Namespace: s.config.Namespace,
					},
					Spec: imagev1.ImageStreamSpec{
						LookupPolicy: imagev1.ImageLookupPolicy{
							Local: true,
						},
					},
				})
			}
			if err != nil {
				return fmt.Errorf("could not ensure target imagestream: %w", err)
			}

			ist := &imagev1.ImageStreamTag{
				ObjectMeta: meta.ObjectMeta{
					Name:      fmt.Sprintf("%s:%s", dst, s.config.Tag),
					Namespace: s.config.Namespace,
				},
				Tag: &imagev1.TagReference{
					Name: s.config.Tag,
					From: valid,
				},
			}
			if err := s.dstClient.Update(ctx, ist); err != nil {
				if errors.IsConflict(err) {
					return err
				}
				return fmt.Errorf("could not promote imagestreamtag %s: %w", dst, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func getImageMirrorTarget(ctx context.Context, client ctrlruntimeclient.Client, config api.PromotionConfiguration, tags map[string]string, pipeline *imagev1.ImageStream) map[string]string {
	if pipeline == nil {
		return nil
	}
	imageMirror := map[string]string{}
	if len(config.Name) > 0 {
		for dst, src := range tags {
			dockerImageReference := findDockerImageReference(pipeline, src)
			if dockerImageReference == "" {
				continue
			}
			dockerImageReference = getPublicImageReference(dockerImageReference, pipeline.Status.PublicDockerImageRepository)
			imageMirror[dockerImageReference] = fmt.Sprintf("%s/%s/%s:%s", api.DomainForService(api.ServiceRegistry), config.Namespace, config.Name, dst)
			if err := createIfNotFound(ctx, client, config.Namespace, config.Name); err != nil {
				log.Println(fmt.Sprintf("failed to ensure imagestream %s/%s: %v", config.Namespace, config.Name, err))
			}
		}
	} else {
		for dst, src := range tags {
			dockerImageReference := findDockerImageReference(pipeline, src)
			if dockerImageReference == "" {
				continue
			}
			dockerImageReference = getPublicImageReference(dockerImageReference, pipeline.Status.PublicDockerImageRepository)
			imageMirror[dockerImageReference] = fmt.Sprintf("%s/%s/%s:%s", api.DomainForService(api.ServiceRegistry), config.Namespace, dst, config.Tag)
			if err := createIfNotFound(ctx, client, config.Namespace, dst); err != nil {
				log.Println(fmt.Sprintf("failed to ensure imagestream %s/%s: %v", config.Namespace, dst, err))
			}
		}
	}
	if len(imageMirror) == 0 {
		return nil
	}
	return imageMirror
}

func createIfNotFound(ctx context.Context, client ctrlruntimeclient.Client, namespace, name string) error {
	is := &imagev1.ImageStream{}
	err := client.Get(ctx, ctrlruntimeclient.ObjectKey{Namespace: namespace, Name: name}, is)
	if errors.IsNotFound(err) {
		is.Namespace = namespace
		is.Name = name
		if err := client.Create(ctx, is); err != nil {
			return fmt.Errorf("could not create target imagestream: %w", err)
		}
	}
	return err
}

func getPublicImageReference(dockerImageReference, publicDockerImageRepository string) string {
	if !strings.Contains(dockerImageReference, ":5000") {
		return dockerImageReference
	}
	splits := strings.Split(publicDockerImageRepository, "/")
	if len(splits) < 2 {
		// This should never happen
		log.Println(fmt.Sprintf("Failed to get hostname from publicDockerImageRepository: %s.", publicDockerImageRepository))
		return dockerImageReference
	}
	publicHost := splits[0]
	splits = strings.Split(dockerImageReference, "/")
	if len(splits) < 2 {
		// This should never happen
		log.Println(fmt.Sprintf("Failed to get hostname from dockerImageReference: %s.", dockerImageReference))
		return dockerImageReference
	}
	return strings.Replace(dockerImageReference, splits[0], publicHost, 1)
}

func getPromotionPod(imageMirrorTarget map[string]string, namespace string) *coreapi.Pod {
	var ocCommands []string
	keys := make([]string, 0, len(imageMirrorTarget))
	for k := range imageMirrorTarget {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		ocCommands = append(ocCommands, fmt.Sprintf("oc image mirror --registry-config=%s %s %s", filepath.Join(api.RegistryPushCredentialsCICentralSecretMountPath, coreapi.DockerConfigJsonKey), k, imageMirrorTarget[k]))
	}
	command := []string{"/bin/sh", "-c"}
	args := []string{strings.Join(ocCommands, " && ")}
	return &coreapi.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      "promotion",
			Namespace: namespace,
		},
		Spec: coreapi.PodSpec{
			RestartPolicy: coreapi.RestartPolicyNever,
			Containers: []coreapi.Container{
				{
					Name:    "promotion",
					Image:   fmt.Sprintf("%s/ocp/4.6:cli", api.DomainForService(api.ServiceRegistry)),
					Command: command,
					Args:    args,
					VolumeMounts: []coreapi.VolumeMount{
						{
							Name:      "push-secret",
							MountPath: "/etc/push-secret",
							ReadOnly:  true,
						},
					},
				},
			},
			Volumes: []coreapi.Volume{
				{
					Name: "push-secret",
					VolumeSource: coreapi.VolumeSource{
						Secret: &coreapi.SecretVolumeSource{SecretName: api.RegistryPushCredentialsCICentralSecret},
					},
				},
			},
		},
	}
}

// findDockerImageReference returns DockerImageReference, the string that can be used to pull this image,
// to a tag if it exists in the ImageStream's Spec
func findDockerImageReference(is *imagev1.ImageStream, tag string) string {
	for _, t := range is.Status.Tags {
		if t.Tag != tag {
			continue
		}
		if len(t.Items) == 0 {
			return ""
		}
		return t.Items[0].DockerImageReference
	}
	return ""
}

// toPromote determines the mapping of local tag to external tag which should be promoted
func toPromote(config api.PromotionConfiguration, images []api.ProjectDirectoryImageBuildStepConfiguration, requiredImages sets.String) (map[string]string, sets.String) {
	tagsByDst := map[string]string{}
	names := sets.NewString()

	if config.Disabled {
		return tagsByDst, names
	}

	for _, image := range images {
		// if the image is required or non-optional, include it in promotion
		tag := string(image.To)
		if requiredImages.Has(tag) || !image.Optional {
			tagsByDst[tag] = tag
			names.Insert(tag)
		}
	}
	for _, tag := range config.ExcludedImages {
		delete(tagsByDst, tag)
		names.Delete(tag)
	}
	for dst, src := range config.AdditionalImages {
		tagsByDst[dst] = src
		names.Insert(dst)
	}

	if config.NamePrefix == "" {
		return tagsByDst, names
	}

	namesByDst := map[string]string{}
	names = sets.NewString()
	for dst, src := range tagsByDst {
		name := fmt.Sprintf("%s%s", config.NamePrefix, dst)
		namesByDst[name] = src
		names.Insert(name)
	}

	return namesByDst, names
}

// PromotedTags returns the tags that are being promoted for the given ReleaseBuildConfiguration
func PromotedTags(configuration *api.ReleaseBuildConfiguration) []api.ImageStreamTagReference {
	if configuration.PromotionConfiguration == nil {
		return nil
	}
	tags, _ := toPromote(*configuration.PromotionConfiguration, configuration.Images, sets.NewString())
	var promotedTags []api.ImageStreamTagReference
	for dst := range tags {
		var tag api.ImageStreamTagReference
		if configuration.PromotionConfiguration.Name != "" {
			tag = api.ImageStreamTagReference{
				Namespace: configuration.PromotionConfiguration.Namespace,
				Name:      configuration.PromotionConfiguration.Name,
				Tag:       dst,
			}
		} else { // promotion.Tag must be set
			tag = api.ImageStreamTagReference{
				Namespace: configuration.PromotionConfiguration.Namespace,
				Name:      dst,
				Tag:       configuration.PromotionConfiguration.Tag,
			}
		}
		promotedTags = append(promotedTags, tag)
	}
	return promotedTags
}

func (s *promotionStep) Requires() []api.StepLink {
	return []api.StepLink{api.AllStepsLink()}
}

func (s *promotionStep) Creates() []api.StepLink {
	return []api.StepLink{}
}

func (s *promotionStep) Provides() api.ParameterMap {
	return nil
}

func (s *promotionStep) Name() string { return "" }

func (s *promotionStep) Description() string {
	return fmt.Sprintf("Promote built images into the release image stream %s", targetName(s.config))
}

// PromotionStep copies tags from the pipeline image stream to the destination defined in the promotion config.
// If the source tag does not exist it is silently skipped.
func PromotionStep(config api.PromotionConfiguration, images []api.ProjectDirectoryImageBuildStepConfiguration, requiredImages sets.String, srcClient, dstClient ctrlruntimeclient.Client, jobSpec *api.JobSpec, podClient steps.PodClient, eventClient coreclientset.EventsGetter, pushSecret *coreapi.Secret, imageCreatorClient ctrlruntimeclient.Client) api.Step {
	return &promotionStep{
		config:             config,
		images:             images,
		requiredImages:     requiredImages,
		srcClient:          srcClient,
		dstClient:          dstClient,
		jobSpec:            jobSpec,
		podClient:          podClient,
		eventClient:        eventClient,
		pushSecret:         pushSecret,
		imageCreatorClient: imageCreatorClient,
	}
}
