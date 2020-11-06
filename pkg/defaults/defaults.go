package defaults

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"

	coreapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/scheme"
	appsclientset "k8s.io/client-go/kubernetes/typed/apps/v1"
	coreclientset "k8s.io/client-go/kubernetes/typed/core/v1"
	rbacclientset "k8s.io/client-go/kubernetes/typed/rbac/v1"
	"k8s.io/client-go/rest"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	imagev1 "github.com/openshift/api/image/v1"
	templateapi "github.com/openshift/api/template/v1"
	buildclientset "github.com/openshift/client-go/build/clientset/versioned/typed/build/v1"
	routeclientset "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	templateclientset "github.com/openshift/client-go/template/clientset/versioned/typed/template/v1"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/lease"
	"github.com/openshift/ci-tools/pkg/release/candidate"
	"github.com/openshift/ci-tools/pkg/release/official"
	"github.com/openshift/ci-tools/pkg/release/prerelease"
	"github.com/openshift/ci-tools/pkg/results"
	"github.com/openshift/ci-tools/pkg/steps"
	"github.com/openshift/ci-tools/pkg/steps/clusterinstall"
	"github.com/openshift/ci-tools/pkg/steps/release"
	"github.com/openshift/ci-tools/pkg/steps/utils"
)

// FromConfig interprets the human-friendly fields in
// the release build configuration and generates steps for
// them, returning the full set of steps requires for the
// build, including defaulted steps, generated steps and
// all raw steps that the user provided.
func FromConfig(
	config *api.ReleaseBuildConfiguration,
	jobSpec *api.JobSpec,
	templates []*templateapi.Template,
	paramFile, artifactDir string,
	promote bool,
	clusterConfig *rest.Config,
	leaseClient *lease.Client,
	requiredTargets []string,
	cloneAuthConfig *steps.CloneAuthConfig,
	pullSecret, pushSecret *coreapi.Secret,
	imageCreatorKubeconfig *rest.Config,

) ([]api.Step, []api.Step, error) {
	if err := addSchemes(); err != nil {
		return nil, nil, fmt.Errorf("failed to add schemes: %w", err)
	}
	var buildSteps []api.Step
	var postSteps []api.Step

	requiredNames := sets.NewString()
	for _, target := range requiredTargets {
		requiredNames.Insert(target)
	}

	var buildClient steps.BuildClient
	var routeGetter routeclientset.RoutesGetter
	var deploymentGetter appsclientset.DeploymentsGetter
	var templateClient steps.TemplateClient
	var configMapGetter coreclientset.ConfigMapsGetter
	var serviceGetter coreclientset.ServicesGetter
	var secretGetter coreclientset.SecretsGetter
	var podClient steps.PodClient
	var rbacClient rbacclientset.RbacV1Interface
	var saGetter coreclientset.ServiceAccountsGetter
	var namespaceClient coreclientset.NamespaceInterface
	var eventClient coreclientset.EventsGetter
	var client ctrlruntimeclient.Client

	if clusterConfig != nil {
		var err error
		client, err = ctrlruntimeclient.New(clusterConfig, ctrlruntimeclient.Options{})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to construct client: %w", err)
		}
		buildGetter, err := buildclientset.NewForConfig(clusterConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("could not get build client for cluster config: %w", err)
		}
		buildClient = steps.NewBuildClient(buildGetter, buildGetter.RESTClient())

		routeGetter, err = routeclientset.NewForConfig(clusterConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("could not get route client for cluster config: %w", err)
		}

		templateGetter, err := templateclientset.NewForConfig(clusterConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("could not get template client for cluster config: %w", err)
		}
		templateClient = steps.NewTemplateClient(templateGetter, templateGetter.RESTClient())

		appsGetter, err := appsclientset.NewForConfig(clusterConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("could not get apps client for cluster config: %w", err)
		}
		deploymentGetter = appsGetter

		coreGetter, err := coreclientset.NewForConfig(clusterConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("could not get core client for cluster config: %w", err)
		}
		serviceGetter = coreGetter
		configMapGetter = coreGetter
		secretGetter = coreGetter
		namespaceClient = coreGetter.Namespaces()
		eventClient = coreGetter

		podClient = steps.NewPodClient(coreGetter, clusterConfig, coreGetter.RESTClient())

		rbacGetter, err := rbacclientset.NewForConfig(clusterConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("could not get RBAC client for cluster config: %w", err)
		}
		rbacClient = rbacGetter
		saGetter = coreGetter
	}

	var imageCreatorClient ctrlruntimeclient.Client
	if imageCreatorKubeconfig != nil {
		var err error
		imageCreatorClient, err = ctrlruntimeclient.New(imageCreatorKubeconfig, ctrlruntimeclient.Options{})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to construct image-creator client: %w", err)
		}
	}

	params := api.NewDeferredParameters()
	params.Add("JOB_NAME", func() (string, error) { return jobSpec.Job, nil })
	params.Add("JOB_NAME_HASH", func() (string, error) { return jobSpec.JobNameHash(), nil })
	params.Add("JOB_NAME_SAFE", func() (string, error) { return strings.Replace(jobSpec.Job, "_", "-", -1), nil })
	params.Add("NAMESPACE", func() (string, error) { return jobSpec.Namespace(), nil })

	var imageStepLinks []api.StepLink
	var hasReleaseStep bool
	rawSteps, err := stepConfigsForBuild(config, jobSpec, ioutil.ReadFile)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get stepConfigsForBuild: %w", err)
	}
	for _, rawStep := range rawSteps {
		var step api.Step
		var isReleaseStep bool
		var additional []api.Step
		var stepLinks []api.StepLink
		if rawStep.InputImageTagStepConfiguration != nil {
			step = steps.InputImageTagStep(*rawStep.InputImageTagStepConfiguration, client, jobSpec)
		} else if rawStep.PipelineImageCacheStepConfiguration != nil {
			step = steps.PipelineImageCacheStep(*rawStep.PipelineImageCacheStepConfiguration, config.Resources, buildClient, client, artifactDir, jobSpec, pullSecret)
		} else if rawStep.SourceStepConfiguration != nil {
			step = steps.SourceStep(*rawStep.SourceStepConfiguration, config.Resources, buildClient, client, artifactDir, jobSpec, cloneAuthConfig, pullSecret)
		} else if rawStep.BundleSourceStepConfiguration != nil {
			step = steps.BundleSourceStep(*rawStep.BundleSourceStepConfiguration, config, config.Resources, buildClient, client, artifactDir, jobSpec, pullSecret)
		} else if rawStep.IndexGeneratorStepConfiguration != nil {
			step = steps.IndexGeneratorStep(*rawStep.IndexGeneratorStepConfiguration, config, config.Resources, buildClient, client, artifactDir, jobSpec, pullSecret)
		} else if rawStep.ProjectDirectoryImageBuildStepConfiguration != nil {
			step = steps.ProjectDirectoryImageBuildStep(*rawStep.ProjectDirectoryImageBuildStepConfiguration, config.Resources, buildClient, client, artifactDir, jobSpec, pullSecret)
		} else if rawStep.ProjectDirectoryImageBuildInputs != nil {
			step = steps.GitSourceStep(*rawStep.ProjectDirectoryImageBuildInputs, config.Resources, buildClient, artifactDir, jobSpec, cloneAuthConfig, pullSecret)
		} else if rawStep.RPMImageInjectionStepConfiguration != nil {
			step = steps.RPMImageInjectionStep(*rawStep.RPMImageInjectionStepConfiguration, config.Resources, buildClient, routeGetter, artifactDir, jobSpec, pullSecret)
		} else if rawStep.RPMServeStepConfiguration != nil {
			step = steps.RPMServerStep(*rawStep.RPMServeStepConfiguration, deploymentGetter, routeGetter, serviceGetter, client, jobSpec)
		} else if rawStep.OutputImageTagStepConfiguration != nil {
			step = steps.OutputImageTagStep(*rawStep.OutputImageTagStepConfiguration, client, jobSpec)
			// all required or non-optional output images are considered part of [images]
			if requiredNames.Has(string(rawStep.OutputImageTagStepConfiguration.From)) || !rawStep.OutputImageTagStepConfiguration.Optional {
				stepLinks = append(stepLinks, step.Creates()...)
			}
		} else if rawStep.ReleaseImagesTagStepConfiguration != nil {
			// if the user has specified a tag_specification we always
			// will import those images to the stable stream
			step = release.ReleaseImagesTagStep(*rawStep.ReleaseImagesTagStepConfiguration, client, routeGetter, configMapGetter, params, jobSpec)
			stepLinks = append(stepLinks, step.Creates()...)

			hasReleaseStep = true

			// However, this user may have specified $RELEASE_IMAGE_foo
			// as well. For backwards compatibility, we explicitly support
			// 'initial' and 'latest': if not provided, we will build them.
			// If a pull spec was provided, however, it will be used.
			for _, name := range []string{api.InitialReleaseName, api.LatestReleaseName} {
				var releaseStep api.Step
				envVar := utils.ReleaseImageEnv(name)
				if params.HasInput(envVar) {
					pullSpec, err := params.Get(envVar)
					if err != nil {
						return nil, nil, results.ForReason("reading_release").ForError(fmt.Errorf("failed to read input release pullSpec %s: %w", name, err))
					}
					log.Printf("Resolved release %s to %s", name, pullSpec)
					releaseStep = release.ImportReleaseStep(name, pullSpec, true, config.Resources, podClient, eventClient, client, artifactDir, jobSpec, pullSecret)
				} else {
					releaseStep = release.AssembleReleaseStep(name, rawStep.ReleaseImagesTagStepConfiguration, config.Resources, podClient, eventClient, client, artifactDir, jobSpec)
				}
				buildSteps = append(buildSteps, releaseStep)
			}
		} else if rawStep.ResolvedReleaseImagesStepConfiguration != nil {
			// this is a disgusting hack but the simplest implementation until we
			// factor release steps into something more reusable
			hasReleaseStep = true
			// we need to expose the release step as 'step' so that it's in the
			// graph and can be targeted with '--target', but we can't let it get
			// removed via env-var, since release steps are apparently not subject
			// to that mechanism ...
			isReleaseStep = true

			var value string
			resolveConfig := rawStep.ResolvedReleaseImagesStepConfiguration
			envVar := utils.ReleaseImageEnv(resolveConfig.Name)
			if current := os.Getenv(envVar); current != "" {
				value = current
				log.Printf("Using explicitly provided pull-spec for release %s (%s)", resolveConfig.Name, value)
			} else {
				var err error
				switch {
				case resolveConfig.Candidate != nil:
					value, err = candidate.ResolvePullSpec(*resolveConfig.Candidate)
				case resolveConfig.Release != nil:
					value, err = official.ResolvePullSpec(*resolveConfig.Release)
				case resolveConfig.Prerelease != nil:
					value, err = prerelease.ResolvePullSpec(*resolveConfig.Prerelease)
				}
				if err != nil {
					return nil, nil, results.ForReason("resolving_release").ForError(fmt.Errorf("failed to resolve release %s: %w", resolveConfig.Name, err))
				}
				log.Printf("Resolved release %s to %s", resolveConfig.Name, value)
			}

			step = release.ImportReleaseStep(resolveConfig.Name, value, false, config.Resources, podClient, eventClient, client, artifactDir, jobSpec, pullSecret)
		} else if testStep := rawStep.TestStepConfiguration; testStep != nil {
			if test := testStep.MultiStageTestConfigurationLiteral; test != nil {
				step = steps.MultiStageTestStep(*testStep, config, params, podClient, eventClient, secretGetter, saGetter, rbacClient, client, artifactDir, jobSpec)
				if test.ClusterProfile != "" {
					leases := []api.StepLease{{
						ResourceType: test.ClusterProfile.LeaseType(),
						Env:          steps.DefaultLeaseEnv,
					}}
					step = steps.LeaseStep(leaseClient, leases, step, jobSpec.Namespace, namespaceClient)
				}
				for _, subStep := range append(append(test.Pre, test.Test...), test.Post...) {
					if link, ok := subStep.FromImageTag(); ok {
						config := api.InputImageTagStepConfiguration{
							BaseImage: *subStep.FromImage,
							To:        link,
						}
						additional = append(additional, steps.InputImageTagStep(config, client, jobSpec))
					}
				}
			} else if test := testStep.OpenshiftInstallerClusterTestConfiguration; test != nil {
				if testStep.OpenshiftInstallerClusterTestConfiguration.Upgrade {
					var err error
					step, err = clusterinstall.E2ETestStep(*testStep.OpenshiftInstallerClusterTestConfiguration, *testStep, params, podClient, eventClient, templateClient, secretGetter, artifactDir, jobSpec, config.Resources)
					if err != nil {
						return nil, nil, fmt.Errorf("unable to create end to end test step: %w", err)
					}
					leases := []api.StepLease{{
						ResourceType: test.ClusterProfile.LeaseType(),
						Env:          steps.DefaultLeaseEnv,
					}}
					step = steps.LeaseStep(leaseClient, leases, step, jobSpec.Namespace, namespaceClient)
				}
			} else {
				step = steps.TestStep(*testStep, config.Resources, podClient, eventClient, artifactDir, jobSpec)
			}
		}
		if !isReleaseStep {
			step, ok := checkForFullyQualifiedStep(step, params)
			if ok {
				log.Printf("Task %s is satisfied by environment variables and will be skipped", step.Name())
			} else {
				imageStepLinks = append(imageStepLinks, stepLinks...)
			}
		}
		buildSteps = append(buildSteps, step)
		buildSteps = append(buildSteps, additional...)
	}

	for _, template := range templates {
		step := steps.TemplateExecutionStep(template, params, podClient, eventClient, templateClient, artifactDir, jobSpec, config.Resources)
		var hasClusterType, hasUseLease bool
		for _, p := range template.Parameters {
			hasClusterType = hasClusterType || p.Name == "CLUSTER_TYPE"
			hasUseLease = hasUseLease || p.Name == "USE_LEASE_CLIENT"
			if hasClusterType && hasUseLease {
				clusterType, err := params.Get("CLUSTER_TYPE")
				if err != nil {
					return nil, nil, fmt.Errorf("failed to get \"CLUSTER_TYPE\" parameter: %w", err)
				}
				lease, err := api.LeaseTypeFromClusterType(clusterType)
				if err != nil {
					return nil, nil, fmt.Errorf("cannot resolve lease type from cluster type: %w", err)
				}
				leases := []api.StepLease{{
					ResourceType: lease,
					Env:          steps.DefaultLeaseEnv,
				}}
				step = steps.LeaseStep(leaseClient, leases, step, jobSpec.Namespace, namespaceClient)
				break
			}
		}
		buildSteps = append(buildSteps, step)
	}

	if len(paramFile) > 0 {
		buildSteps = append(buildSteps, steps.WriteParametersStep(params, paramFile))
	}

	if !hasReleaseStep {
		buildSteps = append(buildSteps, release.StableImagesTagStep(client, jobSpec))
	}

	buildSteps = append(buildSteps, steps.ImagesReadyStep(imageStepLinks))

	for _, step := range buildSteps {
		addProvidesForStep(step, params)
	}

	if promote {
		cfg, err := promotionDefaults(config)
		if err != nil {
			return nil, nil, fmt.Errorf("could not determine promotion defaults: %w", err)
		}
		postSteps = append(postSteps, release.PromotionStep(*cfg, config.Images, requiredNames, client, client, jobSpec, podClient, eventClient, pushSecret, imageCreatorClient))
	}

	return buildSteps, postSteps, nil
}

