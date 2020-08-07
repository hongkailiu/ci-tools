package rehearse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/getlantern/deepcopy"
	"github.com/ghodss/yaml"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"k8s.io/apimachinery/pkg/types"

	v1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/util/wait"
	pjapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	prowconfig "k8s.io/test-infra/prow/config"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift/ci-tools/pkg/api"
	apihelper "github.com/openshift/ci-tools/pkg/api/helper"
	"github.com/openshift/ci-tools/pkg/config"
	"github.com/openshift/ci-tools/pkg/jobconfig"
	"github.com/openshift/ci-tools/pkg/load"
	"github.com/openshift/ci-tools/pkg/registry"
	"github.com/openshift/ci-tools/pkg/testhelper"
)

const testingRegistry = "../../test/multistage-registry/registry"

const testingCiOpCfgJob1YAML = `tests:
- as: job1
  literal_steps:
    cluster_profile: ""
    pre:
    - from_image:
        name: willem
        namespace: fancy
        tag: first
      resources: {}
zz_generated_metadata:
  branch: ""
  org: ""
  repo: ""
`
const testingCiOpCfgJob2YAML = "tests:\n- as: job2\nzz_generated_metadata:\n  branch: \"\"\n  org: \"\"\n  repo: \"\"\n"

// configFiles contains the info needed to allow inlineCiOpConfig to successfully inline
// CONFIG_SPEC and not fail
func generateTestConfigFiles() config.DataByFilename {
	return config.DataByFilename{
		"targetOrg-targetRepo-master.yaml": config.DataWithInfo{
			Configuration: api.ReleaseBuildConfiguration{
				Tests: []api.TestStepConfiguration{
					{
						As: "job1",
						MultiStageTestConfigurationLiteral: &api.MultiStageTestConfigurationLiteral{
							Pre: []api.LiteralTestStep{{FromImage: &api.ImageStreamTagReference{Namespace: "fancy", Name: "willem", Tag: "first"}}},
						},
					},
					{As: "job2"},
				},
			},
			Info: config.Info{
				Metadata: api.Metadata{
					Org:    "targetOrg",
					Repo:   "targetRepo",
					Branch: "master",
				},
			},
		},
		"targetOrg-targetRepo-not-master.yaml": config.DataWithInfo{
			Configuration: api.ReleaseBuildConfiguration{
				Tests: []api.TestStepConfiguration{
					{As: "job1"},
					{As: "job2"},
				},
			},
			Info: config.Info{
				Metadata: api.Metadata{
					Org:    "targetOrg",
					Repo:   "targetRepo",
					Branch: "not-master",
				},
			},
		}, "anotherOrg-anotherRepo-master.yaml": config.DataWithInfo{
			Configuration: api.ReleaseBuildConfiguration{
				Tests: []api.TestStepConfiguration{
					{As: "job1"},
					{As: "job2"},
				},
			},
			Info: config.Info{
				Metadata: api.Metadata{
					Org:    "anotherOrg",
					Repo:   "anotherRepo",
					Branch: "master",
				},
			},
		},
	}
}

var ignoreUnexported = cmpopts.IgnoreUnexported(prowconfig.Presubmit{}, prowconfig.Brancher{}, prowconfig.RegexpChangeMatcher{})

func makeTestingPresubmitForEnv(env []v1.EnvVar) *prowconfig.Presubmit {
	return &prowconfig.Presubmit{
		JobBase: prowconfig.JobBase{
			Agent:  "kubernetes",
			Name:   "test-job-name",
			Labels: map[string]string{"pj-rehearse.openshift.io/can-be-rehearsed": "true"},
			Spec: &v1.PodSpec{
				Containers: []v1.Container{
					{Env: env},
				},
			},
		},
	}
}

func makeCMReference(cmName, key string) *v1.EnvVarSource {
	return &v1.EnvVarSource{
		ConfigMapKeyRef: &v1.ConfigMapKeySelector{
			LocalObjectReference: v1.LocalObjectReference{
				Name: cmName,
			},
			Key: key,
		},
	}
}

var testCiopConfigInfo = api.Metadata{
	Org:    "targetOrg",
	Repo:   "targetRepo",
	Branch: "master",
}

