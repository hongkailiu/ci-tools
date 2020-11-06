package release

import (
	"context"
	"reflect"
	"testing"

	coreapi "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/diff"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	imageapi "github.com/openshift/api/image/v1"

	"github.com/openshift/ci-tools/pkg/api"
)

func TestToPromote(t *testing.T) {
	var testCases = []struct {
		name             string
		config           api.PromotionConfiguration
		images           []api.ProjectDirectoryImageBuildStepConfiguration
		requiredImages   sets.String
		expectedBySource map[string]string
		expectedNames    sets.String
	}{
		{
			name: "disabled config returns nothing",
			config: api.PromotionConfiguration{
				Disabled: true,
			},
			images: []api.ProjectDirectoryImageBuildStepConfiguration{
				{To: api.PipelineImageStreamTagReference("foo")},
				{To: api.PipelineImageStreamTagReference("bar")},
				{To: api.PipelineImageStreamTagReference("baz")},
			},
			requiredImages:   sets.NewString(),
			expectedBySource: map[string]string{},
			expectedNames:    sets.NewString(),
		},
		{
			name: "enabled config returns input list",
			config: api.PromotionConfiguration{
				Disabled: false,
			},
			images: []api.ProjectDirectoryImageBuildStepConfiguration{
				{To: api.PipelineImageStreamTagReference("foo")},
				{To: api.PipelineImageStreamTagReference("bar")},
				{To: api.PipelineImageStreamTagReference("baz")},
			},
			requiredImages:   sets.NewString(),
			expectedBySource: map[string]string{"foo": "foo", "bar": "bar", "baz": "baz"},
			expectedNames:    sets.NewString("foo", "bar", "baz"),
		},
		{
			name: "enabled config with prefix returns prefixed input list",
			config: api.PromotionConfiguration{
				Disabled:   false,
				NamePrefix: "some",
			},
			images: []api.ProjectDirectoryImageBuildStepConfiguration{
				{To: api.PipelineImageStreamTagReference("foo")},
				{To: api.PipelineImageStreamTagReference("bar")},
				{To: api.PipelineImageStreamTagReference("baz")},
			},
			requiredImages:   sets.NewString(),
			expectedBySource: map[string]string{"somefoo": "foo", "somebar": "bar", "somebaz": "baz"},
			expectedNames:    sets.NewString("somefoo", "somebar", "somebaz"),
		},
		{
			name: "enabled config with exclude returns filtered input list",
			config: api.PromotionConfiguration{
				ExcludedImages: []string{"foo"},
				Disabled:       false,
			},
			images: []api.ProjectDirectoryImageBuildStepConfiguration{
				{To: api.PipelineImageStreamTagReference("foo")},
				{To: api.PipelineImageStreamTagReference("bar")},
				{To: api.PipelineImageStreamTagReference("baz")},
			},
			requiredImages:   sets.NewString(),
			expectedBySource: map[string]string{"bar": "bar", "baz": "baz"},
			expectedNames:    sets.NewString("bar", "baz"),
		},
		{
			name: "enabled config with optional image returns subset of input list",
			config: api.PromotionConfiguration{
				Disabled: false,
			},
			images: []api.ProjectDirectoryImageBuildStepConfiguration{
				{To: api.PipelineImageStreamTagReference("foo")},
				{To: api.PipelineImageStreamTagReference("bar"), Optional: true},
				{To: api.PipelineImageStreamTagReference("baz")},
			},
			requiredImages:   sets.NewString(),
			expectedBySource: map[string]string{"foo": "foo", "baz": "baz"},
			expectedNames:    sets.NewString("foo", "baz"),
		},
		{
			name: "enabled config with optional but required image returns full input list",
			config: api.PromotionConfiguration{
				Disabled: false,
			},
			images: []api.ProjectDirectoryImageBuildStepConfiguration{
				{To: api.PipelineImageStreamTagReference("foo")},
				{To: api.PipelineImageStreamTagReference("bar"), Optional: true},
				{To: api.PipelineImageStreamTagReference("baz")},
			},
			requiredImages:   sets.NewString("bar"),
			expectedBySource: map[string]string{"foo": "foo", "bar": "bar", "baz": "baz"},
			expectedNames:    sets.NewString("foo", "bar", "baz"),
		},
		{
			name: "enabled config with additional images returns appended input list",
			config: api.PromotionConfiguration{
				AdditionalImages: map[string]string{"boo": "ah"},
				Disabled:         false,
			},
			images: []api.ProjectDirectoryImageBuildStepConfiguration{
				{To: api.PipelineImageStreamTagReference("foo")},
				{To: api.PipelineImageStreamTagReference("bar")},
				{To: api.PipelineImageStreamTagReference("baz")},
			},
			requiredImages:   sets.NewString(),
			expectedBySource: map[string]string{"foo": "foo", "bar": "bar", "baz": "baz", "boo": "ah"},
			expectedNames:    sets.NewString("foo", "bar", "baz", "boo"),
		},
		{
			name: "enabled config with excludes and additional images returns filtered, appended input list",
			config: api.PromotionConfiguration{
				ExcludedImages:   []string{"foo"},
				AdditionalImages: map[string]string{"boo": "ah"},
				Disabled:         false,
			},
			images: []api.ProjectDirectoryImageBuildStepConfiguration{
				{To: api.PipelineImageStreamTagReference("foo")},
				{To: api.PipelineImageStreamTagReference("bar")},
				{To: api.PipelineImageStreamTagReference("baz")},
			},
			requiredImages:   sets.NewString(),
			expectedBySource: map[string]string{"bar": "bar", "baz": "baz", "boo": "ah"},
			expectedNames:    sets.NewString("bar", "baz", "boo"),
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			bySource, names := toPromote(test.config, test.images, test.requiredImages)
			if actual, expected := bySource, test.expectedBySource; !reflect.DeepEqual(actual, expected) {
				t.Errorf("%s: got incorrect tags by source: %s", test.name, diff.ObjectDiff(actual, expected))
			}

			if actual, expected := names, test.expectedNames; !reflect.DeepEqual(actual, expected) {
				t.Errorf("%s: got incorrect names: %s", test.name, diff.ObjectDiff(actual, expected))
			}
		})
	}
}