// addProvidesForStep adds any required parameters to the deferred parameters map.
// Use this when a step may still need to run even if all parameters are provided
// by the caller as environment variables.
func addProvidesForStep(step api.Step, params *api.DeferredParameters) {
	for name, fn := range step.Provides() {
		params.Add(name, fn)
	}
}

// checkForFullyQualifiedStep if all output parameters of this step are part of the
// environment, replace the step with a shim that automatically provides those variables.
// Returns true if the step was replaced.
func checkForFullyQualifiedStep(step api.Step, params *api.DeferredParameters) (api.Step, bool) {
	provides := step.Provides()

	if values, ok := paramsHasAllParametersAsInput(params, provides); ok {
		step = steps.NewInputEnvironmentStep(step.Name(), values, step.Creates())
		for k, v := range values {
			params.Set(k, v)
		}
		return step, true
	}
	for name, fn := range provides {
		params.Add(name, fn)
	}
	return step, false
}

func promotionDefaults(configSpec *api.ReleaseBuildConfiguration) (*api.PromotionConfiguration, error) {
	config := configSpec.PromotionConfiguration
	if config == nil {
		return nil, fmt.Errorf("cannot promote images, no promotion or release tag configuration defined")
	}
	return config, nil
}

type readFile func(string) ([]byte, error)