func TestInlineCiopConfig(t *testing.T) {
	testCiopConfig := api.ReleaseBuildConfiguration{
		Tests: []api.TestStepConfiguration{{
			As: "test1",
			MultiStageTestConfigurationLiteral: &api.MultiStageTestConfigurationLiteral{
				Pre: []api.LiteralTestStep{{FromImage: &api.ImageStreamTagReference{Namespace: "fancy", Name: "willem", Tag: "first"}}},
			},
		}, {
			As: "test2",
		}},
	}
	testCiopConfigTest1 := api.ReleaseBuildConfiguration{Tests: []api.TestStepConfiguration{testCiopConfig.Tests[0]}}
	testCiopConfigContentTest1, err := yaml.Marshal(&testCiopConfigTest1)
	if err != nil {
		t.Fatal("Failed to marshal ci-operator config")
	}
	testCiopConfigTest2 := api.ReleaseBuildConfiguration{
		Tests: []api.TestStepConfiguration{{
			As: "test2",
		}},
	}
	testCiopConfigContentTest2, err := yaml.Marshal(&testCiopConfigTest2)
	if err != nil {
		t.Fatal("Failed to marshal ci-operator config")
	}

	testCases := []struct {
		description              string
		testname                 string
		sourceEnv                []v1.EnvVar
		configs                  config.DataByFilename
		expectedEnv              []v1.EnvVar
		expectedError            bool
		expectedImageStramTagMap apihelper.ImageStreamTagMap
	}{{
		description: "empty env -> no changes",
		configs:     config.DataByFilename{},
	}, {
		description: "no Env.ValueFrom -> no changes",
		sourceEnv:   []v1.EnvVar{{Name: "T", Value: "V"}},
		configs:     config.DataByFilename{},
		expectedEnv: []v1.EnvVar{{Name: "T", Value: "V"}},
	}, {
		description: "no Env.ValueFrom.ConfigMapKeyRef -> no changes",
		sourceEnv:   []v1.EnvVar{{Name: "T", ValueFrom: &v1.EnvVarSource{ResourceFieldRef: &v1.ResourceFieldSelector{}}}},
		configs:     config.DataByFilename{},
		expectedEnv: []v1.EnvVar{{Name: "T", ValueFrom: &v1.EnvVarSource{ResourceFieldRef: &v1.ResourceFieldSelector{}}}},
	}, {
		description: "CM reference but not ci-operator-configs -> no changes",
		sourceEnv:   []v1.EnvVar{{Name: "T", ValueFrom: makeCMReference("test-cm", "key")}},
		configs:     config.DataByFilename{},
		expectedEnv: []v1.EnvVar{{Name: "T", ValueFrom: makeCMReference("test-cm", "key")}},
	}, {
		description:              "CM reference to ci-operator-configs -> cm content inlined; test1",
		testname:                 "test1",
		sourceEnv:                []v1.EnvVar{{Name: "T", ValueFrom: makeCMReference(testCiopConfigInfo.ConfigMapName(), "filename")}},
		configs:                  config.DataByFilename{"filename": {Info: config.Info{Metadata: testCiopConfigInfo}, Configuration: testCiopConfig}},
		expectedEnv:              []v1.EnvVar{{Name: "T", Value: string(testCiopConfigContentTest1)}},
		expectedImageStramTagMap: apihelper.ImageStreamTagMap{"fancy/willem:first": types.NamespacedName{Namespace: "fancy", Name: "willem:first"}},
	}, {
		description: "CM reference to ci-operator-configs -> cm content inlined; test2",
		testname:    "test2",
		sourceEnv:   []v1.EnvVar{{Name: "T", ValueFrom: makeCMReference(testCiopConfigInfo.ConfigMapName(), "filename")}},
		configs:     config.DataByFilename{"filename": {Info: config.Info{Metadata: testCiopConfigInfo}, Configuration: testCiopConfig}},
		expectedEnv: []v1.EnvVar{{Name: "T", Value: string(testCiopConfigContentTest2)}},
	}, {
		description:   "bad CM key is handled",
		sourceEnv:     []v1.EnvVar{{Name: "T", ValueFrom: makeCMReference(testCiopConfigInfo.ConfigMapName(), "filename")}},
		configs:       config.DataByFilename{},
		expectedError: true,
	}}

	references, chains, workflows, _, _, err := load.Registry(testingRegistry, false)
	if err != nil {
		t.Fatalf("Failed to read registry: %v", err)
	}
	resolver := registry.NewResolver(references, chains, workflows)
	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			testLoggers := Loggers{logrus.New(), logrus.New()}
			job := makeTestingPresubmitForEnv(tc.sourceEnv)
			expectedJob := makeTestingPresubmitForEnv(tc.expectedEnv)

			imageStreamTags, err := inlineCiOpConfig(&job.Spec.Containers[0], tc.configs, resolver, testCiopConfigInfo, tc.testname, testLoggers)

			if tc.expectedError && err == nil {
				t.Fatalf("Expected inlineCiopConfig() to return an error, none returned")
			}

			if !tc.expectedError {
				if err != nil {
					t.Fatalf("Unexpected error returned by inlineCiOpConfig(): %v", err)
				}

				if diff := cmp.Diff(imageStreamTags, tc.expectedImageStramTagMap, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("returned imageStreamTags differ from expected: %s", diff)
				}

				if !equality.Semantic.DeepEqual(expectedJob, job) {
					t.Errorf("Returned job differs from expected:\n%s", cmp.Diff(expectedJob, job, ignoreUnexported))
				}
			}
		})
	}
}

func makeTestingPresubmit(name, context, branch string) *prowconfig.Presubmit {
	return &prowconfig.Presubmit{
		JobBase: prowconfig.JobBase{
			Agent:  "kubernetes",
			Name:   name,
			Labels: map[string]string{rehearseLabel: "123", jobconfig.CanBeRehearsedLabel: "true"},
			Spec: &v1.PodSpec{
				Containers: []v1.Container{{
					Command: []string{"ci-operator"},
					Args:    []string{"--resolver-address=http://ci-operator-resolver", "--org", "openshift", "--repo=origin", "--branch", "master"},
				}},
			},
		},
		RerunCommand: "/test pj-rehearse",
		Reporter:     prowconfig.Reporter{Context: context},
		Brancher: prowconfig.Brancher{Branches: []string{
			fmt.Sprintf("^%s$", branch),
		}},
	}
}

func TestMakeRehearsalPresubmit(t *testing.T) {
	testPrNumber := 123
	testRepo := "org/repo"

	sourcePresubmit := &prowconfig.Presubmit{
		JobBase: prowconfig.JobBase{
			Agent: "kubernetes",
			Name:  "pull-ci-org-repo-branch-test",
			Spec: &v1.PodSpec{
				Containers: []v1.Container{{
					Command: []string{"ci-operator"},
					Args:    []string{"arg1", "arg2"},
				}},
			},
		},
		RerunCommand: "/test test",
		Reporter:     prowconfig.Reporter{Context: "ci/prow/test"},
		Brancher:     prowconfig.Brancher{Branches: []string{"^branch$"}},
	}
	hiddenPresubmit := &prowconfig.Presubmit{}
	if err := deepcopy.Copy(hiddenPresubmit, sourcePresubmit); err != nil {
		t.Fatalf("deepcopy failed: %v", err)
	}
	hiddenPresubmit.Hidden = true

	testCases := []struct {
		testID   string
		refs     *pjapi.Refs
		original *prowconfig.Presubmit
	}{
		{
			testID:   "job that belong to different org/repo than refs",
			refs:     &pjapi.Refs{Org: "anotherOrg", Repo: "anotherRepo"},
			original: sourcePresubmit,
		},
		{
			testID:   "job that belong to the same org/repo with refs",
			refs:     &pjapi.Refs{Org: "org", Repo: "repo"},
			original: sourcePresubmit,
		},
		{
			testID:   "hidden job that belong to the same org/repo with refs",
			refs:     &pjapi.Refs{Org: "org", Repo: "repo"},
			original: hiddenPresubmit,
		},
		{
			testID:   "job that belong to the same org but different repo than refs",
			refs:     &pjapi.Refs{Org: "org", Repo: "anotherRepo"},
			original: sourcePresubmit,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.testID, func(t *testing.T) {
			rehearsal, err := makeRehearsalPresubmit(tc.original, testRepo, testPrNumber, tc.refs)
			if err != nil {
				t.Fatalf("failed to make rehearsal presubmit: %v", err)
			}
			serializedResult, err := yaml.Marshal(rehearsal)
			if err != nil {
				t.Fatalf("failed to serialize job: %v", err)
			}
			testhelper.CompareWithFixture(t, string(serializedResult))
		})
	}
}

