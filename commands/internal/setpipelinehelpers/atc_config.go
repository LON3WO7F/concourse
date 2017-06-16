package setpipelinehelpers

import (
	"fmt"
	"io/ioutil"
	"os"

	yaml "gopkg.in/yaml.v2"

	"github.com/cloudfoundry/bosh-cli/director/template"
	"github.com/concourse/atc"
	"github.com/concourse/atc/web"
	"github.com/concourse/fly/commands/internal/displayhelpers"
	"github.com/concourse/fly/commands/internal/flaghelpers"
	temp "github.com/concourse/fly/template"
	"github.com/concourse/fly/ui"
	"github.com/concourse/go-concourse/concourse"
	"github.com/onsi/gomega/gexec"
	"github.com/tedsuo/rata"
	"github.com/vito/go-interact/interact"
)

type ATCConfig struct {
	PipelineName        string
	Team                concourse.Team
	WebRequestGenerator *rata.RequestGenerator
	SkipInteraction     bool
}

func (atcConfig ATCConfig) ApplyConfigInteraction() bool {
	if atcConfig.SkipInteraction {
		return true
	}

	confirm := false
	err := interact.NewInteraction("apply configuration?").Resolve(&confirm)
	if err != nil {
		return false
	}

	return confirm
}

func (atcConfig ATCConfig) Set(configPath atc.PathFlag, templateVariables []flaghelpers.VariablePairFlag, templateVariablesFiles []atc.PathFlag) error {
	newConfig := atcConfig.newConfig(configPath, templateVariablesFiles, templateVariables)
	existingConfig, _, existingConfigVersion, _, err := atcConfig.Team.PipelineConfig(atcConfig.PipelineName)
	errorMessages := []string{}
	if err != nil {
		if configError, ok := err.(concourse.PipelineConfigError); ok {
			errorMessages = configError.ErrorMessages
		} else {
			return err
		}
	}

	var new atc.Config
	err = yaml.Unmarshal([]byte(newConfig), &new)
	if err != nil {
		return err
	}

	diff(existingConfig, new)

	if len(errorMessages) > 0 {
		atcConfig.showPipelineConfigErrors(errorMessages)
	}

	if !atcConfig.ApplyConfigInteraction() {
		fmt.Println("bailing out")
		return nil
	}

	created, updated, warnings, err := atcConfig.Team.CreateOrUpdatePipelineConfig(
		atcConfig.PipelineName,
		existingConfigVersion,
		newConfig,
	)
	if err != nil {
		return err
	}

	if len(warnings) > 0 {
		atcConfig.showWarnings(warnings)
	}

	atcConfig.showHelpfulMessage(created, updated)
	return nil
}

func (atcConfig ATCConfig) newConfig(configPath atc.PathFlag, templateVariablesFiles []atc.PathFlag, templateVariables []flaghelpers.VariablePairFlag) []byte {
	evaluatedConfig, err := ioutil.ReadFile(string(configPath))
	if err != nil {
		displayhelpers.FailWithErrorf("could not read config file", err)
	}

	var paramPayloads [][]byte
	for _, path := range templateVariablesFiles {
		templateVars, err := ioutil.ReadFile(string(path))
		if err != nil {
			displayhelpers.FailWithErrorf("could not read template variables file (%s)", err, string(path))
		}

		paramPayloads = append(paramPayloads, templateVars)
	}

	if temp.Present(evaluatedConfig) {
		evaluatedConfig, err = atcConfig.resolveDeprecatedTemplateStyle(evaluatedConfig, paramPayloads, templateVariables)
		if err != nil {
			displayhelpers.FailWithErrorf("could not resolve old-style template vars", err)
		}
	}

	evaluatedConfig, err = atcConfig.resolveTemplates(evaluatedConfig, paramPayloads, templateVariables)
	if err != nil {
		displayhelpers.Failf("could not resolve template vars", err)
	}

	return evaluatedConfig
}