func stepConfigsForBuild(config *api.ReleaseBuildConfiguration, jobSpec *api.JobSpec, readFile readFile) ([]api.StepConfiguration, error) {
	var buildSteps []api.StepConfiguration

	if config.InputConfiguration.BaseImages == nil {
		config.InputConfiguration.BaseImages = make(map[string]api.ImageStreamTagReference)
	}
	if config.InputConfiguration.BaseRPMImages == nil {
		config.InputConfiguration.BaseRPMImages = make(map[string]api.ImageStreamTagReference)
	}

	// ensure the "As" field is set to the provided alias.
	for alias, target := range config.InputConfiguration.BaseImages {
		target.As = alias
		config.InputConfiguration.BaseImages[alias] = target
	}
	for alias, target := range config.InputConfiguration.BaseRPMImages {
		target.As = alias
		config.InputConfiguration.BaseRPMImages[alias] = target
	}

	if target := config.InputConfiguration.BuildRootImage; target != nil {
		if target.FromRepository {
			istTagRef, err := buildRootImageStreamFromRepository(readFile)
			if err != nil {
				return nil, fmt.Errorf("failed to read buildRootImageStream from repository: %w", err)
			}
			target.ImageStreamTagReference = istTagRef
		}
		if isTagRef := target.ImageStreamTagReference; isTagRef != nil {
			buildSteps = append(buildSteps, createStepConfigForTagRefImage(*isTagRef, jobSpec))
		} else if gitSourceRef := target.ProjectImageBuild; gitSourceRef != nil {
			buildSteps = append(buildSteps, createStepConfigForGitSource(*gitSourceRef))
		}
	}

	if jobSpec.Refs != nil || len(jobSpec.ExtraRefs) > 0 {
		step := api.StepConfiguration{SourceStepConfiguration: &api.SourceStepConfiguration{
			From: api.PipelineImageStreamTagReferenceRoot,
			To:   api.PipelineImageStreamTagReferenceSource,
			ClonerefsImage: api.ImageStreamTagReference{
				Namespace: "ci",
				Name:      "clonerefs",
				Tag:       "latest",
			},
			ClonerefsPath: "/app/prow/cmd/clonerefs/app.binary.runfiles/io_k8s_test_infra/prow/cmd/clonerefs/linux_amd64_pure_stripped/app.binary",
		}}
		buildSteps = append(buildSteps, step)
	}

	if len(config.BinaryBuildCommands) > 0 {
		buildSteps = append(buildSteps, api.StepConfiguration{PipelineImageCacheStepConfiguration: &api.PipelineImageCacheStepConfiguration{
			From:     api.PipelineImageStreamTagReferenceSource,
			To:       api.PipelineImageStreamTagReferenceBinaries,
			Commands: config.BinaryBuildCommands,
		}})
	}

	if len(config.TestBinaryBuildCommands) > 0 {
		buildSteps = append(buildSteps, api.StepConfiguration{PipelineImageCacheStepConfiguration: &api.PipelineImageCacheStepConfiguration{
			From:     api.PipelineImageStreamTagReferenceSource,
			To:       api.PipelineImageStreamTagReferenceTestBinaries,
			Commands: config.TestBinaryBuildCommands,
		}})
	}

	if len(config.RpmBuildCommands) > 0 {
		var from api.PipelineImageStreamTagReference
		if len(config.BinaryBuildCommands) > 0 {
			from = api.PipelineImageStreamTagReferenceBinaries
		} else {
			from = api.PipelineImageStreamTagReferenceSource
		}

		var out string
		if config.RpmBuildLocation != "" {
			out = config.RpmBuildLocation
		} else {
			out = api.DefaultRPMLocation
		}

		buildSteps = append(buildSteps, api.StepConfiguration{PipelineImageCacheStepConfiguration: &api.PipelineImageCacheStepConfiguration{
			From:     from,
			To:       api.PipelineImageStreamTagReferenceRPMs,
			Commands: fmt.Sprintf(`%s; ln -s $( pwd )/%s %s`, config.RpmBuildCommands, out, api.RPMServeLocation),
		}})

		buildSteps = append(buildSteps, api.StepConfiguration{RPMServeStepConfiguration: &api.RPMServeStepConfiguration{
			From: api.PipelineImageStreamTagReferenceRPMs,
		}})
	}

	for alias, baseImage := range config.BaseImages {
		buildSteps = append(buildSteps, api.StepConfiguration{InputImageTagStepConfiguration: &api.InputImageTagStepConfiguration{
			BaseImage: defaultImageFromReleaseTag(baseImage, config.ReleaseTagConfiguration),
			To:        api.PipelineImageStreamTagReference(alias),
		}})
	}

	for alias, target := range config.InputConfiguration.BaseRPMImages {
		intermediateTag := api.PipelineImageStreamTagReference(fmt.Sprintf("%s-without-rpms", alias))
		buildSteps = append(buildSteps, api.StepConfiguration{InputImageTagStepConfiguration: &api.InputImageTagStepConfiguration{
			BaseImage: defaultImageFromReleaseTag(target, config.ReleaseTagConfiguration),
			To:        intermediateTag,
		}})

		buildSteps = append(buildSteps, api.StepConfiguration{RPMImageInjectionStepConfiguration: &api.RPMImageInjectionStepConfiguration{
			From: intermediateTag,
			To:   api.PipelineImageStreamTagReference(alias),
		}})
	}

	for i := range config.Images {
		image := &config.Images[i]
		buildSteps = append(buildSteps, api.StepConfiguration{ProjectDirectoryImageBuildStepConfiguration: image})
		var outputImageStreamName string
		if config.ReleaseTagConfiguration != nil {
			outputImageStreamName = fmt.Sprintf("%s%s", config.ReleaseTagConfiguration.NamePrefix, api.StableImageStream)
		} else {
			outputImageStreamName = api.StableImageStream
		}
		buildSteps = append(buildSteps, api.StepConfiguration{OutputImageTagStepConfiguration: &api.OutputImageTagStepConfiguration{
			From: image.To,
			To: api.ImageStreamTagReference{
				Name: outputImageStreamName,
				Tag:  string(image.To),
			},
			Optional: image.Optional,
		}})
	}

	if config.Operator != nil {
		// Build a bundle source image that substitutes all values in `substitutions` in all `manifests` directories
		buildSteps = append(buildSteps, api.StepConfiguration{BundleSourceStepConfiguration: &api.BundleSourceStepConfiguration{
			Substitutions: config.Operator.Substitutions,
		}})
		// Build bundles
		var bundles []string
		for index, bundle := range config.Operator.Bundles {
			bundleName := api.BundleName(index)
			bundles = append(bundles, bundleName)
			image := &api.ProjectDirectoryImageBuildStepConfiguration{
				To: api.PipelineImageStreamTagReference(bundleName),
				ProjectDirectoryImageBuildInputs: api.ProjectDirectoryImageBuildInputs{
					ContextDir:     bundle.ContextDir,
					DockerfilePath: bundle.DockerfilePath,
				},
			}
			buildSteps = append(buildSteps, api.StepConfiguration{ProjectDirectoryImageBuildStepConfiguration: image})
		}
		// Build index generator
		buildSteps = append(buildSteps, api.StepConfiguration{IndexGeneratorStepConfiguration: &api.IndexGeneratorStepConfiguration{
			To:            api.PipelineImageStreamTagReferenceIndexImageGenerator,
			OperatorIndex: bundles,
		}})
		// Build the index
		image := &api.ProjectDirectoryImageBuildStepConfiguration{
			To: api.PipelineImageStreamTagReferenceIndexImage,
			ProjectDirectoryImageBuildInputs: api.ProjectDirectoryImageBuildInputs{
				DockerfilePath: steps.IndexDockerfileName,
			},
		}
		buildSteps = append(buildSteps, api.StepConfiguration{ProjectDirectoryImageBuildStepConfiguration: image})
	}

	for i := range config.Tests {
		test := &config.Tests[i]
		if test.ContainerTestConfiguration != nil || test.MultiStageTestConfigurationLiteral != nil || (test.OpenshiftInstallerClusterTestConfiguration != nil && test.OpenshiftInstallerClusterTestConfiguration.Upgrade) {
			if test.Secret != nil {
				test.Secrets = append(test.Secrets, test.Secret)
			}
			buildSteps = append(buildSteps, api.StepConfiguration{TestStepConfiguration: test})
		}
	}

	if config.ReleaseTagConfiguration != nil {
		buildSteps = append(buildSteps, api.StepConfiguration{ReleaseImagesTagStepConfiguration: config.ReleaseTagConfiguration})
	}
	for name := range config.Releases {
		buildSteps = append(buildSteps, api.StepConfiguration{ResolvedReleaseImagesStepConfiguration: &api.ReleaseConfiguration{
			Name:              name,
			UnresolvedRelease: config.Releases[name],
		}})
	}

	buildSteps = append(buildSteps, config.RawSteps...)

	return buildSteps, nil
}