func TestPromotedTags(t *testing.T) {
	var testCases = []struct {
		name     string
		input    *api.ReleaseBuildConfiguration
		expected []api.ImageStreamTagReference
	}{
		{
			name:     "no promotion, no output",
			input:    &api.ReleaseBuildConfiguration{},
			expected: nil,
		},
		{
			name: "promoted image means output tags",
			input: &api.ReleaseBuildConfiguration{
				Images: []api.ProjectDirectoryImageBuildStepConfiguration{
					{To: api.PipelineImageStreamTagReference("foo")},
				},
				PromotionConfiguration: &api.PromotionConfiguration{
					Namespace: "roger",
					Name:      "fred",
				},
			},
			expected: []api.ImageStreamTagReference{{
				Namespace: "roger",
				Name:      "fred",
				Tag:       "foo",
			}},
		},
		{
			name: "promoted image by tag means output tags",
			input: &api.ReleaseBuildConfiguration{
				Images: []api.ProjectDirectoryImageBuildStepConfiguration{
					{To: api.PipelineImageStreamTagReference("foo")},
				},
				PromotionConfiguration: &api.PromotionConfiguration{
					Namespace: "roger",
					Tag:       "fred",
				},
			},
			expected: []api.ImageStreamTagReference{{
				Namespace: "roger",
				Name:      "foo",
				Tag:       "fred",
			}},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if actual, expected := PromotedTags(testCase.input), testCase.expected; !reflect.DeepEqual(actual, expected) {
				t.Errorf("%s: got incorrect promoted tags: %v", testCase.name, diff.ObjectDiff(actual, expected))
			}
		})
	}
}