func makeTestingProwJob(namespace, jobName, context string, refs *pjapi.Refs, org, repo, branch, configSpec string) *pjapi.ProwJob {
	return &pjapi.ProwJob{
		TypeMeta: metav1.TypeMeta{Kind: "ProwJob", APIVersion: "prow.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "generatedTestName",
			Namespace: namespace,
			Labels: map[string]string{
				"created-by-prow":       "true",
				"prow.k8s.io/job":       jobName,
				"prow.k8s.io/refs.org":  refs.Org,
				"prow.k8s.io/refs.repo": refs.Repo,
				"prow.k8s.io/type":      "presubmit",
				"prow.k8s.io/refs.pull": strconv.Itoa(refs.Pulls[0].Number),
				rehearseLabel:           strconv.Itoa(refs.Pulls[0].Number),
			},
			Annotations: map[string]string{"prow.k8s.io/job": jobName},
		},
		Spec: pjapi.ProwJobSpec{
			Agent:        "kubernetes",
			Type:         pjapi.PresubmitJob,
			Job:          jobName,
			Refs:         refs,
			Report:       true,
			Context:      context,
			RerunCommand: "/test pj-rehearse",
			ExtraRefs: []pjapi.Refs{
				{
					Org:     org,
					Repo:    repo,
					BaseRef: branch,
					WorkDir: true,
				},
			},
			PodSpec: &v1.PodSpec{
				Containers: []v1.Container{{
					Command: []string{"ci-operator"},
					Args:    []string{},
					Env:     []v1.EnvVar{{Name: "CONFIG_SPEC", Value: configSpec}},
				}},
			},
		},
		Status: pjapi.ProwJobStatus{
			State: pjapi.TriggeredState,
		},
	}
}

func makeTestData() (int, string, string, *pjapi.Refs) {
	testPrNumber := 123
	testNamespace := "test-namespace"
	testRefs := &pjapi.Refs{
		Org:     "testOrg",
		Repo:    "testRepo",
		BaseRef: "testBaseRef",
		BaseSHA: "testBaseSHA",
		Pulls:   []pjapi.Pull{{Number: testPrNumber, Author: "testAuthor", SHA: "testPrSHA"}},
	}
	testReleasePath := "path/to/openshift/release"

	return testPrNumber, testNamespace, testReleasePath, testRefs
}

func setSuccessCreateRactor(in runtime.Object) error {
	pj := in.(*pjapi.ProwJob)
	pj.Status.State = pjapi.SuccessState
	return nil
}

func TestExecuteJobsErrors(t *testing.T) {
	testPrNumber, testNamespace, testRepoPath, testRefs := makeTestData()
	targetOrgRepo := "targetOrg/targetRepo"
	testCiopConfigs := generateTestConfigFiles()

	testCases := []struct {
		description  string
		jobs         map[string][]prowconfig.Presubmit
		failToCreate sets.String
	}{{
		description: "fail to Create a prowjob",
		jobs: map[string][]prowconfig.Presubmit{targetOrgRepo: {
			*makeTestingPresubmit("job1", "ci/prow/job1", "master"),
		}},
		failToCreate: sets.NewString("rehearse-123-job1"),
	}, {
		description: "fail to Create one of two prowjobs",
		jobs: map[string][]prowconfig.Presubmit{targetOrgRepo: {
			*makeTestingPresubmit("job1", "ci/prow/job1", "master"),
			*makeTestingPresubmit("job2", "ci/prow/job2", "master"),
		}},
		failToCreate: sets.NewString("rehearse-123-job2"),
	}}

	references, chains, workflows, _, _, err := load.Registry(testingRegistry, false)
	if err != nil {
		t.Fatalf("Failed to read registry: %v", err)
	}
	resolver := registry.NewResolver(references, chains, workflows)
	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			testLoggers := Loggers{logrus.New(), logrus.New()}
			client := newTC()
			client.createReactors = append(client.createReactors,
				func(in runtime.Object) error {
					pj := in.(*pjapi.ProwJob)
					if tc.failToCreate.Has(pj.Spec.Job) {
						return errors.New("fail")
					}
					return nil
				},
				setSuccessCreateRactor,
			)

			jc := NewJobConfigurer(testCiopConfigs, resolver, testPrNumber, testLoggers, nil, nil, makeBaseRefs())

			_, presubmits, err := jc.ConfigurePresubmitRehearsals(tc.jobs)
			if err != nil {
				t.Errorf("Expected to get no error, but got one: %v", err)
			}
			executor := NewExecutor(presubmits, testPrNumber, testRepoPath, testRefs, true, testLoggers, client, testNamespace)
			executor.pollFunc = threetimesTryingPoller
			_, err = executor.ExecuteJobs()

			if err == nil {
				t.Errorf("Expected to return error, got nil")
			}
		})
	}
}