func createStepConfigForTagRefImage(target api.ImageStreamTagReference, jobSpec *api.JobSpec) api.StepConfiguration {
	if target.Namespace == "" {
		target.Namespace = jobSpec.BaseNamespace
	}
	if target.Name == "" {
		if jobSpec.Refs != nil {
			target.Name = fmt.Sprintf("%s-test-base", jobSpec.Refs.Repo)
		} else {
			target.Name = "test-base"
		}
	}

	return api.StepConfiguration{
		InputImageTagStepConfiguration: &api.InputImageTagStepConfiguration{
			BaseImage: target,
			To:        api.PipelineImageStreamTagReferenceRoot,
		}}
}

func createStepConfigForGitSource(target api.ProjectDirectoryImageBuildInputs) api.StepConfiguration {
	return api.StepConfiguration{
		ProjectDirectoryImageBuildInputs: &api.ProjectDirectoryImageBuildInputs{
			DockerfilePath: target.DockerfilePath,
			ContextDir:     target.ContextDir,
		},
	}
}

func paramsHasAllParametersAsInput(p api.Parameters, params map[string]func() (string, error)) (map[string]string, bool) {
	if len(params) == 0 {
		return nil, false
	}
	var values map[string]string
	for k := range params {
		if !p.HasInput(k) {
			return nil, false
		}
		if values == nil {
			values = make(map[string]string)
		}
		v, err := p.Get(k)
		if err != nil {
			return nil, false
		}
		values[k] = v
	}
	return values, true
}

func defaultImageFromReleaseTag(base api.ImageStreamTagReference, release *api.ReleaseTagConfiguration) api.ImageStreamTagReference {
	if release == nil {
		return base
	}
	if len(base.Tag) == 0 || len(base.Name) > 0 || len(base.Namespace) > 0 {
		return base
	}
	base.Name = release.Name
	base.Namespace = release.Namespace
	return base
}

func buildRootImageStreamFromRepository(readFile readFile) (*api.ImageStreamTagReference, error) {
	data, err := readFile(api.CIOperatorInrepoConfigFileName)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s file: %w", api.CIOperatorInrepoConfigFileName, err)
	}
	config := api.CIOperatorInrepoConfig{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal %s: %w", api.CIOperatorInrepoConfigFileName, err)
	}
	return &config.BuildRootImage, nil
}

func addSchemes() error {
	if err := imagev1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add imagev1 to scheme: %w", err)
	}

	return nil
}
