package context

import (
	"fmt"
	"os"
	"path"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/commitdev/zero/internal/config/globalconfig"
	"github.com/commitdev/zero/internal/config/moduleconfig"
	"github.com/commitdev/zero/internal/config/projectconfig"
	"github.com/commitdev/zero/internal/module"
	project "github.com/commitdev/zero/pkg/credentials"
	"github.com/commitdev/zero/pkg/util/exit"
	"github.com/commitdev/zero/pkg/util/flog"
	"github.com/k0kubun/pp"
	"github.com/manifoldco/promptui"
)

type Registry map[string][]string

// Create cloud provider context
func Init(outDir string) *projectconfig.ZeroProjectConfig {
	projectConfig := defaultProjConfig()

	projectConfig.Name = getProjectNamePrompt().GetParam(projectConfig.Parameters)

	rootDir := path.Join(outDir, projectConfig.Name)
	flog.Infof(":tada: Initializing project")

	err := os.MkdirAll(rootDir, os.ModePerm)
	if os.IsExist(err) {
		exit.Fatal("Directory %v already exists! Error: %v", projectConfig.Name, err)
	} else if err != nil {
		exit.Fatal("Error creating root: %v ", err)
	}

	moduleSources := chooseStack(getRegistry())
	moduleConfigs := loadAllModules(moduleSources)

	prompts := getProjectPrompts(projectConfig.Name, moduleConfigs)

	initParams := make(map[string]string)
	projectConfig.ShouldPushRepositories = true
	initParams["ShouldPushRepositories"] = prompts["ShouldPushRepositories"].GetParam(initParams)
	if initParams["ShouldPushRepositories"] == "n" {
		projectConfig.ShouldPushRepositories = false
	}

	// Prompting for push-up stream, then conditionally prompting for github
	initParams["GithubRootOrg"] = prompts["GithubRootOrg"].GetParam(initParams)
	projectCredentials := globalconfig.GetProjectCredentials(projectConfig.Name)
	credentialPrompts := getCredentialPrompts(projectCredentials, moduleConfigs)
	projectCredentials = promptCredentialsAndFillProjectCreds(credentialPrompts, projectCredentials)
	globalconfig.Save(projectCredentials)
	projectParameters := promptAllModules(moduleConfigs)

	// Map parameter values back to specific modules
	for moduleName, module := range moduleConfigs {
		repoName := prompts[moduleName].GetParam(initParams)
		repoURL := fmt.Sprintf("%s/%s", initParams["GithubRootOrg"], repoName)
		projectModuleParams := make(projectconfig.Parameters)

		// Loop through all the prompted values and find the ones relevant to this module
		for parameterKey, parameterValue := range projectParameters {
			for _, moduleParameter := range module.Parameters {
				if moduleParameter.Field == parameterKey {
					projectModuleParams[parameterKey] = parameterValue
				}
			}

		}

		projectConfig.Modules[moduleName] = projectconfig.NewModule(projectModuleParams, repoName, repoURL)
	}

	// TODO : Write the project config file. For now, print.
	pp.Println(projectConfig)
	pp.Print(projectCredentials)

	// TODO: load ~/.zero/config.yml (or credentials)
	// TODO: prompt global credentials

	return &projectConfig
}

// loadAllModules takes a list of module sources, downloads those modules, and parses their config
func loadAllModules(moduleSources []string) map[string]moduleconfig.ModuleConfig {
	modules := make(map[string]moduleconfig.ModuleConfig)

	wg := sync.WaitGroup{}
	wg.Add(len(moduleSources))
	for _, moduleSource := range moduleSources {
		go module.FetchModule(moduleSource, &wg)
	}
	wg.Wait()

	for _, moduleSource := range moduleSources {
		mod, err := module.ParseModuleConfig(moduleSource)
		if err != nil {
			exit.Fatal("Unable to load module:  %v\n", err)
		}
		modules[mod.Name] = mod
	}
	return modules
}

// promptAllModules takes a map of all the modules and prompts the user for values for all the parameters
func promptAllModules(modules map[string]moduleconfig.ModuleConfig) map[string]string {
	parameterValues := make(map[string]string)
	for _, config := range modules {
		var err error
		parameterValues, err = PromptModuleParams(config, parameterValues)
		if err != nil {
			exit.Fatal("Exiting prompt:  %v\n", err)
		}
	}
	return parameterValues
}

// Project name is prompt individually because the rest of the prompts
// requires the projectName to populate defaults
func getProjectNamePrompt() PromptHandler {
	return PromptHandler{
		moduleconfig.Parameter{
			Field:   "projectName",
			Label:   "Project Name",
			Default: "",
		},
		NoCondition,
		NoValidation,
	}
}

func getProjectPrompts(projectName string, modules map[string]moduleconfig.ModuleConfig) map[string]PromptHandler {
	handlers := map[string]PromptHandler{
		"ShouldPushRepositories": {
			moduleconfig.Parameter{
				Field:   "ShouldPushRepositories",
				Label:   "Should the created projects be checked into github automatically? (y/n)",
				Default: "y",
			},
			NoCondition,
			SpecificValueValidation("y", "n"),
		},
		"GithubRootOrg": {
			moduleconfig.Parameter{
				Field:   "GithubRootOrg",
				Label:   "What's the root of the github org to create repositories in?",
				Default: "github.com/",
			},
			KeyMatchCondition("ShouldPushRepositories", "y"),
			NoValidation,
		},
	}

	for moduleName, module := range modules {
		label := fmt.Sprintf("What do you want to call the %s project?", moduleName)

		handlers[moduleName] = PromptHandler{
			moduleconfig.Parameter{
				Field:   moduleName,
				Label:   label,
				Default: module.OutputDir,
			},
			NoCondition,
			NoValidation,
		}
	}

	return handlers
}