func TestGetPromotionPod(t *testing.T) {
	var testCases = []struct {
		name        string
		imageMirror map[string]string
		namespace   string
		expected    *coreapi.Pod
	}{
		{
			name: "basic case",
			imageMirror: map[string]string{
				"docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:afd71aa3cbbf7d2e00cd8696747b2abf164700147723c657919c20b13d13ec62": "registy.ci.openshift.org/ci/applyconfig:latest",
				"docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:bbb":                                                              "registy.ci.openshift.org/ci/bin:latest",
			},
			namespace: "ci-op-zyvwvffx",
			expected: &coreapi.Pod{
				ObjectMeta: meta.ObjectMeta{
					Name:      "promotion",
					Namespace: "ci-op-zyvwvffx",
				},
				Spec: coreapi.PodSpec{
					RestartPolicy: coreapi.RestartPolicyNever,
					Containers: []coreapi.Container{
						{
							Name: "promotion",
							// TODO use local image image-registry.openshift-image-registry.svc:5000/ocp/4.6:cli after migrating promotion jobs to OCP4 clusters
							Image:   "registry.svc.ci.openshift.org/ocp/4.6:cli",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{"oc image mirror --registry-config=/etc/push-secret/.dockerconfigjson docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:afd71aa3cbbf7d2e00cd8696747b2abf164700147723c657919c20b13d13ec62 registy.ci.openshift.org/ci/applyconfig:latest && oc image mirror --registry-config=/etc/push-secret/.dockerconfigjson docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:bbb registy.ci.openshift.org/ci/bin:latest"},
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
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if actual, expected := getPromotionPod(testCase.imageMirror, testCase.namespace), testCase.expected; !reflect.DeepEqual(actual, expected) {
				t.Errorf("%s: got incorrect promotion pod: %v", testCase.name, diff.ObjectDiff(actual, expected))
			}
		})
	}
}

func TestGetImageMirrorTarget(t *testing.T) {
	var testCases = []struct {
		name     string
		config   api.PromotionConfiguration
		tags     map[string]string
		pipeline *imageapi.ImageStream
		expected map[string]string
	}{
		{
			name: "empty input",
		},
		{
			name:     "nil tags",
			pipeline: &imageapi.ImageStream{},
		},
		{
			name: "basic case: empty config.Name",
			config: api.PromotionConfiguration{
				Namespace: "ci",
				Tag:       "latest",
			},
			tags: map[string]string{
				"a": "b",
				"c": "d",
				"x": "y",
			},
			pipeline: &imageapi.ImageStream{
				Status: imageapi.ImageStreamStatus{
					Tags: []imageapi.NamedTagEventList{
						{
							Tag: "b",
							Items: []imageapi.TagEvent{
								{
									DockerImageReference: "docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:bbb",
								},
							},
						},
						{
							Tag: "d",
							Items: []imageapi.TagEvent{
								{
									DockerImageReference: "docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:ddd",
								},
							},
						},
					},
				},
			},
			expected: map[string]string{"docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:bbb": "registry.svc.ci.openshift.org/ci/a:latest", "docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:ddd": "registry.svc.ci.openshift.org/ci/c:latest"},
		},
		{
			name: "basic case: config.Name",
			config: api.PromotionConfiguration{
				Namespace: "ci",
				Name:      "name",
				Tag:       "latest",
			},
			tags: map[string]string{
				"a": "b",
				"c": "d",
				"x": "y",
			},
			pipeline: &imageapi.ImageStream{
				Status: imageapi.ImageStreamStatus{
					Tags: []imageapi.NamedTagEventList{
						{
							Tag: "b",
							Items: []imageapi.TagEvent{
								{
									DockerImageReference: "docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:bbb",
								},
							},
						},
						{
							Tag: "d",
							Items: []imageapi.TagEvent{
								{
									DockerImageReference: "docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:ddd",
								},
							},
						},
					},
				},
			},
			expected: map[string]string{"docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:bbb": "registry.svc.ci.openshift.org/ci/name:a", "docker-registry.default.svc:5000/ci-op-y2n8rsh3/pipeline@sha256:ddd": "registry.svc.ci.openshift.org/ci/name:c"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if actual, expected := getImageMirrorTarget(context.TODO(), fakeclient.NewFakeClientWithScheme(scheme.Scheme), testCase.config, testCase.tags, testCase.pipeline), testCase.expected; !reflect.DeepEqual(actual, expected) {
				t.Errorf("%s: got incorrect ImageMirror mapping: %v", testCase.name, diff.ObjectDiff(actual, expected))
			}
		})
	}
}

func TestGetPublicImageReference(t *testing.T) {
	var testCases = []struct {
		name                        string
		dockerImageReference        string
		publicDockerImageRepository string
		expected                    string
	}{
		{
			name:                        "basic case",
			dockerImageReference:        "docker-registry.default.svc:5000/ci-op-bgqwwknr/pipeline@sha256:d8385fb539f471d4f41da131366b559bb90eeeeca2edd265e10d7c2aa052a1af",
			publicDockerImageRepository: "registry.svc.ci.openshift.org/ci-op-bgqwwknr/pipeline",
			expected:                    "registry.svc.ci.openshift.org/ci-op-bgqwwknr/pipeline@sha256:d8385fb539f471d4f41da131366b559bb90eeeeca2edd265e10d7c2aa052a1af",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if actual, expected := getPublicImageReference(testCase.dockerImageReference, testCase.publicDockerImageRepository), testCase.expected; !reflect.DeepEqual(actual, expected) {
				t.Errorf("%s: got incorrect public image reference: %v", testCase.name, diff.ObjectDiff(actual, expected))
			}
		})
	}
}