func (atcConfig ATCConfig) resolveTemplates(configPayload []byte, paramPayloads [][]byte, variables []flaghelpers.VariablePairFlag) ([]byte, error) {
	tpl := template.NewTemplate(configPayload)

	flagVars := template.StaticVariables{}
	for _, f := range variables {
		flagVars[f.Name] = f.Value
	}

	vars := []template.Variables{flagVars}
	for i := len(paramPayloads) - 1; i >= 0; i-- {
		payload := paramPayloads[i]

		var staticVars template.StaticVariables
		err := yaml.Unmarshal(payload, &staticVars)
		if err != nil {
			return nil, err
		}

		vars = append(vars, staticVars)
	}

	bytes, err := tpl.Evaluate(template.NewMultiVars(vars), nil, template.EvaluateOpts{
		ExpectAllKeys: true,
	})
	if err != nil {
		return nil, err
	}

	return bytes, nil
}

func (atcConfig ATCConfig) resolveDeprecatedTemplateStyle(configPayload []byte, paramPayloads [][]byte, variables []flaghelpers.VariablePairFlag) ([]byte, error) {
	vars := temp.Variables{}
	for _, payload := range paramPayloads {
		var payloadVars temp.Variables
		err := yaml.Unmarshal(payload, &payloadVars)
		if err != nil {
			return nil, err
		}

		vars = vars.Merge(payloadVars)
	}

	flagVars := temp.Variables{}
	for _, flag := range variables {
		flagVars[flag.Name] = flag.OldValue
	}

	vars = vars.Merge(flagVars)

	return temp.Evaluate(configPayload, vars)
}

func (atcConfig ATCConfig) showPipelineConfigErrors(errorMessages []string) {
	fmt.Fprintln(ui.Stderr, "")
	displayhelpers.PrintWarningHeader()

	fmt.Fprintln(ui.Stderr, "Error loading existing config:")
	for _, errorMessage := range errorMessages {
		fmt.Fprintf(ui.Stderr, "  - %s\n", errorMessage)
	}

	fmt.Fprintln(ui.Stderr, "")
}

func (atcConfig ATCConfig) showWarnings(warnings []concourse.ConfigWarning) {
	fmt.Fprintln(ui.Stderr, "")
	displayhelpers.PrintDeprecationWarningHeader()

	for _, warning := range warnings {
		fmt.Fprintf(ui.Stderr, "  - %s\n", warning.Message)
	}

	fmt.Fprintln(ui.Stderr, "")
}

func (atcConfig ATCConfig) showHelpfulMessage(created bool, updated bool) {
	if updated {
		fmt.Println("configuration updated")
	} else if created {
		pipelineWebReq, _ := atcConfig.WebRequestGenerator.CreateRequest(
			web.Pipeline,
			rata.Params{
				"pipeline":  atcConfig.PipelineName,
				"team_name": atcConfig.Team.Name(),
			},
			nil,
		)

		fmt.Println("pipeline created!")

		pipelineURL := pipelineWebReq.URL
		// don't show username and password
		pipelineURL.User = nil

		fmt.Printf("you can view your pipeline here: %s\n", pipelineURL.String())

		fmt.Println("")
		fmt.Println("the pipeline is currently paused. to unpause, either:")
		fmt.Println("  - run the unpause-pipeline command")
		fmt.Println("  - click play next to the pipeline in the web ui")
	} else {
		panic("Something really went wrong!")
	}
}

func diff(existingConfig atc.Config, newConfig atc.Config) {
	indent := gexec.NewPrefixedWriter("  ", os.Stdout)

	groupDiffs := diffIndices(GroupIndex(existingConfig.Groups), GroupIndex(newConfig.Groups))
	if len(groupDiffs) > 0 {
		fmt.Println("groups:")

		for _, diff := range groupDiffs {
			diff.Render(indent, "group")
		}
	}

	resourceDiffs := diffIndices(ResourceIndex(existingConfig.Resources), ResourceIndex(newConfig.Resources))
	if len(resourceDiffs) > 0 {
		fmt.Println("resources:")

		for _, diff := range resourceDiffs {
			diff.Render(indent, "resource")
		}
	}

	resourceTypeDiffs := diffIndices(ResourceTypeIndex(existingConfig.ResourceTypes), ResourceTypeIndex(newConfig.ResourceTypes))
	if len(resourceTypeDiffs) > 0 {
		fmt.Println("resource types:")

		for _, diff := range resourceTypeDiffs {
			diff.Render(indent, "resource type")
		}
	}

	jobDiffs := diffIndices(JobIndex(existingConfig.Jobs), JobIndex(newConfig.Jobs))
	if len(jobDiffs) > 0 {
		fmt.Println("jobs:")

		for _, diff := range jobDiffs {
			diff.Render(indent, "job")
		}
	}
}