func getCredentialPrompts(projectCredentials globalconfig.ProjectCredential, moduleConfigs map[string]moduleconfig.ModuleConfig) map[string][]PromptHandler {
	var uniqueVendors []string
	for _, module := range moduleConfigs {
		uniqueVendors = appendToSet(uniqueVendors, module.RequiredCredentials)
	}
	// map is to keep track of which vendor they belong to, to fill them back into the projectConfig
	prompts := map[string][]PromptHandler{}
	for _, vendor := range uniqueVendors {
		prompts[vendor] = mapVendorToPrompts(projectCredentials, vendor)
	}
	return prompts
}

func mapVendorToPrompts(projectCred globalconfig.ProjectCredential, vendor string) []PromptHandler {
	var prompts []PromptHandler

	switch vendor {
	case "aws":
		awsPrompts := []PromptHandler{
			{
				moduleconfig.Parameter{
					Field:   "accessKeyId",
					Label:   "AWS Access Key ID",
					Default: projectCred.AWSResourceConfig.AccessKeyId,
				},
				NoCondition,
				NoValidation,
			},
			{
				moduleconfig.Parameter{
					Field:   "secretAccessKey",
					Label:   "AWS Secret access key",
					Default: projectCred.AWSResourceConfig.SecretAccessKey,
				},
				NoCondition,
				NoValidation,
			},
		}
		prompts = append(prompts, awsPrompts...)
	case "github":
		githubPrompt := PromptHandler{
			moduleconfig.Parameter{
				Field:   "accessToken",
				Label:   "Github Personal Access Token with access to the above organization",
				Default: projectCred.GithubResourceConfig.AccessToken,
			},
			NoCondition,
			NoValidation,
		}
		prompts = append(prompts, githubPrompt)
	case "circleci":
		circleCiPrompt := PromptHandler{
			moduleconfig.Parameter{
				Field:   "apiKey",
				Label:   "Circleci api key for CI/CD",
				Default: projectCred.CircleCiResourceConfig.ApiKey,
			},
			NoCondition,
			NoValidation,
		}
		prompts = append(prompts, circleCiPrompt)
	}
	return prompts
}

func chooseCloudProvider(projectConfig *projectconfig.ZeroProjectConfig) {
	// @TODO move options into configs
	providerPrompt := promptui.Select{
		Label: "Select Cloud Provider",
		Items: []string{"Amazon AWS", "Google GCP", "Microsoft Azure"},
	}

	_, providerResult, err := providerPrompt.Run()
	if err != nil {
		exit.Fatal("Prompt failed %v\n", err)
	}

	if providerResult != "Amazon AWS" {
		exit.Fatal("Only the AWS provider is available at this time")
	}
}

func getRegistry() Registry {
	return Registry{
		// TODO: better place to store these options as configuration file or any source
		"EKS + Go + React": []string{
			"github.com/commitdev/zero-aws-eks-stack",
			"github.com/commitdev/zero-deployable-backend",
			"github.com/commitdev/zero-deployable-react-frontend",
		},
		"Custom": []string{},
	}
}

func (registry Registry) availableLabels() []string {
	labels := make([]string, len(registry))
	i := 0
	for label := range registry {
		labels[i] = label
		i++
	}
	return labels
}

func chooseStack(registry Registry) []string {
	providerPrompt := promptui.Select{
		Label: "Pick a stack you'd like to use",
		Items: registry.availableLabels(),
	}
	_, providerResult, err := providerPrompt.Run()
	if err != nil {
		exit.Fatal("Prompt failed %v\n", err)
	}

	return registry[providerResult]
}

func fillProviderDetails(projectConfig *projectconfig.ZeroProjectConfig, s project.Secrets) {
	if projectConfig.Infrastructure.AWS != nil {
		sess, err := session.NewSession(&aws.Config{
			Region:      aws.String(projectConfig.Infrastructure.AWS.Region),
			Credentials: credentials.NewStaticCredentials(s.AWS.AccessKeyID, s.AWS.SecretAccessKey, ""),
		})

		svc := sts.New(sess)
		input := &sts.GetCallerIdentityInput{}

		awsCaller, err := svc.GetCallerIdentity(input)
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				default:
					exit.Error(aerr.Error())
				}
			} else {
				exit.Error(err.Error())
			}
		}

		if awsCaller != nil && awsCaller.Account != nil {
			projectConfig.Infrastructure.AWS.AccountID = *awsCaller.Account
		}
	}
}

func defaultProjConfig() projectconfig.ZeroProjectConfig {
	return projectconfig.ZeroProjectConfig{
		Name: "",
		Infrastructure: projectconfig.Infrastructure{
			AWS: nil,
		},

		Parameters: map[string]string{},
		Modules:    projectconfig.Modules{},
	}
}
