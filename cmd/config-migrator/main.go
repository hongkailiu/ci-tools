package main

import (
	"flag"
	"os"

	"github.com/getlantern/deepcopy"
	"github.com/sirupsen/logrus"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/config"
)

const (
	prowClusterURL = "https://api.ci.openshift.org"
)

type options struct {
	ConfigDir string
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.StringVar(&o.ConfigDir, "config-dir", "", "Path to CI Operator configuration directory.")
	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("could not parse input")
	}
	return o
}

func main() {
	o := gatherOptions()

	var toCommit []config.DataWithInfo
	if err := config.OperateOnCIOperatorConfigDir(o.ConfigDir, func(configuration *api.ReleaseBuildConfiguration, info *config.Info) error {
		for _, output := range generateMigratedConfigs(config.DataWithInfo{Configuration: *configuration, Info: *info}) {
			// we are walking the config so we need to commit once we're done
			toCommit = append(toCommit, output)
		}

		return nil
	}); err != nil {
		logrus.WithError(err).Fatal("Could not branch configurations.")
	}

	var failed bool
	for _, output := range toCommit {
		if err := output.CommitTo(o.ConfigDir); err != nil {
			failed = true
		}
	}
	if failed {
		logrus.Fatal("Failed to commit configuration to disk.")
	}
}

func generateMigratedConfigs(input config.DataWithInfo) []config.DataWithInfo {
	logrus.Infof("%s/%s is migrated", input.Info.Org, input.Info.Repo)

	var output []config.DataWithInfo
	input.Logger().Info("Migrating configuration.")
	currentConfig := input.Configuration

	var futureConfig api.ReleaseBuildConfiguration
	if err := deepcopy.Copy(&futureConfig, &currentConfig); err != nil {
		input.Logger().WithError(err).Error("failed to copy input CI Operator configuration")
		return nil
	}

	var rc api.ResourceConfiguration
	for k, rr := range futureConfig.Resources {
		if rc == nil {
			rc = map[string]api.ResourceRequirements{}
		}
		if _, ok := rr.Limits["memory"]; ok {
			delete(rr.Limits, "memory")
		}
		rc[k] = rr
	}
	futureConfig.Resources = rc

	// this config will promote to the new location on the release branch
	output = append(output, config.DataWithInfo{Configuration: futureConfig, Info: input.Info})
	return output
}