func TestExecuteJobsUnsuccessful(t *testing.T) {
	testPrNumber, testNamespace, testRepoPath, testRefs := makeTestData()
	targetOrgRepo := "targetOrg/targetRepo"
	testCiopConfigs := generateTestConfigFiles()

	testCases := []struct {
		description string
		jobs        map[string][]prowconfig.Presubmit
		results     map[string]pjapi.ProwJobState
	}{{
		description: "single job that fails",
		jobs: map[string][]prowconfig.Presubmit{targetOrgRepo: {
			*makeTestingPresubmit("job1", "ci/prow/job1", "master"),
		}},
		results: map[string]pjapi.ProwJobState{"rehearse-123-job1": pjapi.FailureState},
	}, {
		description: "single job that aborts",
		jobs: map[string][]prowconfig.Presubmit{targetOrgRepo: {
			*makeTestingPresubmit("job1", "ci/prow/job1", "master"),
		}},
		results: map[string]pjapi.ProwJobState{"rehearse-123-job1": pjapi.AbortedState},
	}, {
		description: "one job succeeds, one fails",
		jobs: map[string][]prowconfig.Presubmit{targetOrgRepo: {
			*makeTestingPresubmit("job1", "ci/prow/job1", "master"),
			*makeTestingPresubmit("job2", "ci/prow/job2", "master"),
		}},
		results: map[string]pjapi.ProwJobState{
			"rehearse-123-job1": pjapi.SuccessState,
			"rehearse-123-job2": pjapi.FailureState,
		},
	}}

	references, chains, workflows, _, _, err := load.Registry(testingRegistry, false)
	if err != nil {
		t.Fatalf("Failed to read registry: %v", err)
	}
	resolver := registry.NewResolver(references, chains, workflows)
	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			testLoggers := Loggers{logrus.New(), logrus.New()}
			client := newTC()
			client.createReactors = append(client.createReactors,
				func(in runtime.Object) error {
					pj := in.(*pjapi.ProwJob)
					pj.Status.State = tc.results[pj.Spec.Job]
					return nil
				},
			)

			jc := NewJobConfigurer(testCiopConfigs, resolver, testPrNumber, testLoggers, nil, nil, makeBaseRefs())
			_, presubmits, err := jc.ConfigurePresubmitRehearsals(tc.jobs)
			if err != nil {
				t.Errorf("Expected to get no error, but got one: %v", err)
			}
			executor := NewExecutor(presubmits, testPrNumber, testRepoPath, testRefs, false, testLoggers, client, testNamespace)
			executor.pollFunc = threetimesTryingPoller
			success, _ := executor.ExecuteJobs()

			if success {
				t.Errorf("Expected to return success=false, got true")
			}
		})
	}
}

func TestExecuteJobsPositive(t *testing.T) {
	testPrNumber, testNamespace, testRepoPath, testRefs := makeTestData()
	rehearseJobContextTemplate := "ci/rehearse/%s/%s/%s"
	targetOrgRepo := "targetOrg/targetRepo"
	anotherTargetOrgRepo := "anotherOrg/anotherRepo"
	targetOrg := "targetOrg"
	targetRepo := "targetRepo"
	anotherTargetOrg := "anotherOrg"
	anotherTargetRepo := "anotherRepo"
	testCiopConfigs := generateTestConfigFiles()

	testCases := []struct {
		description              string
		jobs                     map[string][]prowconfig.Presubmit
		expectedJobs             []pjapi.ProwJobSpec
		expectedImageStramTagMap apihelper.ImageStreamTagMap
	}{
		{
			description: "two jobs in a single repo",
			jobs: map[string][]prowconfig.Presubmit{targetOrgRepo: {
				*makeTestingPresubmit("job1", "ci/prow/job1", "master"),
				*makeTestingPresubmit("job2", "ci/prow/job2", "master"),
			}},
			expectedJobs: []pjapi.ProwJobSpec{
				makeTestingProwJob(testNamespace,
					"rehearse-123-job1",
					fmt.Sprintf(rehearseJobContextTemplate, targetOrgRepo, "master", "job1"),
					testRefs, targetOrg, targetRepo, "master", testingCiOpCfgJob1YAML).Spec,
				makeTestingProwJob(testNamespace,
					"rehearse-123-job2",
					fmt.Sprintf(rehearseJobContextTemplate, targetOrgRepo, "master", "job2"),
					testRefs, targetOrg, targetRepo, "master", testingCiOpCfgJob2YAML).Spec,
			},
			expectedImageStramTagMap: apihelper.ImageStreamTagMap{"fancy/willem:first": types.NamespacedName{Namespace: "fancy", Name: "willem:first"}},
		}, {
			description: "two jobs in a single repo, same context but different branch",
			jobs: map[string][]prowconfig.Presubmit{targetOrgRepo: {
				*makeTestingPresubmit("job1", "ci/prow/job1", "master"),
				*makeTestingPresubmit("job2", "ci/prow/job2", "not-master"),
			}},
			expectedJobs: []pjapi.ProwJobSpec{
				makeTestingProwJob(testNamespace,
					"rehearse-123-job1",
					fmt.Sprintf(rehearseJobContextTemplate, targetOrgRepo, "master", "job1"),
					testRefs, targetOrg, targetRepo, "master", testingCiOpCfgJob1YAML).Spec,
				makeTestingProwJob(testNamespace,
					"rehearse-123-job2",
					fmt.Sprintf(rehearseJobContextTemplate, targetOrgRepo, "not-master", "job2"),
					testRefs, targetOrg, targetRepo, "not-master", testingCiOpCfgJob2YAML).Spec,
			},
			expectedImageStramTagMap: apihelper.ImageStreamTagMap{"fancy/willem:first": types.NamespacedName{Namespace: "fancy", Name: "willem:first"}},
		},
		{
			description: "two jobs in a separate repos",
			jobs: map[string][]prowconfig.Presubmit{
				targetOrgRepo:        {*makeTestingPresubmit("job1", "ci/prow/job1", "master")},
				anotherTargetOrgRepo: {*makeTestingPresubmit("job2", "ci/prow/job2", "master")},
			},
			expectedJobs: []pjapi.ProwJobSpec{
				makeTestingProwJob(testNamespace,
					"rehearse-123-job1",
					fmt.Sprintf(rehearseJobContextTemplate, targetOrgRepo, "master", "job1"),
					testRefs, targetOrg, targetRepo, "master", testingCiOpCfgJob1YAML).Spec,
				makeTestingProwJob(testNamespace,
					"rehearse-123-job2",
					fmt.Sprintf(rehearseJobContextTemplate, anotherTargetOrgRepo, "master", "job2"),
					testRefs, anotherTargetOrg, anotherTargetRepo, "master", testingCiOpCfgJob2YAML).Spec,
			},
			expectedImageStramTagMap: apihelper.ImageStreamTagMap{"fancy/willem:first": types.NamespacedName{Namespace: "fancy", Name: "willem:first"}},
		}, {
			description:  "no jobs",
			jobs:         map[string][]prowconfig.Presubmit{},
			expectedJobs: []pjapi.ProwJobSpec{},
		},
	}

	references, chains, workflows, _, _, err := load.Registry(testingRegistry, false)
	if err != nil {
		t.Fatalf("Failed to read registry: %v", err)
	}
	resolver := registry.NewResolver(references, chains, workflows)
	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			testLoggers := Loggers{logrus.New(), logrus.New()}
			client := newTC()
			client.createReactors = append(client.createReactors, setSuccessCreateRactor)

			jc := NewJobConfigurer(testCiopConfigs, resolver, testPrNumber, testLoggers, nil, nil, makeBaseRefs())
			imageStreamTags, presubmits, err := jc.ConfigurePresubmitRehearsals(tc.jobs)
			if err != nil {
				t.Errorf("Expected to get no error, but got one: %v", err)
			}
			if diff := cmp.Diff(imageStreamTags, tc.expectedImageStramTagMap, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("returned imageStreamTags do not match expected: %s", diff)
			}
			executor := NewExecutor(presubmits, testPrNumber, testRepoPath, testRefs, true, testLoggers, client, testNamespace)
			success, err := executor.ExecuteJobs()

			if err != nil {
				t.Errorf("Expected ExecuteJobs() to not return error, returned %v", err)
				return
			}

			if !success {
				t.Errorf("Expected ExecuteJobs() to return success=true, got false")
			}

			createdJobs := &pjapi.ProwJobList{}
			if err := client.List(context.Background(), createdJobs); err != nil {
				t.Fatalf("failed to list prowjobs from client: %v", err)
			}

			var createdJobSpecs []pjapi.ProwJobSpec
			for _, job := range createdJobs.Items {
				createdJobSpecs = append(createdJobSpecs, job.Spec)
			}

			// Sort to allow comparison
			sort.Slice(tc.expectedJobs, func(a, b int) bool { return tc.expectedJobs[a].Job < tc.expectedJobs[b].Job })
			sort.Slice(createdJobSpecs, func(a, b int) bool { return createdJobSpecs[a].Job < createdJobSpecs[b].Job })

			if !equality.Semantic.DeepEqual(tc.expectedJobs, createdJobSpecs) {
				t.Errorf("Created ProwJobs differ from expected:\n%s", cmp.Diff(tc.expectedJobs, createdJobSpecs, ignoreUnexported))
			}
		})
	}
}

func TestWaitForJobs(t *testing.T) {
	loggers := Loggers{logrus.New(), logrus.New()}
	pjSuccess0 := pjapi.ProwJob{
		ObjectMeta: metav1.ObjectMeta{Name: "success0"},
		Status:     pjapi.ProwJobStatus{State: pjapi.SuccessState},
	}
	pjSuccess1 := pjapi.ProwJob{
		ObjectMeta: metav1.ObjectMeta{Name: "success1"},
		Status:     pjapi.ProwJobStatus{State: pjapi.SuccessState},
	}
	pjFailure := pjapi.ProwJob{
		ObjectMeta: metav1.ObjectMeta{Name: "failure"},
		Status:     pjapi.ProwJobStatus{State: pjapi.FailureState},
	}
	pjPending := pjapi.ProwJob{
		ObjectMeta: metav1.ObjectMeta{Name: "pending"},
		Status:     pjapi.ProwJobStatus{State: pjapi.PendingState},
	}
	pjAborted := pjapi.ProwJob{
		ObjectMeta: metav1.ObjectMeta{Name: "aborted"},
		Status:     pjapi.ProwJobStatus{State: pjapi.AbortedState},
	}
	pjTriggered := pjapi.ProwJob{
		ObjectMeta: metav1.ObjectMeta{Name: "triggered"},
		Status:     pjapi.ProwJobStatus{State: pjapi.TriggeredState},
	}
	pjError := pjapi.ProwJob{
		ObjectMeta: metav1.ObjectMeta{Name: "error"},
		Status:     pjapi.ProwJobStatus{State: pjapi.ErrorState},
	}
	testCases := []struct {
		id      string
		pjs     sets.String
		events  []runtime.Object
		success bool
		err     error
	}{{
		id:      "empty",
		success: true,
	}, {
		id:      "one successful job",
		success: true,
		pjs:     sets.NewString("success0"),
		events:  []runtime.Object{&pjSuccess0},
	}, {
		id:  "mixed states",
		pjs: sets.NewString("failure", "success0", "aborted", "error"),
		events: []runtime.Object{
			&pjFailure, &pjPending, &pjSuccess0,
			&pjTriggered, &pjAborted, &pjError,
		},
	}, {
		id:      "ignored states",
		success: true,
		pjs:     sets.NewString("success0"),
		events:  []runtime.Object{&pjPending, &pjSuccess0, &pjTriggered},
	}, {
		id:      "not watched",
		success: true,
		pjs:     sets.NewString("success1"),
		events:  []runtime.Object{&pjSuccess0, &pjFailure, &pjSuccess1},
	}, {
		id:     "not watched failure",
		pjs:    sets.NewString("failure"),
		events: []runtime.Object{&pjSuccess0, &pjFailure},
	}}
	for idx := range testCases {
		tc := testCases[idx]
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			client := newTC(tc.events...)

			executor := NewExecutor(nil, 0, "", &pjapi.Refs{}, true, loggers, client, "")
			executor.pollFunc = threetimesTryingPoller
			success, err := executor.waitForJobs(tc.pjs, &ctrlruntimeclient.ListOptions{})
			if err != tc.err {
				t.Fatalf("want `err` == %v, got %v", tc.err, err)
			}
			if success != tc.success {
				t.Fatalf("want `success` == %v, got %v", tc.success, success)
			}
		})
	}
}

func TestWaitForJobsRetries(t *testing.T) {
	client := newTC()
	var try int
	client.postListReactors = append(client.postListReactors, func(in runtime.Object) error {
		if try < 1 {
			try++
		} else {
			pjList := in.(*pjapi.ProwJobList)
			pjList.Items = append(pjList.Items, pjapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{Name: "j"},
				Status:     pjapi.ProwJobStatus{State: pjapi.SuccessState},
			})
		}
		return nil
	})

	executor := NewExecutor(nil, 0, "", &pjapi.Refs{}, true, Loggers{logrus.New(), logrus.New()}, client, "")
	executor.pollFunc = threetimesTryingPoller
	success, err := executor.waitForJobs(sets.String{"j": {}}, &ctrlruntimeclient.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !success {
		t.Error("expected success, didn't get it")
	}
}

func TestWaitForJobsLog(t *testing.T) {
	jobLogger, jobHook := logrustest.NewNullLogger()
	dbgLogger, dbgHook := logrustest.NewNullLogger()
	dbgLogger.SetLevel(logrus.DebugLevel)
	client := fakectrlruntimeclient.NewFakeClient(
		&pjapi.ProwJob{
			ObjectMeta: metav1.ObjectMeta{Name: "success"},
			Status:     pjapi.ProwJobStatus{State: pjapi.SuccessState}},
		&pjapi.ProwJob{
			ObjectMeta: metav1.ObjectMeta{Name: "failure"},
			Status:     pjapi.ProwJobStatus{State: pjapi.FailureState}},
	)
	loggers := Loggers{jobLogger, dbgLogger}

	executor := NewExecutor(nil, 0, "", &pjapi.Refs{}, true, loggers, client, "")
	executor.pollFunc = threetimesTryingPoller
	_, err := executor.waitForJobs(sets.NewString("success", "failure"), &ctrlruntimeclient.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	check := func(hook *logrustest.Hook, name string, level logrus.Level, state *pjapi.ProwJobState) {
		for _, entry := range hook.Entries {
			if entry.Level == level && entry.Data["name"] == name && (state == nil || entry.Data["state"].(pjapi.ProwJobState) == *state) {
				return
			}
		}
		if state == nil {
			t.Errorf("no log entry with name == %q, level == %q found", name, level)
		} else {
			t.Errorf("no log entry with name == %q, level == %q, and state == %q found", name, level, *state)
		}
	}
	successState, failureState := pjapi.SuccessState, pjapi.FailureState
	check(jobHook, "success", logrus.InfoLevel, &successState)
	check(jobHook, "failure", logrus.ErrorLevel, &failureState)
	check(dbgHook, "success", logrus.DebugLevel, nil)
	check(dbgHook, "failure", logrus.DebugLevel, nil)
}

func TestFilterPresubmits(t *testing.T) {
	labels := map[string]string{"pj-rehearse.openshift.io/can-be-rehearsed": "true"}

	testCases := []struct {
		description string
		crippleFunc func(*prowconfig.Presubmit) map[string][]prowconfig.Presubmit
		expected    func(*prowconfig.Presubmit) config.Presubmits
	}{
		{
			description: "basic presubmit job, allowed",
			crippleFunc: func(j *prowconfig.Presubmit) map[string][]prowconfig.Presubmit {
				j.Spec.Volumes = []v1.Volume{{Name: "volume"}}
				j.Labels = labels
				return map[string][]prowconfig.Presubmit{"org/repo": {*j}}
			},
			expected: func(j *prowconfig.Presubmit) config.Presubmits {
				j.Spec.Volumes = []v1.Volume{{Name: "volume"}}
				return config.Presubmits{"org/repo": {*j}}
			},
		},
		{
			description: "job with no rehearse label, not allowed",
			crippleFunc: func(j *prowconfig.Presubmit) map[string][]prowconfig.Presubmit {
				return map[string][]prowconfig.Presubmit{"org/repo": {*j}}
			},
			expected: func(j *prowconfig.Presubmit) config.Presubmits {
				return config.Presubmits{}
			},
		},
		{
			description: "hidden job, not allowed",
			crippleFunc: func(j *prowconfig.Presubmit) map[string][]prowconfig.Presubmit {
				j.Labels = labels
				j.Hidden = true
				return map[string][]prowconfig.Presubmit{"org/repo": {*j}}
			},
			expected: func(j *prowconfig.Presubmit) config.Presubmits {
				return config.Presubmits{}
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			basePresubmit := makeBasePresubmit()
			tc.crippleFunc(basePresubmit)
			p := filterPresubmits(map[string][]prowconfig.Presubmit{"org/repo": {*basePresubmit}}, logrus.New())

			expected := tc.expected(basePresubmit)
			if !equality.Semantic.DeepEqual(expected, p) {
				t.Fatalf("Found: %#v\nExpected: %#v", p, expected)
			}
		})

	}
}

func makeBasePresubmit() *prowconfig.Presubmit {
	return &prowconfig.Presubmit{
		JobBase: prowconfig.JobBase{
			Agent:  "kubernetes",
			Name:   "pull-ci-organization-repo-master-test",
			Labels: map[string]string{"ci.openshift.org/rehearse": "123"},
			Spec: &v1.PodSpec{
				Containers: []v1.Container{{
					Command: []string{"ci-operator"},
					Args:    []string{"arg"},
				}},
			},
		},
		RerunCommand: "/test pj-rehearse",
		Reporter:     prowconfig.Reporter{Context: "ci/prow/test"},
		Brancher:     prowconfig.Brancher{Branches: []string{"^master$"}},
	}
}

func TestReplaceConfigMaps(t *testing.T) {
	replacedConfigMaps := map[string]string{
		"changed-template":        "rehearse-template-test-template-00000000",
		"changed-cluster-profile": "rehearse-cluster-profile-test-cp-00000000",
	}

	testCases := []struct {
		description string
		jobVolumes  []v1.Volume
		expected    []v1.Volume
	}{
		{
			description: "no volumes",
			jobVolumes:  []v1.Volume{},
			expected:    []v1.Volume{},
		},
		{
			description: "replace a configmap name in configmap-backed volume",
			jobVolumes:  []v1.Volume{cmVolume("volume-name", "changed-template")},
			expected:    []v1.Volume{cmVolume("volume-name", "rehearse-template-test-template-00000000")},
		},
		{
			description: "replace a configmap name in projected configmap-backed volume",
			jobVolumes:  []v1.Volume{projectedCmVolume("volume-name", "changed-template")},
			expected:    []v1.Volume{projectedCmVolume("volume-name", "rehearse-template-test-template-00000000")},
		},
		{
			description: "do not replace a configmap name in configmap-backed volume",
			jobVolumes:  []v1.Volume{cmVolume("volume-name", "unchanged-template")},
			expected:    []v1.Volume{cmVolume("volume-name", "unchanged-template")},
		},
		{
			description: "do not replace a configmap name in projected configmap-backed volume",
			jobVolumes:  []v1.Volume{projectedCmVolume("volume-name", "unchanged-template")},
			expected:    []v1.Volume{projectedCmVolume("volume-name", "unchanged-template")},
		},
		{
			description: "replace multiple configmap names in many volumes",
			jobVolumes: []v1.Volume{
				cmVolume("first-volume", "changed-template"),
				projectedCmVolume("second-volume", "unchanged-cluster-profile"),
				projectedCmVolume("third-volume", "irrelevant-configmap"),
				cmVolume("fourth-volume", "another-irrelevant-template"),
				projectedCmVolume("fifth-volume", "changed-cluster-profile"),
			},
			expected: []v1.Volume{
				cmVolume("first-volume", "rehearse-template-test-template-00000000"),
				projectedCmVolume("second-volume", "unchanged-cluster-profile"),
				projectedCmVolume("third-volume", "irrelevant-configmap"),
				cmVolume("fourth-volume", "another-irrelevant-template"),
				projectedCmVolume("fifth-volume", "rehearse-cluster-profile-test-cp-00000000"),
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			logger := logrus.WithField("testId", testCase.description)
			replaceConfigMaps(testCase.jobVolumes, replacedConfigMaps, logger)
			if !reflect.DeepEqual(testCase.expected, testCase.jobVolumes) {
				t.Fatalf("Volumes differ:\n%v", cmp.Diff(testCase.expected, testCase.jobVolumes))
			}
		})

	}
}

func cmVolume(name, cmName string) v1.Volume {
	return v1.Volume{
		Name: name,
		VolumeSource: v1.VolumeSource{
			ConfigMap: &v1.ConfigMapVolumeSource{
				LocalObjectReference: v1.LocalObjectReference{Name: cmName},
			},
		},
	}
}

func projectedCmVolume(name, cmName string) v1.Volume {
	return v1.Volume{
		Name: name,
		VolumeSource: v1.VolumeSource{
			Projected: &v1.ProjectedVolumeSource{
				Sources: []v1.VolumeProjection{
					{
						ConfigMap: &v1.ConfigMapProjection{
							LocalObjectReference: v1.LocalObjectReference{Name: cmName},
						},
					},
				},
			},
		},
	}
}

func TestGetClusterTypes(t *testing.T) {
	makeJob := func(clusterType string) prowconfig.Presubmit {
		ret := prowconfig.Presubmit{
			JobBase: prowconfig.JobBase{
				Agent: string(pjapi.KubernetesAgent),
			},
		}
		if clusterType != "" {
			ret.Spec = &v1.PodSpec{
				Containers: []v1.Container{{
					Env: []v1.EnvVar{{
						Name:  clusterTypeEnvName,
						Value: clusterType,
					}},
				}},
			}
		}
		return ret
	}
	type Jobs map[string][]prowconfig.Presubmit
	for _, tc := range []struct {
		id   string
		jobs Jobs
		want []string
	}{{
		id:   "no types",
		jobs: Jobs{"org/repo": {makeJob("")}},
	}, {
		id:   "one type",
		jobs: Jobs{"org/repo": {makeJob(""), makeJob("aws")}},
		want: []string{"aws"},
	}, {
		id: "multiple types",
		jobs: Jobs{
			"org/repo":   {makeJob(""), makeJob("aws")},
			"org/sitory": {makeJob("azure"), makeJob("vsphere")},
		},
		want: []string{"aws", "azure", "vsphere"},
	}} {
		t.Run(tc.id, func(t *testing.T) {
			ret := getClusterTypes(tc.jobs)
			if !reflect.DeepEqual(tc.want, ret) {
				t.Fatal(cmp.Diff(tc.want, ret))
			}
		})
	}
}

func makeBaseRefs() *pjapi.Refs {
	return &pjapi.Refs{
		Org:      "openshift",
		Repo:     "release",
		RepoLink: "https://github.com/openshift/release",
		BaseRef:  "master",
		BaseSHA:  "80af9fee7a9f63a79e01da0c74d9dd323118daf0",
		BaseLink: "",
		Pulls: []pjapi.Pull{
			{
				Number: 39612,
				Author: "droslean",
				SHA:    "bc825725cfe0acebb06a7e0b11c8228f5a3b89c0",
			},
		},
	}
}

func TestRemoveConfigResolverFlags(t *testing.T) {
	var testCases = []struct {
		description  string
		input        []string
		expectedArgs []string
		expectedInfo api.Metadata
	}{{
		description:  "just resolver flags",
		input:        []string{"--resolver-address=http://ci-operator-resolver", "--org=openshift", "--repo=origin", "--branch=master", "--variant=v2"},
		expectedArgs: nil,
		expectedInfo: api.Metadata{Org: "openshift", Repo: "origin", Branch: "master", Variant: "v2"},
	}, {
		description:  "no resolver flags",
		input:        []string{"--artifact-dir=$(ARTIFACTS)", "--target=target"},
		expectedArgs: []string{"--artifact-dir=$(ARTIFACTS)", "--target=target"},
	}, {
		description:  "mixed resolver and non-resolver flags",
		input:        []string{"--artifact-dir=$(ARTIFACTS)", "--resolver-address=http://ci-operator-resolver", "--org=openshift", "--target=target", "--repo=origin", "--branch=master", "--variant=v2"},
		expectedArgs: []string{"--artifact-dir=$(ARTIFACTS)", "--target=target"},
		expectedInfo: api.Metadata{Org: "openshift", Repo: "origin", Branch: "master", Variant: "v2"},
	}, {
		description:  "spaces in between flag and value",
		input:        []string{"--artifact-dir=$(ARTIFACTS)", "--resolver-address=http://ci-operator-resolver", "--org", "openshift", "--target=target", "--repo", "origin", "--branch", "master", "--variant=v2"},
		expectedArgs: []string{"--artifact-dir=$(ARTIFACTS)", "--target=target"},
		expectedInfo: api.Metadata{Org: "openshift", Repo: "origin", Branch: "master", Variant: "v2"},
	}, {
		description:  "reporting flags",
		input:        []string{"--report-password-file=/etc/report/password.txt", "--report-username=ci", "--artifact-dir=$(ARTIFACTS)", "--resolver-address=http://ci-operator-resolver", "--org", "openshift", "--target=target", "--repo", "origin", "--branch", "master", "--variant=v2"},
		expectedArgs: []string{"--report-password-file=/etc/report/password.txt", "--report-username=ci", "--artifact-dir=$(ARTIFACTS)", "--target=target"},
		expectedInfo: api.Metadata{Org: "openshift", Repo: "origin", Branch: "master", Variant: "v2"},
	}}
	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			newArgs, info := removeConfigResolverFlags(testCase.input)
			if !reflect.DeepEqual(testCase.expectedArgs, newArgs) {
				t.Fatalf("Args differ from expected: %v", cmp.Diff(testCase.expectedArgs, newArgs))
			}
			if !reflect.DeepEqual(testCase.expectedInfo, info) {
				t.Fatalf("ci-operator config info differs from expected: %v", cmp.Diff(testCase.expectedInfo, info))
			}
		})
	}
}

func TestGetTrimmedBranch(t *testing.T) {
	testCases := []struct {
		name     string
		input    []string
		expected string
	}{{
		name:     "master with regex",
		input:    []string{"^master$"},
		expected: "master",
	}, {
		name:     "release-4.2 no regex",
		input:    []string{"release-4.2"},
		expected: "release-4.2",
	}}
	for _, testCase := range testCases {
		branch := getTrimmedBranch(testCase.input)
		if branch != testCase.expected {
			t.Errorf("%s: getTrimmedBranches returned %s, expected %s", testCase.name, branch, testCase.expected)
		}
	}
}

func TestVariantFromLabels(t *testing.T) {
	testCases := []struct {
		name     string
		input    map[string]string
		expected string
	}{{
		name:     "no labels",
		input:    map[string]string{},
		expected: "",
	}, {
		name: "generated label",
		input: map[string]string{
			jobconfig.ProwJobLabelGenerated: "true",
		},
		expected: "",
	}, {
		name: "generated and variant labels",
		input: map[string]string{
			jobconfig.ProwJobLabelGenerated: "true",
			jobconfig.ProwJobLabelVariant:   "v2",
		},
		expected: "v2",
	}}
	for _, testCase := range testCases {
		variant := variantFromLabels(testCase.input)
		if variant != testCase.expected {
			t.Errorf("%s: variantFromLabels returned %s, expected %s", testCase.name, variant, testCase.expected)
		}
	}
}

func newTC(initObjs ...runtime.Object) *tc {
	return &tc{Client: fakectrlruntimeclient.NewFakeClient(initObjs...)}
}

type tc struct {
	ctrlruntimeclient.Client
	createReactors   []func(runtime.Object) error
	postListReactors []func(runtime.Object) error
}

func (tc *tc) Create(ctx context.Context, obj runtime.Object, opts ...ctrlruntimeclient.CreateOption) error {
	for _, createReactor := range tc.createReactors {
		if err := createReactor(obj); err != nil {
			return err
		}
	}

	return tc.Client.Create(ctx, obj, opts...)
}

func (tc *tc) List(ctx context.Context, obj runtime.Object, opts ...ctrlruntimeclient.ListOption) error {
	if err := tc.Client.List(ctx, obj, opts...); err != nil {
		return err
	}
	for _, listReactor := range tc.postListReactors {
		if err := listReactor(obj); err != nil {
			return err
		}
	}
	return nil
}

func threetimesTryingPoller(_, _ time.Duration, cf wait.ConditionFunc) error {
	for i := 0; i < 3; i++ {
		success, err := cf()
		if err != nil {
			return err
		}
		if success {
			return nil
		}
	}
	return wait.ErrWaitTimeout
}
